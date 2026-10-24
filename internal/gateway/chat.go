package gateway

import (
	"context"
	"sync"
)

// chat is one external conversation: its live session, the queue of messages
// waiting for it, and the only way back to the platform.
//
// Exactly one goroutine consumes the queue. While it runs a turn it does not
// read, so a message that arrives mid-turn waits for the next turn; while an
// approval is pending, the same goroutine reads, so the next message is the
// answer and never starts a turn.
type chat struct {
	core     *Core
	platform string
	id       string

	sink  *sink
	done  chan struct{} // closed after the session is ended
	wake  chan struct{} // buffered(1): "the queue is not empty"
	mu    sync.Mutex
	send  func(ctx context.Context, text string) error
	user  string
	inbox []string
}

func newChat(c *Core, platform, id string) *chat {
	return &chat{
		core:     c,
		platform: platform,
		id:       id,
		sink:     &sink{},
		done:     make(chan struct{}),
		wake:     make(chan struct{}, 1),
	}
}

// serve is the chat's single worker: it opens the session, runs one turn per
// queued message, and ends the session when the gateway stops.
func (c *chat) serve(ctx context.Context) {
	var s Session
	defer func() {
		if s != nil {
			s.End(closeReason)
		}
		close(c.done)
	}()
	s = c.core.o.NewSession(ctx, c.platform, c.sink)
	c.core.o.Logger.Debug("chat session opened",
		"platform", c.platform, "chat", c.id, "session", s.ID(), "at", c.core.o.Now())

	for {
		text, ok := c.next(ctx)
		if !ok {
			return
		}
		c.sink.reset()
		start := c.core.o.Now()
		err := s.Run(withChat(ctx, c), text)
		reply := c.sink.take()
		if ctx.Err() != nil {
			// The gateway is stopping: the partial turn is not delivered.
			return
		}
		if err != nil {
			c.core.o.Logger.Warn("gateway turn failed",
				"platform", c.platform, "chat", c.id, "session", s.ID(), "err", err)
			c.reply(ctx, "⚠️ Error: "+err.Error())
			continue
		}
		c.core.o.Logger.Debug("gateway turn done",
			"platform", c.platform, "chat", c.id, "session", s.ID(),
			"took", c.core.o.Now().Sub(start).String(), "chars", len(reply))
		c.reply(ctx, reply)
	}
}

// enqueue adds a message to the chat's queue and wakes the worker. It never
// blocks, so the adapter keeps receiving.
func (c *chat) enqueue(in Inbound) {
	c.mu.Lock()
	c.send = in.Send
	c.user = in.UserName
	c.inbox = append(c.inbox, in.Text)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default: // one pending wake-up is enough: the worker drains the queue
	}
}

// next takes the oldest queued message, waiting until one arrives or ctx ends.
// The worker and an approval share it, one at a time, on the chat's goroutine.
func (c *chat) next(ctx context.Context) (string, bool) {
	for {
		c.mu.Lock()
		if len(c.inbox) > 0 {
			text := c.inbox[0]
			c.inbox = c.inbox[1:]
			c.mu.Unlock()
			return text, true
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", false
		case <-c.wake:
		}
	}
}

// sender is the latest send function for this chat, nil before the first
// message.
func (c *chat) sender() func(ctx context.Context, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.send
}

// say delivers one gateway message (a prompt, never a reply chunk).
func (c *chat) say(ctx context.Context, text string) error {
	send := c.sender()
	if send == nil {
		return nil
	}
	if err := send(ctx, text); err != nil {
		c.core.o.Logger.Warn("gateway send failed",
			"platform", c.platform, "chat", c.id, "err", err)
		return err
	}
	return nil
}

// reply delivers one turn's text, split into platform-sized chunks.
func (c *chat) reply(ctx context.Context, text string) {
	send := c.sender()
	if send == nil {
		return
	}
	for _, chunk := range Split(text, c.core.o.MaxChunk) {
		if err := send(ctx, chunk); err != nil {
			c.core.o.Logger.Warn("gateway send failed",
				"platform", c.platform, "chat", c.id, "err", err)
			return
		}
	}
}

// chatContext carries the chat through a turn, so a shared approval gate can
// route its question back to the conversation that made the command.
type chatContext struct{}

func withChat(ctx context.Context, c *chat) context.Context {
	return context.WithValue(ctx, chatContext{}, c)
}

func chatFrom(ctx context.Context) (*chat, bool) {
	ch, ok := ctx.Value(chatContext{}).(*chat)
	return ch, ok
}
