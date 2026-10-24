// Package discord implements the Discord adapter for the gateway: a
// standard-library WebSocket client for the Gateway v10 API plus a REST client
// for posting messages.
//
// Inbound, text only:
//
//   - MESSAGE_CREATE events become gateway.Inbound values. ChatID is the
//     channel id (a DM channel id or a guild channel id), UserID is the
//     author's id and UserName is the guild member's display name (nickname),
//     else the author's display name, else the username.
//   - Messages authored by bots, including the bot itself, are dropped. That
//     matches upstream's default DISCORD_ALLOW_BOTS=none.
//   - Guild channel messages are accepted only when they mention the bot, and
//     the bot's own mention is stripped from the text. Direct messages are
//     accepted without a mention. This matches upstream
//     discord.require_mention (default true) and its DM exemption.
//   - Message types other than DEFAULT (0) and REPLY (19) are ignored, as in
//     upstream's admission gate.
//   - AllowedChats, when non-empty, whitelists guild channel ids ("*" opens
//     every channel). Like upstream's discord.allowed_channels, DMs are not
//     covered by that list.
//
// Outbound: plain text via POST /channels/{channel_id}/messages, split at
// Discord's 2000-character limit, with 429 retry_after honoured. Sends are
// serialised so a reply reaches the channel in order.
//
// Deliberately not implemented: slash commands, components/buttons/modals,
// attachments and embeds, voice, reactions, typing indicators, message edits
// and deletes, thread semantics (a thread is just another channel id and still
// needs a mention), allowed-user and allowed-role gates, missed-message
// backfill, and RESUME. Without RESUME the adapter identifies again after a
// dropped session, so events Discord did not replay are lost.
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/gateway"
)

const (
	// DefaultAPIBase is the Discord REST endpoint for API v10.
	DefaultAPIBase = "https://discord.com/api/v10"
	// DefaultGatewayURL is the Discord gateway. The adapter appends
	// /?v=10&encoding=json.
	DefaultGatewayURL = "wss://gateway.discord.gg"

	// PlatformName is the session source recorded for Discord chats.
	PlatformName = "discord"

	defaultBackoffFloor = time.Second
	defaultBackoffMax   = 60 * time.Second
)

// Options configures an Adapter.
type Options struct {
	// Token is the bot token from the Discord Developer Portal. Required.
	Token string
	// APIBase is the REST base URL. Empty means DefaultAPIBase.
	APIBase string
	// GatewayURL is the gateway base URL. Empty means DefaultGatewayURL.
	GatewayURL string
	// AllowedChats whitelists guild channel ids. Empty allows every chat; "*"
	// also allows every chat.
	AllowedChats []string
	// Logger receives connection and drop diagnostics. Nil discards them.
	Logger *slog.Logger
	// HTTPClient is the REST client. Nil uses a client with a 30s timeout.
	HTTPClient *http.Client
}

// Adapter is the Discord implementation of gateway.Adapter. Create it with New.
type Adapter struct {
	token      string
	apiBase    string
	gatewayURL string
	allowed    map[string]struct{}
	allowAll   bool
	log        *slog.Logger
	httpClient *http.Client

	// sendMu serialises REST sends so replies keep their order and concurrent
	// callers cannot exceed the per-channel rate limit together.
	sendMu sync.Mutex

	// botID is the authenticated bot's user id, learned from READY. Only the
	// gateway read loop writes and reads it, so it needs no lock.
	botID string

	backoffMin time.Duration
	backoffMax time.Duration
}

