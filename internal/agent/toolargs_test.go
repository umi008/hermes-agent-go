package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// recordingTools remembers every call that reached execution.
type recordingTools struct {
	mu    sync.Mutex
	calls []llm.ToolCall
}

func (r *recordingTools) Specs() []llm.ToolSpec {
	return []llm.ToolSpec{{Name: "terminal", Parameters: json.RawMessage(`{"type":"object"}`)}}
}

func (r *recordingTools) Execute(_ context.Context, call llm.ToolCall) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
	return "ran", nil
}

func (r *recordingTools) executed() []llm.ToolCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]llm.ToolCall(nil), r.calls...)
}

// cutToolCall answers a call whose arguments were cut by the output limit: the
// tool name and the arguments' prefix arrived, nothing else did.
func cutToolCall(id, name, args string) reply {
	return func(_ llm.Request, on func(llm.Event)) (llm.Response, error) {
		on(llm.Event{Kind: llm.EventText, Text: "working"})
		return llm.Response{
			Message: llm.Message{Role: llm.RoleAssistant, Content: "working",
				ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(args)}}},
			StopReason: llm.StopMaxTokens,
		}, nil
	}
}

// TestTruncatedToolCallIsNotExecuted pins the guard: a stream cut while the
// model was still writing its arguments must never reach the tool, because the
// prefix alone can be a valid command that the model never finished asking for.
func TestTruncatedToolCallIsNotExecuted(t *testing.T) {
	tools := &recordingTools{}
	c := &scriptClient{replies: []reply{
		cutToolCall("c1", "terminal", `{"command":"rm -rf /tmp/x`),
		text("done"),
	}}
	a := New(Options{Client: c, Tools: tools, Backoff: noWait})
	res, err := a.Run(context.Background(), "clean up")
	if err != nil {
		t.Fatal(err)
	}
	if got := tools.executed(); len(got) != 0 {
		t.Fatalf("a truncated call reached the tool: %+v", got)
	}
	if len(c.requests) != 2 {
		t.Fatalf("model calls = %d, want a retry", len(c.requests))
	}
	history := c.requests[1].Messages
	nudge := history[len(history)-1]
	if nudge.Role != llm.RoleUser || !strings.Contains(nudge.Content, "too large") {
		t.Fatalf("continuation nudge = %+v", nudge)
	}
	if res.Text != "workingdone" {
		t.Errorf("text = %q, want the stitched answer", res.Text)
	}
}

// TestInvalidToolArgumentsAreNotExecuted pins the other half: arguments that
// are not JSON at all are refused as a tool result naming the reason, so the
// model can re-send a well-formed call instead of the tool acting on garbage.
func TestInvalidToolArgumentsAreNotExecuted(t *testing.T) {
	tools := &recordingTools{}
	broken := func(_ llm.Request, _ func(llm.Event)) (llm.Response, error) {
		return llm.Response{
			Message: llm.Message{Role: llm.RoleAssistant, Content: "calling",
				ToolCalls: []llm.ToolCall{{ID: "c1", Name: "terminal", Arguments: json.RawMessage(`{"command":`)}}},
			StopReason: llm.StopToolUse,
		}, nil
	}
	c := &scriptClient{replies: []reply{broken, text("fixed")}}
	a := New(Options{Client: c, Tools: tools, Backoff: noWait})
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if got := tools.executed(); len(got) != 0 {
		t.Fatalf("malformed arguments reached the tool: %+v", got)
	}
	var told bool
	for _, m := range c.requests[1].Messages {
		if m.Role == llm.RoleTool && strings.Contains(m.Content, "not valid JSON") {
			told = true
		}
	}
	if !told {
		t.Fatalf("the model was not told its arguments were invalid: %+v", c.requests[1].Messages)
	}
	if res.Text != "fixed" {
		t.Errorf("text = %q", res.Text)
	}
}
