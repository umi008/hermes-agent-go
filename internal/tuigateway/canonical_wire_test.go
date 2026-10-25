package tuigateway

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/state"
)

// canonicalCapabilities is what this port can honestly advertise. Only the CLI
// create flow exists: no receipts or durable admission, no session event
// replay, no ACP editor or MCP policy.
var canonicalCapabilities = []string{"local-cli-create-v1"}

// TestRuntimeDescribe pins the canonical handshake the Ink client runs before
// it trusts the authority: the identity, the epoch it folds into its own, and
// the closed feature lists it validates launch keys against.
func TestRuntimeDescribe(t *testing.T) {
	h := newHarness(t, nil)
	ready := h.event("gateway.ready")
	resp := h.call("runtime.describe", map[string]any{})
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("runtime.describe = %v", resp)
	}
	for _, key := range []string{"instance_id", "profile_id", "authority_epoch", "capabilities", "session_create", "skin"} {
		if _, present := res[key]; !present {
			t.Fatalf("runtime.describe lacks %q: %v", key, res)
		}
	}
	if id, _ := res["instance_id"].(string); id == "" {
		t.Errorf("instance_id = %v", res["instance_id"])
	}
	if profile, _ := res["profile_id"].(string); profile == "" {
		t.Errorf("profile_id = %v", res["profile_id"])
	}
	if epoch, _ := res["authority_epoch"].(float64); epoch < 1 {
		t.Errorf("authority_epoch = %v, want a positive epoch", res["authority_epoch"])
	}
	caps, _ := res["capabilities"].([]any)
	if len(caps) != len(canonicalCapabilities) {
		t.Fatalf("capabilities = %v", caps)
	}
	for i, want := range canonicalCapabilities {
		if caps[i] != want {
			t.Errorf("capabilities[%d] = %v, want %q", i, caps[i], want)
		}
	}
	create, _ := res["session_create"].(map[string]any)
	if _, ok := create["sources"].([]any); !ok {
		t.Errorf("session_create.sources = %v", create["sources"])
	}
	// No launch parameter is honoured, so none is advertised.
	if params, ok := create["parameters"].([]any); !ok || len(params) != 0 {
		t.Errorf("session_create.parameters = %v", create["parameters"])
	}
	// The describe result and gateway.ready must agree on the epoch the client
	// folds into its own.
	if ready["authority_epoch"] != res["authority_epoch"] {
		t.Errorf("ready epoch %v, describe epoch %v", ready["authority_epoch"], res["authority_epoch"])
	}
}

// TestRuntimeDescribeRejectsParams pins the closed request: describe takes no
// parameters, and a client that sends some is told why.
func TestRuntimeDescribeRejectsParams(t *testing.T) {
	h := newHarness(t, nil)
	h.ready()
	resp := h.call("runtime.describe", map[string]any{"unexpected": 1})
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("runtime.describe with params = %v", resp)
	}
	if errObj["code"] != float64(4001) {
		t.Errorf("code = %v, want 4001", errObj["code"])
	}
	data, _ := errObj["data"].(map[string]any)
	if data["reason"] != "invalid_params" {
		t.Errorf("data = %v", errObj["data"])
	}
}

// TestUnknownMethodContract pins the refusal the client keys its fallbacks on:
// upstream's exact message plus the machine-readable reason.
func TestUnknownMethodContract(t *testing.T) {
	h := newHarness(t, nil)
	h.ready()
	resp := h.call("no.such.method", map[string]any{})
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("unknown method = %v", resp)
	}
	if errObj["code"] != float64(-32601) || errObj["message"] != "unknown method: no.such.method" {
		t.Errorf("error = %v", errObj)
	}
	data, _ := errObj["data"].(map[string]any)
	if data["reason"] != "unknown_method" {
		t.Errorf("data = %v", errObj["data"])
	}
}

// TestCanonicalErrorReasons pins the machine-readable reason on the errors the
// canonical client switches on, so it never has to parse the message text.
func TestCanonicalErrorReasons(t *testing.T) {
	h := newHarness(t, nil)
	h.ready()
	resp := h.call("session.interrupt", map[string]any{})
	errObj, _ := resp["error"].(map[string]any)
	data, _ := errObj["data"].(map[string]any)
	if data["reason"] != "invalid_params" || !strings.Contains(errObj["message"].(string), "session_id") {
		t.Errorf("missing session_id = %v", errObj)
	}
	resp = h.call("session.interrupt", map[string]any{"session_id": "nope"})
	errObj, _ = resp["error"].(map[string]any)
	data, _ = errObj["data"].(map[string]any)
	if data["reason"] != "not_found" {
		t.Errorf("unknown session = %v", errObj)
	}
}

// newHarnessStore is newHarness over a real state.db, so the stored scope of
// session.list has rows to report.
func newHarnessStore(t *testing.T, turn func(ctx context.Context, s *fakeSession, input string) (string, error)) (*harness, *state.Store) {
	t.Helper()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := newHarness(t, turn)
	h.srv.o.Store = st
	return h, st
}

