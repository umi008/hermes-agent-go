package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/config"
)

// approvalConfig parses one config.yaml body.
func approvalConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	raw, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return config.FromMap(raw)
}

// TestApprovalsMode pins upstream _normalize_approval_mode: YAML 1.1 reads a
// bare `off` as false, an unknown mode falls back to manual, and the warning
// says why.
func TestApprovalsMode(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"", "manual"},
		{"approvals: {mode: manual}\n", "manual"},
		{"approvals: {mode: smart}\n", "smart"},
		{"approvals: {mode: SMART}\n", "smart"},
		{"approvals: {mode: off}\n", "off"},
		{"approvals: {mode: false}\n", "off"},
		{"approvals: {mode: true}\n", "manual"},
		{"approvals: {mode: auto}\n", "manual"},
		{"approvals: {mode: \"\"}\n", "manual"},
	} {
		cfg := approvalConfig(t, tc.yaml)
		if got := approvalsMode(cfg, slog.New(slog.DiscardHandler)); got != tc.want {
			t.Errorf("%q = %q, want %q", tc.yaml, got, tc.want)
		}
	}
	if got := denialBreakerThreshold(approvalConfig(t, "approvals: {}\n")); got != 3 {
		t.Errorf("default breaker = %d, want 3", got)
	}
	if got := denialBreakerThreshold(approvalConfig(t, "approvals: {denial_breaker_threshold: 0}\n")); got != 0 {
		t.Errorf("disabled breaker = %d, want 0", got)
	}
	if got := denialBreakerThreshold(approvalConfig(t, "approvals: {denial_breaker_threshold: 7}\n")); got != 7 {
		t.Errorf("configured breaker = %d, want 7", got)
	}
}

// guardianModel answers every request with one verdict word and records the
// bodies, so a test can see what the guardian was asked.
func guardianModel(t *testing.T, verdict string) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		calls = append(calls, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": verdict}, "finish_reason": "stop"}},
		})
		_, _ = io.WriteString(w, "data: "+string(payload)+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), calls...)
	}
}

// TestSmartApprovalDecidesTheCommand drives one interactive turn through
// approvals.mode: smart: the guardian decides, and only a deny or an escalate
// reaches the human.
func TestSmartApprovalDecidesTheCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verdict  string
		answer   string
		wantMenu string
		wantOut  string
		blocked  bool
		asked    bool
	}{
		{name: "approve", verdict: "APPROVE", answer: "", asked: false},
		{name: "deny once", verdict: "DENY", answer: "o", wantMenu: "[o]nce  |  [d]eny", wantOut: "Allowed once", asked: true},
		{name: "deny denies", verdict: "DENY", answer: "d", wantMenu: "[o]nce  |  [d]eny", wantOut: "Denied", blocked: true, asked: true},
		{name: "escalate", verdict: "ESCALATE", answer: "o", wantMenu: "[o]nce  |  [s]ession  |  [d]eny", wantOut: "Allowed once", asked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// git push --force is flagged; the temp cwd is no repository, so an
			// approved run only fails.
			srv, record := toolCallModel(t, "terminal", `{"command":"git push --force"}`)
			judge, guardianCalls := guardianModel(t, tc.verdict)
			f := newHome(t, modelConfig(srv.URL)+
				"approvals: {mode: smart}\n"+
				"auxiliary: {approval: {base_url: \""+judge.URL+"\", model: judge}}\n")
			f.stdin = "push it\n" + tc.answer + "\n"
			if code := Run(context.Background(), []string{"chat", "--toolsets", "terminal"}, f.env()); code != 0 {
				t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
			}
			out := f.stdout.String()
			if strings.Contains(out, "DANGEROUS COMMAND") != tc.asked {
				t.Fatalf("prompt shown = %v, want %v:\nstdout: %s\nstderr: %s", !tc.asked, tc.asked, out, f.stderr.String())
			}
			if tc.wantMenu != "" && !strings.Contains(out, tc.wantMenu) {
				t.Errorf("stdout lacks %q:\n%s", tc.wantMenu, out)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("stdout lacks %q:\n%s", tc.wantOut, out)
			}
			if res := lastToolResult(t, record()); strings.Contains(res, "User denied this command") != tc.blocked {
				t.Errorf("tool result = %s", res)
			}
			// The guardian saw the command in the untrusted block, never a
			// comment the model could hide behind.
			calls := guardianCalls()
			if tc.verdict == "APPROVE" && len(calls) == 0 {
				t.Fatal("the guardian was never asked")
			}
			if len(calls) > 0 {
				msgs, _ := calls[0]["messages"].([]any)
				if len(msgs) != 2 {
					t.Fatalf("guardian messages = %v", msgs)
				}
				user, _ := msgs[1].(map[string]any)["content"].(string)
				if !strings.Contains(user, "<command>\ngit push --force\n</command>") {
					t.Errorf("guardian prompt = %s", user)
				}
			}
		})
	}
}
