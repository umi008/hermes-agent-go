package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

// Upstream reconnect policy (tools/mcp_tool.py, mcp_tool_server_run.py).
const (
	maxInitialRetries   = 3
	maxReconnectRetries = 5
	maxBackoff          = 60 * time.Second
	parkedInterval      = 300 * time.Second
	// respawnWait bounds a call waiting for a reconnect
	// (_STDIO_RESPAWN_WAIT_SEC).
	respawnWait = 15 * time.Second

	sessionUncertainMsg = "The MCP transport session to '%s' expired while this write-capable call was in flight, so " +
		"the outcome is UNKNOWN — the operation may or may not have taken effect server-side. It was NOT automatically " +
		"retried to avoid a duplicate side effect. The connection has %s. Verify whether the operation took effect " +
		"(e.g. with a read-only tool) before re-invoking it."
)

var errNoEndpoint = errors.New("no 'command' or 'url' in config")

// conn is one server: the current session, swapped by the supervisor
// when the transport drops.
type conn struct {
	server Server
	opts   Options
	// tools and caps come from the first session and stay registered
	// across reconnects.
	tools []*mcp.Tool
	caps  *mcp.ServerCapabilities

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu          sync.Mutex
	session     *mcp.ClientSession // nil while reconnecting or parked
	stopSession context.CancelFunc
	changed     chan struct{} // closed and replaced on every state change
	retries     int           // reconnect attempts since the last proven session
	parked      bool
	closed      bool
}

func newConn(ctx context.Context, s Server, o Options) *conn {
	cctx, cancel := context.WithCancel(ctx)
	return &conn{server: s, opts: o, ctx: cctx, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{})}
}

// signal wakes calls waiting for a state change; c.mu must be held.
func (c *conn) signal() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// publish makes session current; false when the conn closed meanwhile.
func (c *conn) publish(session *mcp.ClientSession, stop context.CancelFunc) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		_ = session.Close()
		stop()
		return false
	}
	if c.parked {
		// A probe that reconnects keeps the budget spent: the next drop
		// parks again unless a call proves this session first.
		c.retries, c.parked = maxReconnectRetries, false
	}
	c.session, c.stopSession = session, stop
	c.signal()
	return true
}

// supervise waits for the current session to end and reconnects with
// upstream's backoff: up to five jittered exponential retries, then a
// parked probe every five minutes.
func (c *conn) supervise() {
	defer close(c.done)
	for {
		c.mu.Lock()
		s := c.session
		c.mu.Unlock()
		_ = s.Wait()
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		stop := c.stopSession
		c.session, c.stopSession = nil, nil
		c.signal()
		c.mu.Unlock()
		stop()
		c.opts.Logger.Warn("MCP server connection lost, reconnecting", "server", c.server.Name)
		session, stop, ok := c.reconnect()
		if !ok || !c.publish(session, stop) {
			return
		}
		c.opts.Logger.Info("MCP server reconnected", "server", c.server.Name)
	}
}

func (c *conn) reconnect() (*mcp.ClientSession, context.CancelFunc, bool) {
	for {
		c.mu.Lock()
		c.retries++
		n := c.retries
		parking := n > maxReconnectRetries && !c.parked
		if parking {
			c.parked = true
			c.signal()
		}
		parked := c.parked
		c.mu.Unlock()
		if parking {
			c.opts.Logger.Warn("MCP server parked after failed reconnects, probing periodically", "server", c.server.Name,
				"retries", maxReconnectRetries, "interval", parkedInterval)
		}
		d := parkedInterval
		if !parked {
			d = jittered(backoff(n - 1))
		}
		if c.opts.wait(c.ctx, d) != nil {
			return nil, nil, false
		}
		session, stop, err := c.open()
		if err == nil {
			return session, stop, true
		}
		if c.ctx.Err() != nil {
			return nil, nil, false
		}
		c.opts.Logger.Warn("MCP server reconnect failed", "server", c.server.Name, "attempt", n, "err", sanitize(err.Error()))
	}
}

// awaitSession returns a current session other than old, waiting for a
// reconnect in flight up to respawnWait. It returns nil at once when the
// server is parked or closed.
func (c *conn) awaitSession(ctx context.Context, old *mcp.ClientSession) *mcp.ClientSession {
	timer := time.NewTimer(c.opts.respawnWait)
	defer timer.Stop()
	for {
		c.mu.Lock()
		s, ch, gone := c.session, c.changed, c.parked || c.closed
		c.mu.Unlock()
		if s != nil && s != old {
			return s
		}
		if gone {
			return nil
		}
		select {
		case <-ch:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// call runs one request on the current session. A transport drop
// mid-request is replayed once on the new session only when retryable
// (a read-only tool or a utility); otherwise the outcome is unknown.
func (c *conn) call(ctx context.Context, retryable bool, fn func(context.Context, *mcp.ClientSession) error) error {
	s := c.awaitSession(ctx, nil)
	if s == nil {
		return c.notConnected()
	}
	err := c.attempt(ctx, s, fn)
	if err != nil && dropped(err) {
		next := c.awaitSession(ctx, s)
		if !retryable {
			state := "not recovered yet"
			if next != nil {
				state = "been re-established"
			}
			return tools.Error(fmt.Sprintf(sessionUncertainMsg, c.server.Name, state))
		}
		if next == nil {
			return c.notConnected()
		}
		err = c.attempt(ctx, next, fn)
	}
	if err != nil {
		return tools.Error(sanitize("MCP call failed: " + err.Error()))
	}
	return nil
}

func (c *conn) attempt(ctx context.Context, s *mcp.ClientSession, fn func(context.Context, *mcp.ClientSession) error) error {
	cctx, cancel := context.WithTimeout(ctx, c.callTimeout())
	defer cancel()
	if err := fn(cctx, s); err != nil {
		return err
	}
	// A served call proves the session and resets the retry budget.
	c.mu.Lock()
	if c.session == s {
		c.retries = 0
	}
	c.mu.Unlock()
	return nil
}

func (c *conn) notConnected() error {
	return tools.Error(fmt.Sprintf("MCP server '%s' is not connected", c.server.Name))
}

// close ends the session and stops the supervisor.
func (c *conn) close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	s, stop := c.session, c.stopSession
	c.signal()
	c.mu.Unlock()
	var err error
	if s != nil {
		if err = s.Close(); err != nil && dropped(err) {
			err = nil
		}
	}
	c.cancel()
	if stop != nil {
		stop()
	}
	<-c.done
	return err
}

// permanent is upstream _classify_mcp_failure's permanent class: a retry
// cannot fix a missing command, a missing endpoint or a refused login.
func permanent(err error) bool {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNoEndpoint) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"401", "403", "Unauthorized", "Forbidden"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// backoff is 1s doubling per attempt, capped at a minute.
func backoff(attempt int) time.Duration {
	if attempt >= 6 {
		return maxBackoff
	}
	return min(time.Second<<attempt, maxBackoff)
}

// jittered is upstream _jittered: +/-20% uniform jitter.
func jittered(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dropped reports a request that failed because the transport ended.
func dropped(err error) bool {
	return errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