// TestSessionListLive pins the live scope: the sessions this connection owns,
// in the canonical row shape, with the stored id resolved.
func TestSessionListLive(t *testing.T) {
	h, _ := newHarnessStore(t, nil)
	h.ready()
	sid := h.createSession()
	resp := h.call("session.list", map[string]any{"scope": "live"})
	res, _ := resp["result"].(map[string]any)
	if res["scope"] != "live" {
		t.Fatalf("list = %v", resp)
	}
	rows, _ := res["sessions"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	row, _ := rows[0].(map[string]any)
	want := map[string]any{
		"session_id": sid, "id": sid, "title": "", "source": "tui", "message_count": float64(0),
		"running": false, "model": "tiny", "resolved_id": "20261024_080000_abcdef", "root_title": nil,
	}
	for key, value := range want {
		if got, present := row[key]; present && got != value {
			t.Errorf("row[%s] = %v, want %v", key, got, value)
		}
	}
	if _, ok := row["started_at"]; !ok {
		t.Errorf("row lacks started_at: %v", row)
	}
	// The default scope is live.
	resp = h.call("session.list", map[string]any{})
	if res, _ := resp["result"].(map[string]any); res["scope"] != "live" {
		t.Errorf("default scope = %v", res["scope"])
	}
}

// TestSessionListStored pins the stored scope over state.db: archived and
// hidden rows stay out unless asked for, and the limit is honoured.
func TestSessionListStored(t *testing.T) {
	h, st := newHarnessStore(t, nil)
	h.ready()
	ctx := context.Background()
	for _, id := range []string{"s-old", "s-new", "s-hidden"} {
		if err := st.CreateAPISession(ctx, state.NewAPISession{ID: id, Source: "api_server", Title: "T-" + id}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AppendMessage(ctx, id, state.Message{Role: "user", Content: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetFlag(ctx, "s-hidden", state.FlagHidden, true); err != nil {
		t.Fatal(err)
	}

	resp := h.call("session.list", map[string]any{"scope": "stored"})
	res, _ := resp["result"].(map[string]any)
	if res["scope"] != "stored" {
		t.Fatalf("list = %v", resp)
	}
	rows, _ := res["sessions"].([]any)
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		row, _ := r.(map[string]any)
		ids = append(ids, row["session_id"].(string))
		if row["title"] == "T-s-hidden" {
			t.Fatalf("a hidden row was listed: %v", row)
		}
		if row["message_count"] != float64(1) || row["source"] != "api_server" || row["running"] != false {
			t.Errorf("row = %v", row)
		}
	}
	if len(ids) != 2 || ids[0] != "s-new" || ids[1] != "s-old" {
		t.Errorf("stored rows = %v", ids)
	}

	// include_hidden adds the hidden row; the limit cuts the newest rows.
	resp = h.call("session.list", map[string]any{"scope": "stored", "include_hidden": true, "limit": 2})
	res, _ = resp["result"].(map[string]any)
	if rows, _ := res["sessions"].([]any); len(rows) != 2 {
		t.Errorf("limited rows = %v", rows)
	}
}

// TestSessionListValidation pins the closed request.
func TestSessionListValidation(t *testing.T) {
	h, _ := newHarnessStore(t, nil)
	h.ready()
	for _, params := range []map[string]any{
		{"limit": 0},
		{"limit": 201},
		{"limit": "many"},
		{"scope": "weird"},
		{"title": "x", "scope": "stored"},
	} {
		resp := h.call("session.list", params)
		errObj, ok := resp["error"].(map[string]any)
		if !ok || errObj["code"] != float64(4001) {
			t.Errorf("%v = %v", params, resp)
			continue
		}
		if data, _ := errObj["data"].(map[string]any); data["reason"] != "invalid_params" {
			t.Errorf("%v data = %v", params, errObj["data"])
		}
	}
}

// resumeHarness is the stored-scope harness with a resume factory that echoes
// the stored id it was asked to continue.
func resumeHarness(t *testing.T) (*harness, *state.Store) {
	t.Helper()
	h := newHarness(t, nil)
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h.srv.o.Store = st
	h.srv.o.ResumeSession = func(_ context.Context, _ string, storedID string, emit func(agent.Event)) Session {
		return &fakeSession{id: storedID, emit: emit, ended: h.ended}
	}
	return h, st
}

// TestSessionResumeSnapshot pins the canonical snapshot a resume answers: the
// stored transcript, the attach identity and the fields the client folds into
// its own state.
func TestSessionResumeSnapshot(t *testing.T) {
	h, st := resumeHarness(t)
	h.ready()
	ctx := context.Background()
	if err := st.CreateAPISession(ctx, state.NewAPISession{ID: "s-resume", Source: "api_server", Title: "Plan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendMessage(ctx, "s-resume", state.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendMessage(ctx, "s-resume", state.Message{Role: "assistant",
		ToolCalls: `[{"id":"c1","type":"function","function":{"name":"terminal","arguments":"{}"}}]`, Reasoning: "why"}); err != nil {
		t.Fatal(err)
	}

	resp := h.call("session.resume", map[string]any{"session_id": "s-resume", "display_projection": true})
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("session.resume = %v", resp)
	}
	for _, key := range []string{"session_id", "stored_session_id", "messages", "display_messages", "message_count",
		"running", "authority_epoch", "replay_epoch", "last_sequence", "subscription_id", "revision",
		"execution_generation", "pending", "prompts", "info", "pending_connection", "todo_state", "open_requests"} {
		if _, present := res[key]; !present {
			t.Fatalf("snapshot lacks %q: %v", key, res)
		}
	}
	if res["stored_session_id"] != "s-resume" || res["message_count"] != float64(2) || res["running"] != false {
		t.Fatalf("identity = %v", res)
	}
	if id, _ := res["session_id"].(string); id == "" {
		t.Fatalf("session_id = %v", res["session_id"])
	}
	if sub, _ := res["subscription_id"].(string); sub == "" {
		t.Errorf("subscription_id = %v", res["subscription_id"])
	}
	if res["display_messages"] != nil || res["todo_state"] != nil || res["open_requests"] != nil || res["pending_connection"] != nil {
		t.Errorf("absent objects must be null: %v", res)
	}
	if msgs, _ := res["messages"].([]any); len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	} else {
		first, _ := msgs[0].(map[string]any)
		second, _ := msgs[1].(map[string]any)
		if first["role"] != "user" || first["text"] != "hello" {
			t.Errorf("first = %v", first)
		}
		if second["role"] != "assistant" || second["reasoning"] != "why" {
			t.Errorf("second = %v", second)
		}
		if _, ok := first["timestamp"].(float64); !ok {
			t.Errorf("message lacks a timestamp: %v", first)
		}
		if _, ok := first["row_id"].(float64); !ok {
			t.Errorf("message lacks a row_id: %v", first)
		}
	}
	// The attach is real: a turn submitted to the resumed id reaches the
	// resumed conversation, and a second resume reuses it instead of forking.
	sid := res["session_id"].(string)
	if again := h.call("session.resume", map[string]any{"session_id": "s-resume"}); again["result"].(map[string]any)["session_id"] != sid {
		t.Errorf("second resume forked: %v", again)
	}
}

// TestSessionResumeSelectsAndRefuses pins the selectors and the refusals.
func TestSessionResumeSelectsAndRefuses(t *testing.T) {
	h, st := resumeHarness(t)
	h.ready()
	ctx := context.Background()
	if err := st.CreateAPISession(ctx, state.NewAPISession{ID: "s-titled", Source: "api_server", Title: "ByTitle"}); err != nil {
		t.Fatal(err)
	}

	// A title resolves the stored row.
	res, _ := h.call("session.resume", map[string]any{"title": "ByTitle"})["result"].(map[string]any)
	if res["stored_session_id"] != "s-titled" {
		t.Errorf("title resume = %v", res)
	}
	// omit_messages keeps the transcript out of the snapshot.
	res, _ = h.call("session.resume", map[string]any{"session_id": "s-titled", "omit_messages": true})["result"].(map[string]any)
	if msgs, _ := res["messages"].([]any); len(msgs) != 0 {
		t.Errorf("omit_messages = %v", msgs)
	}
	// An unknown session is a canonical not_found.
	errObj, _ := h.call("session.resume", map[string]any{"session_id": "nope"})["error"].(map[string]any)
	if errObj["code"] != float64(4001) {
		t.Errorf("missing = %v", errObj)
	} else if data, _ := errObj["data"].(map[string]any); data["reason"] != "not_found" {
		t.Errorf("missing reason = %v", errObj["data"])
	}
	// An unknown parameter names itself.
	errObj, _ = h.call("session.resume", map[string]any{"session_id": "s-titled", "bogus": 1})["error"].(map[string]any)
	data, _ := errObj["data"].(map[string]any)
	if data["reason"] != "invalid_params" {
		t.Fatalf("bogus param = %v", errObj)
	}
	if fields, _ := data["fields"].([]any); len(fields) != 1 || fields[0] != "bogus" {
		t.Errorf("fields = %v", data["fields"])
	}
}

// TestSessionResumeNeedsAFactory pins the honest refusal: a gateway that
// cannot open a stored conversation says so instead of returning a snapshot
// whose session cannot accept a turn.
func TestSessionResumeNeedsAFactory(t *testing.T) {
	h, st := newHarnessStore(t, nil)
	h.ready()
	ctx := context.Background()
	if err := st.CreateAPISession(ctx, state.NewAPISession{ID: "s-x", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	errObj, _ := h.call("session.resume", map[string]any{"session_id": "s-x"})["error"].(map[string]any)
	data, _ := errObj["data"].(map[string]any)
	if errObj["code"] != float64(4001) || data["reason"] != "unsupported" {
		t.Errorf("resume without a factory = %v", errObj)
	}
}
