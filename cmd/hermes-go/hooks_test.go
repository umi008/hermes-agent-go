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

// TestChatLifecycleHooks covers T4 end to end: pre_llm_call context rides
// the user message sent to the model, and the session events fire with the
// session id and the cli platform.
func TestChatLifecycleHooks(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dumpDir := t.TempDir()
	t.Setenv("HERMES_HOOK_DUMP", dumpDir)
	dump, _ := json.Marshal(`"` + exe + `" fake-hook-dump`)
	note, _ := json.Marshal(`"` + exe + `" fake-hook-context`)
	srv, record := toolCallModel(t, "todo", `{"todos":[]}`)
	f := newHome(t, "model:\n  provider: custom\n  default: tiny\n  base_url: "+srv.URL+"\nhooks:\n"+
		"  pre_llm_call:\n    - command: "+string(note)+"\n"+
		"  on_session_start:\n    - command: "+string(dump)+"\n"+
		"  on_session_end:\n    - command: "+string(dump)+"\n")
	f.vars["HERMES_ACCEPT_HOOKS"] = "1"
	if code := Run(context.Background(), []string{"chat", "-q", "hi"}, f.env()); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
	}
	calls := record()
	for i, c := range calls {
		var user map[string]any
		for _, m := range c["messages"].([]any) {
			if m := m.(map[string]any); m["role"] == "user" {
				user = m
				break
			}
		}
		if user["content"] != "hi\n\nPLUGIN NOTE" {
			t.Errorf("call %d user message = %v", i, user)
		}
	}
	entries, err := os.ReadDir(dumpDir)
	if err != nil {
		t.Fatal(err)
	}
	events := map[string]map[string]any{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dumpDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		events[p["hook_event_name"].(string)] = p
	}
	start, end := events["on_session_start"], events["on_session_end"]
	if len(events) != 2 || start == nil || end == nil {
		t.Fatalf("events = %v", events)
	}
	if start["session_id"] == "" || start["session_id"] != end["session_id"] {
		t.Errorf("session ids: start %v, end %v", start["session_id"], end["session_id"])
	}
	se, ee := start["extra"].(map[string]any), end["extra"].(map[string]any)
	if se["platform"] != "cli" || se["model"] != "tiny" {
		t.Errorf("start extra = %v", se)
	}
	if ee["completed"] != true || ee["turn_exit_reason"] != "text_response(finish_reason=stop)" || ee["platform"] != "cli" {
		t.Errorf("end extra = %v", ee)
	}
}
