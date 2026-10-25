package compress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

type fakeClient struct {
	prompts []string
	reply   string
	err     error
}

func (f *fakeClient) Stream(_ context.Context, req llm.Request, _ func(llm.Event)) (llm.Response, error) {
	f.prompts = append(f.prompts, req.Messages[0].Content)
	if f.err != nil {
		return llm.Response{}, f.err
	}
	return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: f.reply}, StopReason: llm.StopEnd}, nil
}

func TestThresholdTokens(t *testing.T) {
	tests := []struct {
		ctx, maxTokens int
		threshold      float64
		want           int
	}{
		{256_000, 0, 0.5, 192_000},      // small windows use at least 0.75
		{1_000_000, 0, 0.5, 500_000},    // large windows keep the configured fraction
		{32_000, 0, 0.5, 27_200},        // the 64K floor cannot pass 85% of the window
		{200_000, 40_000, 0.5, 120_000}, // output reservation shrinks the window
	}
	for _, tt := range tests {
		c := New(Options{ContextLength: tt.ctx, MaxTokens: tt.maxTokens, Threshold: tt.threshold})
		if got := c.ThresholdTokens(); got != tt.want {
			t.Errorf("ctx %d: got %d want %d", tt.ctx, got, tt.want)
		}
	}
	if !New(Options{}).ShouldCompress(192_000) || New(Options{}).ShouldCompress(191_999) {
		t.Fatal("ShouldCompress boundary")
	}
}

func TestEstimateTokens(t *testing.T) {
	for s, want := range map[string]int{"": 0, "abcd": 1, "abcde": 2, "héllo": 2, "日本": 2, "日本ab": 3} {
		if got := EstimateTokens(s); got != want {
			t.Errorf("%q = %d want %d", s, got, want)
		}
	}
}

// conversation builds rounds of user ask, tool call, long tool output and
// assistant reply, each row roughly 1000 tokens.
func conversation(rounds int) []llm.Message {
	big := strings.Repeat("x", 4000)
	var msgs []llm.Message
	for i := range rounds {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("task %d %s", i, big)},
			llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: "terminal", Arguments: json.RawMessage(`{"command":"ls -la","timeout":5}`)}}},
			llm.Message{Role: llm.RoleTool, ToolCallID: id, Content: fmt.Sprintf("error %d: %s", i, big)},
			llm.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("done %d %s", i, big)},
		)
	}
	return msgs
}

func checkPairs(t *testing.T, msgs []llm.Message) {
	t.Helper()
	calls := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = true
		}
		if m.Role == llm.RoleTool && !calls[m.ToolCallID] {
			t.Fatalf("orphan tool result %s", m.ToolCallID)
		}
	}
}

func TestCompressSummarizesMiddle(t *testing.T) {
	fc := &fakeClient{reply: "<think>scratch</think>## Goal\nship it"}
	now := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	c := New(Options{Client: fc, Model: "m", ContextLength: 40_000, Now: func() time.Time { return now }})
	in := conversation(10)
	orig := in[5].Content
	out, err := c.Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if in[5].Content != orig {
		t.Fatal("input was modified")
	}
	if len(out) >= len(in) || out[0].Content != in[0].Content || out[2].Content != in[2].Content {
		t.Fatalf("head not kept: %d rows", len(out))
	}
	var summary string
	for _, m := range out {
		if strings.Contains(m.Content, prompts.EndMarker) {
			summary = m.Content
		}
	}
	if !strings.HasPrefix(summary, prompts.SummaryPrefix+"\n## Goal\nship it") || strings.Contains(summary, "scratch") {
		t.Fatalf("summary %q", truncRunes(summary, 300))
	}
	last := out[len(out)-1]
	if last.Content != in[len(in)-1].Content {
		t.Fatal("newest row not in tail")
	}
	lastUser := -1
	for i, m := range out {
		if m.Role == llm.RoleUser && strings.HasPrefix(m.Content, "task 9") {
			lastUser = i
		}
	}
	if lastUser < 0 {
		t.Fatal("newest user turn not kept")
	}
	checkPairs(t, out)
	p := fc.prompts[0]
	for _, want := range []string{
		"You are a summarization agent", "TURNS TO SUMMARIZE:", "[TOOL RESULT c1]: [terminal] command=ls -la timeout=5 (4,009 chars result)",
		"Target ~2000 tokens", "The current date is 2026-10-24", "\n  terminal({\"command\":\"ls -la\",\"timeout\":5})\n]",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "@@") || strings.Contains(p, "987654321") {
		t.Fatal("placeholder left in prompt")
	}

	// A second pass updates the previous summary and protects no head.
	out2, err := c.Compress(context.Background(), append(out, conversation(6)...))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fc.prompts[1], "PREVIOUS SUMMARY:\n## Goal\nship it") || c.Count() != 2 {
		t.Fatalf("iterative prompt %q", truncRunes(fc.prompts[1], 200))
	}
	if !strings.Contains(out2[0].Content, prompts.EndMarker) || out2[0].Role != llm.RoleUser {
		t.Fatalf("first row %q", truncRunes(out2[0].Content, 100))
	}
	checkPairs(t, out2)
}

