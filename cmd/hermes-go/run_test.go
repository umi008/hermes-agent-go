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
		{"empty message", []string{"chat"}, "", 1, "chat needs a message"},
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
