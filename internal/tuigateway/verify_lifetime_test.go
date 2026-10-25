package tuigateway

// Durable probes for the session-lifetime contract, kept because each pins a
// failure mode that reading the code does not prevent.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/state"
	"github.com/umi008/hermes-agent-go/internal/tools/approval"
)

// TestReapRacesServerCloseWithoutWaitGroupMisuse pins that the reaper's wait
// group Add stays inside the lock that removes the session: an Add after Close
// cleared the map races its Wait.
func TestReapRacesServerCloseWithoutWaitGroupMisuse(t *testing.T) {
	for i := 0; i < 500; i++ {
		ends := make(chan string, 8)
		srv, err := New(Options{Token: testToken, NewSession: func(context.Context, string, func(agent.Event)) Session {
			return &fakeSession{id: "stored", ended: ends}
		}})
		if err != nil {
			t.Fatal(err)
		}
		ls := &liveSession{srv: srv, id: "r" + strconv.Itoa(i), started: time.Now(),
			sess: &fakeSession{id: "stored", ended: ends}}
		srv.mu.Lock()
		srv.sessions[ls.id] = ls
		srv.mu.Unlock()
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; srv.reapSession(ls.id) }()
		go func() { defer wg.Done(); <-start; srv.Close() }()
		close(start)
		wg.Wait()
		select {
		case <-ends:
		default:
			t.Fatalf("iteration %d: Close returned before the reaped session ended", i)
		}
		select {
		case r := <-ends:
			t.Fatalf("iteration %d: duplicate End(%q)", i, r)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestCloseAnswersAreIndistinguishable pins that close cannot be used to learn
// which ids exist: unknown, absent, empty, foreign and parked answer the same
// bytes, and only the caller's own session closes.
func TestCloseAnswersAreIndistinguishable(t *testing.T) {
	h := newHarnessWith(t, func(context.Context, *fakeSession, string) (string, error) { return "ok", nil },
		func(o *Options) { o.OrphanGrace = time.Second })
	h.ready()
	b := newPeer(t, h)
	b.ready()
	ownB := b.createSession()
	parkedConn := newPeer(t, h)
	parkedConn.ready()
	parked := parkedConn.createSession()
	_ = parkedConn.conn.CloseNow()
	time.Sleep(80 * time.Millisecond) // let detach park it

	var first string
	for _, params := range []map[string]any{
		{"session_id": "0badc0de"}, {}, {"session_id": ""},
		// ownB belongs to the caller and closes; it is asserted below, not here.
		{"session_id": ownB + "x"}, {"session_id": parked},
	} {
		res, _ := json.Marshal(b.call("session.close", params)["result"])
		if first == "" {
			first = string(res)
			if first != `{"closed":false,"messages":[]}` {
				t.Fatalf("unknown id answered %s", res)
			}
			continue
		}
		if string(res) != first {
			t.Fatalf("session.close %v answered %s, want %s", params, res, first)
		}
	}
	if _, err := h.srv.session(parked); err != nil {
		t.Fatalf("the parked close ended the parked session: %v", err)
	}
	own, _ := json.Marshal(b.call("session.close", map[string]any{"session_id": ownB})["result"])
	if string(own) != `{"closed":true,"messages":[]}` {
		t.Fatalf("own close answered %s, want closed:true", own)
	}
}

// TestDropThenReattachSettlesTheTurn pins that a turn asking for approval
// across a drop and a reattach settles instead of staying "running" forever.
func TestDropThenReattachSettlesTheTurn(t *testing.T) {
	approveErr := make(chan error, 1)
	var srv *Server
	ctx := context.Background()
	st, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.CreateAPISession(ctx, state.NewAPISession{ID: "20261024_080000_abcdef", Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 10; round++ {
		h := newHarnessWith(t, func(c context.Context, _ *fakeSession, in string) (string, error) {
			if in != "approve" {
				return "ok", nil
			}
			time.Sleep(time.Duration(2+round) * time.Millisecond)
			_, aerr := srv.Approve(c, approval.Request{Command: "x"})
			approveErr <- aerr
			return "", aerr
		}, func(o *Options) {
			o.OrphanGrace = 700 * time.Millisecond
			o.Store = st
			o.ResumeSession = func(_ context.Context, _ string, stored string, emit func(agent.Event)) Session {
				return &fakeSession{id: stored, emit: emit, ended: make(chan string, 4)}
			}
		})
		srv = h.srv
		h.ready()
		sid := h.createSession()
		stored := storedID(t, h, sid)
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "prompt.submit",
			"params": map[string]any{"session_id": sid, "text": "approve"}})
		wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := h.conn.Write(wctx, websocket.MessageText, body); err != nil {
			t.Fatal(err)
		}
		cancel()
		time.Sleep(30 * time.Millisecond)
		_ = h.conn.CloseNow()

		peer := newPeer(t, h)
		peer.ready()
		res, _ := peer.call("session.resume", map[string]any{"session_id": stored})["result"].(map[string]any)
		if res["session_id"] != sid {
			t.Fatalf("round %d: reattach = %v, want %s", round, res["session_id"], sid)
		}
		select {
		case <-approveErr:
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: the turn never settled after the drop and reattach", round)
		}
	}
}
