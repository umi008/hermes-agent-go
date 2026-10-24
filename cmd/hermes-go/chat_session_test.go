package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// serverRuntime builds a runtime the way a server would: no REPL session.
func serverRuntime(t *testing.T, configURL string) *chatRuntime {
	t.Helper()
	f := newHome(t, configURL)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rt, err := newChatRuntime(context.Background(), chatFlags{}, f.env(), logger, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.close)
	return rt
}

// TestSessionsShareOneRuntime proves the seam the API server, cron and the
// gateways need: several sessions over one runtime, each with its own id,
// output sink and conversation, and none of them becoming the REPL's.
func TestSessionsShareOneRuntime(t *testing.T) {
	srv, _ := echoModel(t)
	rt := serverRuntime(t, modelConfig(srv.URL))
	ctx := context.Background()

	var first, second lockedBuffer
	a := rt.newSession(ctx, "api", &first)
	b := rt.newSession(ctx, "telegram", &second)
	if a.id == "" || b.id == "" {
		t.Fatalf("sessions have no id: %q %q", a.id, b.id)
	}
	if a.id == b.id {
		t.Fatalf("two sessions share an id: %q", a.id)
	}
	if rt.currentSession() != nil {
		t.Fatal("a server session must not become the REPL's current session")
	}

	// Interleaved turns: each reply lands in its own sink.
	var wg sync.WaitGroup
	for _, job := range []struct {
		s    *chatSession
		text string
	}{{a, "one"}, {b, "two"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := job.s.run(ctx, job.text); err != nil {
				t.Errorf("run %q: %v", job.text, err)
			}
		}()
	}
	wg.Wait()

	if got := first.String(); got != "re: one" {
		t.Errorf("first sink = %q, want %q", got, "re: one")
	}
	if got := second.String(); got != "re: two" {
		t.Errorf("second sink = %q, want %q", got, "re: two")
	}

	// Each session keeps its own conversation.
	for _, c := range []struct {
		name string
		s    *chatSession
		want string
	}{{"first", a, "one"}, {"second", b, "two"}} {
		var got []string
		for _, m := range c.s.agent.History() {
			got = append(got, m.Content)
		}
		if want := c.want + "|re: " + c.want; strings.Join(got, "|") != want {
			t.Errorf("%s history = %q, want %q", c.name, strings.Join(got, "|"), want)
		}
	}

	// Both conversations are persisted under their own ids.
	for _, c := range []struct {
		name string
		id   string
		want string
	}{{"first", a.id, "one|re: one"}, {"second", b.id, "two|re: two"}} {
		msgs, err := rt.store.Messages(ctx, c.id)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range msgs {
			got = append(got, m.Content)
		}
		if strings.Join(got, "|") != c.want {
			t.Errorf("%s stored = %q, want %q", c.name, strings.Join(got, "|"), c.want)
		}
	}
}

// TestServerSessionEndsAndDropsEmpty checks that a session nobody used leaves
// no row behind, so a health check or a cron tick does not litter the store.
func TestServerSessionEndsAndDropsEmpty(t *testing.T) {
	srv, _ := echoModel(t)
	rt := serverRuntime(t, modelConfig(srv.URL))
	ctx := context.Background()

	empty := rt.newSession(ctx, "api", io.Discard)
	empty.end("cli_close")
	if msgs, err := rt.store.Messages(ctx, empty.id); err != nil || len(msgs) != 0 {
		t.Fatalf("empty session kept messages: %v %v", msgs, err)
	}

	used := rt.newSession(ctx, "api", io.Discard)
	if _, err := used.run(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	used.end("cli_close")
	msgs, err := rt.store.Messages(ctx, used.id)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("used session kept %d messages, want 2", len(msgs))
	}
}

// TestSessionsHookPayloadCarriesOwnID covers S4 for the seam: each session
// wraps the shared registry with its own id, so a hook payload names the
// session that made the call, never another one.
func TestSessionsHookPayloadCarriesOwnID(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dumpDir := t.TempDir()
	t.Setenv("HERMES_HOOK_DUMP", dumpDir)
	command, _ := json.Marshal(`"` + exe + `" fake-hook-dump`)

	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		n++
		id := fmt.Sprintf("c%d", n)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if body.Messages[len(body.Messages)-1].Role == "tool" {
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, fmt.Sprintf(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":"todo_list","arguments":"{\"todos\":[{\"id\":\"1\",\"content\":\"x\",\"status\":\"pending\"}]}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n", id))
	}))
	t.Cleanup(srv.Close)

	f := newHome(t, "model:\n  provider: custom\n  default: tiny\n  base_url: "+srv.URL+
		"\nhooks:\n  pre_tool_call:\n    - command: "+string(command)+"\n")
	f.vars["HERMES_ACCEPT_HOOKS"] = "1"
	rt, err := newChatRuntime(context.Background(), chatFlags{}, f.env(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.close)

	ctx := context.Background()
	a := rt.newSession(ctx, "api", io.Discard)
	b := rt.newSession(ctx, "telegram", io.Discard)
	var wg sync.WaitGroup
	for _, s := range []*chatSession{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.run(ctx, "hi"); err != nil {
				t.Errorf("run: %v", err)
			}
		}()
	}
	wg.Wait()
	a.end("api_close")
	b.end("telegram_close")
	if a.id == b.id {
		t.Fatalf("two sessions share an id: %q", a.id)
	}

	entries, err := os.ReadDir(dumpDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("hook payloads = %d, want 2", len(entries))
	}
	var got []string
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dumpDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("payload %q: %v", raw, err)
		}
		got = append(got, p.SessionID)
	}
	want := []string{a.id, b.id}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("payload ids = %q, want %q", got, want)
	}
}