// New validates opts and returns an adapter.
func New(opts Options) (*Adapter, error) {
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		return nil, errors.New("discord: bot token is required")
	}
	apiBase := strings.TrimRight(strings.TrimSpace(opts.APIBase), "/")
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	gatewayURL := strings.TrimSpace(opts.GatewayURL)
	if gatewayURL == "" {
		gatewayURL = DefaultGatewayURL
	}
	endpoint, err := gatewayEndpoint(gatewayURL)
	if err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	a := &Adapter{
		token:      token,
		apiBase:    apiBase,
		gatewayURL: endpoint,
		log:        logger,
		httpClient: client,
		backoffMin: defaultBackoffFloor,
		backoffMax: defaultBackoffMax,
	}
	for _, chat := range opts.AllowedChats {
		chat = strings.TrimSpace(chat)
		if chat == "" {
			continue
		}
		if chat == "*" {
			a.allowAll = true
			continue
		}
		if a.allowed == nil {
			a.allowed = make(map[string]struct{}, len(opts.AllowedChats))
		}
		a.allowed[chat] = struct{}{}
	}
	return a, nil
}

// Name implements gateway.Adapter.
func (a *Adapter) Name() string { return PlatformName }

// Run implements gateway.Adapter. It connects to the Discord gateway, hands
// every accepted message to handle, and reconnects with exponential backoff
// when the session drops. It returns nil when ctx is cancelled and an error
// only for permanent failures such as a rejected token or a bad intent.
func (a *Adapter) Run(ctx context.Context, handle func(gateway.Inbound) error) error {
	return a.runGateway(ctx, handle)
}

// gatewayEndpoint adds the v10 query the Discord gateway expects.
func gatewayEndpoint(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("discord: parse gateway url: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("discord: gateway url %q has no host", base)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	q := u.Query()
	q.Set("v", "10")
	q.Set("encoding", "json")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// authHeader is sent with the gateway upgrade. Discord's gateway does not
// require it (IDENTIFY carries the token), but it keeps the credential on the
// handshake as well.
func (a *Adapter) authHeader() http.Header {
	header := http.Header{}
	header.Set("Authorization", "Bot "+a.token)
	return header
}

// chatAllowed reports whether the guild channel passes the AllowedChats gate.
func (a *Adapter) chatAllowed(channelID string) bool {
	if len(a.allowed) == 0 || a.allowAll {
		return true
	}
	_, ok := a.allowed[channelID]
	return ok
}

// discordMessage is the MESSAGE_CREATE payload subset this adapter reads.
type discordMessage struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	Content   string `json:"content"`
	Type      int    `json:"type"`
	Author    struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
		Bot        bool   `json:"bot"`
	} `json:"author"`
	Member *struct {
		Nick string `json:"nick"`
	} `json:"member"`
	Mentions []struct {
		ID string `json:"id"`
	} `json:"mentions"`
}

// Discord message types this adapter accepts. Everything else (system
// messages, application commands, thread starters) is out of scope.
const (
	messageTypeDefault = 0
	messageTypeReply   = 19
)

// handleMessage applies the ingress rules and forwards accepted messages.
func (a *Adapter) handleMessage(_ context.Context, msg discordMessage, handle func(gateway.Inbound) error) {
	if msg.Author.Bot {
		a.log.Debug("discord: ignoring bot message", "channel_id", msg.ChannelID, "author_id", msg.Author.ID)
		return
	}
	if msg.Author.ID == "" || msg.ChannelID == "" {
		a.log.Debug("discord: ignoring malformed message", "message_id", msg.ID)
		return
	}
	if msg.Type != messageTypeDefault && msg.Type != messageTypeReply {
		a.log.Debug("discord: ignoring unsupported message type", "type", msg.Type, "message_id", msg.ID)
		return
	}
	if msg.GuildID != "" {
		if !a.chatAllowed(msg.ChannelID) {
			a.log.Debug("discord: ignoring message in a channel outside allowed_chats", "channel_id", msg.ChannelID)
			return
		}
		if !a.mentionsSelf(&msg) {
			a.log.Debug("discord: ignoring guild message without a mention", "channel_id", msg.ChannelID)
			return
		}
	}
	text := strings.TrimSpace(a.stripSelfMention(msg.Content))
	if text == "" {
		a.log.Debug("discord: ignoring message with no text", "channel_id", msg.ChannelID, "message_id", msg.ID)
		return
	}
	chatID := msg.ChannelID
	inbound := gateway.Inbound{
		Platform: PlatformName,
		ChatID:   chatID,
		UserID:   msg.Author.ID,
		UserName: displayName(msg),
		Text:     text,
		Send: func(ctx context.Context, text string) error {
			return a.Send(ctx, chatID, text)
		},
	}
	if err := handle(inbound); err != nil {
		a.log.Warn("discord: inbound handler failed", "channel_id", chatID, "error", err)
	}
}

