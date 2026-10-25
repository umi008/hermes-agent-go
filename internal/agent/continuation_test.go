package agent

import (
	"context"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// reply is one canned streamed answer.
type reply = func(llm.Request, func(llm.Event)) (llm.Response, error)

// TestRetryAfterHonored pins the rate-limit contract: a provider that says how
// long to wait is obeyed instead of the local backoff, which exists only for
// providers that say nothing.
func TestRetryAfterHonored(t *testing.T) {
	var waited []time.Duration
	c := &scriptClient{replies: []reply{
		fail(&llm.APIError{Provider: "test", Status: 429, RetryAfter: 2500 * time.Millisecond}),
		text("ok"),
	}}
	a := New(Options{
		Client: c, MaxAttempts: 3, Backoff: func(int) time.Duration { return time.Hour },
		Sleep: func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil },
	})
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if len(waited) != 1 || waited[0] != 2500*time.Millisecond {
		t.Fatalf("waited = %v, want the provider's Retry-After", waited)
	}
	// Without a Retry-After the local backoff still decides.
	waited = nil
	c = &scriptClient{replies: []reply{fail(&llm.APIError{Provider: "test", Status: 503}), text("ok")}}
	a = New(Options{
		Client: c, MaxAttempts: 3, Backoff: func(int) time.Duration { return 7 * time.Second },
		Sleep: func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil },
	})
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if len(waited) != 1 || waited[0] != 7*time.Second {
		t.Fatalf("waited = %v, want the local backoff", waited)
	}
}

// truncated answers one assistant fragment cut by the output limit.
func truncated(s string) reply {
	return func(_ llm.Request, on func(llm.Event)) (llm.Response, error) {
		on(llm.Event{Kind: llm.EventText, Text: s})
		return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: s}, StopReason: llm.StopMaxTokens}, nil
	}
}

// TestLengthContinuation pins the output-limit continuation: the fragment is
// kept, the model is asked to continue exactly where it stopped, and the turn
// ends on the model's own stop reason with the pieces stitched together.
func TestLengthContinuation(t *testing.T) {
	c := &scriptClient{replies: []reply{truncated("part one"), text(" and two")}}
	a := New(Options{Client: c, Backoff: noWait})
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.requests) != 2 {
		t.Fatalf("calls = %d, want a continuation", len(c.requests))
	}
	second := c.requests[1].Messages
	nudge := second[len(second)-1]
	if nudge.Role != llm.RoleUser || nudge.Content != lengthContinuationNudge {
		t.Fatalf("continuation nudge = %+v", nudge)
	}
	if res.StopReason != llm.StopEnd {
		t.Errorf("stop reason = %q, want the model's own", res.StopReason)
	}
	if res.Text != "part one and two" {
		t.Errorf("text = %q, want the stitched answer", res.Text)
	}
}

// TestLengthContinuationCaps pins upstream's four-continuation cap: a model
// that keeps being cut off stops being asked, and the turn reports the limit.
func TestLengthContinuationCaps(t *testing.T) {
	replies := make([]reply, 0, 6)
	for i := 0; i < 6; i++ {
		replies = append(replies, truncated("frag"))
	}
	c := &scriptClient{replies: replies}
	a := New(Options{Client: c, Backoff: noWait})
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.requests) != MaxLengthContinuations+1 {
		t.Fatalf("calls = %d, want %d", len(c.requests), MaxLengthContinuations+1)
	}
	if res.StopReason != llm.StopMaxTokens {
		t.Errorf("stop reason = %q, want max_tokens", res.StopReason)
	}
}