func TestCompressFallsBackDeterministically(t *testing.T) {
	c := New(Options{Client: &fakeClient{err: errors.New("boom")}, ContextLength: 40_000})
	out, err := c.Compress(context.Background(), conversation(10))
	if err != nil {
		t.Fatal(err)
	}
	var summary string
	for _, m := range out {
		if strings.Contains(m.Content, prompts.EndMarker) {
			summary = m.Content
		}
	}
	for _, want := range []string{"deterministic fallback", "User asked: 'task ", "Summary failure reason: boom.", ". [terminal] command=ls -la timeout=5 (0 chars result)", "## Blocked\n- error "} {
		if !strings.Contains(summary, want) {
			t.Fatalf("fallback lacks %q:\n%s", want, truncRunes(summary, 3000))
		}
	}
}

func TestCompressTooShort(t *testing.T) {
	c := New(Options{Client: &fakeClient{reply: "x"}})
	if _, err := c.Compress(context.Background(), conversation(1)); !errors.Is(err, ErrNothingToCompress) {
		t.Fatalf("err %v", err)
	}
}

func TestSanitizeToolPairs(t *testing.T) {
	in := []llm.Message{
		{Role: llm.RoleTool, ToolCallID: "gone", Content: "orphan"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: llm.RoleTool, ToolCallID: "a", Content: "ok"},
		{Role: llm.RoleUser, Content: "next"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "live"}}},
	}
	out := sanitizeToolPairs(in)
	if len(out) != 4 || len(out[0].ToolCalls) != 1 || out[0].ToolCalls[0].ID != "a" || len(out[3].ToolCalls) != 1 {
		t.Fatalf("%+v", out)
	}
	out = sanitizeToolPairs([]llm.Message{{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "x"}}}, {Role: llm.RoleUser, Content: "u"}, {Role: llm.RoleAssistant, Content: "a"}})
	if out[0].Content != "(tool call removed)" || len(out[0].ToolCalls) != 0 {
		t.Fatalf("%+v", out[0])
	}
}

func TestPlaceMergesWhenNoRoleFits(t *testing.T) {
	head := []llm.Message{{Role: llm.RoleUser, Content: "h"}}
	tail := []llm.Message{{Role: llm.RoleAssistant, Content: "t"}}
	// Head ends in user, tail starts with assistant: neither role alternates.
	got := place("S", head, tail, true)
	if len(got) != 1 || !strings.HasPrefix(got[0].Content, prompts.MergedHeader+"\nt\n\n"+prompts.MergedDelimiter+"\n\nS") {
		t.Fatalf("%+v", got)
	}
	got = place("S", nil, []llm.Message{{Role: llm.RoleUser, Content: "u"}}, true)
	if len(got) != 1 || got[0].Content != "S\n\n"+prompts.EndMarker+"\n\nu" {
		t.Fatalf("%+v", got)
	}
}

// blockingClient waits for its context, as a stalled provider would.
type blockingClient struct{}

func (blockingClient) Stream(ctx context.Context, _ llm.Request, _ func(llm.Event)) (llm.Response, error) {
	<-ctx.Done()
	return llm.Response{}, ctx.Err()
}

func TestSummaryTimeoutFallsBack(t *testing.T) {
	c := New(Options{Client: blockingClient{}, ContextLength: 40_000, Timeout: 10 * time.Millisecond})
	out, err := c.Compress(context.Background(), conversation(10))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range out {
		found = found || strings.Contains(m.Content, "Summary failure reason: context deadline exceeded")
	}
	if !found {
		t.Fatal("a timed-out summary must fall back deterministically")
	}
}
