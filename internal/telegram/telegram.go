// Package telegram is the Telegram Bot API adapter for the gateway: long
// polling with getUpdates in, sendMessage out, text only.
//
// Deliberately not ported from upstream plugins/platforms/telegram: inline
// keyboards and approval buttons (approvals are typed in the chat), forum
// topics, media, attachments, voice, reactions, typing indicators and
// streaming message edits. The port answers text messages and asks text
// questions; everything else is ignored.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/umi008/hermes-agent-go/internal/gateway"
)

const (
	// MaxMessageLength is Telegram's limit for one message text, in UTF-16
	// code units. The gateway splits replies with a rune budget, which is
	// never larger.
	MaxMessageLength = gateway.DefaultMaxChunk
	// DefaultAPIBase is the public Bot API. Tests point it at an httptest
	// server through Options.APIBase.
	DefaultAPIBase = "https://api.telegram.org"

	defaultPollTimeout = 30 * time.Second
	// pollGrace is added to the long-poll timeout as the HTTP deadline, so the
	// request outlives the server-side wait.
	pollGrace = 15 * time.Second
	// sendTimeout bounds one sendMessage round trip.
	sendTimeout = 30 * time.Second
	// defaultAttempts is how many times one call is tried on 429 and network
	// errors.
	defaultAttempts = 3
	// emptyPagePause keeps a hot loop off the API when getUpdates returns
	// without waiting: real long polls hold the request open instead.
	emptyPagePause = 250 * time.Millisecond
	maxBackoff     = 30 * time.Second
	maxBodyBytes   = 4 << 20
)

// Options configure an Adapter.
type Options struct {
	// Token is the bot token from gateway.telegram.token or
	// TELEGRAM_BOT_TOKEN.
	Token string
	// APIBase overrides the Bot API root (gateway.telegram.api_base).
	APIBase string
	// HTTP is the client; nil means a client with no global timeout, since
	// long polling owns its own deadline per request.
	HTTP *http.Client
	// Logger receives polling and retry lines. Nil discards.
	Logger *slog.Logger
	// PollTimeout is the getUpdates long-poll timeout. Zero means 30s.
	PollTimeout time.Duration
	// Sleep waits between retries. Tests replace it to stay fast and to
	// observe the 429 retry_after delays.
	Sleep func(ctx context.Context, d time.Duration) error
	// Attempts bounds the retries of one call on 429 and network errors. Zero
	// means 3.
	Attempts int
}

// Adapter is the Telegram platform. It implements gateway.Adapter.
type Adapter struct {
	o      Options
	base   string
	client *http.Client
}

// New builds an adapter. It does not call the Bot API; Run does that.
func New(o Options) (*Adapter, error) {
	if strings.TrimSpace(o.Token) == "" {
		return nil, errors.New("telegram: token is required")
	}
	if o.APIBase == "" {
		o.APIBase = DefaultAPIBase
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.PollTimeout <= 0 {
		o.PollTimeout = defaultPollTimeout
	}
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	if o.Attempts <= 0 {
		o.Attempts = defaultAttempts
	}
	return &Adapter{o: o, base: strings.TrimRight(o.APIBase, "/"), client: o.HTTP}, nil
}

// Name is the platform name, used as the session source.
func (a *Adapter) Name() string { return "telegram" }

// Run long-polls getUpdates and hands every text message to handle until ctx
// ends. handle returns at once (the core queues per chat), so a slow turn never
// stalls this loop.
func (a *Adapter) Run(ctx context.Context, handle func(gateway.Inbound) error) error {
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		updates, err := a.poll(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			in, ok := a.inbound(u)
			if !ok {
				continue
			}
			if err := handle(in); err != nil {
				a.o.Logger.Debug("gateway rejected an update",
					"chat", in.ChatID, "err", err)
			}
		}
		if len(updates) == 0 {
			// Real long polls hold the request open; keep a hot loop off the
			// API when a page comes back empty immediately.
			_ = a.o.Sleep(ctx, emptyPagePause)
		}
	}
}

// inbound turns one update into a message. Only text messages become turns:
// edited messages, media, stickers and service updates are ignored.
func (a *Adapter) inbound(u update) (gateway.Inbound, bool) {
	m := u.Message
	if m == nil || m.Text == "" || m.From == nil {
		return gateway.Inbound{}, false
	}
	if m.From.IsBot {
		// Never answer another bot: two bots can otherwise loop forever.
		return gateway.Inbound{}, false
	}
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	name := strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	if name == "" {
		name = m.From.Username
	}
	return gateway.Inbound{
		Platform: a.Name(),
		ChatID:   chatID,
		UserID:   strconv.FormatInt(m.From.ID, 10),
		UserName: name,
		Text:     m.Text,
		Send:     a.sender(chatID),
	}, true
}

