package apiserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// fakeBackend scripts one reply per run and records how each session was
// opened.
type fakeBackend struct {
	mu     sync.Mutex
	opened []SessionOptions
	inputs []string
	// script runs the turn: it may write to out and fire tool events.
	script func(o SessionOptions, input string) (Reply, error)
	// ctxScript, when set, runs the turn with the run's context.
	ctxScript func(ctx context.Context, o SessionOptions, input string) (Reply, error)
}

func (b *fakeBackend) Model() string { return "tiny" }

func (b *fakeBackend) NewSession(_ context.Context, o SessionOptions) Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opened = append(b.opened, o)
	id := o.ID
	if id == "" {
		id = fmt.Sprintf("sess-%d", len(b.opened))
	}
	return &fakeSession{b: b, o: o, id: id}
}

func (b *fakeBackend) last() (SessionOptions, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.opened[len(b.opened)-1], b.inputs[len(b.inputs)-1]
}

type fakeSession struct {
	b  *fakeBackend
	o  SessionOptions
	id string
}

func (s *fakeSession) ID() string { return s.id }
func (s *fakeSession) End(string) {}

func (s *fakeSession) Run(ctx context.Context, input string) (Reply, error) {
	s.b.mu.Lock()
	s.b.inputs = append(s.b.inputs, input)
	script, ctxScript := s.b.script, s.b.ctxScript
	s.b.mu.Unlock()
	if ctxScript != nil {
		return ctxScript(ctx, s.o, input)
	}
	if script == nil {
		reply := Reply{Text: "echo " + input, Usage: llm.Usage{InputTokens: 3, OutputTokens: 2}}
		reply.History = append(append(append([]llm.Message(nil), s.o.History...),
			llm.Message{Role: llm.RoleUser, Content: input}), llm.Message{Role: llm.RoleAssistant, Content: reply.Text})
		reply.Turn = reply.History[len(reply.History)-1:]
		return reply, nil
	}
	return script(s.o, input)
}

func responsesServer(t *testing.T, b *fakeBackend, cfg Config) *httptest.Server {
	t.Helper()
	cfg.Backend = b
	cfg.Now = func() time.Time { return time.Unix(1700000000, 0) }
	srv := httptest.NewServer(New(cfg).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string) (*http.Response, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rdr)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp, strings.TrimSpace(string(data))
}

var respIDRE = regexp.MustCompile(`resp_[0-9a-f]{28}`)

