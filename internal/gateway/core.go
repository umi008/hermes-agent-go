package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/umi008/hermes-agent-go/internal/tools/approval"
)

// Session is one live conversation bound to an external chat. The CLI opens
// such a conversation per chat through a SessionFactory and keeps it until the
// gateway shuts down.
type Session interface {
	// ID is the session id recorded in the store, for logs.
	ID() string
	// Run executes one turn. Text the agent produces is written to the sink
	// the session was opened with, and ctx cancellation interrupts the turn.
	Run(ctx context.Context, input string) error
	// End closes the session with a reason, as upstream "gateway_close".
	End(reason string)
}

// SessionFactory opens a conversation for one chat. source is the platform
// name ("telegram"), which the session stores as its source and shows to the
// prompt; out receives the turn's text.
type SessionFactory func(ctx context.Context, source string, out io.Writer) Session

const (
	// DefaultMaxChunk is Telegram's message limit, the smallest limit of the
	// platforms this port serves. Replies longer than it are split.
	DefaultMaxChunk = 4096
	// closeReason is the upstream close reason recorded when the gateway
	// stops: sessions end with "gateway_close", never silently.
	closeReason = "gateway_close"
)

// Options configure a Core. Everything that touches the outside world is
// injected, so the core runs under a fake adapter, a fake session factory and a
// fake clock in tests.
type Options struct {
	// NewSession is required: it opens one session per chat.
	NewSession SessionFactory
	// Logger receives the gateway's debug and warning lines. Nil discards.
	Logger *slog.Logger
	// Now is the clock used for log timestamps. Nil means time.Now.
	Now func() time.Time
	// MaxChunk caps one outgoing message in characters. Zero means
	// DefaultMaxChunk.
	MaxChunk int
	// AllowedChats is gateway.allowed_chats. When it is non-empty a chat that
	// is not listed is ignored and logged at debug level; an empty list
	// serves every chat.
	AllowedChats []string
}

// Core is the platform-independent gateway: one session per external chat, a
// per-chat message queue and worker, replies delivered back to the chat, and
// dangerous-command approvals asked inside the conversation.
type Core struct {
	o       Options
	allowed map[string]bool

	mu     sync.Mutex
	runCtx context.Context
	chats  map[string]*chat
	closed bool
}

// New builds a Core. It does not contact any platform; Run does that.
func New(o Options) (*Core, error) {
	if o.NewSession == nil {
		return nil, errors.New("gateway: NewSession is required")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxChunk <= 0 {
		o.MaxChunk = DefaultMaxChunk
	}
	c := &Core{o: o, chats: map[string]*chat{}, allowed: map[string]bool{}}
	for _, id := range o.AllowedChats {
		if id = strings.TrimSpace(id); id != "" {
			c.allowed[id] = true
		}
	}
	return c, nil
}

// Approve is the approval.Prompter for the whole process: the running turn's
// context carries the chat that started it, so one shared command gate can ask
// the conversation that triggered the command. An approval asked outside a
// turn is denied rather than left waiting.
func (c *Core) Approve(ctx context.Context, r approval.Request) (approval.Choice, error) {
	ch, ok := chatFrom(ctx)
	if !ok {
		return approval.Deny, errors.New("gateway: approval asked outside a chat")
	}
	return ch.ask(ctx, r)
}

// Run serves the adapter until ctx ends or the adapter stops on its own, then
// waits for every chat worker and ends its session with gateway_close. A
// cancelled ctx is a clean stop and returns nil.
func (c *Core) Run(ctx context.Context, a Adapter) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c.mu.Lock()
	c.runCtx = ctx
	c.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, c.Handle) }()

	var err error
	select {
	case <-ctx.Done():
	case err = <-done:
	}
	cancel()
	c.shutdown()
	return err
}

// Handle accepts one inbound message and returns at once: the chat's worker
// owns the turn, so a slow model never stalls the adapter's receive loop.
func (c *Core) Handle(in Inbound) error {
	if in.Send == nil {
		c.o.Logger.Debug("drop message from a chat that cannot be answered",
			"platform", in.Platform, "chat", in.ChatID)
		return nil
	}
	if len(c.allowed) > 0 && !c.allowed[in.ChatID] {
		c.o.Logger.Debug("chat is not in gateway.allowed_chats",
			"platform", in.Platform, "chat", in.ChatID, "user", in.UserID)
		return nil
	}
	if strings.TrimSpace(in.Text) == "" {
		c.o.Logger.Debug("drop empty message", "platform", in.Platform, "chat", in.ChatID)
		return nil
	}
	ch, err := c.chatFor(in.Platform, in.ChatID)
	if err != nil {
		return err
	}
	ch.enqueue(in)
	return nil
}

// chatFor returns the chat's live state, starting its worker on first use.
func (c *Core) chatFor(platform, id string) (*chat, error) {
	key := platform + ":" + id
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("gateway: shutting down")
	}
	if ch, ok := c.chats[key]; ok {
		return ch, nil
	}
	ctx := c.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	ch := newChat(c, platform, id)
	c.chats[key] = ch
	go ch.serve(ctx)
	return ch, nil
}

// shutdown stops every worker (the run ctx is already cancelled, which cancels
// the running turns) and waits until each session has been ended.
func (c *Core) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	chats := make([]*chat, 0, len(c.chats))
	for _, ch := range c.chats {
		chats = append(chats, ch)
	}
	c.mu.Unlock()
	for _, ch := range chats {
		<-ch.done
	}
	c.o.Logger.Debug("gateway stopped", "chats", len(chats), "at", c.o.Now())
}

// sink is the session's output: it accumulates one turn's text so the core can
// deliver it as platform messages.
type sink struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *sink) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

func (s *sink) take() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	text := strings.TrimSpace(s.b.String())
	s.b.Reset()
	return text
}