// sender is the reply channel for one chat: every Send becomes one
// sendMessage. The gateway already chunks to MaxMessageLength; this splits
// again only so a caller that skipped that cannot post an over-long message.
func (a *Adapter) sender(chatID string) func(ctx context.Context, text string) error {
	return func(ctx context.Context, text string) error {
		for _, chunk := range gateway.Split(text, MaxMessageLength) {
			if err := a.sendMessage(ctx, chatID, chunk); err != nil {
				return err
			}
		}
		return nil
	}
}

// poll fetches the next page of updates. Polling retries forever while ctx
// lives: a dropped network is not a reason to take the gateway down.
func (a *Adapter) poll(ctx context.Context, offset int64) ([]update, error) {
	var out []update
	body := pollBody{
		Offset:         offset,
		Timeout:        int(a.o.PollTimeout / time.Second),
		AllowedUpdates: []string{"message"},
	}
	if err := a.call(ctx, "getUpdates", body, &out, a.o.PollTimeout+pollGrace, 0); err != nil {
		return nil, err
	}
	return out, nil
}

// sendMessage posts one reply chunk.
func (a *Adapter) sendMessage(ctx context.Context, chatID, text string) error {
	body := sendBody{ChatID: chatID, Text: text, DisableWebPagePreview: true}
	return a.call(ctx, "sendMessage", body, nil, sendTimeout, a.o.Attempts)
}

// call posts one Bot API method and retries the failures Telegram expects a
// client to retry: 429 (honouring `parameters.retry_after`) and network errors,
// with exponential backoff. attempts <= 0 means retry until ctx ends.
func (a *Adapter) call(ctx context.Context, method string, body, out any, timeout time.Duration, attempts int) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("telegram %s: encode request: %w", method, err)
	}
	var wait time.Duration
	var retry bool
	for attempt := 0; ; attempt++ {
		err := a.once(ctx, method, payload, out, timeout)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait, retry = a.retryWait(err, attempt)
		if !retry || (attempts > 0 && attempt+1 >= attempts) {
			return err
		}
		a.o.Logger.Warn("telegram call failed, retrying",
			"method", method, "attempt", attempt+1, "retry_in", wait.String(), "err", err)
		if err := a.o.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// retryWait decides whether err is retryable and how long to wait first.
func (a *Adapter) retryWait(err error, attempt int) (time.Duration, bool) {
	var ae *apiError
	if errors.As(err, &ae) {
		if ae.Code != http.StatusTooManyRequests {
			return 0, false
		}
		if ae.RetryAfter > 0 {
			return ae.RetryAfter, true
		}
		return backoff(attempt), true
	}
	var network *retryableError
	if errors.As(err, &network) {
		return backoff(attempt), true
	}
	return 0, false
}

// once performs exactly one HTTP round trip.
func (a *Adapter) once(ctx context.Context, method string, payload []byte, out any, timeout time.Duration) error {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := fmt.Sprintf("%s/bot%s/%s", a.base, a.o.Token, method)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("telegram %s: build request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return &retryableError{fmt.Errorf("telegram %s: %w", method, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return &retryableError{fmt.Errorf("telegram %s: read response: %w", method, err)}
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		// A proxy or a reset connection can answer with something that is not
		// JSON; treat it like a network error.
		return &retryableError{fmt.Errorf("telegram %s: decode response (HTTP %d): %w", method, resp.StatusCode, err)}
	}
	if !env.OK || resp.StatusCode >= http.StatusBadRequest {
		e := &apiError{Code: env.ErrorCode, Description: env.Description}
		if e.Code == 0 {
			e.Code = resp.StatusCode
		}
		if env.Parameters != nil {
			e.RetryAfter = time.Duration(env.Parameters.RetryAfter) * time.Second
		}
		if e.Description == "" {
			e.Description = resp.Status
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("telegram %s: decode result: %w", method, err)
	}
	return nil
}

// apiError is one error response from the Bot API.
type apiError struct {
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *apiError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("telegram: %d %s (retry after %s)", e.Code, e.Description, e.RetryAfter)
	}
	return fmt.Sprintf("telegram: %d %s", e.Code, e.Description)
}

// retryableError marks a transport failure, which is worth retrying.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// backoff is 500ms, 1s, 2s, ... capped at maxBackoff.
func backoff(attempt int) time.Duration {
	if attempt > 8 {
		return maxBackoff
	}
	if d := 500 * time.Millisecond << attempt; d < maxBackoff {
		return d
	}
	return maxBackoff
}

// sleepCtx waits for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// envelope is the Bot API response wrapper.
type envelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// update is the subset of a getUpdates entry this adapter reads. requested with
// allowed_updates=["message"], so no edited_message or callback_query arrives.
type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

type message struct {
	MessageID int64  `json:"message_id"`
	From      *user  `json:"from"`
	Chat      chat   `json:"chat"`
	Text      string `json:"text"`
	Date      int64  `json:"date"`
}

type user struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// pollBody is the getUpdates request.
type pollBody struct {
	Offset         int64    `json:"offset"`
	Timeout        int      `json:"timeout"`
	AllowedUpdates []string `json:"allowed_updates"`
}

// sendBody is the sendMessage request. Only text: no reply_markup, no topics,
// no attachments.
type sendBody struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}
