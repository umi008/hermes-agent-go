package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/state"
)

type fakeEnv struct {
	vars   map[string]string
	cwd    string
	stdin  string
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func (f *fakeEnv) env() Env {
	return Env{
		GOOS:        "linux",
		Getenv:      func(k string) string { return f.vars[k] },
		LookupEnv:   func(k string) (string, bool) { v, ok := f.vars[k]; return v, ok },
		UserHomeDir: func() (string, error) { return f.vars["HOME"], nil },
		Getwd:       func() (string, error) { return f.cwd, nil },
		Now:         func() time.Time { return time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC) },
		Stdin:       strings.NewReader(f.stdin),
		Stdout:      &f.stdout,
		Stderr:      &f.stderr,
	}
}

// newHome lays out an isolated Hermes home; tests never read the real one.
func newHome(t *testing.T, configYAML string) *fakeEnv {
	t.Helper()
	root := t.TempDir()
	hermesHome := filepath.Join(root, "hermes")
	if err := os.MkdirAll(hermesHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hermesHome, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeEnv{vars: map[string]string{"HOME": root, "HERMES_HOME": hermesHome}, cwd: cwd}
}

func TestChatStreamsReplyFromCustomProvider(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"choices":[{"delta":{"content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		} {
			_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	f := newHome(t, "model:\n  provider: custom\n  default: tiny\n  base_url: "+srv.URL+"\n")
	if err := os.WriteFile(filepath.Join(f.cwd, "AGENTS.md"), []byte("Use tabs."), 0o644); err != nil {
		t.Fatal(err)
	}
	code := Run(context.Background(), []string{"chat", "-q", "hi there"}, f.env())
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
	}
	if f.stdout.String() != "Hello\n" {
		t.Fatalf("stdout = %q", f.stdout.String())
	}
	if body["model"] != "tiny" {
		t.Errorf("model = %v", body["model"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	system, _ := msgs[0].(map[string]any)["content"].(string)
	for _, want := range []string{"You are Hermes Agent", "## AGENTS.md\n\nUse tabs.", "Provider: custom", "Model: tiny", "Platform: cli"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if user := msgs[1].(map[string]any)["content"]; user != "hi there" {
		t.Errorf("user = %v", user)
	}
}

func TestChatErrors(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		config   string
		wantCode int
		wantErr  string
	}{
		{"no command", nil, "", 2, "Usage: hermes-go"},
		{"unknown command", []string{"fly"}, "", 2, `unknown command "fly"`},
		{"empty message", []string{"chat", "-q", " "}, "", 1, "chat needs a message"},
		{"no provider", []string{"chat", "-q", "x"}, "", 1, "no LLM provider configured"},
		{"unknown provider", []string{"chat", "--provider", "nope", "-q", "x"}, "", 1, `unknown provider "nope"`},
		{"missing profile", []string{"chat", "-p", "ghost", "-q", "x"}, "", 1, "ghost"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newHome(t, tt.config)
			code := Run(context.Background(), tt.args, f.env())
			if code != tt.wantCode || !strings.Contains(f.stderr.String(), tt.wantErr) {
				t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
			}
		})
	}
}

func TestChatRunsToolCalls(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{`{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`}
		if len(bodies) == 1 {
			chunks = []string{`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"todo_list","arguments":"{\"todos\":[{\"id\":\"1\",\"content\":\"plan\",\"status\":\"pending\"}]}"}}]},"finish_reason":"tool_calls"}]}`}
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	f := newHome(t, "model:\n  provider: custom\n  default: tiny\n  base_url: "+srv.URL+"\n")
	code := Run(context.Background(), []string{"chat", "--toolsets", "todo", "-v", "-q", "plan it"}, f.env())
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
	}
	if f.stdout.String() != "done\n" || !strings.Contains(f.stderr.String(), "[tool] todo_list") {
		t.Fatalf("stdout %q stderr %s", f.stdout.String(), f.stderr.String())
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d", len(bodies))
	}
	specs, _ := bodies[0]["tools"].([]any)
	if len(specs) != 1 {
		t.Fatalf("tools = %v", specs)
	}
	msgs, _ := bodies[1]["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" || !strings.Contains(last["content"].(string), `"revision":1`) {
		t.Fatalf("tool result = %v", last)
	}
}

func TestChatRecordsSessionAndMemory(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`
		if len(bodies) == 1 {
			chunk = `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"memory","arguments":"{\"target\":\"user\",\"action\":\"add\",\"content\":\"Name is Uli\"}"}}]},"finish_reason":"tool_calls"}]}`
		}
		_, _ = io.WriteString(w, "data: "+chunk+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	f := newHome(t, "model:\n  provider: custom\n  default: tiny\n  base_url: "+srv.URL+"\n")
	for _, q := range []string{"remember my name", "hello again"} {
		if code := Run(context.Background(), []string{"chat", "--toolsets", "memory,session_search", "-q", q}, f.env()); code != 0 {
			t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
		}
	}
	if len(bodies) != 3 {
		t.Fatalf("requests = %d", len(bodies))
	}
	// The second session's frozen prompt carries the profile and the guidance.
	for _, want := range []string{"USER PROFILE (who the user is)", "Name is Uli", "use session_search to recall it", `"name":"memory"`, `"name":"session_search"`} {
		if !strings.Contains(bodies[2], want) {
			t.Fatalf("second session request lacks %q: %s", want, bodies[2])
		}
	}
	hermes := f.vars["HERMES_HOME"]
	if data, err := os.ReadFile(filepath.Join(hermes, "hermes-go", "memories", "USER.md")); err != nil || string(data) != "Name is Uli" {
		t.Fatalf("USER.md %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(hermes, "memories")); !os.IsNotExist(err) {
		t.Fatalf("upstream memories dir touched: %v", err)
	}
	ctx := context.Background()
	st, err := state.Open(ctx, filepath.Join(hermes, "hermes-go", "state.db"), state.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sessions, err := st.RecentSessions(ctx, 5, nil)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("sessions %+v %v", sessions, err)
	}
	var first state.Session
	for _, s := range sessions {
		if s.Preview == "remember my name" {
			first = s
		}
	}
	if first.Source != "cli" || first.Model != "tiny" || first.MessageCount != 4 || first.ToolCallCount != 1 || first.EndReason != "cli_close" || first.InputTokens != 7 {
		t.Fatalf("session %+v", first)
	}
	msgs, err := st.Messages(ctx, first.ID)
	if err != nil || len(msgs) != 4 || msgs[2].ToolName != "memory" || !strings.Contains(msgs[1].ToolCalls, `"name":"memory"`) {
		t.Fatalf("messages %+v %v", msgs, err)
	}
}
