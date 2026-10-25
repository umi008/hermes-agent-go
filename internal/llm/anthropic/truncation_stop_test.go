package anthropic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// cutToolStream is a message cut while a tool call was being written.
func cutToolStream(stop string, withTool bool) string {
	var b strings.Builder
	b.WriteString(event("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1}}}`))
	b.WriteString(event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	b.WriteString(event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut"}}`))
	b.WriteString(event("content_block_stop", `{"type":"content_block_stop","index":0}`))
	if withTool {
		b.WriteString(event("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_9","name":"terminal","input":{}}}`))
		b.WriteString(event("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"rm -rf /tm"}}`))
		b.WriteString(event("content_block_stop", `{"type":"content_block_stop","index":1}`))
	}
	b.WriteString(event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stop+`"},"usage":{"output_tokens":40}}`))
	b.WriteString(event("message_stop", `{"type":"message_stop"}`))
	return b.String()
}

// TestMaxTokensWinsOverToolUseBlocks pins the precedence: a max_tokens cut must
// keep its signal even when the message carries tool_use blocks, or the agent
// loop treats a truncated call as a finished one.
func TestMaxTokensWinsOverToolUseBlocks(t *testing.T) {
	for _, c := range []struct {
		stop     string
		withTool bool
		want     string
	}{
		{"max_tokens", true, llm.StopMaxTokens}, {"max_tokens", false, llm.StopMaxTokens},
		{"tool_use", true, llm.StopToolUse}, {"end_turn", true, llm.StopToolUse},
		{"end_turn", false, llm.StopEnd},
	} {
		srv := serve(t, 200, cutToolStream(c.stop, c.withTool), nil)
		resp, err := New(Options{BaseURL: srv.URL, APIKey: "k"}).Stream(context.Background(),
			llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}}}, nil)
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if resp.StopReason != c.want {
			t.Errorf("stop=%q tools=%v = %q, want %q", c.stop, c.withTool, resp.StopReason, c.want)
		}
	}
}

// TestTruncatedToolUseReplaysWithPairedRow pins that the recorded cut message
// still serializes into a request once its tool row answers it.
func TestTruncatedToolUseReplaysWithPairedRow(t *testing.T) {
	srv := serve(t, 200, cutToolStream("max_tokens", true), nil)
	resp, err := New(Options{BaseURL: srv.URL, APIKey: "k"}).Stream(context.Background(),
		llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls) != 1 || !json.Valid(resp.Message.ToolCalls[0].Arguments) {
		t.Fatalf("calls = %+v", resp.Message.ToolCalls)
	}
	p, err := BuildParams(llm.Request{Model: "m", Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "go"}, resp.Message,
		{Role: llm.RoleTool, ToolCallID: resp.Message.ToolCalls[0].ID, Content: "not executed", IsError: true},
	}})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, err := json.Marshal(p); err != nil {
		t.Fatalf("replay body: %v", err)
	}
}
