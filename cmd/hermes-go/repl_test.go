package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/state"
)

// echoModel answers every request with "re: <last user message>" and
// records the user messages each request carried.
func echoModel(t *testing.T) (srv *httptest.Server, seen func() [][]string) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls [][]string
	)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		var users []string
		for _, m := range body.Messages {
			if s, ok := m.Content.(string); ok && m.Role == "user" {
				users = append(users, s)
			}
		}
		mu.Lock()
		calls = append(calls, users)
		mu.Unlock()
		reply, _ := json.Marshal("re: " + users[len(users)-1])
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":`+string(reply)+`},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

func modelConfig(url string) string {
	return "model:\n  provider: custom\n  default: tiny\n  base_url: " + url + "\n"
}

// sessions lists the sessions in the Go store, oldest first.
func sessions(t *testing.T, f *fakeEnv) ([]state.Session, *state.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := state.Open(ctx, filepath.Join(f.vars["HERMES_HOME"], "hermes-go", "state.db"), state.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	all, err := st.RecentSessions(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	return all, st
}

func activeContents(t *testing.T, st *state.Store, id string) string {
	t.Helper()
	msgs, err := st.Messages(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return strings.Join(out, "|")
}

func TestREPLConversation(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantSeen   string // user messages of each request, ";" between requests
		wantOut    []string
		wantActive []string // active messages of each saved session
		wantEnds   []string
	}{
		{
			name:       "multi turn",
			input:      "hi\nagain\n/quit\nignored\n",
			wantSeen:   "hi;hi,again",
			wantOut:    []string{"re: hi\n", "re: again\n"},
			wantActive: []string{"hi|re: hi|again|re: again"},
			wantEnds:   []string{"cli_close"},
		},
		{
			name:       "undo",
			input:      "one\ntwo\n/undo\nthree\n",
			wantSeen:   "one;one,two;one,three",
			wantOut:    []string{`Undid 1 turn (2 message(s)). Backed up to: "two"`, "2 message(s) remaining in history."},
			wantActive: []string{"one|re: one|three|re: three"},
			wantEnds:   []string{"cli_close"},
		},
		{
			name:       "undo n clamps",
			input:      "one\ntwo\n/undo 5\n/undo\n/undo x\n",
			wantSeen:   "one;one,two",
			wantOut:    []string{`Undid 2 turns (4 message(s)). Backed up to: "one"`, "No messages to undo.", "Usage: /undo [N]"},
			wantActive: []string{""},
			wantEnds:   []string{"cli_close"},
		},
		{
			name:       "retry",
			input:      "/retry\none\n/retry\n",
			wantSeen:   "one;one",
			wantOut:    []string{"No messages to retry.", `Retrying: "one"`},
			wantActive: []string{"one|re: one"},
			wantEnds:   []string{"cli_close"},
		},
		{
			name:       "new session",
			input:      "one\n/new\ntwo\n",
			wantSeen:   "one;two",
			wantOut:    []string{"New session started: 20261024_080000_"},
			wantActive: []string{"one|re: one", "two|re: two"},
			wantEnds:   []string{"new_session", "cli_close"},
		},
		{
			name:     "empty sessions are dropped",
			input:    "/new\n/reset\n/clear\n",
			wantOut:  []string{"\x1b[H\x1b[2J"},
			wantEnds: nil,
		},
		{
			name:     "info commands",
			input:    "one\n/title Demo\n/status\n/model gpt\n/history\n/tools\n/toolsets\n/skills\n/help\n/nope\n/exit\n",
			wantSeen: "one",
			wantOut: []string{
				"Session title set: Demo", "Model:    tiny (custom)", "Messages: 2", "Tokens:   3 in, 1 out",
				"restart with -m gpt", "user: one\nassistant: re: one\n", "  todo_list ", "  todo\n", "No skills installed.",
				"/undo [N]", "(alias: /reset)", "Unknown command: /nope (use /help)",
			},
			wantActive: []string{"one|re: one"},
			wantEnds:   []string{"cli_close"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, seen := echoModel(t)
			f := newHome(t, modelConfig(srv.URL))
			f.stdin = tt.input
			if code := Run(context.Background(), []string{"chat", "--toolsets", "todo"}, f.env()); code != 0 {
				t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
			}
			var got []string
			for _, users := range seen() {
				got = append(got, strings.Join(users, ","))
			}
			if s := strings.Join(got, ";"); s != tt.wantSeen {
				t.Errorf("requests = %q, want %q", s, tt.wantSeen)
			}
			out := f.stdout.String()
			for _, want := range tt.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("stdout lacks %q:\n%s", want, out)
				}
			}
			all, st := sessions(t, f)
			if len(all) != len(tt.wantEnds) {
				t.Fatalf("sessions = %+v", all)
			}
			for i, s := range all {
				if s.EndReason != tt.wantEnds[i] {
					t.Errorf("session %d end = %q, want %q", i, s.EndReason, tt.wantEnds[i])
				}
				if got := activeContents(t, st, s.ID); got != tt.wantActive[i] {
					t.Errorf("session %d active = %q, want %q", i, got, tt.wantActive[i])
				}
			}
			if tt.name == "info commands" && all[0].Title != "Demo" {
				t.Errorf("title = %q", all[0].Title)
			}
		})
	}
}

