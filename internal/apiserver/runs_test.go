package apiserver

import (
	"context"
	"encoding/json"
	"errors"
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

var runIDRE = regexp.MustCompile(`run_[0-9a-f]{32}`)

// startRun posts a run and returns its id after checking the 202 body.
func startRun(t *testing.T, srv *httptest.Server, body string) string {
	t.Helper()
	resp, got := do(t, srv, "POST", "/v1/runs", body)
	id := runIDRE.FindString(got)
	if resp.StatusCode != http.StatusAccepted || got != `{"run_id":"`+id+`","status":"started","replayed":false}` {
		t.Fatalf("POST /v1/runs: %d %s", resp.StatusCode, got)
	}
	return id
}

// events reads the run's whole event stream with the given headers.
func events(t *testing.T, srv *httptest.Server, path string, header ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

// frame is one sequenced SSE frame as the server writes it.
func frame(seq int, data string) string {
	return "id: " + itoa(seq) + "\ndata: " + data[:len(data)-1] + `,"seq":` + itoa(seq) + "}\n\n"
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestRunsLifecycle(t *testing.T) {
	b := &fakeBackend{script: func(o SessionOptions, _ string) (Reply, error) {
		_, _ = io.WriteString(o.Out, "Hel")
		_, _ = io.WriteString(o.Out, "lo")
		call := llm.ToolCall{ID: "c1", Name: "terminal", Arguments: json.RawMessage(`{"command":"ls  -la\n x"}`)}
		o.OnTool(ToolEvent{Call: call})
		o.OnTool(ToolEvent{Done: true, Call: call, Output: strings.Repeat("y", 600)})
		return Reply{Text: "Hello", Usage: llm.Usage{InputTokens: 3, OutputTokens: 2, CacheReadTokens: 1},
			Provider: "p", Model: "m"}, nil
	}}
	srv := responsesServer(t, b, Config{})
	id := startRun(t, srv, `{"input":"hi"}`)

	resp, got := events(t, srv, "/v1/runs/"+id+"/events")
	if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers %v", resp.Header)
	}
	head := `{"event":"%s","run_id":"` + id + `","timestamp":1700000000,`
	ev := func(name, rest string) string { return strings.Replace(head, "%s", name, 1) + rest }
	outcome := `"output":"Hello","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5,"cache_read_tokens":1,"cache_write_tokens":0},` +
		`"runtime":{"provider":"p","model":"m","route_source":"global"},"completed":true,"partial":false,"interrupted":false}`
	want := ": open\n\n" +
		frame(0, ev("message.delta", `"delta":"Hel"}`)) +
		frame(1, ev("message.delta", `"delta":"lo"}`)) +
		frame(2, ev("tool.started", `"tool":"terminal","preview":"ls -la x","tool_call_id":"c1"}`)) +
		frame(3, ev("tool.completed", `"tool":"terminal","duration":0,"error":false,"tool_call_id":"c1","preview":"`+strings.Repeat("y", 497)+`..."}`)) +
		frame(4, ev("run.completed", outcome)) +
		": stream closed\n\n"
	if got != want {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}

	status := `{"object":"hermes.run","run_id":"` + id + `","status":"completed","updated_at":1700000000,"created_at":1700000000,` +
		`"session_id":"` + id + `","model":"tiny","last_event":"run.completed",` + outcome
	if resp, got := do(t, srv, "GET", "/v1/runs/"+id, ""); resp.StatusCode != 200 || got != status {
		t.Fatalf("status %d:\n got %s\nwant %s", resp.StatusCode, got, status)
	}
	o, input := b.last()
	if o.Source != "api" || o.ID != id || o.Resume || len(o.History) != 0 || input != "hi" {
		t.Errorf("session opened with %+v, input %q", o, input)
	}
	// A finished run answers stop with its terminal status.
	if resp, got := do(t, srv, "POST", "/v1/runs/"+id+"/stop", ""); resp.StatusCode != 200 || got != status {
		t.Errorf("stop after finish: %d %s", resp.StatusCode, got)
	}
}

func TestRunsReplayAndTruncation(t *testing.T) {
	b := &fakeBackend{script: func(o SessionOptions, _ string) (Reply, error) {
		for range 1002 {
			_, _ = io.WriteString(o.Out, "x")
		}
		return Reply{Text: "done"}, nil
	}}
	srv := responsesServer(t, b, Config{})
	id := startRun(t, srv, `{"input":"go"}`)
	waitFor(t, func() bool {
		_, got := do(t, srv, "GET", "/v1/runs/"+id, "")
		return strings.Contains(got, `"status":"completed"`)
	})
	_, all := events(t, srv, "/v1/runs/"+id+"/events")
	trunc := "data: {\"event\":\"replay.truncated\",\"run_id\":\"" + id + "\",\"timestamp\":1700000000,\"oldest_retained_seq\":4}\n\n"
	if !strings.HasPrefix(all, ": open\n\n"+trunc+"id: 4\n") || !strings.HasSuffix(all, ": stream closed\n\n") {
		t.Fatalf("truncated replay starts %q", all[:min(len(all), 300)])
	}

	// Last-Event-ID resumes after the given seq; the requested seq is echoed
	// when it fell out of the backlog.
	_, got := events(t, srv, "/v1/runs/"+id+"/events", "Last-Event-ID", "1")
	if !strings.Contains(got, `"oldest_retained_seq":4,"requested_seq":1}`) {
		t.Errorf("Last-Event-ID 1: %q", got[:min(len(got), 300)])
	}
	_, got = events(t, srv, "/v1/runs/"+id+"/events?last_seq=1001")
	if !strings.HasPrefix(got, ": open\n\nid: 1002\ndata: {\"event\":\"run.completed\"") || strings.Contains(got, "replay.truncated") {
		t.Errorf("last_seq 1001: %q", got)
	}
}

func TestRunsValidation(t *testing.T) {
	srv := responsesServer(t, &fakeBackend{}, Config{})
	env := func(msg string) string {
		return `{"error":{"message":"` + msg + `","type":"invalid_request_error","param":null,"code":null}}`
	}
	for _, tc := range []struct{ body, want string }{
		{`{`, env("Invalid JSON")},
		{`[1]`, env("Invalid JSON")},
		{`{"input":""}`, env("Missing 'input' field")},
		{`{}`, env("Missing 'input' field")},
		{`{"input":[{"role":"user","content":""}]}`, env("No user message found in input")},
		{`{"input":"hi","conversation_history":"x"}`, env("'conversation_history' must be an array of message objects")},
		{`{"input":"hi","conversation_history":[{"role":"user"}]}`, env("conversation_history[0] must have 'role' and 'content' fields")},
	} {
		if resp, got := do(t, srv, "POST", "/v1/runs", tc.body); resp.StatusCode != 400 || got != tc.want {
			t.Errorf("%s: %d %s", tc.body, resp.StatusCode, got)
		}
	}
	missing := `{"error":{"message":"Run not found: run_nope","type":"invalid_request_error","param":null,"code":"run_not_found"}}`
	for _, path := range []string{"GET /v1/runs/run_nope", "GET /v1/runs/run_nope/events", "POST /v1/runs/run_nope/stop"} {
		method, p, _ := strings.Cut(path, " ")
		if resp, got := do(t, srv, method, p, ""); resp.StatusCode != 404 || got != missing {
			t.Errorf("%s: %d %s", path, resp.StatusCode, got)
		}
	}
}

func TestRunsHistorySources(t *testing.T) {
	b := &fakeBackend{}
	srv := responsesServer(t, b, Config{})
	wait := func(id string) { events(t, srv, "/v1/runs/"+id+"/events") }

	// conversation_history wins over the input items.
	wait(startRun(t, srv, `{"input":[{"role":"user","content":"old"},{"role":"user","content":"now"}],`+
		`"conversation_history":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`))
	if o, in := b.last(); len(o.History) != 2 || o.History[1].Content != "b" || in != "now" {
		t.Fatalf("conversation_history: %+v %q", o.History, in)
	}

	// The input items before the last become history; text parts join with
	// spaces.
	wait(startRun(t, srv, `{"input":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"text","text":"y"}]},`+
		`{"role":"assistant","content":""},{"role":"user","content":"z"}]}`))
	if o, in := b.last(); len(o.History) != 1 || o.History[0].Content != "x y" || in != "z" {
		t.Fatalf("input history: %+v %q", o.History, in)
	}

	// previous_response_id replays the stored chain with its session and
	// instructions.
	resp, body := do(t, srv, "POST", "/v1/responses", `{"input":"hi","instructions":"be brief"}`)
	prev, sess := respIDRE.FindString(body), resp.Header.Get("X-Hermes-Session-Id")
	wait(startRun(t, srv, `{"input":"next","previous_response_id":"`+prev+`"}`))
	o, _ := b.last()
	if sess == "" || o.ID != sess || o.Instructions != "be brief" || o.Resume || len(o.History) != 2 || o.History[1].Content != "echo hi" {
		t.Fatalf("chained run opened with %+v", o)
	}

	// A body session_id with no history resumes the stored transcript.
	id := startRun(t, srv, `{"input":"again","session_id":"sess-9"}`)
	wait(id)
	if o, _ := b.last(); o.ID != "sess-9" || !o.Resume || len(o.History) != 0 {
		t.Fatalf("session_id run opened with %+v", o)
	}
	if _, got := do(t, srv, "GET", "/v1/runs/"+id, ""); !strings.Contains(got, `"session_id":"sess-9","model":"tiny"`) {
		t.Errorf("status %s", got)
	}
}

func TestRunsStop(t *testing.T) {
	started := make(chan struct{})
	b := &fakeBackend{ctxScript: func(ctx context.Context, _ SessionOptions, _ string) (Reply, error) {
		close(started)
		<-ctx.Done()
		return Reply{ExitReason: "interrupted_by_user"}, ctx.Err()
	}}
	srv := responsesServer(t, b, Config{})
	id := startRun(t, srv, `{"input":"wait"}`)
	<-started
	if resp, got := do(t, srv, "POST", "/v1/runs/"+id+"/stop", ""); resp.StatusCode != 200 || got != `{"run_id":"`+id+`","status":"stopping"}` {
		t.Fatalf("stop: %d %s", resp.StatusCode, got)
	}
	_, got := events(t, srv, "/v1/runs/"+id+"/events")
	tail := `"completed":false,"partial":false,"interrupted":true,"turn_exit_reason":"interrupted_by_user"}`
	if !strings.Contains(got, `{"event":"run.cancelled","run_id":"`+id+`","timestamp":1700000000,`+tail[:len(tail)-1]+`,"seq":0}`) {
		t.Fatalf("events %q", got)
	}
	want := `{"object":"hermes.run","run_id":"` + id + `","status":"cancelled","updated_at":1700000000,"created_at":1700000000,` +
		`"session_id":"` + id + `","model":"tiny","last_event":"run.cancelled",` + tail
	if _, got := do(t, srv, "GET", "/v1/runs/"+id, ""); got != want {
		t.Fatalf("status:\n got %s\nwant %s", got, want)
	}
}

func TestRunsFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  Reply
		err    error
		status string
		fields string
	}{
		{"error", Reply{ExitReason: "all_retries_exhausted_no_response"}, errors.New("provider said secret"), "failed",
			`"error":"agent run failed","completed":false,"partial":false,"interrupted":false,"turn_exit_reason":"all_retries_exhausted_no_response"}`},
		{"budget", Reply{Text: "part", ExitReason: "max_iterations_reached(2/2)"}, nil, "failed",
			`"output":"part","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0},` +
				`"runtime":{"provider":"","model":"","route_source":"global"},"completed":false,"partial":false,"interrupted":false,"turn_exit_reason":"max_iterations_reached(2/2)"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBackend{script: func(SessionOptions, string) (Reply, error) { return tc.reply, tc.err }}
			srv := responsesServer(t, b, Config{})
			id := startRun(t, srv, `{"input":"go"}`)
			_, got := events(t, srv, "/v1/runs/"+id+"/events")
			want := frame(0, `{"event":"run.`+tc.status+`","run_id":"`+id+`","timestamp":1700000000,`+tc.fields)
			if got != ": open\n\n"+want+": stream closed\n\n" {
				t.Fatalf("events:\n got %q\nwant %q", got, want)
			}
			if _, got := do(t, srv, "GET", "/v1/runs/"+id, ""); !strings.Contains(got, `"status":"`+tc.status+`"`) || strings.Contains(got, "secret") {
				t.Errorf("status %s", got)
			}
		})
	}
}

func TestRunsConcurrencyCapAndShutdown(t *testing.T) {
	var once sync.Once
	started := make(chan struct{})
	b := &fakeBackend{ctxScript: func(ctx context.Context, _ SessionOptions, _ string) (Reply, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return Reply{}, ctx.Err()
	}}
	s := New(Config{Backend: b, MaxConcurrentRuns: 1, Now: func() time.Time { return time.Unix(1700000000, 0) }})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	id := startRun(t, srv, `{"input":"one"}`)
	<-started
	resp, got := do(t, srv, "POST", "/v1/runs", `{"input":"two"}`)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "1" || !strings.Contains(got, "Too many concurrent runs (max 1)") {
		t.Fatalf("over cap: %d %s", resp.StatusCode, got)
	}
	// A bad request is rejected before it takes a slot.
	if resp, _ := do(t, srv, "POST", "/v1/runs", `{}`); resp.StatusCode != 400 {
		t.Errorf("bad request at cap: %d", resp.StatusCode)
	}

	s.runs.interruptAll()
	_, got = events(t, srv, "/v1/runs/"+id+"/events")
	if !strings.Contains(got, `"event":"run.interrupted","run_id":"`+id+`","timestamp":1700000000,"error":"Gateway shutdown interrupted the run.","seq":0}`) {
		t.Fatalf("events %q", got)
	}
	if _, got := do(t, srv, "GET", "/v1/runs/"+id, ""); !strings.Contains(got, `"status":"interrupted"`) {
		t.Errorf("status %s", got)
	}
	// The slot is free again.
	waitFor(t, func() bool { return s.inflight.Load() == 0 })
	startRun(t, srv, `{"input":"three"}`)
	s.runs.interruptAll()
}

func TestRunsSweep(t *testing.T) {
	now := time.Unix(1700000000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	srv := httptest.NewServer(New(Config{Backend: &fakeBackend{}, Now: clock}).Handler())
	t.Cleanup(srv.Close)
	id := startRun(t, srv, `{"input":"hi"}`)
	events(t, srv, "/v1/runs/"+id+"/events")

	advance(301 * time.Second)
	if resp, _ := do(t, srv, "GET", "/v1/runs/"+id, ""); resp.StatusCode != 200 {
		t.Fatalf("status after stream TTL: %d", resp.StatusCode)
	}
	if resp, _ := events(t, srv, "/v1/runs/"+id+"/events"); resp.StatusCode != 404 {
		t.Errorf("events after stream TTL: %d", resp.StatusCode)
	}
	advance(time.Hour)
	if resp, _ := do(t, srv, "GET", "/v1/runs/"+id, ""); resp.StatusCode != 404 {
		t.Errorf("status after status TTL: %d", resp.StatusCode)
	}
}

func TestToolPreview(t *testing.T) {
	for _, tc := range []struct{ name, args, want string }{
		{"terminal", `{"command":"echo  hi\nthere"}`, "echo hi there"},
		{"terminal", `{"query":"no primary"}`, ""},
		{"web_extract", `{"urls":["https://a","https://b"]}`, "https://a"},
		{"web_extract", `{"urls":[]}`, ""},
		{"custom_tool", `{"x":1,"path":"/tmp/f"}`, "/tmp/f"},
		{"custom_tool", `{"x":1}`, ""},
		{"skills_list", `{}`, ""},
		{"todo", `{"goal":5}`, "5"},
	} {
		if got := toolPreview(tc.name, json.RawMessage(tc.args)); got != tc.want {
			t.Errorf("toolPreview(%s, %s) = %q, want %q", tc.name, tc.args, got, tc.want)
		}
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
