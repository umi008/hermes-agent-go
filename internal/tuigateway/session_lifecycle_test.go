package tuigateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/state"
)

// lifecycleHarness is a harness with a short orphan grace and a resume factory,
// so the store path of a reaped session can be exercised too.
func lifecycleHarness(t *testing.T, grace time.Duration) *harness {
	t.Helper()
	ctx := context.Background()
	st, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// The harness's session id, so a reaped session still has its row.
	if err := st.CreateAPISession(ctx, state.NewAPISession{ID: "20261024_080000_abcdef", Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	return newHarnessWith(t, func(context.Context, *fakeSession, string) (string, error) { return "ok", nil }, func(o *Options) {
		o.OrphanGrace = grace
		o.Store = st
		o.ResumeSession = func(_ context.Context, _ string, stored string, emit func(agent.Event)) Session {
			return &fakeSession{id: stored, emit: emit, ended: make(chan string, 4)}
		}
	})
}

// storedID asks the server which stored session a live session writes to.
func storedID(t *testing.T, h *harness, sid string) string {
	t.Helper()
	res, ok := h.call("session.resume", map[string]any{"session_id": sid})["result"].(map[string]any)
	if !ok {
		t.Fatalf("session.resume = %v", res)
	}
	stored, _ := res["stored_session_id"].(string)
	if stored == "" {
		t.Fatalf("no stored id in %v", res)
	}
	return stored
}

// TestSessionSurvivesReconnect pins the lifetime contract: dropping the socket
// detaches the conversation instead of ending it, and a reconnect resumes the
// same live session rather than rebuilding one from the store.
func TestSessionSurvivesReconnect(t *testing.T) {
	h := lifecycleHarness(t, time.Second)
	h.ready()
	sid := h.createSession()
	stored := storedID(t, h, sid)

	_ = h.conn.CloseNow()
	peer := newPeer(t, h)
	peer.ready()
	res, _ := peer.call("session.resume", map[string]any{"session_id": stored})["result"].(map[string]any)
	if res["session_id"] != sid {
		t.Fatalf("resume after reconnect = %v, want the live session %s", res["session_id"], sid)
	}
}

// TestDetachedSessionIsReaped pins the other half: a session nobody comes back
// for is closed after the grace, and its stored transcript stays resumable.
func TestDetachedSessionIsReaped(t *testing.T) {
	h := lifecycleHarness(t, 80*time.Millisecond)
	h.ready()
	sid := h.createSession()
	stored := storedID(t, h, sid)

	_ = h.conn.CloseNow()
	select {
	case reason := <-h.ended:
		if reason != "tui_close" {
			t.Errorf("end reason = %q", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the detached session was never reaped")
	}

	peer := newPeer(t, h)
	peer.ready()
	res, _ := peer.call("session.resume", map[string]any{"session_id": stored})["result"].(map[string]any)
	if res["session_id"] == sid {
		t.Fatalf("the reaped session is still live: %v", res["session_id"])
	}
}

// TestResumeCancelsTheReap pins that coming back in time keeps the session: the
// grace timer must be cancelled by the reattach, not left armed.
func TestResumeCancelsTheReap(t *testing.T) {
	h := lifecycleHarness(t, 300*time.Millisecond)
	h.ready()
	sid := h.createSession()
	stored := storedID(t, h, sid)

	_ = h.conn.CloseNow()
	peer := newPeer(t, h)
	peer.ready()
	first, _ := peer.call("session.resume", map[string]any{"session_id": stored})["result"].(map[string]any)
	if first["session_id"] != sid {
		t.Fatalf("first reattach = %v, want %s", first["session_id"], sid)
	}

	// Wait past the original grace while the peer stays connected: a cancelled
	// timer must not close a session that has a client again. Asking the peer
	// keeps the ownership rule out of the way.
	time.Sleep(500 * time.Millisecond)
	again, _ := peer.call("session.resume", map[string]any{"session_id": stored})["result"].(map[string]any)
	if again["session_id"] != sid {
		t.Fatalf("the reap was not cancelled: %v is no longer %s", again["session_id"], sid)
	}
}

// TestEmitWhileDetachedIsDropped pins that a turn running across a drop does
// not write through a detached session's missing connection.
func TestEmitWhileDetachedIsDropped(t *testing.T) {
	h := lifecycleHarness(t, time.Second)
	h.ready()
	sid := h.createSession()
	ls, err := h.srv.session(sid)
	if err != nil {
		t.Fatal(err)
	}

	_ = h.conn.CloseNow()
	// Give detach a moment to run, then emit as a live turn would.
	time.Sleep(50 * time.Millisecond)
	ls.emit(agent.Event{Kind: agent.EventText, Text: "while detached"})
}

// TestParkedSessionIsNotDrivableByAnotherClient pins the ownership rule the
// grace window makes reachable: a conversation parked for its own client must
// not answer another one that happens to know its id.
func TestParkedSessionIsNotDrivableByAnotherClient(t *testing.T) {
	h := lifecycleHarness(t, time.Second)
	h.ready()
	sid := h.createSession()
	stored := storedID(t, h, sid)

	_ = h.conn.CloseNow()
	time.Sleep(50 * time.Millisecond) // let detach park it
	peer := newPeer(t, h)
	peer.ready()
	for _, call := range []struct {
		method string
		params map[string]any
	}{
		{"prompt.submit", map[string]any{"session_id": sid, "text": "hijack"}},
		{"session.interrupt", map[string]any{"session_id": sid}},
		{"slash.exec", map[string]any{"session_id": sid, "command": "/status"}},
	} {
		resp := peer.call(call.method, call.params)
		if _, ok := resp["error"].(map[string]any); !ok {
			t.Fatalf("%s reached a parked session: %v", call.method, resp)
		}
	}
	// Closing is not an error, but it must not close anything either: an
	// unknown id and one belonging to somebody else answer the same way, so
	// close cannot be used to find out which ids exist.
	resp := peer.call("session.close", map[string]any{"session_id": sid})
	if res, _ := resp["result"].(map[string]any); res["closed"] != false {
		t.Fatalf("session.close on a parked session = %v, want closed=false", resp)
	}

	// The parked conversation is intact for the client that owns it.
	res, _ := peer.call("session.resume", map[string]any{"session_id": stored})["result"].(map[string]any)
	if res["session_id"] != sid {
		t.Fatalf("resume after the refused calls = %v, want %s", res["session_id"], sid)
	}
}