func TestResponsesCreateGetDelete(t *testing.T) {
	srv := responsesServer(t, &fakeBackend{}, Config{})
	resp, body := do(t, srv, "POST", "/v1/responses", `{"input":"hi","temperature":0.5}`)
	if resp.StatusCode != 200 || resp.Header.Get("X-Hermes-Session-Id") != "sess-1" {
		t.Fatalf("status %d session %q: %s", resp.StatusCode, resp.Header.Get("X-Hermes-Session-Id"), body)
	}
	id := respIDRE.FindString(body)
	want := `{"id":"` + id + `","object":"response","status":"completed","created_at":1700000000,"model":"tiny",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"echo hi"}]}],` +
		`"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
	if id == "" || body != want {
		t.Fatalf("body:\n got %s\nwant %s", body, want)
	}
	if _, got := do(t, srv, "GET", "/v1/responses/"+id, ""); got != want {
		t.Fatalf("GET:\n got %s\nwant %s", got, want)
	}
	if resp, got := do(t, srv, "DELETE", "/v1/responses/"+id, ""); resp.StatusCode != 200 || got != `{"id":"`+id+`","object":"response","deleted":true}` {
		t.Fatalf("DELETE %d %s", resp.StatusCode, got)
	}
	missing := `{"error":{"message":"Response not found: ` + id + `","type":"invalid_request_error","param":null,"code":null}}`
	for _, method := range []string{"GET", "DELETE"} {
		if resp, got := do(t, srv, method, "/v1/responses/"+id, ""); resp.StatusCode != 404 || got != missing {
			t.Errorf("%s after delete: %d %s", method, resp.StatusCode, got)
		}
	}
}

func TestResponsesToolOutputItems(t *testing.T) {
	b := &fakeBackend{script: func(SessionOptions, string) (Reply, error) {
		return Reply{Text: "done", Turn: []llm.Message{
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)}}},
			{Role: llm.RoleTool, ToolCallID: "c1", Content: "out"},
			{Role: llm.RoleAssistant, Content: "done"},
		}}, nil
	}}
	srv := responsesServer(t, b, Config{})
	_, body := do(t, srv, "POST", "/v1/responses", `{"input":"go"}`)
	got := regexp.MustCompile(`"id":"(fco?)_[0-9a-f]{24}"`).ReplaceAllString(body, `"id":"${1}_X"`)
	want := `"output":[{"id":"fc_X","type":"function_call","status":"completed","name":"echo","arguments":"{\"x\":1}","call_id":"c1"},` +
		`{"id":"fco_X","type":"function_call_output","status":"completed","call_id":"c1","output":"out"},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`
	if !strings.Contains(got, want) {
		t.Fatalf("output:\n got %s\nwant %s", got, want)
	}
}

func TestResponsesValidation(t *testing.T) {
	srv := responsesServer(t, &fakeBackend{}, Config{})
	env := func(msg string) string {
		return `{"error":{"message":"` + msg + `","type":"invalid_request_error","param":null,"code":null}}`
	}
	for _, tc := range []struct {
		body   string
		status int
		want   string
	}{
		{`{`, 400, `{"error":{"message":"Invalid JSON in request body","type":"invalid_request_error"}}`},
		{`{"model":"x"}`, 400, env("Missing 'input' field")},
		{`{"input":null}`, 400, env("Missing 'input' field")},
		{`{"input":"a","conversation":"c","previous_response_id":"resp_x"}`, 400, env("Cannot use both 'conversation' and 'previous_response_id'")},
		{`{"input":5}`, 400, env("'input' must be a string or array")},
		{`{"input":"a","conversation_history":{"role":"user"}}`, 400, env("'conversation_history' must be an array of message objects")},
		{`{"input":"a","conversation_history":[{"role":"user","content":"x"},{"role":"user"}]}`, 400, env("conversation_history[1] must have 'role' and 'content' fields")},
		{`{"input":"a","previous_response_id":"resp_gone"}`, 404, env("Previous response not found: resp_gone")},
		{`{"input":"   "}`, 400, env("No user message found in input")},
		{`{"input":[]}`, 400, env("No user message found in input")},
		{`{"input":[{"type":"reasoning","summary":[]}]}`, 400, env("No user message found in input")},
	} {
		resp, got := do(t, srv, "POST", "/v1/responses", tc.body)
		if resp.StatusCode != tc.status || got != tc.want {
			t.Errorf("%s: %d %s\nwant %d %s", tc.body, resp.StatusCode, got, tc.status, tc.want)
		}
	}
}

func TestResponsesInputArrayAndHistory(t *testing.T) {
	b := &fakeBackend{}
	srv := responsesServer(t, b, Config{})
	do(t, srv, "POST", "/v1/responses", `{"input":["first",{"type":"reasoning"},{"role":"assistant","content":[{"type":"output_text","text":"ok"}]},{"content":[{"type":"input_text","text":"now"}]}],`+
		`"conversation_history":[{"role":"user","content":"old"}],"instructions":"be brief"}`)
	o, input := b.last()
	if input != "now" || o.Instructions != "be brief" || o.ID != "" {
		t.Fatalf("input %q instructions %q id %q", input, o.Instructions, o.ID)
	}
	if got := contents(o.History); got != "user:old,user:first,assistant:ok" {
		t.Fatalf("history = %s", got)
	}
}

func contents(msgs []llm.Message) string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, string(m.Role)+":"+m.Content)
	}
	return strings.Join(out, ",")
}

func TestResponsesChaining(t *testing.T) {
	b := &fakeBackend{}
	srv := responsesServer(t, b, Config{})
	_, body := do(t, srv, "POST", "/v1/responses", `{"input":"one","instructions":"sys"}`)
	first := respIDRE.FindString(body)

	resp, _ := do(t, srv, "POST", "/v1/responses", `{"input":"two","previous_response_id":"`+first+`"}`)
	o, _ := b.last()
	if got := contents(o.History); got != "user:one,assistant:echo one" || o.ID != "sess-1" || o.Instructions != "sys" {
		t.Fatalf("chained: history %s id %q instructions %q", got, o.ID, o.Instructions)
	}
	if resp.Header.Get("X-Hermes-Session-Id") != "sess-1" {
		t.Fatalf("chained session header = %q", resp.Header.Get("X-Hermes-Session-Id"))
	}

	do(t, srv, "POST", "/v1/responses", `{"input":"three","previous_response_id":"`+first+`","instructions":"other"}`)
	if o, _ := b.last(); o.Instructions != "other" {
		t.Fatalf("explicit instructions = %q", o.Instructions)
	}
	// An explicit history wins over the chain.
	do(t, srv, "POST", "/v1/responses", `{"input":"four","previous_response_id":"`+first+`","conversation_history":[{"role":"user","content":"mine"}]}`)
	if o, _ := b.last(); contents(o.History) != "user:mine" || o.ID != "" {
		t.Fatalf("explicit history: %s id %q", contents(o.History), o.ID)
	}
}

func TestResponsesConversationName(t *testing.T) {
	b := &fakeBackend{}
	srv := responsesServer(t, b, Config{})
	do(t, srv, "POST", "/v1/responses", `{"input":"one","conversation":"chat"}`)
	if o, _ := b.last(); len(o.History) != 0 {
		t.Fatalf("unknown conversation is not fresh: %s", contents(o.History))
	}
	do(t, srv, "POST", "/v1/responses", `{"input":"two","conversation":"chat"}`)
	if o, _ := b.last(); contents(o.History) != "user:one,assistant:echo one" {
		t.Fatalf("second turn history = %s", contents(o.History))
	}
	do(t, srv, "POST", "/v1/responses", `{"input":"three","conversation":"chat"}`)
	if o, _ := b.last(); contents(o.History) != "user:one,assistant:echo one,user:two,assistant:echo two" {
		t.Fatalf("third turn history = %s", contents(o.History))
	}
}

func TestResponsesStoreFalseAndTruncation(t *testing.T) {
	b := &fakeBackend{}
	srv := responsesServer(t, b, Config{})
	_, body := do(t, srv, "POST", "/v1/responses", `{"input":"x","store":"false"}`)
	if resp, _ := do(t, srv, "GET", "/v1/responses/"+respIDRE.FindString(body), ""); resp.StatusCode != 404 {
		t.Fatalf("store false was stored: %d", resp.StatusCode)
	}
	hist := make([]string, 120)
	for i := range hist {
		hist[i] = fmt.Sprintf(`{"role":"user","content":"m%d"}`, i)
	}
	do(t, srv, "POST", "/v1/responses", `{"input":"x","truncation":"auto","conversation_history":[`+strings.Join(hist, ",")+`]}`)
	if o, _ := b.last(); len(o.History) != 100 || o.History[0].Content != "m20" {
		t.Fatalf("truncated history: %d starting %q", len(o.History), o.History[0].Content)
	}
	do(t, srv, "POST", "/v1/responses", `{"input":"x","conversation_history":[`+strings.Join(hist, ",")+`]}`)
	if o, _ := b.last(); len(o.History) != 120 {
		t.Fatalf("untruncated history: %d", len(o.History))
	}
}

func TestResponsesFailedAndIncomplete(t *testing.T) {
	b := &fakeBackend{script: func(_ SessionOptions, input string) (Reply, error) {
		if input == "fail" {
			return Reply{}, errors.New("provider exploded with key=secret")
		}
		return Reply{Text: "part", Stop: llm.StopMaxTokens}, nil
	}}
	srv := responsesServer(t, b, Config{})
	_, body := do(t, srv, "POST", "/v1/responses", `{"input":"fail"}`)
	if !strings.Contains(body, `"status":"failed"`) || strings.Contains(body, "secret") ||
		!strings.HasSuffix(body, `"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The agent run failed."}]}],`+
			`"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0},"error":{"code":"agent_error","message":"The agent run failed."}}`) {
		t.Fatalf("failed body = %s", body)
	}
	if _, body := do(t, srv, "POST", "/v1/responses", `{"input":"more"}`); !strings.Contains(body, `"status":"incomplete"`) {
		t.Fatalf("incomplete body = %s", body)
	}
}