// displayName prefers the guild nickname, then the account's display name, then
// the username.
func displayName(msg discordMessage) string {
	if msg.Member != nil {
		if nick := strings.TrimSpace(msg.Member.Nick); nick != "" {
			return nick
		}
	}
	if name := strings.TrimSpace(msg.Author.GlobalName); name != "" {
		return name
	}
	if name := strings.TrimSpace(msg.Author.Username); name != "" {
		return name
	}
	return msg.Author.ID
}

// mentionsSelf reports whether the message addresses the bot, either through
// the mentions array or through a raw mention token in the content.
func (a *Adapter) mentionsSelf(msg *discordMessage) bool {
	if a.botID == "" {
		return false
	}
	for _, mention := range msg.Mentions {
		if mention.ID == a.botID {
			return true
		}
	}
	return mentionsUserID(msg.Content, a.botID)
}

func mentionsUserID(content, userID string) bool {
	if userID == "" {
		return false
	}
	return strings.Contains(content, "<@"+userID+">") ||
		strings.Contains(content, "<@!"+userID+">")
}

// stripSelfMention removes the bot's own mention tokens from the content.
func (a *Adapter) stripSelfMention(content string) string {
	if a.botID == "" {
		return content
	}
	content = strings.ReplaceAll(content, "<@!"+a.botID+">", "")
	return strings.ReplaceAll(content, "<@"+a.botID+">", "")
}

// FromConfig builds Options from config.yaml (discord.token,
// discord.allowed_channels, discord.allowed_chats) with environment fallbacks
// (DISCORD_BOT_TOKEN, DISCORD_ALLOWED_CHANNELS, DISCORD_ALLOWED_CHATS).
// lookup may be nil when no environment is available.
func FromConfig(cfg *config.Config, lookup func(string) (string, bool), logger *slog.Logger) (Options, error) {
	opts := Options{Logger: logger}
	if cfg != nil {
		opts.Token = strings.TrimSpace(cfg.String("discord", "token"))
		opts.AllowedChats = append(opts.AllowedChats, configStringList(cfg, "discord", "allowed_channels")...)
		opts.AllowedChats = append(opts.AllowedChats, configStringList(cfg, "discord", "allowed_chats")...)
	}
	if opts.Token == "" && lookup != nil {
		if value, ok := lookup("DISCORD_BOT_TOKEN"); ok {
			opts.Token = strings.TrimSpace(value)
		}
	}
	if len(opts.AllowedChats) == 0 && lookup != nil {
		for _, key := range []string{"DISCORD_ALLOWED_CHANNELS", "DISCORD_ALLOWED_CHATS"} {
			value, ok := lookup(key)
			if !ok || strings.TrimSpace(value) == "" {
				continue
			}
			opts.AllowedChats = splitCSV(value)
			break
		}
	}
	if opts.Token == "" {
		return Options{}, errors.New("discord: no bot token; set discord.token in config.yaml or DISCORD_BOT_TOKEN")
	}
	return opts, nil
}

// configStringList reads a string, a CSV string, or a list of strings and
// numbers from cfg at path.
func configStringList(cfg *config.Config, path ...string) []string {
	value, ok := cfg.Get(path...)
	if !ok {
		return nil
	}
	switch v := value.(type) {
	case string:
		return splitCSV(v)
	case []string:
		return splitCSV(strings.Join(v, ","))
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			switch t := item.(type) {
			case string:
				out = append(out, strings.TrimSpace(t))
			case int:
				out = append(out, fmt.Sprint(t))
			case int64:
				out = append(out, fmt.Sprint(t))
			case float64:
				if t == float64(int64(t)) {
					out = append(out, fmt.Sprint(int64(t)))
				}
			}
		}
		return out
	default:
		return nil
	}
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
