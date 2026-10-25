package openai

import (
	"context"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// TestStreamLengthWithToolCallKeepsMaxTokens pins the signal the agent loop
// keys on: a stream cut by the output limit while a tool call was still being
// written must report max_tokens, not tool_use, or the loop executes a prefix
// the model never finished asking for.
func TestStreamLengthWithToolCallKeepsMaxTokens(t *testing.T) {
	body := sse(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"rm -rf /tmp/x"}}]}}]}`,
		`{"choices":[{"finish_reason":"length"}]}`,
		`[DONE]`,
	)
	srv := server(t, 200, body, nil)
	c := &Client{BaseURL: srv.URL + "/v1/", APIKey: "k"}
	resp, err := c.Stream(context.Background(), llm.Request{Model: "m",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}, func(llm.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopMaxTokens {
		t.Fatalf("stop = %q, want %q so the loop treats the call as truncated", resp.StopReason, llm.StopMaxTokens)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("calls = %v", resp.Message.ToolCalls)
	}
}

// TestStopReasonTable pins every mapping, so the length precedence cannot move
// another one with it.
func TestStopReasonTable(t *testing.T) {
	cases := []struct {
		finish   string
		hasTools bool
		want     string
	}{
		{"length", true, llm.StopMaxTokens}, {"length", false, llm.StopMaxTokens},
		{"tool_calls", true, llm.StopToolUse}, {"tool_calls", false, llm.StopToolUse},
		{"function_call", true, llm.StopToolUse}, {"stop", true, llm.StopToolUse},
		{"stop", false, llm.StopEnd}, {"", true, llm.StopToolUse}, {"", false, llm.StopEnd},
	}
	for _, c := range cases {
		if got := stopReason(c.finish, c.hasTools); got != c.want {
			t.Errorf("stopReason(%q,%v) = %q, want %q", c.finish, c.hasTools, got, c.want)
		}
	}
}