func TestResponsesConcurrencyCap(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	b := &fakeBackend{script: func(SessionOptions, string) (Reply, error) {
		once.Do(func() { close(started) })
		<-release
		return Reply{Text: "ok"}, nil
	}}
	srv := responsesServer(t, b, Config{MaxConcurrentRuns: 1})
	done := make(chan struct{})
	go func() {
		defer close(done)
		do(t, srv, "POST", "/v1/responses", `{"input":"slow"}`)
	}()
	<-started
	for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
		resp, body := do(t, srv, "POST", path, `{"input":"x","messages":[{"role":"user","content":"x"}]}`)
		want := `{"error":{"message":"Too many concurrent runs (max 1)","type":"rate_limit_error","param":null,"code":"rate_limit_exceeded"}}`
		if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "1" || body != want {
			t.Errorf("%s: %d retry %q %s", path, resp.StatusCode, resp.Header.Get("Retry-After"), body)
		}
	}
	close(release)
	<-done
	if resp, _ := do(t, srv, "POST", "/v1/responses", `{"input":"again"}`); resp.StatusCode != 200 {
		t.Fatalf("slot not released: %d", resp.StatusCode)
	}
}

// sseEvent is one parsed "event:/data:" frame.
type sseEvent struct {
	name string
	data map[string]any
	raw  string
}

