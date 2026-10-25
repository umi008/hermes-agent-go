package tuigateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/tools/approval"
	"github.com/umi008/hermes-agent-go/internal/tools/clarify"
)

const testToken = "secret-token"

// fakeSession plays a scripted turn: it calls turn with the run context and
// the event sink the server passed to the factory.
type fakeSession struct {
	id    string
	emit  func(agent.Event)
	turn  func(ctx context.Context, s *fakeSession, input string) (string, error)
	ended chan string
}

func (f *fakeSession) ID() string { return f.id }

func (f *fakeSession) Run(ctx context.Context, input string) (string, error) {
	return f.turn(ctx, f, input)
}

func (f *fakeSession) End(reason string) { f.ended <- reason }

type harness struct {
	t      *testing.T
	srv    *Server
	conn   *websocket.Conn
	nextID int
	ended  chan string
	// events holds frames read while waiting for something else.
	backlog []map[string]any
}

func newHarness(t *testing.T, turn func(ctx context.Context, s *fakeSession, input string) (string, error)) *harness {
	t.Helper()
	h := &harness{t: t, ended: make(chan string, 4)}
	srv, err := New(Options{
		Token: testToken,
		Info:  Info{Model: "tiny", Provider: "custom", Cwd: "/work", Version: "1.2.3"},
		NewSession: func(_ context.Context, _ string, emit func(agent.Event)) Session {
			return &fakeSession{id: "20261024_080000_abcdef", emit: emit, turn: turn, ended: h.ended}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws?token=" + testToken
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	h.conn = conn
	return h
}

func (h *harness) read() map[string]any {
	h.t.Helper()
	if len(h.backlog) > 0 {
		f := h.backlog[0]
		h.backlog = h.backlog[1:]
		return f
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := h.conn.Read(ctx)
	if err != nil {
		h.t.Fatalf("read: %v", err)
	}
	var f map[string]any
	if err := json.Unmarshal(data, &f); err != nil {
		h.t.Fatalf("frame %s: %v", data, err)
	}
	return f
}

func (h *harness) send(frame map[string]any) {
	h.t.Helper()
	data, _ := json.Marshal(frame)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.conn.Write(ctx, websocket.MessageText, data); err != nil {
		h.t.Fatalf("write: %v", err)
	}
}

// call sends a request and returns its response; frames read before it are
// kept for later reads.
func (h *harness) call(method string, params map[string]any) map[string]any {
	h.t.Helper()
	h.nextID++
	id := float64(h.nextID)
	h.send(map[string]any{"jsonrpc": "2.0", "id": h.nextID, "method": method, "params": params})
	var skipped []map[string]any
	for {
		f := h.read()
		if f["id"] == id && f["method"] == nil {
			h.backlog = append(skipped, h.backlog...)
			return f
		}
		skipped = append(skipped, f)
	}
}

// event reads frames until an event of kind typ arrives and returns its
// payload. Server requests met on the way fail the test.
func (h *harness) event(typ string) map[string]any {
	h.t.Helper()
	for {
		f := h.read()
		if f["method"] != "event" {
			h.t.Fatalf("waiting for %s, got %v", typ, f)
		}
		params := f["params"].(map[string]any)
		if params["type"] == typ {
			payload, _ := params["payload"].(map[string]any)
			return payload
		}
	}
}

func (h *harness) ready() {
	h.t.Helper()
	p := h.event("gateway.ready")
	if p["heartbeat"] != true {
		h.t.Errorf("gateway.ready = %v, want heartbeat", p)
	}
	// The canonical wire: ready advertises the authority, and the client folds
	// the epoch into its own before asking runtime.describe for the rest.
	if p["session_authority"] != true {
		h.t.Errorf("gateway.ready = %v, want session_authority", p)
	}
	if epoch, _ := p["authority_epoch"].(float64); epoch < 1 {
		h.t.Errorf("gateway.ready = %v, want an authority epoch", p)
	}
	if replay, _ := p["replay_epoch"].(string); replay == "" {
		h.t.Errorf("gateway.ready = %v, want a replay epoch", p)
	}
	if skin, present := p["skin"]; !present || skin != nil {
		h.t.Errorf("gateway.ready = %v, want a null skin", p)
	}
	if events, ok := p["change_events"].([]any); !ok || len(events) != 0 {
		h.t.Errorf("gateway.ready = %v, want no change events", p)
	}
}

func (h *harness) createSession() string {
	h.t.Helper()
	resp := h.call("session.create", map[string]any{"cols": 80})
	res, ok := resp["result"].(map[string]any)
	if !ok {
		h.t.Fatalf("session.create = %v", resp)
	}
	info := res["info"].(map[string]any)
	if info["version"] != "1.2.3" || info["model"] != "tiny" || info["provider"] != "custom" || info["cwd"] != "/work" {
		h.t.Errorf("info = %v", info)
	}
	if res["stored_session_id"] != "20261024_080000_abcdef" || res["message_count"] != float64(0) {
		h.t.Errorf("session.create = %v", res)
	}
	sid, _ := res["session_id"].(string)
	if sid == "" {
		h.t.Fatalf("session.create = %v", res)
	}
	return sid
}

func TestRejectsMissingToken(t *testing.T) {
	srv, err := New(Options{Token: testToken, NewSession: func(context.Context, string, func(agent.Event)) Session { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	for _, path := range []string{"/api/ws", "/api/ws?token=wrong"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", path, resp.StatusCode)
		}
	}
	resp, err := http.Get(ts.URL + "/elsewhere?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("other path: status = %d, want 404", resp.StatusCode)
	}
}

func TestStartupMethods(t *testing.T) {
	h := newHarness(t, nil)
	h.ready()
	for _, m := range []string{"client.capabilities", "shared_metrics.startup_latency", "config.get", "wake.start",
		"commands.catalog", "session.control.read", "subagent.list", "process.list", "session.active_list",
		"system.battery", "reload.mcp", "terminal.resize"} {
		if resp := h.call(m, map[string]any{}); resp["error"] != nil {
			t.Errorf("%s = %v", m, resp)
		}
	}
	setup := h.call("setup.status", map[string]any{})["result"].(map[string]any)
	if setup["provider_configured"] != true {
		t.Errorf("setup.status = %v", setup)
	}
	if pong := h.call("gateway.ping", map[string]any{})["result"].(map[string]any); pong["ok"] != true {
		t.Errorf("gateway.ping = %v", pong)
	}
	unknown := h.call("no.such.method", map[string]any{})["error"].(map[string]any)
	if unknown["code"] != float64(-32601) || !strings.Contains(unknown["message"].(string), "unknown method: no.such.method") {
		t.Errorf("unknown method = %v", unknown)
	}
}

func TestPromptStreamsEvents(t *testing.T) {
	h := newHarness(t, func(_ context.Context, s *fakeSession, input string) (string, error) {
		s.emit(agent.Event{Kind: agent.EventReasoning, Text: "hmm"})
		s.emit(agent.Event{Kind: agent.EventToolStart, ToolCall: &llm.ToolCall{ID: "c1", Name: "todo", Arguments: json.RawMessage(`{"x":1}`)}})
		s.emit(agent.Event{Kind: agent.EventToolResult, ToolCall: &llm.ToolCall{ID: "c1", Name: "todo"}, Text: "done"})
		s.emit(agent.Event{Kind: agent.EventText, Text: "Hel"})
		s.emit(agent.Event{Kind: agent.EventText, Text: "lo " + input})
		return "Hello " + input, nil
	})
	h.ready()
	sid := h.createSession()

	submit := h.call("prompt.submit", map[string]any{"session_id": sid, "text": "you"})
	if res := submit["result"].(map[string]any); res["status"] != "streaming" {
		t.Fatalf("prompt.submit = %v", submit)
	}
	h.event("message.start")
	if p := h.event("reasoning.delta"); p["text"] != "hmm" {
		t.Errorf("reasoning.delta = %v", p)
	}
	if p := h.event("tool.start"); p["tool_id"] != "c1" || p["name"] != "todo" || p["args_text"] != `{"x":1}` {
		t.Errorf("tool.start = %v", p)
	}
	if p := h.event("tool.complete"); p["tool_id"] != "c1" || p["name"] != "todo" || p["result_text"] != "done" {
		t.Errorf("tool.complete = %v", p)
	}
	if p := h.event("message.delta"); p["text"] != "Hel" {
		t.Errorf("message.delta = %v", p)
	}
	if p := h.event("message.delta"); p["text"] != "lo you" {
		t.Errorf("message.delta = %v", p)
	}
	if p := h.event("message.complete"); p["text"] != "Hello you" || p["status"] != "complete" {
		t.Errorf("message.complete = %v", p)
	}

	h.call("session.close", map[string]any{"session_id": sid})
	select {
	case reason := <-h.ended:
		if reason != "tui_close" {
			t.Errorf("end reason = %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session not ended")
	}
}

func TestPromptRefusesSubmissionIDAsLegacy(t *testing.T) {
	h := newHarness(t, nil)
	h.ready()
	sid := h.createSession()
	resp := h.call("prompt.submit", map[string]any{"session_id": sid, "text": "x", "submission_id": "abc"})
	e, _ := resp["error"].(map[string]any)
	if e["code"] != float64(4000) || !strings.Contains(e["message"].(string), "submission_id") {
		t.Fatalf("prompt.submit = %v", resp)
	}
}

func TestPromptErrors(t *testing.T) {
	h := newHarness(t, func(context.Context, *fakeSession, string) (string, error) {
		return "partial", errors.New("provider down")
	})
	h.ready()
	if resp := h.call("prompt.submit", map[string]any{"session_id": "nope", "text": "x"}); resp["error"] == nil {
		t.Errorf("unknown session = %v", resp)
	}
	sid := h.createSession()
	h.call("prompt.submit", map[string]any{"session_id": sid, "text": "x"})
	p := h.event("message.complete")
	if p["status"] != "error" || p["text"] != "partial" || !strings.Contains(p["warning"].(string), "provider down") {
		t.Errorf("message.complete = %v", p)
	}
}

func TestInterrupt(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, func(ctx context.Context, _ *fakeSession, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	h.ready()
	sid := h.createSession()
	h.call("prompt.submit", map[string]any{"session_id": sid, "text": "x"})
	<-started
	res := h.call("session.interrupt", map[string]any{"session_id": sid})["result"].(map[string]any)
	if res["status"] != "interrupted" {
		t.Errorf("session.interrupt = %v", res)
	}
	if p := h.event("message.complete"); p["status"] != "interrupted" {
		t.Errorf("message.complete = %v", p)
	}
	res = h.call("session.interrupt", map[string]any{"session_id": sid})["result"].(map[string]any)
	if res["status"] != "not_interrupted" {
		t.Errorf("idle session.interrupt = %v", res)
	}
}

// serverRequest reads frames until a server→client request arrives.
func (h *harness) serverRequest(method string) (string, map[string]any) {
	h.t.Helper()
	for {
		f := h.read()
		if f["method"] == method {
			id, _ := f["id"].(string)
			if !strings.HasPrefix(id, "srq-") {
				h.t.Errorf("request id = %v", f["id"])
			}
			return id, f["params"].(map[string]any)
		}
		if f["method"] != "event" {
			h.t.Fatalf("waiting for %s, got %v", method, f)
		}
	}
}

func TestApprovalRequest(t *testing.T) {
	var srv *Server
	h := newHarness(t, func(ctx context.Context, _ *fakeSession, _ string) (string, error) {
		c, err := srv.Approve(ctx, approval.Request{Command: "rm -rf /tmp/x", Description: "recursive delete"})
		if err != nil {
			return "", err
		}
		return map[approval.Choice]string{approval.Once: "once", approval.Deny: "deny", approval.Session: "session"}[c], nil
	})
	srv = h.srv
	h.ready()
	sid := h.createSession()
	h.call("prompt.submit", map[string]any{"session_id": sid, "text": "x"})
	id, params := h.serverRequest("approval")
	if params["session_id"] != sid || params["command"] != "rm -rf /tmp/x" || params["description"] != "recursive delete" {
		t.Errorf("approval params = %v", params)
	}
	choices, _ := params["choices"].([]any)
	if len(choices) != 3 || choices[0] != "once" || choices[1] != "session" || choices[2] != "deny" || params["allow_permanent"] != false {
		t.Errorf("approval choices = %v", params)
	}
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"choice": "session"}})
	if p := h.event("message.complete"); p["text"] != "session" {
		t.Errorf("message.complete = %v", p)
	}
}

func TestApprovalOutsideATurnHasNoAnswer(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.srv.Approve(context.Background(), approval.Request{Command: "x"}); err == nil {
		t.Fatal("Approve without a session: want an error")
	}
	// Upstream reports a clarify batch nobody received as undelivered.
	r, err := h.srv.Ask(context.Background(), []clarify.Question{{ID: "q1", Text: "?"}})
	if err != nil || r.Outcome != "undelivered" {
		t.Fatalf("Ask without a session = %+v, %v; want undelivered", r, err)
	}
}

func TestClarifyLocks(t *testing.T) {
	var srv *Server
	h := newHarness(t, func(ctx context.Context, _ *fakeSession, _ string) (string, error) {
		r, err := srv.Ask(ctx, []clarify.Question{
			{ID: "q1", Text: "Color?", Choices: []string{"red (recommended)", "blue"}, Offered: []string{"red", "blue"}},
			{ID: "q2", Text: "Name?"},
		})
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(r)
		return string(b), nil
	})
	srv = h.srv
	h.ready()
	sid := h.createSession()
	h.call("prompt.submit", map[string]any{"session_id": sid, "text": "x"})
	id, params := h.serverRequest("clarify")
	qs := params["questions"].([]any)
	q1 := qs[0].(map[string]any)
	if len(qs) != 2 || q1["qid"] != "q1" || q1["question"] != "Color?" || q1["multi_select"] != false {
		t.Fatalf("clarify params = %v", params)
	}
	if ch := q1["choices"].([]any); len(ch) != 2 || ch[0] != "red (recommended)" {
		t.Errorf("choices = %v", q1["choices"])
	}

	first := h.call("clarify.lock", map[string]any{"request_id": id, "question_id": "q1", "answer": "blue"})["result"].(map[string]any)
	if first["status"] != "ok" || len(first["remaining"].([]any)) != 1 {
		t.Errorf("first lock = %v", first)
	}
	bad := h.call("clarify.lock", map[string]any{"request_id": id, "question_id": "zz", "answer": "x"})
	if e, _ := bad["error"].(map[string]any); e["code"] != float64(4002) {
		t.Errorf("unknown question lock = %v", bad)
	}
	last := h.call("clarify.lock", map[string]any{"request_id": id, "question_id": "q2", "answer": nil})["result"].(map[string]any)
	if last["status"] != "ok" || len(last["remaining"].([]any)) != 0 {
		t.Errorf("last lock = %v", last)
	}
	p := h.event("message.complete")
	if p["text"] != `{"Answers":{"q1":"blue","q2":null},"Outcome":"submitted","Notice":""}` {
		t.Errorf("reply = %v", p["text"])
	}
	expired := h.call("clarify.lock", map[string]any{"request_id": id, "question_id": "q1", "answer": "red"})["result"].(map[string]any)
	if expired["status"] != "expired" {
		t.Errorf("late lock = %v", expired)
	}
}

func TestClarifyResponseWithoutAnswersCancels(t *testing.T) {
	var srv *Server
	h := newHarness(t, func(ctx context.Context, _ *fakeSession, _ string) (string, error) {
		r, err := srv.Ask(ctx, []clarify.Question{{ID: "q1", Text: "?"}})
		if err != nil {
			return "", err
		}
		return r.Outcome, nil
	})
	srv = h.srv
	h.ready()
	sid := h.createSession()
	h.call("prompt.submit", map[string]any{"session_id": sid, "text": "x"})
	id, _ := h.serverRequest("clarify")
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
	if p := h.event("message.complete"); p["text"] != "cancelled" {
		t.Errorf("outcome = %v", p["text"])
	}
}

func TestDisconnectEndsSessionsAndPendingRequests(t *testing.T) {
	var srv *Server
	approveErr := make(chan error, 1)
	h := newHarness(t, func(ctx context.Context, _ *fakeSession, _ string) (string, error) {
		_, err := srv.Approve(ctx, approval.Request{Command: "x"})
		approveErr <- err
		return "", err
	})
	srv = h.srv
	h.ready()
	sid := h.createSession()
	h.call("prompt.submit", map[string]any{"session_id": sid, "text": "x"})
	h.serverRequest("approval")
	_ = h.conn.Close(websocket.StatusNormalClosure, "bye")

	select {
	case err := <-approveErr:
		if err == nil {
			t.Error("Approve after disconnect: want an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Approve still waiting after disconnect")
	}
	select {
	case reason := <-h.ended:
		if reason != "tui_close" {
			t.Errorf("end reason = %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session not ended after disconnect")
	}
}

func TestQueuedPromptRunsAfterTheCurrentTurn(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(_ context.Context, _ *fakeSession, input string) (string, error) {
		if input == "first" {
			<-release
		}
		return input, nil
	})
	h.ready()
	sid := h.createSession()
	h.call("prompt.submit", map[string]any{"session_id": sid, "text": "first"})
	second := h.call("prompt.submit", map[string]any{"session_id": sid, "text": "second"})
	if res := second["result"].(map[string]any); res["status"] != "queued" {
		t.Fatalf("second prompt.submit = %v", second)
	}
	close(release)
	if p := h.event("message.complete"); p["text"] != "first" {
		t.Errorf("first turn = %v", p)
	}
	if p := h.event("message.complete"); p["text"] != "second" {
		t.Errorf("queued turn = %v", p)
	}
}

// TestApprovalRequestSmartDeny pins the smart-deny wire: the client may only
// grant one operation, so the choices are reduced and a scoped answer is
// rejected rather than silently narrowed.
func TestApprovalRequestSmartDeny(t *testing.T) {
	var srv *Server
	h := newHarness(t, func(ctx context.Context, _ *fakeSession, _ string) (string, error) {
		c, err := srv.Approve(ctx, approval.Request{
			Command: "rm -rf /tmp/x", Description: "recursive delete", SmartDenied: true})
		if err != nil {
			return "", err
		}
		return map[approval.Choice]string{approval.Once: "once", approval.Deny: "deny", approval.Session: "session"}[c], nil
	})
	srv = h.srv
	h.ready()
	sid := h.createSession()
	h.call("prompt.submit", map[string]any{"session_id": sid, "text": "x"})
	id, params := h.serverRequest("approval")
	choices, _ := params["choices"].([]any)
	if len(choices) != 2 || choices[0] != "once" || choices[1] != "deny" {
		t.Fatalf("smart-deny choices = %v", params)
	}
	// A client that answers a scope it was not offered gets a denial: the
	// guardian already judged the command dangerous.
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"choice": "session"}})
	if p := h.event("message.complete"); p["text"] != "deny" {
		t.Errorf("message.complete = %v", p)
	}
}
