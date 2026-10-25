package tuigateway

// Regression probes for the session-lifetime change (T12c).
//
//	TestRebindMustSettleRequestsWrittenToTheDeadSocket  F1 (detach window)
//	TestServerCloseWaitsForReapedSessionEnd             F2 (untracked End)
//	TestCloseOfUnknownSessionKeepsUpstreamShape         F3 (wire regression)
//	TestSecondConnectionCannotAnswerApproval            F4 (ownership gap)

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/tools/approval"
)

// lifetimeSession is a scripted session with an inspectable End.
type lifetimeSession struct {
	id    string
	turn  func(ctx context.Context, in string) (string, error)
	ended func(string)
}

func (s *lifetimeSession) ID() string { return s.id }
func (s *lifetimeSession) Run(ctx context.Context, in string) (string, error) {
	return s.turn(ctx, in)
}
func (s *lifetimeSession) End(reason string) { s.ended(reason) }

// lifetimeServer builds a server whose sessions are lifetimeSession values.
func lifetimeServer(t *testing.T, grace time.Duration, mk func(id string) *lifetimeSession) (*Server, string) {
	t.Helper()
	var mu sync.Mutex
	var n int
	srv, err := New(Options{
		Token: testToken, Info: Info{Model: "tiny"}, OrphanGrace: grace,
		NewSession: func(_ context.Context, _ string, _ func(agent.Event)) Session {
			mu.Lock()
			n++
			id := fmt.Sprintf("stored-%d", n)
			mu.Unlock()
			return mk(id)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	t.Cleanup(srv.Close)
	return srv, "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws?token=" + testToken
}

// lifetimeConn is a minimal client.
type lifetimeConn struct {
	ws      *websocket.Conn
	next    int
	backlog []map[string]any
}

func lifetimeDial(t *testing.T, url string) *lifetimeConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	c := &lifetimeConn{ws: ws}
	c.read(t) // gateway.ready
	return c
}

func (c *lifetimeConn) read(t *testing.T) map[string]any {
	t.Helper()
	if len(c.backlog) > 0 {
		f := c.backlog[0]
		c.backlog = c.backlog[1:]
		return f
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f map[string]any
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("frame %s: %v", data, err)
	}
	return f
}

func (c *lifetimeConn) send(t *testing.T, frame map[string]any) {
	t.Helper()
	data, _ := json.Marshal(frame)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (c *lifetimeConn) call(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	c.next++
	id := float64(c.next)
	c.send(t, map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method, "params": params})
	var skipped []map[string]any
	for {
		f := c.read(t)
		if f["id"] == id && f["method"] == nil {
			c.backlog = append(skipped, c.backlog...)
			return f
		}
		skipped = append(skipped, f)
	}
}

func lifetimeApproval() approval.Request { return approval.Request{Command: "x"} }

// TestRebindMustSettleRequestsWrittenToTheDeadSocket covers F1: a turn that
// opens a server request in the window between detach's request sweep and the
// session's detachConn registers a request against a dead conn. detach has
// already swept, the answer was written to the closed socket, and the rebind
// cancels the reap that would have cancelled the turn: nothing can settle it,
// so the conversation stays "running" forever and every later prompt queues
// behind it. Pre-change the same window was closed by ls.close()'s cancel.
func TestRebindMustSettleRequestsWrittenToTheDeadSocket(t *testing.T) {
	for attempt := 0; attempt < 8; attempt++ {
		wedged, detail := rebindWithOrphanedRequest(t)
		if wedged {
			t.Fatalf("attempt %d: after a rebind %s", attempt, detail)
		}
	}
}

// rebindWithOrphanedRequest runs one drop/rebind round with enough padding
// sessions that detach's post-sweep loop is wide enough to enter.
func rebindWithOrphanedRequest(t *testing.T) (bool, string) {
	t.Helper()
	var srv *Server
	var url string
	srv, url = lifetimeServer(t, 10*time.Second, func(id string) *lifetimeSession {
		return &lifetimeSession{id: id, ended: func(string) {},
			turn: func(ctx context.Context, in string) (string, error) {
				if _, err := srv.Approve(ctx, lifetimeApproval()); err != nil {
					return "", err
				}
				return "ok", nil
			}}
	})
	a := lifetimeDial(t, url)
	var owner *conn
	srv.mu.Lock()
	for c := range srv.conns {
		owner = c
	}
	srv.mu.Unlock()
	if owner == nil {
		t.Fatal("no registered conn")
	}
	srv.mu.Lock()
	for i := 0; i < 4000; i++ {
		id := fmt.Sprintf("pad-%d", i)
		srv.sessions[id] = &liveSession{srv: srv, conn: owner, id: id, started: time.Now(),
			sess: &lifetimeSession{id: "padstored-" + id, ended: func(string) {},
				turn: func(context.Context, string) (string, error) { return "ok", nil }}}
	}
	srv.mu.Unlock()

	res, _ := a.call(t, "session.create", map[string]any{})["result"].(map[string]any)
	liveID, _ := res["session_id"].(string)
	stored, _ := res["stored_session_id"].(string)
	a.send(t, map[string]any{"jsonrpc": "2.0", "id": 99999, "method": "prompt.submit",
		"params": map[string]any{"session_id": liveID, "text": "go"}})
	time.Sleep(300 * time.Microsecond)
	_ = a.ws.CloseNow()

	peer := lifetimeDial(t, url)
	r, _ := peer.call(t, "session.resume", map[string]any{"session_id": stored})["result"].(map[string]any)
	if got, _ := r["session_id"].(string); got != liveID {
		return false, "" // rebind lost the race with the reap; not this probe
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		ls, err := srv.session(liveID)
		if err != nil {
			return false, ""
		}
		ls.mu.Lock()
		running := ls.running
		ls.mu.Unlock()
		if !running {
			return false, ""
		}
		time.Sleep(5 * time.Millisecond)
	}
	srv.mu.Lock()
	pending := 0
	for _, req := range srv.requests {
		if req.conn == owner {
			pending++
		}
	}
	srv.mu.Unlock()
	return true, fmt.Sprintf("the turn is still running with %d request(s) registered against the dead conn", pending)
}

// TestServerCloseWaitsForReapedSessionEnd covers F2: reapSession closes off the
// read loop with `go ls.close()`, which is not added to s.wg, so Server.Close
// can return while the reaped session's End is still running.
func TestServerCloseWaitsForReapedSessionEnd(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var once sync.Once
	srv, url := lifetimeServer(t, 15*time.Millisecond, func(id string) *lifetimeSession {
		return &lifetimeSession{id: id, turn: func(context.Context, string) (string, error) { return "ok", nil },
			ended: func(string) { once.Do(func() { entered <- struct{}{} }); <-release }}
	})
	a := lifetimeDial(t, url)
	a.call(t, "session.create", map[string]any{})
	_ = a.ws.CloseNow()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the detached session was never reaped")
	}
	closed := make(chan struct{})
	go func() { srv.Close(); close(closed) }()
	select {
	case <-closed:
		close(release)
		t.Fatal("Server.Close returned while the reaped session's End was still running")
	case <-time.After(time.Second):
	}
	close(release)
}

// TestCloseOfUnknownSessionKeepsUpstreamShape covers F3: upstream session.close
// pops the id and answers {"closed": false} when nothing matched; the baseline
// port did the same. Routing close through ownedSession made an unknown or
// empty id an error.
func TestCloseOfUnknownSessionKeepsUpstreamShape(t *testing.T) {
	_, url := lifetimeServer(t, time.Second, func(id string) *lifetimeSession {
		return &lifetimeSession{id: id, turn: func(context.Context, string) (string, error) { return "ok", nil },
			ended: func(string) {}}
	})
	a := lifetimeDial(t, url)
	for _, id := range []string{"deadbeef", ""} {
		params := map[string]any{}
		if id != "" {
			params["session_id"] = id
		}
		resp := a.call(t, "session.close", params)
		if _, isErr := resp["error"]; isErr {
			t.Fatalf("session.close %q = %v, want upstream's {\"closed\": false}", id, resp["error"])
		}
		res, _ := resp["result"].(map[string]any)
		if closed, ok := res["closed"].(bool); !ok || closed {
			t.Fatalf("session.close %q = %v, want closed=false", id, res)
		}
	}
}

// TestSecondConnectionCannotAnswerApproval covers F4: resolveResponse and
// clarify.lock key on the request id alone, so a second connection that learns
// an id can answer another client's approval or clarify batch.
func TestSecondConnectionCannotAnswerApproval(t *testing.T) {
	got := make(chan int, 1)
	var srv *Server
	var url string
	srv, url = lifetimeServer(t, time.Second, func(id string) *lifetimeSession {
		return &lifetimeSession{id: id, ended: func(string) {},
			turn: func(ctx context.Context, in string) (string, error) {
				ch, err := srv.Approve(ctx, lifetimeApproval())
				got <- int(ch)
				if err != nil {
					return "", err
				}
				return "ok", nil
			}}
	})
	a := lifetimeDial(t, url)
	res, _ := a.call(t, "session.create", map[string]any{})["result"].(map[string]any)
	sid, _ := res["session_id"].(string)
	a.send(t, map[string]any{"jsonrpc": "2.0", "id": 42, "method": "prompt.submit",
		"params": map[string]any{"session_id": sid, "text": "go"}})
	var reqID string
	for reqID == "" {
		f := a.read(t)
		if f["method"] == "approval" {
			reqID, _ = f["id"].(string)
		}
	}
	b := lifetimeDial(t, url)
	b.send(t, map[string]any{"jsonrpc": "2.0", "id": reqID, "result": map[string]any{"choice": "deny"}})
	select {
	case choice := <-got:
		t.Fatalf("a second connection answered %s: the turn saw choice %d", reqID, choice)
	case <-time.After(200 * time.Millisecond):
	}
	// The owner must still be able to answer it.
	a.send(t, map[string]any{"jsonrpc": "2.0", "id": reqID, "result": map[string]any{"choice": "once"}})
	select {
	case choice := <-got:
		if choice != int(approval.Once) {
			t.Fatalf("the owner's answer resolved as choice %d, want once", choice)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the owner's approval was not settled")
	}
}

// TestCloseRacesSessionCloseWithoutWaitGroupMisuse covers the second review's
// blocker: a session closed while Server.Close is already waiting must not add
// to the wait group from outside the lock that removes it.
func TestCloseRacesSessionCloseWithoutWaitGroupMisuse(t *testing.T) {
	for i := 0; i < 300; i++ {
		ended := make(chan string, 4)
		srv, err := New(Options{Token: testToken, NewSession: func(context.Context, string, func(agent.Event)) Session {
			return &fakeSession{id: "stored", ended: ended}
		}})
		if err != nil {
			t.Fatal(err)
		}
		ls := &liveSession{srv: srv, id: "s" + strconv.Itoa(i), started: time.Now(),
			sess: &fakeSession{id: "stored", ended: ended}}
		srv.mu.Lock()
		srv.sessions[ls.id] = ls
		srv.mu.Unlock()
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; srv.closeSession(ls.id) }()
		go func() { defer wg.Done(); <-start; srv.Close() }()
		close(start)
		wg.Wait()
	}
}

// TestCloseOfUnownedSessionKeepsUpstreamShape covers the existence oracle: an
// id that belongs to another client must look exactly like one that does not
// exist.
func TestCloseOfUnownedSessionKeepsUpstreamShape(t *testing.T) {
	h := newHarness(t, func(context.Context, *fakeSession, string) (string, error) { return "ok", nil })
	h.ready()
	sid := h.createSession()
	peer := newPeer(t, h)
	peer.ready()
	for _, params := range []map[string]any{{"session_id": "deadbeef"}, {"session_id": ""}, {}, {"session_id": sid}} {
		resp := peer.call("session.close", params)
		if _, isErr := resp["error"]; isErr {
			t.Fatalf("session.close %v = %v, want {\"closed\": false}", params, resp["error"])
		}
		if res, _ := resp["result"].(map[string]any); res["closed"] != false {
			t.Fatalf("session.close %v = %v, want closed=false", params, res)
		}
	}
}

// TestDetachedSessionRefusesServerRequests pins the invariant F1 rests on,
// deterministically: once a session is detached a server request is refused
// instead of being registered against a connection nobody can answer on.
func TestDetachedSessionRefusesServerRequests(t *testing.T) {
	srv, err := New(Options{Token: testToken, NewSession: func(context.Context, string, func(agent.Event)) Session {
		return &fakeSession{id: "stored", ended: make(chan string, 1)}
	}})
	if err != nil {
		t.Fatal(err)
	}
	ls := &liveSession{srv: srv, id: "s1", started: time.Now(), sess: &fakeSession{id: "stored"}}
	srv.mu.Lock()
	srv.sessions[ls.id] = ls
	srv.mu.Unlock()
	ls.detachConn(nil)

	if _, err := srv.request(withSession(context.Background(), ls), "approval", nil, nil); !errors.Is(err, errDetached) {
		t.Fatalf("request on a detached session = %v, want errDetached", err)
	}
	srv.mu.Lock()
	pending := len(srv.requests)
	srv.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d request(s) were registered against a detached session", pending)
	}
}
