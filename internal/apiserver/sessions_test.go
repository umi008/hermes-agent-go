package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/state"
)

func sessionsServer(t *testing.T) (*httptest.Server, *state.Store) {
	t.Helper()
	var mu sync.Mutex
	tick := time.Unix(1_700_000_000, 0)
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), state.Options{Now: func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		tick = tick.Add(time.Second)
		return tick
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return responsesServer(t, &fakeBackend{}, Config{Sessions: st}), st
}

func expect(t *testing.T, srv *httptest.Server, method, path, body string, status int, want string) string {
	t.Helper()
	resp, got := do(t, srv, method, path, body)
	if resp.StatusCode != status || (want != "" && got != want) {
		t.Fatalf("%s %s %s = %d %s\nwant %d %s", method, path, body, resp.StatusCode, got, status, want)
	}
	return got
}

func apiErr(code, message string) string {
	c := "null"
	if code != "" {
		c = `"` + code + `"`
	}
	return `{"error":{"message":"` + message + `","type":"invalid_request_error","param":null,"code":` + c + `}}`
}

func TestSessionsUnavailableWithoutStore(t *testing.T) {
	srv := responsesServer(t, &fakeBackend{}, Config{})
	for _, path := range []string{"/api/sessions", "/api/sessions/x", "/api/sessions/x/messages"} {
		expect(t, srv, http.MethodGet, path, "", 503, apiErr("session_db_unavailable", "Session database unavailable"))
	}
}

func TestSessionsCreateAndGet(t *testing.T) {
	srv, _ := sessionsServer(t)
	expect(t, srv, http.MethodPost, "/api/sessions", `{"id":"s1","source":"Browser","title":"Plan","system_prompt":"sp"}`, 201,
		`{"object":"hermes.session","session":{"id":"s1","source":"hermes_browser","user_id":null,"model":null,"title":"Plan",`+
			`"started_at":1700000002,"ended_at":null,"end_reason":null,"message_count":0,"tool_call_count":0,"input_tokens":0,`+
			`"output_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0,"reasoning_tokens":0,"estimated_cost_usd":null,`+
			`"actual_cost_usd":null,"api_call_count":0,"parent_session_id":null,"pinned":false,"archived":false,"hidden":false,`+
			`"has_system_prompt":true,"has_model_config":false,"is_internal_child":false}}`)
	got := expect(t, srv, http.MethodGet, "/api/sessions/s1", "", 200, "")
	if !strings.Contains(got, `"id":"s1","source":"hermes_browser"`) {
		t.Fatalf("get = %s", got)
	}
	expect(t, srv, http.MethodGet, "/api/sessions/nope", "", 404, apiErr("session_not_found", "Session not found: nope"))

	// A generated id, and an unknown source falls back to api_server.
	got = expect(t, srv, http.MethodPost, "/api/sessions", `{"id":"","source":"martian"}`, 201, "")
	if !regexp.MustCompile(`"id":"api_1700000000_[0-9a-f]{8}","source":"api_server"`).MatchString(got) {
		t.Fatalf("generated = %s", got)
	}

	cases := []struct{ body, want string }{
		{`{`, apiErr("", "Invalid JSON in request body")},
		{`[1]`, apiErr("", "Request body must be a JSON object")},
		{`{"id":"../x"}`, apiErr("invalid_session_id", "Invalid session ID")},
		{`{"id":"C:x"}`, apiErr("invalid_session_id", "Invalid session ID")},
		{`{"id":"a\nb"}`, apiErr("invalid_session_id", "Invalid session ID")},
		{`{"id":"` + strings.Repeat("x", 257) + `"}`, apiErr("invalid_session_id", "Session ID too long")},
		{`{"id":"s2","system_prompt":5}`, apiErr("invalid_system_prompt", "system_prompt must be a string")},
		{`{"id":"s2","title":"Plan"}`, apiErr("invalid_title", "Title 'Plan' is already in use by session s1")},
	}
	for _, c := range cases {
		expect(t, srv, http.MethodPost, "/api/sessions", c.body, 400, c.want)
	}
	expect(t, srv, http.MethodPost, "/api/sessions", `{"session_id":"s1"}`, 409, apiErr("session_exists", "Session already exists: s1"))
}

