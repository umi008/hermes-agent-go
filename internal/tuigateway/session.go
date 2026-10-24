package tuigateway

import (
	"context"
	"fmt"
	"sync"

	"github.com/umi008/hermes-agent-go/internal/agent"
)

// liveSession is a session bound to the connection that created it. At most
// one turn runs at a time; inputs submitted meanwhile wait in a FIFO.
type liveSession struct {
	srv  *Server
	conn *conn
	id   string
	sess Session

	mu          sync.Mutex
	running     bool
	interrupted bool
	closed      bool
	cancel      context.CancelFunc
	done        chan struct{}
	queue       []string
}

type sessionKey struct{}

func withSession(ctx context.Context, ls *liveSession) context.Context {
	return context.WithValue(ctx, sessionKey{}, ls)
}

func sessionFrom(ctx context.Context) (*liveSession, bool) {
	ls, ok := ctx.Value(sessionKey{}).(*liveSession)
	return ls, ok
}

// start runs text now, or queues it behind the running turn. It reports
// whether the turn started.
func (ls *liveSession) start(text string) bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.running {
		ls.queue = append(ls.queue, text)
		return false
	}
	return ls.launch(text)
}

// launch starts a turn goroutine; ls.mu is held. It refuses once the server
// is closing, so Close never races a new turn into its wait group.
func (ls *liveSession) launch(text string) bool {
	ls.srv.mu.Lock()
	defer ls.srv.mu.Unlock()
	if ls.srv.closed || ls.closed {
		ls.running = false
		return false
	}
	ctx, cancel := context.WithCancel(withSession(ls.srv.base, ls))
	ls.running, ls.interrupted, ls.cancel, ls.done = true, false, cancel, make(chan struct{})
	ls.srv.wg.Add(1)
	go ls.turn(ctx, text, ls.done)
	return true
}

func (ls *liveSession) turn(ctx context.Context, text string, done chan struct{}) {
	defer ls.srv.wg.Done()
	defer close(done)
	ls.conn.event("message.start", ls.id, map[string]any{})
	reply, err := ls.sess.Run(ctx, text)

	ls.mu.Lock()
	interrupted := ls.interrupted
	ls.mu.Unlock()
	payload := map[string]any{"text": reply, "status": "complete"}
	switch {
	case interrupted:
		payload["status"] = "interrupted"
	case err != nil:
		payload["status"] = "error"
		payload["warning"] = err.Error()
	}
	ls.conn.event("message.complete", ls.id, payload)

	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.cancel()
	ls.running = false
	if len(ls.queue) > 0 && !ls.closed {
		next := ls.queue[0]
		ls.queue = ls.queue[1:]
		ls.launch(next)
	}
}

// interrupt cancels the running turn and reports whether one was running.
func (ls *liveSession) interrupt() bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if !ls.running {
		return false
	}
	ls.interrupted = true
	ls.cancel()
	return true
}

// close drops queued input, interrupts the running turn, waits for it and ends
// the session. The turn's open requests end with it, since they wait on its
// context.
func (ls *liveSession) close() {
	ls.mu.Lock()
	ls.closed = true
	ls.queue = nil
	var done chan struct{}
	if ls.running {
		ls.interrupted = true
		ls.cancel()
		done = ls.done
	}
	ls.mu.Unlock()
	if done != nil {
		<-done
	}
	ls.sess.End(closeReason)
}

// emit maps an agent event to the TUI's session events.
func (ls *liveSession) emit(e agent.Event) {
	switch e.Kind {
	case agent.EventText:
		ls.conn.event("message.delta", ls.id, map[string]any{"text": e.Text})
	case agent.EventReasoning:
		ls.conn.event("reasoning.delta", ls.id, map[string]any{"text": e.Text})
	case agent.EventToolStart:
		if e.ToolCall != nil {
			ls.conn.event("tool.start", ls.id, map[string]any{
				"tool_id": e.ToolCall.ID, "name": e.ToolCall.Name, "args_text": string(e.ToolCall.Arguments),
			})
		}
	case agent.EventToolResult:
		if e.ToolCall != nil {
			ls.conn.event("tool.complete", ls.id, map[string]any{
				"tool_id": e.ToolCall.ID, "name": e.ToolCall.Name, "result_text": e.Text,
			})
		}
	case agent.EventRetry:
		ls.conn.event("status.update", ls.id, map[string]any{
			"kind": "status", "text": fmt.Sprintf("retrying (attempt %d): %v", e.Attempt, e.Err),
		})
	case agent.EventCompress:
		text := "context compacted"
		if e.Err != nil {
			text = "context compaction failed: " + e.Err.Error()
		}
		ls.conn.event("status.update", ls.id, map[string]any{"kind": "compacting", "text": text})
	}
}