func readSSE(t *testing.T, r io.Reader) []sseEvent {
	t.Helper()
	var out []sseEvent
	var name string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			raw := strings.TrimPrefix(line, "data: ")
			var m map[string]any
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				t.Fatalf("bad frame %q: %v", raw, err)
			}
			out = append(out, sseEvent{name, m, raw})
		}
	}
	return out
}

func TestResponsesStream(t *testing.T) {
	b := &fakeBackend{script: func(o SessionOptions, _ string) (Reply, error) {
		_, _ = io.WriteString(o.Out, "Hel")
		_, _ = io.WriteString(o.Out, "lo")
		call := llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"q":1}`)}
		o.OnTool(ToolEvent{Call: call})
		o.OnTool(ToolEvent{Done: true, Call: call, Output: strings.Repeat("y", 1200)})
		_, _ = io.WriteString(o.Out, " world")
		return Reply{Text: "Hello world", Usage: llm.Usage{InputTokens: 4, OutputTokens: 6}}, nil
	}}
	srv := responsesServer(t, b, Config{})
	resp, err := srv.Client().Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(`{"input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("X-Hermes-Session-Id") != "sess-1" {
		t.Fatalf("headers %v", resp.Header)
	}
	events := readSSE(t, resp.Body)
	var names []string
	for i, e := range events {
		names = append(names, e.name)
		if e.data["type"] != e.name || e.data["sequence_number"] != float64(i) {
			t.Errorf("event %d %s: type %v seq %v", i, e.name, e.data["type"], e.data["sequence_number"])
		}
		if !strings.HasSuffix(e.raw, fmt.Sprintf(`"sequence_number":%d}`, i)) {
			t.Errorf("sequence_number is not the last key: %s", e.raw)
		}
	}
	want := []string{
		"response.created",
		"response.output_item.added", "response.output_text.delta", // message opens on "Hello"
		"response.output_item.added", "response.output_item.done", // function_call
		"response.output_item.added", "response.output_item.done", // function_call_output
		"response.output_text.delta", // " world"
		"response.output_text.done", "response.output_item.done", "response.completed",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events:\n got %v\nwant %v", names, want)
	}
	id := respIDRE.FindString(events[0].raw)
	if events[0].raw != `{"type":"response.created","response":{"id":"`+id+`","object":"response","status":"in_progress","created_at":1700000000,"model":"tiny","output":[]},"sequence_number":0}` {
		t.Errorf("created = %s", events[0].raw)
	}
	msgID := itemIDFor(id, "message", "", 0)
	if events[1].raw != `{"type":"response.output_item.added","output_index":0,"item":{"id":"`+msgID+`","type":"message","status":"in_progress","role":"assistant","content":[]},"sequence_number":1}` {
		t.Errorf("message added = %s", events[1].raw)
	}
	if events[2].raw != `{"type":"response.output_text.delta","item_id":"`+msgID+`","output_index":0,"content_index":0,"delta":"Hello","logprobs":[],"sequence_number":2}` {
		t.Errorf("batched delta = %s", events[2].raw)
	}
	fc := itemIDFor(id, "function_call", "c1", 0)
	if events[3].raw != `{"type":"response.output_item.added","output_index":1,"item":{"id":"`+fc+`","type":"function_call","status":"in_progress","name":"echo","call_id":"c1","arguments":"{\"q\":1}"},"sequence_number":3}` {
		t.Errorf("function_call added = %s", events[3].raw)
	}
	fco := itemIDFor(id, "function_call_output", "c1", 0)
	if !strings.HasPrefix(events[5].raw, `{"type":"response.output_item.added","output_index":2,"item":{"id":"`+fco+`","type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"yyy`) {
		t.Errorf("function_call_output added = %s", events[5].raw)
	}
	if events[8].raw != `{"type":"response.output_text.done","item_id":"`+msgID+`","output_index":0,"content_index":0,"text":"Hello world","logprobs":[],"sequence_number":8}` {
		t.Errorf("text done = %s", events[8].raw)
	}
	term := events[10].raw
	wantTerm := `{"type":"response.completed","response":{"id":"` + id + `","object":"response","status":"completed","created_at":1700000000,"model":"tiny","output":[` +
		`{"type":"function_call","name":"echo","arguments":"{\"q\":1}","call_id":"c1"},` +
		`{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"` + strings.Repeat("y", 500) + `...[700 more chars]"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello world"}]}],` +
		`"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10}},"sequence_number":10}`
	if term != wantTerm {
		t.Errorf("terminal:\n got %s\nwant %s", term, wantTerm)
	}
	// The stored record is the terminal envelope.
	if _, got := do(t, srv, "GET", "/v1/responses/"+id, ""); !strings.Contains(wantTerm, got) {
		t.Errorf("stored = %s", got)
	}
}

// itemIDFor recomputes upstream next_item_id for one occurrence.
func itemIDFor(responseID, kind, callID string, _ int) string {
	st := &responsesStream{t: responsesTurn{id: responseID}, counts: map[string]int{}}
	return st.itemID(kind, callID)
}

func TestResponsesStreamFailure(t *testing.T) {
	b := &fakeBackend{script: func(SessionOptions, string) (Reply, error) {
		return Reply{}, errors.New("boom")
	}}
	srv := responsesServer(t, b, Config{})
	resp, err := srv.Client().Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(`{"input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	events := readSSE(t, resp.Body)
	last := events[len(events)-1]
	if len(events) != 2 || last.name != "response.failed" ||
		!strings.Contains(last.raw, `"status":"failed","created_at":1700000000,"model":"tiny","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The agent run failed."}]}],"error":{"message":"The agent run failed.","type":"server_error"},"usage":`) {
		t.Fatalf("events = %v", events)
	}
}

func TestResponsesStreamDisconnectStoresIncomplete(t *testing.T) {
	ctxDone := make(chan struct{})
	b := &fakeBackend{script: func(o SessionOptions, _ string) (Reply, error) {
		_, _ = io.WriteString(o.Out, "partial")
		<-ctxDone
		return Reply{}, context.Canceled
	}}
	s := New(Config{Backend: b})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"hi","stream":true}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
		close(ctxDone)
	}()
	s.Handler().ServeHTTP(rec, req)
	id := respIDRE.FindString(rec.Body.String())
	stored, found, err := s.responses.Get(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("no snapshot: %v", err)
	}
	if !strings.Contains(string(stored.Response), `"status":"incomplete"`) || !strings.Contains(string(stored.Response), `"text":"partial"`) {
		t.Fatalf("snapshot = %s", stored.Response)
	}
	if got := len(stored.ConversationHistory); got != 2 || stored.ConversationHistory[1].Content != "partial" {
		t.Fatalf("history = %+v", stored.ConversationHistory)
	}
}

func TestResponseStoreLRU(t *testing.T) {
	ctx := context.Background()
	st := MemoryResponseStore()
	t.Cleanup(func() { _ = st.Close() })
	st.max = 3
	put := func(id string) {
		if err := st.Put(ctx, id, StoredResponse{Response: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	put("a")
	put("b")
	put("c")
	_ = st.SetConversation(ctx, "conv-a", "a")
	_ = st.SetConversation(ctx, "conv-b", "b")
	if _, found, _ := st.Get(ctx, "a"); !found { // a is now the most recent
		t.Fatal("a missing")
	}
	put("d")
	if _, found, _ := st.Get(ctx, "b"); found {
		t.Fatal("b survived eviction")
	}
	if _, found, _ := st.Conversation(ctx, "conv-b"); found {
		t.Fatal("evicted response kept its conversation name")
	}
	for _, id := range []string{"a", "c", "d"} {
		if _, found, _ := st.Get(ctx, id); !found {
			t.Errorf("%s evicted", id)
		}
	}
	if id, found, _ := st.Conversation(ctx, "conv-a"); !found || id != "a" {
		t.Errorf("conv-a = %q %v", id, found)
	}
	if ok, _ := st.Delete(ctx, "a"); !ok {
		t.Fatal("delete a")
	}
	if _, found, _ := st.Conversation(ctx, "conv-a"); found {
		t.Fatal("deleted response kept its conversation name")
	}
}

// TestItemIDMatchesUpstream pins next_item_id against digests computed by
// upstream's Python implementation.
func TestItemIDMatchesUpstream(t *testing.T) {
	if got := itemIDFor("resp_abc", "message", "", 0); got != "msg_ba46484c31334d4fa9dd942d" {
		t.Errorf("message id = %s", got)
	}
	if got := itemIDFor("resp_abc", "function_call", "c1", 0); got != "fc_07d5d6dd41911516742a38d4" {
		t.Errorf("function_call id = %s", got)
	}
}