func TestSessionsPatch(t *testing.T) {
	srv, st := sessionsServer(t)
	expect(t, srv, http.MethodPost, "/api/sessions", `{"id":"s1"}`, 201, "")
	expect(t, srv, http.MethodPost, "/api/sessions", `{"id":"s2","title":"Taken"}`, 201, "")
	expect(t, srv, http.MethodPatch, "/api/sessions/s1", `{"zeta":1,"alpha":2,"title":"x"}`, 400,
		apiErr("unsupported_session_field", "Unsupported session fields: alpha, zeta"))
	expect(t, srv, http.MethodPatch, "/api/sessions/s1", `{"pinned":"yes"}`, 400,
		apiErr("invalid_session_field", "'pinned' must be a boolean"))
	expect(t, srv, http.MethodPatch, "/api/sessions/s1", `{"title":"Taken"}`, 400,
		apiErr("invalid_title", "Title 'Taken' is already in use by session s2"))
	expect(t, srv, http.MethodPatch, "/api/sessions/nope", `{}`, 404, apiErr("session_not_found", "Session not found: nope"))

	got := expect(t, srv, http.MethodPatch, "/api/sessions/s1",
		`{"title":"  New  name ","hidden":true,"pinned":true,"archived":true,"unread":true,"end_reason":"done"}`, 200, "")
	for _, want := range []string{`"title":"New name"`, `"end_reason":"done"`, `"pinned":true,"archived":true,"hidden":false`} {
		if !strings.Contains(got, want) {
			t.Fatalf("patch = %s, missing %s", got, want)
		}
	}
	rec, _ := st.SessionRecord(context.Background(), "s1")
	if rec.EndedAt == 0 {
		t.Fatalf("end_reason must end the session: %+v", rec)
	}
	got = expect(t, srv, http.MethodPatch, "/api/sessions/s1", `{"title":null}`, 200, "")
	if !strings.Contains(got, `"title":null`) {
		t.Fatalf("clear title = %s", got)
	}
}

