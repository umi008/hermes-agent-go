package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatShellHookBlocksTool(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := `"` + exe + `" fake-hook`
	cmdYAML, _ := json.Marshal(command)
	hooksYAML := "hooks:\n  pre_tool_call:\n    - command: " + string(cmdYAML) + "\n      matcher: todo\n"
	allowlist := func(f *fakeEnv) {
		body, _ := json.Marshal(map[string]any{"approvals": []any{map[string]any{"event": "pre_tool_call", "command": command}}})
		if err := os.WriteFile(filepath.Join(f.vars["HERMES_HOME"], "shell-hooks-allowlist.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tests := map[string]struct {
		args    []string
		extra   string
		setup   func(*fakeEnv)
		env     map[string]string
		blocked bool
	}{
		"not allowlisted": {},
		"accept flag":     {args: []string{"--accept-hooks"}, blocked: true},
		"accept env":      {env: map[string]string{"HERMES_ACCEPT_HOOKS": "1"}, blocked: true},
		"auto accept":     {extra: "hooks_auto_accept: true\n", blocked: true},
		"allowlisted":     {setup: allowlist, blocked: true},
		"safe mode":       {args: []string{"--accept-hooks"}, env: map[string]string{"HERMES_SAFE_MODE": "1"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv, record := toolCallModel(t, "todo", `{"todos":[]}`)
			f := newHome(t, "model:\n  provider: custom\n  default: tiny\n  base_url: "+srv.URL+"\n"+hooksYAML+tc.extra)
			for k, v := range tc.env {
				f.vars[k] = v
			}
			if tc.setup != nil {
				tc.setup(f)
			}
			args := append([]string{"chat", "-q", "hi"}, tc.args...)
			if code := Run(context.Background(), args, f.env()); code != 0 {
				t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
			}
			res := lastToolResult(t, record())
			if blocked := res == `{"error":"todo is off limits"}`; blocked != tc.blocked {
				t.Errorf("tool result = %s, want blocked=%v", res, tc.blocked)
			}
			if !tc.blocked && tc.env == nil && !strings.Contains(f.stderr.String(), "not allowlisted") {
				t.Errorf("missing skip warning: %s", f.stderr.String())
			}
		})
	}
}
