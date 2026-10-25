package apiserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/state"
)

// sseFrame is one decoded SSE frame: its event name and its JSON payload.
type sseFrame struct {
	event string
	data  map[string]any
}

// readFrames decodes a whole SSE body.
func readFrames(t *testing.T, raw string) []sseFrame {
	t.Helper()
	var out []sseFrame
	for _, block := range strings.Split(strings.TrimSuffix(raw, "\n\n"), "\n\n") {
		var f sseFrame
		hasData := false
		for _, line := range strings.Split(block, "\n") {
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				f.event = name
				continue
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				if err := json.Unmarshal([]byte(data), &f.data); err != nil {
					t.Fatalf("frame %q: %v", block, err)
				}
				hasData = true
			}
		}
		if !hasData || (f.event == "" && f.data == nil) {
			t.Fatalf("unexpected frame %q", block)
		}
		out = append(out, f)
	}
	return out
}

// eventNames lists the frames' event names in order.
func eventNames(frames []sseFrame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.event)
	}
	return out
}

// TestSessionChatStream pins the SSE ladder: the sequenced events of one
// turn, the addressed session, and the terminal run status the poll route
// reports.
func TestSessionChatStream(t *testing.T) {
	b := &fakeBackend{script: func(o SessionOptions, _ string) (Reply, error) {
		_, _ = io.WriteString(o.Out, "Hel")
		_, _ = io.WriteString(o.Out, "lo")
		call := llm.ToolCall{ID: "c1", Name: "terminal", Arguments: json.RawMessage(`{"command":"ls  -la"}`)}
		o.OnTool(ToolEvent{Call: call})
		o.OnTool(ToolEvent{Done: true, Call: call, Output: "out", IsError: true})
		return Reply{Text: "Hello", Usage: llm.Usage{InputTokens: 3, OutputTokens: 2},
			Provider: "p", Model: "m"}, nil
	}}
	srv, st := sessionsServerWith(t, b)
	seedSession(t, st, "s1", state.Message{Role: "user", Content: "q1"})

	resp, got := chatStream(t, srv, "/api/sessions/s1/chat/stream", `{"message":"hi"}`)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" ||
		resp.Header.Get("X-Hermes-Session-Id") != "s1" {
		t.Fatalf("stream = %d %v", resp.StatusCode, resp.Header)
	}
	frames := readFrames(t, got)
	wantEvents := []string{"run.started", "message.started", "assistant.delta", "assistant.delta",
		"tool.started", "tool.failed", "assistant.completed", "run.completed", "done"}
	if names := eventNames(frames); strings.Join(names, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("events = %v", names)
	}
	// Every payload is stamped, once, with the session, the run and its seq.
	runID, _ := frames[0].data["run_id"].(string)
	for i, f := range frames {
		if f.data["session_id"] != "s1" || f.data["run_id"] != runID || f.data["seq"] != float64(i+1) ||
			f.data["ts"] != float64(1700000000) {
			t.Fatalf("frame %d stamps = %v", i, f.data)
		}
	}
	if !strings.HasPrefix(runID, "run_") {
		t.Fatalf("run id = %q", runID)
	}
	msgID, _ := frames[1].data["message"].(map[string]any)["id"].(string)
	if !strings.HasPrefix(msgID, "msg_") {
		t.Fatalf("message id = %q", msgID)
	}
	started, _ := json.Marshal(frames[0].data)
	if !strings.Contains(string(started), `"user_message":{"content":"hi","role":"user"}`) ||
		!strings.Contains(string(started), `"runtime":{"model":"","provider":"","route_source":"global"}`) {
		t.Fatalf("run.started = %s", started)
	}
	if frames[2].data["message_id"] != msgID || frames[2].data["delta"] != "Hel" || frames[3].data["delta"] != "lo" {
		t.Fatalf("deltas = %v %v", frames[2].data, frames[3].data)
	}
	startedTool, _ := json.Marshal(frames[4].data)
	if !strings.Contains(string(startedTool), `"tool_name":"terminal"`) ||
		!strings.Contains(string(startedTool), `"preview":"ls -la"`) {
		t.Fatalf("tool.started = %s", startedTool)
	}
	completed, _ := json.Marshal(frames[6].data)
	if !strings.Contains(string(completed), `"content":"Hello"`) || !strings.Contains(string(completed), `"completed":true`) {
		t.Fatalf("assistant.completed = %s", completed)
	}
	terminal, _ := json.Marshal(frames[7].data)
	for _, want := range []string{`"completed":true`, `"partial":false`, `"interrupted":false`,
		`"usage":{"cache_read_tokens":0,"cache_write_tokens":0,"input_tokens":3,"output_tokens":2,"total_tokens":5}`,
		`"runtime":{"model":"m","provider":"p","route_source":"global"}`, `"messages":`} {
		if !strings.Contains(string(terminal), want) {
			t.Fatalf("run.completed = %s, missing %s", terminal, want)
		}
	}
	if o, input := b.last(); input != "hi" || o.ID != "s1" || o.Source != "api" || o.Instructions != "" ||
		len(o.History) != 1 || o.History[0].Content != "q1" {
		t.Fatalf("session opened with %+v, input %q", o, input)
	}
	// The stream is pollable as a run after it ended.
	if resp, got := do(t, srv, "GET", "/v1/runs/"+runID, ""); resp.StatusCode != 200 ||
		!strings.Contains(got, `"status":"completed"`) || !strings.Contains(got, `"session_id":"s1"`) {
		t.Fatalf("run status = %d %s", resp.StatusCode, got)
	}
}

// TestSessionChatTurnLease pins the turn lease: a chat stream driving a
// session blocks the routes that require an idle one.
func TestSessionChatTurnLease(t *testing.T) {
	gate := make(chan struct{})
	b := &fakeBackend{script: func(SessionOptions, string) (Reply, error) {
		<-gate
		return Reply{Text: "ok"}, nil
	}}
	srv, st := sessionsServerWith(t, b)
	seedSession(t, st, "s1")
	done := make(chan struct{}, 1)
	go goDo(srv, "/api/sessions/s1/chat/stream", `{"message":"slow"}`, done)
	waitUntil(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.inputs) == 1
	})
	resp, got := do(t, srv, "DELETE", "/api/sessions/s1", "")
	close(gate)
	<-done
	if resp.StatusCode != http.StatusConflict || !strings.Contains(got, `"code":"session_active_turn"`) {
		t.Fatalf("delete during turn = %d %s", resp.StatusCode, got)
	}
}

// chatStream posts a body to a session chat stream and reads it to its end.
func chatStream(t *testing.T, srv *httptest.Server, path, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}