func TestSessionsListAndDelete(t *testing.T) {
	srv, st := sessionsServer(t)
	ctx := context.Background()
	for _, body := range []string{`{"id":"a","title":"Alpha"}`, `{"id":"b","source":"cli"}`, `{"id":"h","title":"Bot Chat"}`} {
		expect(t, srv, http.MethodPost, "/api/sessions", body, 201, "")
	}
	_ = st.SetFlag(ctx, "h", state.FlagHidden, true)
	if _, err := st.AppendMessage(ctx, "a", state.Message{Role: "user", Content: "hello\nthere"}); err != nil {
		t.Fatal(err)
	}
	type listed struct {
		Data []struct {
			ID      string `json:"id"`
			Preview string `json:"preview"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
		Limit   int  `json:"limit"`
	}
	list := func(q string) listed {
		t.Helper()
		var l listed
		if err := json.Unmarshal([]byte(expect(t, srv, http.MethodGet, "/api/sessions"+q, "", 200, "")), &l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	l := list("")
	if len(l.Data) != 2 || l.Data[0].ID != "a" || l.Data[0].Preview != "hello there" || l.Data[1].ID != "b" || l.HasMore || l.Limit != 50 {
		t.Fatalf("default list = %+v", l)
	}
	if l = list("?limit=1"); len(l.Data) != 1 || !l.HasMore {
		t.Fatalf("limit 1 = %+v", l)
	}
	if l = list("?limit=999&source=cli"); len(l.Data) != 1 || l.Data[0].ID != "b" || l.Limit != 200 {
		t.Fatalf("source filter = %+v", l)
	}
	// include_hidden counts only with a title filter, which matches exactly.
	if l = list("?include_hidden=true"); len(l.Data) != 2 {
		t.Fatalf("blanket hidden = %+v", l)
	}
	if l = list("?title=Bot%20Chat&include_hidden=1"); len(l.Data) != 1 || l.Data[0].ID != "h" {
		t.Fatalf("title lookup = %+v", l)
	}
	if l = list("?title=Alph"); len(l.Data) != 0 {
		t.Fatalf("title must match exactly = %+v", l)
	}

	expect(t, srv, http.MethodDelete, "/api/sessions/a", "", 200, `{"object":"hermes.session.deleted","id":"a","deleted":true}`)
	expect(t, srv, http.MethodDelete, "/api/sessions/a", "", 404, apiErr("session_not_found", "Session not found: a"))
}

func TestSessionsDeleteRefusesActiveTurn(t *testing.T) {
	_, st := sessionsServer(t)
	s := New(Config{Backend: &fakeBackend{}, Sessions: st})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	expect(t, srv, http.MethodPost, "/api/sessions", `{"id":"busy"}`, 201, "")
	status := newRecord()
	status.set("session_id", "busy")
	s.runs.mu.Lock()
	s.runs.runs["run_1"] = &run{id: "run_1", status: status, active: true}
	s.runs.mu.Unlock()
	expect(t, srv, http.MethodDelete, "/api/sessions/busy", "", 409,
		apiErr("session_active_turn", "session 'busy' (or a delegate child) has an active turn lease or compression lock"))
	s.runs.mu.Lock()
	s.runs.runs["run_1"].active = false
	s.runs.mu.Unlock()
	expect(t, srv, http.MethodDelete, "/api/sessions/busy", "", 200, `{"object":"hermes.session.deleted","id":"busy","deleted":true}`)
}

func TestSessionsMessages(t *testing.T) {
	srv, st := sessionsServer(t)
	ctx := context.Background()
	expect(t, srv, http.MethodPost, "/api/sessions", `{"id":"s"}`, 201, "")
	for i, c := range []string{"m1", "m2", "m3"} {
		m := state.Message{Role: "user", Content: c}
		if i == 1 {
			m = state.Message{Role: "assistant", ToolCalls: `[{"id":"c1"}]`, FinishReason: "tool_calls"}
		}
		if _, err := st.AppendMessage(ctx, "s", m); err != nil {
			t.Fatal(err)
		}
	}
	got := expect(t, srv, http.MethodGet, "/api/sessions/s/messages?limit=1&offset=1", "", 200, "")
	if !regexp.MustCompile(`^\{"object":"list","session_id":"s","data":\[\{"id":\d+,"session_id":"s","role":"assistant","content":null,` +
		`"tool_call_id":null,"tool_calls":\[\{"id":"c1"\}\],"tool_name":null,"timestamp":[\d.]+,"token_count":null,` +
		`"finish_reason":"tool_calls","reasoning":null,"reasoning_content":null,"display_kind":null\}\],` +
		`"pagination":\{"limit":1,"offset":1,"order":"oldest","returned":1\}\}$`).MatchString(got) {
		t.Fatalf("oldest page = %s", got)
	}
	got = expect(t, srv, http.MethodGet, "/api/sessions/s/messages", "", 200, "")
	if !strings.Contains(got, `"pagination":{"limit":500,"offset":0,"order":"latest","returned":3}`) {
		t.Fatalf("default page = %s", got)
	}
	got = expect(t, srv, http.MethodGet, "/api/sessions/s/messages?limit=2&order=latest", "", 200, "")
	if !strings.Contains(got, `"m3"`) || strings.Contains(got, `"m1"`) {
		t.Fatalf("latest 2 = %s", got)
	}
	for _, q := range []string{"?order=newest"} {
		expect(t, srv, http.MethodGet, "/api/sessions/s/messages"+q, "", 400, apiErr("invalid_pagination", "order must be one of: oldest, latest"))
	}
	for _, q := range []string{"?limit=-1", "?offset=-2", "?limit=abc"} {
		expect(t, srv, http.MethodGet, "/api/sessions/s/messages"+q, "", 400,
			apiErr("invalid_pagination", "limit and offset must be non-negative integers"))
	}
}

func TestSessionsFork(t *testing.T) {
	srv, st := sessionsServer(t)
	ctx := context.Background()
	expect(t, srv, http.MethodPost, "/api/sessions", `{"id":"src","title":"Plan"}`, 201, "")
	_, _ = st.AppendMessage(ctx, "src", state.Message{Role: "user", Content: "q"})
	got := expect(t, srv, http.MethodPost, "/api/sessions/src/fork", `{"id":"f1"}`, 201, "")
	for _, want := range []string{`"id":"f1","source":"api_server"`, `"title":"Plan #2"`, `"message_count":1`,
		`"parent_session_id":"src"`, `"has_model_config":true,"is_internal_child":false`} {
		if !strings.Contains(got, want) {
			t.Fatalf("fork = %s, missing %s", got, want)
		}
	}
	if src, _ := st.SessionRecord(ctx, "src"); src.EndReason != "branched" {
		t.Fatalf("source end = %q", src.EndReason)
	}
	got = expect(t, srv, http.MethodPost, "/api/sessions/src/fork", `{"id":"f2","title":"Mine"}`, 201, "")
	if !strings.Contains(got, `"title":"Mine"`) {
		t.Fatalf("titled fork = %s", got)
	}
	expect(t, srv, http.MethodPost, "/api/sessions/src/fork", `{"id":"f1"}`, 409, apiErr("session_exists", "Session already exists: f1"))
	expect(t, srv, http.MethodPost, "/api/sessions/src/fork", `{"id":"a\u0000b"}`, 400, apiErr("invalid_session_id", "Invalid session ID"))
	expect(t, srv, http.MethodPost, "/api/sessions/nope/fork", `{}`, 404, apiErr("session_not_found", "Session not found: nope"))
	got = expect(t, srv, http.MethodPost, "/api/sessions/f2/fork", `{}`, 201, "")
	if !strings.Contains(got, `"title":"Mine #2"`) {
		t.Fatalf("default lineage title = %s", got)
	}
}
