package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/state"
)

// seedSession stores a session with the given rows.
func seedSession(t *testing.T, st *state.Store, id string, msgs ...state.Message) {
	t.Helper()
	if err := st.CreateAPISession(context.Background(), state.NewAPISession{ID: id, Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if _, err := st.AppendMessage(context.Background(), id, m); err != nil {
			t.Fatal(err)
		}
	}
}

// goDo posts a body from a background goroutine and reports when the whole
// response was read, so a handler that waits on the test can be observed.
func goDo(srv *httptest.Server, path, body string, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
	resp, err := srv.Client().Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// waitUntil polls cond for a bounded time: the servers listen on real
// sockets, so the handler's first tick is not observable from the test
// goroutine.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition never held")
}

// TestSessionChatCompletion pins the synchronous turn: the stored transcript
// seeds it, the session keeps its id, and the envelope is upstream's.
func TestSessionChatCompletion(t *testing.T) {
	b := &fakeBackend{script: func(SessionOptions, string) (Reply, error) {
		return Reply{Text: "Hello", Usage: llm.Usage{InputTokens: 3, OutputTokens: 2, CacheReadTokens: 1},
			Provider: "p", Model: "m"}, nil
	}}
	srv, st := sessionsServerWith(t, b)
	seedSession(t, st, "s1", state.Message{Role: "user", Content: "q1"},
		state.Message{Role: "assistant", Content: "a1"})

	resp, got := do(t, srv, "POST", "/api/sessions/s1/chat", `{"message":"hi","system_message":"be brief"}`)
	want := `{"object":"hermes.session.chat.completion","status":"completed","session_id":"s1",` +
		`"message":{"role":"assistant","content":"Hello"},` +
		`"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5,"cache_read_tokens":1,"cache_write_tokens":0},` +
		`"runtime":{"provider":"p","model":"m","route_source":"global"}}`
	if resp.StatusCode != http.StatusOK || got != want || resp.Header.Get("X-Hermes-Session-Id") != "s1" {
		t.Fatalf("chat = %d %s, session %q", resp.StatusCode, got, resp.Header.Get("X-Hermes-Session-Id"))
	}
	o, input := b.last()
	if input != "hi" || o.Source != "api" || o.ID != "s1" || o.Instructions != "be brief" {
		t.Fatalf("session opened with %+v, input %q", o, input)
	}
	if len(o.History) != 2 || o.History[0].Content != "q1" || o.History[1].Content != "a1" {
		t.Fatalf("history = %+v", o.History)
	}

	// `input` is the message's alias and its text parts flatten with newlines.
	resp, got = do(t, srv, "POST", "/api/sessions/s1/chat",
		`{"input":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`)
	if resp.StatusCode != http.StatusOK ||
		!strings.HasPrefix(got, `{"object":"hermes.session.chat.completion","status":"completed","session_id":"s1"`) {
		t.Fatalf("input alias = %d %s", resp.StatusCode, got)
	}
	if _, input := b.last(); input != "a\nb" {
		t.Fatalf("flattened input = %q", input)
	}
}

// TestSessionChatUnfinished pins the terminal flags: a budget-exhausted turn
// is incomplete, a failed one failed, and both still answer as a completion.
func TestSessionChatUnfinished(t *testing.T) {
	usage := `"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0}`
	for _, tc := range []struct {
		name   string
		script func(SessionOptions, string) (Reply, error)
		want   string
	}{
		{"incomplete", func(SessionOptions, string) (Reply, error) {
			return Reply{Text: "part", ExitReason: "max_iterations_reached(250/250)"}, nil
		}, `{"object":"hermes.session.chat.completion","status":"incomplete","session_id":"s1",` +
			`"message":{"role":"assistant","content":"part"},` + usage +
			`,"runtime":{"provider":"","model":"","route_source":"global"}}`},
		{"failed", func(SessionOptions, string) (Reply, error) {
			return Reply{}, errors.New("provider exploded")
		}, `{"object":"hermes.session.chat.completion","status":"failed","session_id":"s1",` +
			`"message":{"role":"assistant","content":""},` + usage +
			`,"runtime":{"provider":"","model":"","route_source":"global"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st := sessionsServerWith(t, &fakeBackend{script: tc.script})
			seedSession(t, st, "s1")
			if resp, got := do(t, srv, "POST", "/api/sessions/s1/chat", `{"message":"hi"}`); resp.StatusCode != 200 || got != tc.want {
				t.Fatalf("%s = %d %s", tc.name, resp.StatusCode, got)
			}
		})
	}
}

// TestSessionChatValidation pins the request rules, their codes and messages.
func TestSessionChatValidation(t *testing.T) {
	b := &fakeBackend{}
	srv, st := sessionsServerWith(t, b)
	seedSession(t, st, "s1")
	for _, tc := range []struct {
		body   string
		status int
		want   string
	}{
		{`{`, 400, apiErr("", "Invalid JSON in request body")},
		{`[1]`, 400, apiErr("", "Request body must be a JSON object")},
		{`{}`, 400, apiErr("missing_message", "Missing 'message' field")},
		{`{"message":""}`, 400, apiErr("missing_message", "Missing 'message' field")},
		{`{"message":[]}`, 400, apiErr("missing_message", "Missing 'message' field")},
		{`{"message":"hi","system_message":5}`, 400, apiErr("invalid_system_message", "system_message must be a string")},
		{`{"message":"hi","instructions":["x"]}`, 400, apiErr("invalid_system_message", "system_message must be a string")},
		{`{"message":"hi","require_model_lock":true}`, 400,
			apiErr("missing_model", "require_model_lock was set but no model/provider was provided")},
		{`{"message":"hi","require_model_lock":true,"model":"tiny"}`, 400,
			apiErr("missing_model", "require_model_lock was set but no model/provider was provided")},
		{`{"message":"hi","require_model_lock":true,"model":"other","provider":"openai"}`, 409,
			apiErr("model_lock_unavailable", "Requested Browser model lock cannot be routed; refusing silent global fallback")},
		{`{"message":"hi","require_model_lock":true,"provider":"openai"}`, 409,
			apiErr("model_lock_unavailable", "Requested Browser model lock cannot be routed; refusing silent global fallback")},
	} {
		if resp, got := do(t, srv, "POST", "/api/sessions/s1/chat", tc.body); resp.StatusCode != tc.status || got != tc.want {
			t.Errorf("%s: %d %s", tc.body, resp.StatusCode, got)
		}
	}
	if resp, got := do(t, srv, "POST", "/api/sessions/nope/chat", `{"message":"hi"}`); resp.StatusCode != 404 ||
		got != apiErr("session_not_found", "Session not found: nope") {
		t.Errorf("unknown session = %d %s", resp.StatusCode, got)
	}
	// None of the refused requests reached the backend.
	if b.mu.Lock(); len(b.opened) != 0 {
		b.mu.Unlock()
		t.Errorf("a refused request ran a turn: %d sessions opened", len(b.opened))
	} else {
		b.mu.Unlock()
	}
}

// TestSessionChatModelLock pins the Browser model lock: the request persists
// it, later turns reuse it, and an unroutable lock is refused.
func TestSessionChatModelLock(t *testing.T) {
	b := &fakeBackend{script: func(SessionOptions, string) (Reply, error) {
		return Reply{Text: "ok", Provider: "p", Model: "other"}, nil
	}}
	srv, st := sessionsServerWith(t, b)
	seedSession(t, st, "s1")

	resp, got := do(t, srv, "POST", "/api/sessions/s1/chat",
		`{"message":"hi","require_model_lock":true,"model":"other"}`)
	want := `{"object":"hermes.session.chat.completion","status":"completed","session_id":"s1",` +
		`"message":{"role":"assistant","content":"ok"},` +
		`"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0},` +
		`"runtime":{"provider":"p","model":"other","route_source":"raw_request",` +
		`"requested":{"provider":"","model":"other"},"model_lock":"confirmed"}}`
	if resp.StatusCode != 200 || got != want {
		t.Fatalf("locked turn = %d %s", resp.StatusCode, got)
	}
	if o, _ := b.last(); o.Model != "other" {
		t.Fatalf("locked model = %q", o.Model)
	}
	rec, err := st.SessionRecord(context.Background(), "s1")
	if err != nil || rec.Model != "other" {
		t.Fatalf("row model = %+v, %v", rec, err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rec.ModelConfig), &cfg); err != nil {
		t.Fatalf("model_config = %s: %v", rec.ModelConfig, err)
	}
	var lock state.ModelLock
	if err := json.Unmarshal(cfg["browser_model_lock"], &lock); err != nil {
		t.Fatalf("lock = %v", err)
	}
	if lock.Model != "other" || lock.Provider != "" || lock.RouteSource != "raw_request" || !lock.Confirmed ||
		string(lock.ModelOpts) != "{}" {
		t.Fatalf("lock = %+v", lock)
	}

	// The stored lock carries a later turn that names no model at all.
	resp, got = do(t, srv, "POST", "/api/sessions/s1/chat", `{"message":"again"}`)
	if resp.StatusCode != 200 || !strings.Contains(got, `"route_source":"session_model_lock"`) ||
		!strings.Contains(got, `"requested":{"provider":"","model":"other"},"model_lock":"confirmed"`) {
		t.Fatalf("reused lock = %d %s", resp.StatusCode, got)
	}
	if o, _ := b.last(); o.Model != "other" {
		t.Fatalf("reused model = %q", o.Model)
	}

	// The virtual model alias is never reported as a model id: the request
	// falls back to the stored lock instead of asking for "tiny".
	resp, got = do(t, srv, "POST", "/api/sessions/s1/chat", `{"message":"hi","model":"tiny"}`)
	if resp.StatusCode != 200 || strings.Contains(got, `"tiny"`) ||
		!strings.Contains(got, `"model_lock":"confirmed"`) {
		t.Fatalf("virtual alias = %d %s", resp.StatusCode, got)
	}
}

// TestSessionChatConcurrency pins the shared run cap on the chat route.
func TestSessionChatConcurrency(t *testing.T) {
	gate := make(chan struct{})
	b := &fakeBackend{script: func(SessionOptions, string) (Reply, error) {
		<-gate
		return Reply{Text: "ok"}, nil
	}}
	srv, st := sessionsServerConfig(t, b, Config{MaxConcurrentRuns: 1})
	seedSession(t, st, "s1")
	done := make(chan struct{}, 1)
	go goDo(srv, "/api/sessions/s1/chat", `{"message":"slow"}`, done)
	waitUntil(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.inputs) == 1
	})
	resp, got := do(t, srv, "POST", "/api/sessions/s1/chat", `{"message":"second"}`)
	close(gate)
	<-done
	want := `{"error":{"message":"Too many concurrent runs (max 1)","type":"rate_limit_error","param":null,"code":"rate_limit_exceeded"}}`
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" || got != want {
		t.Fatalf("second turn = %d %s", resp.StatusCode, got)
	}
}