func TestREPLInterrupt(t *testing.T) {
	started := make(chan struct{}, 4)
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 { // hang until the client gives up
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"fine"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	f := newHome(t, modelConfig(srv.URL))
	in, feed := io.Pipe()
	intr := make(chan struct{})
	env := f.env()
	out := &lockedBuffer{}
	env.Stdin, env.Interrupt, env.Stdout = in, intr, out
	done := make(chan int)
	go func() { done <- Run(context.Background(), []string{"chat", "--toolsets", "todo"}, env) }()

	_, _ = io.WriteString(feed, "slow\n")
	<-started
	intr <- struct{}{} // cancels the hanging turn
	_, _ = io.WriteString(feed, "next\n")
	deadline := time.After(10 * time.Second)
	for !strings.Contains(out.String(), "fine") {
		select {
		case <-deadline:
			t.Fatalf("no reply after interrupt: %s", out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	intr <- struct{}{} // idle: exits
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("idle interrupt did not exit")
	}
	_ = feed.Close()
	if s := out.String(); !strings.Contains(s, "Interrupted.") {
		t.Errorf("stdout: %s", s)
	}
	all, st := sessions(t, f)
	// The interrupted turn keeps its user message, like upstream.
	if len(all) != 1 || activeContents(t, st, all[0].ID) != "slow|next|fine" {
		t.Fatalf("sessions %+v", all)
	}
}

// lockedBuffer is a writer the test reads while the REPL goroutine writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestChatInterruptCancelsSingleQuery(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server notices a gone client only after the body
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	f := newHome(t, modelConfig(srv.URL))
	intr := make(chan struct{}, 1)
	env := f.env()
	env.Interrupt = intr
	go func() { <-started; intr <- struct{}{} }()
	if code := Run(context.Background(), []string{"chat", "-q", "hi"}, env); code != 1 || !strings.Contains(f.stderr.String(), "context canceled") {
		t.Fatalf("exit %d: %s", code, f.stderr.String())
	}
}

func TestREPLClarify(t *testing.T) {
	tests := []struct {
		name, args, answer, want string
	}{
		{"choice by number", `{"questions":[{"question":"Color?","choices":["red","blue"]}]}`, "2", `"user_response":"blue"`},
		{"free text", `{"questions":[{"question":"Color?","choices":["red","blue"]}]}`, "green", `"user_response":"green"`},
		{"skip", `{"questions":[{"question":"Name?"}]}`, "", `"status":"skipped"`},
		{"multi select", `{"questions":[{"question":"Pick","choices":["a","b","c"],"multi_select":true}]}`, "1,3", `"user_response":["a","c"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, record := toolCallModel(t, "clarify", tt.args)
			f := newHome(t, modelConfig(srv.URL))
			f.stdin = "ask me\n" + tt.answer + "\n"
			if code := Run(context.Background(), []string{"chat", "--toolsets", "clarify"}, f.env()); code != 0 {
				t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
			}
			if res := lastToolResult(t, record()); !strings.Contains(res, tt.want) || !strings.Contains(res, `"outcome":"submitted"`) {
				t.Errorf("tool result = %s", res)
			}
			if !strings.Contains(f.stdout.String(), "? ") {
				t.Errorf("question not shown: %s", f.stdout.String())
			}
		})
	}
}

func TestSingleQueryClarifyIsUnavailable(t *testing.T) {
	srv, record := toolCallModel(t, "clarify", `{"questions":[{"question":"Name?"}]}`)
	f := newHome(t, modelConfig(srv.URL))
	if code := Run(context.Background(), []string{"chat", "--toolsets", "clarify", "-q", "hi"}, f.env()); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
	}
	if res := lastToolResult(t, record()); !strings.Contains(res, "not available in this execution context") {
		t.Errorf("tool result = %s", res)
	}
}
