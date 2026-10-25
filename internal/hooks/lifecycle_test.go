package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/llm"
)

func lifecycle(t *testing.T, specs ...Spec) *Lifecycle {
	t.Helper()
	for i := range specs {
		specs[i].Timeout = 10 * time.Second
	}
	return NewLifecycle(Options{
		Specs: specs, Runner: &Runner{GOOS: runtime.GOOS}, SessionID: func() string { return "s1" }, Cwd: "/work",
	}, "cli")
}

func payloadLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestLifecycleNilWithoutLifecycleSpecs(t *testing.T) {
	if l := NewLifecycle(Options{Specs: []Spec{{Event: PreToolCall, Command: "x"}}}, "cli"); l != nil {
		t.Fatalf("lifecycle = %+v, want nil for tool-only hooks", l)
	}
	var l *Lifecycle
	if l.Turns() != nil {
		t.Fatal("nil lifecycle has turn hooks")
	}
	l.SubagentStart(t.Context(), Subagent{})
	l.SubagentStop(t.Context(), Subagent{}, nil, nil, nil, 0)
	only := lifecycle(t, Spec{Event: SubagentStop, Command: "x"})
	if only == nil || only.Turns() != nil {
		t.Fatal("a subagent-only lifecycle must exist without turn hooks")
	}
}

func TestLifecycleTurnPayloads(t *testing.T) {
	log := filepath.Join(t.TempDir(), "p.jsonl")
	l := lifecycle(t,
		Spec{Event: OnSessionStart, Command: hookCmd(t, "record", log)},
		Spec{Event: PreLLMCall, Command: hookCmd(t, "record", log)},
		Spec{Event: PreLLMCall, Command: hookCmd(t, "context", "first")},
		Spec{Event: PreLLMCall, Command: hookCmd(t, "block")},
		Spec{Event: PreLLMCall, Command: hookCmd(t, "context", "second")},
		Spec{Event: PostLLMCall, Command: hookCmd(t, "record", log)},
		Spec{Event: OnSessionEnd, Command: hookCmd(t, "record", log)},
	)
	h := l.Turns()
	turn := agent.TurnInfo{UserMessage: "hi", FirstTurn: true, Model: "m1", History: []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
	}}
	h.SessionStart(t.Context(), "m1")
	if got := h.PreLLMCall(t.Context(), turn); got != "first\n\nsecond" {
		t.Fatalf("pre_llm_call context = %q", got)
	}
	turn.History = append(turn.History,
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "terminal", Arguments: json.RawMessage(`{"command":"ls"}`)}}},
		llm.Message{Role: llm.RoleTool, ToolCallID: "c1", Content: "a.txt"},
		llm.Message{Role: llm.RoleAssistant, Content: "done"})
	h.PostLLMCall(t.Context(), turn, "done")
	h.TurnEnd(t.Context(), turn, agent.TurnEnd{Completed: true, Reason: "text_response(finish_reason=stop)"})

	lines := payloadLines(t, log)
	head := `{"hook_event_name":"%s","tool_name":null,"tool_input":null,"session_id":"s1","cwd":"/work","profile":"default","extra":`
	want := []string{
		strings.Replace(head, "%s", OnSessionStart, 1) + `{"model":"m1","platform":"cli"}}`,
		strings.Replace(head, "%s", PreLLMCall, 1) + `{"task_id":"","turn_id":"","user_message":"hi",` +
			`"conversation_history":[{"role":"user","content":"hi"}],"is_first_turn":true,"model":"m1","platform":"cli","sender_id":""}}`,
		strings.Replace(head, "%s", PostLLMCall, 1) + `{"task_id":"","turn_id":"","user_message":"hi","assistant_response":"done",` +
			`"conversation_history":[{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"ls\"}"}}]},` +
			`{"role":"tool","content":"a.txt","tool_call_id":"c1"},{"role":"assistant","content":"done"}],"model":"m1","platform":"cli"}}`,
		strings.Replace(head, "%s", OnSessionEnd, 1) + `{"task_id":"","turn_id":"","completed":true,"failed":false,"interrupted":false,` +
			`"turn_exit_reason":"text_response(finish_reason=stop)","model":"m1","platform":"cli"}}`,
	}
	if len(lines) != len(want) {
		t.Fatalf("payloads = %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("payload %d\n got  %s\n want %s", i, lines[i], want[i])
		}
	}
}

func TestLifecycleSubagentPayloads(t *testing.T) {
	log := filepath.Join(t.TempDir(), "p.jsonl")
	l := lifecycle(t,
		Spec{Event: SubagentStart, Command: hookCmd(t, "record", log)},
		Spec{Event: SubagentStop, Command: hookCmd(t, "record", log)},
	)
	c := Subagent{ParentSessionID: "parent", SessionID: "child", ID: "sa-0-abcdef12", Goal: "audit"}
	l.SubagentStart(t.Context(), c)
	l.SubagentStop(t.Context(), c, "summary", "completed", []map[string]any{}, 1500)
	lines := payloadLines(t, log)
	head := `{"hook_event_name":"%s","tool_name":null,"tool_input":null,"session_id":"parent","cwd":"/work","profile":"default","extra":`
	want := []string{
		strings.Replace(head, "%s", SubagentStart, 1) + `{"parent_turn_id":"","parent_subagent_id":null,"child_session_id":"child",` +
			`"child_subagent_id":"sa-0-abcdef12","child_role":"leaf","child_goal":"audit"}}`,
		strings.Replace(head, "%s", SubagentStop, 1) + `{"parent_turn_id":"","child_session_id":"child","child_role":"leaf",` +
			`"child_summary":"summary","child_status":"completed","tool_call_history":[],"duration_ms":1500}}`,
	}
	if len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("payloads\n got  %s\n want %s", strings.Join(lines, "\n      "), strings.Join(want, "\n      "))
	}
}
