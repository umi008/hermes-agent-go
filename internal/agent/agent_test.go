package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// scriptClient replays canned replies and records each request.
type scriptClient struct {
	mu       sync.Mutex
	replies  []func(llm.Request, func(llm.Event)) (llm.Response, error)
	requests []llm.Request
}

func (c *scriptClient) Stream(_ context.Context, req llm.Request, onEvent func(llm.Event)) (llm.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	req.Messages = append([]llm.Message(nil), req.Messages...)
	c.requests = append(c.requests, req)
	if len(c.replies) == 0 {
		return llm.Response{}, errors.New("script exhausted")
	}
	next := c.replies[0]
	c.replies = c.replies[1:]
	return next(req, onEvent)
}

func text(s string) func(llm.Request, func(llm.Event)) (llm.Response, error) {
	return func(_ llm.Request, on func(llm.Event)) (llm.Response, error) {
		on(llm.Event{Kind: llm.EventText, Text: s})
		return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: s}, StopReason: llm.StopEnd, Usage: llm.Usage{InputTokens: 1}}, nil
	}
}

func toolCalls(calls ...llm.ToolCall) func(llm.Request, func(llm.Event)) (llm.Response, error) {
	return func(llm.Request, func(llm.Event)) (llm.Response, error) {
		return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: calls}, StopReason: llm.StopToolUse}, nil
	}
}

func fail(err error) func(llm.Request, func(llm.Event)) (llm.Response, error) {
	return func(llm.Request, func(llm.Event)) (llm.Response, error) { return llm.Response{}, err }
}

// fakeTools echoes arguments; "boom" fails; "slow" waits on a barrier so the
// test proves calls run concurrently.
type fakeTools struct{ barrier *sync.WaitGroup }

func (fakeTools) Specs() []llm.ToolSpec {
	return []llm.ToolSpec{{Name: "echo", Parameters: json.RawMessage(`{"type":"object"}`)}}
}

func (f fakeTools) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	switch call.Name {
	case "boom":
		return "", errors.New("tool failed")
	case "slow":
		f.barrier.Done()
		done := make(chan struct{})
		go func() { f.barrier.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			return "", errors.New("calls did not run concurrently")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return call.Name + ":" + string(call.Arguments), nil
}

func noWait(int) time.Duration { return 0 }

func TestRun(t *testing.T) {
	retryable := &llm.APIError{Provider: "p", Status: 529}
	tests := []struct {
		name      string
		replies   []func(llm.Request, func(llm.Event)) (llm.Response, error)
		maxTurns  int
		wantText  string
		wantTurns int
		wantErr   error
		wantCalls int
	}{
		{"plain answer", []func(llm.Request, func(llm.Event)) (llm.Response, error){text("hi")}, 0, "hi", 1, nil, 1},
		{"tool then answer", []func(llm.Request, func(llm.Event)) (llm.Response, error){
			toolCalls(llm.ToolCall{ID: "1", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)}), text("done"),
		}, 0, "done", 2, nil, 2},
		{"retry transient", []func(llm.Request, func(llm.Event)) (llm.Response, error){fail(retryable), text("ok")}, 0, "ok", 1, nil, 2},
		{"retries exhausted", []func(llm.Request, func(llm.Event)) (llm.Response, error){fail(retryable), fail(retryable), fail(retryable)}, 0, "", 1, retryable, 3},
		{"fatal not retried", []func(llm.Request, func(llm.Event)) (llm.Response, error){fail(&llm.APIError{Status: 400})}, 0, "", 1, nil, 1},
		{"budget", []func(llm.Request, func(llm.Event)) (llm.Response, error){
			toolCalls(llm.ToolCall{ID: "1", Name: "echo"}), toolCalls(llm.ToolCall{ID: "2", Name: "echo"}),
		}, 2, "", 2, ErrMaxTurns, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &scriptClient{replies: tt.replies}
			a := New(Options{Client: c, Tools: fakeTools{}, MaxTurns: tt.maxTurns, Backoff: noWait})
			res, err := a.Run(context.Background(), "hello")
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.name == "fatal not retried":
				if err == nil {
					t.Fatal("want error")
				}
			case err != nil:
				t.Fatal(err)
			}
			if res.Text != tt.wantText || res.Turns != tt.wantTurns || len(c.requests) != tt.wantCalls {
				t.Fatalf("text=%q turns=%d calls=%d", res.Text, res.Turns, len(c.requests))
			}
		})
	}
}

func TestParallelToolsKeepOrderAndErrors(t *testing.T) {
	var barrier sync.WaitGroup
	barrier.Add(2)
	c := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){
		toolCalls(
			llm.ToolCall{ID: "a", Name: "slow"},
			llm.ToolCall{ID: "b", Name: "slow"},
			llm.ToolCall{ID: "c", Name: "boom"},
		),
		text("end"),
	}}
	var events []EventKind
	a := New(Options{Client: c, Tools: fakeTools{barrier: &barrier}, OnEvent: func(e Event) {
		if e.Kind != EventUsage {
			events = append(events, e.Kind)
		}
	}})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	second := c.requests[1].Messages
	if len(second) != 5 {
		t.Fatalf("history len = %d", len(second))
	}
	for i, id := range []string{"a", "b", "c"} {
		m := second[2+i]
		if m.Role != llm.RoleTool || m.ToolCallID != id {
			t.Fatalf("result %d = %+v", i, m)
		}
	}
	if second[2].IsError || !second[4].IsError || second[4].Content != "tool failed" {
		t.Fatalf("results = %+v", second[2:])
	}
	want := []EventKind{EventToolStart, EventToolStart, EventToolStart, EventToolResult, EventToolResult, EventToolResult, EventText}
	if len(events) != len(want) {
		t.Fatalf("events = %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v", events)
		}
	}
}

func TestSteerInjectsBetweenIterations(t *testing.T) {
	steer := make(chan string, 1)
	c := &scriptClient{}
	c.replies = []func(llm.Request, func(llm.Event)) (llm.Response, error){
		func(llm.Request, func(llm.Event)) (llm.Response, error) {
			steer <- "also do y"
			return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "x done"}}, nil
		},
		text("y done"),
	}
	a := New(Options{Client: c, Steer: steer})
	res, err := a.Run(context.Background(), "do x")
	if err != nil {
		t.Fatal(err)
	}
	last := c.requests[1].Messages
	if res.Text != "y done" || last[len(last)-1].Content != "also do y" {
		t.Fatalf("res=%+v last=%+v", res, last[len(last)-1])
	}
}

func TestCancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){fail(llm.ErrStreamInterrupted)}}
	a := New(Options{Client: c, Backoff: func(int) time.Duration { cancel(); return time.Hour }})
	if _, err := a.Run(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestHistoryIsAppendOnly(t *testing.T) {
	c := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){text("one"), text("two")}}
	a := New(Options{Client: c})
	for _, in := range []string{"a", "b"} {
		if _, err := a.Run(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	first, second := c.requests[0].Messages, c.requests[1].Messages
	for i := range first {
		if first[i].Content != second[i].Content {
			t.Fatalf("prefix changed at %d", i)
		}
	}
	got := make([]string, 0, len(a.History()))
	for _, m := range a.History() {
		got = append(got, m.Content)
	}
	if strings.Join(got, ",") != "a,one,b,two" {
		t.Fatalf("history = %v", got)
	}
}

func TestJitteredBackoff(t *testing.T) {
	for _, tt := range []struct {
		attempt  int
		min, max time.Duration
	}{{1, 5 * time.Second, 7500 * time.Millisecond}, {3, 20 * time.Second, 30 * time.Second}, {50, 120 * time.Second, 180 * time.Second}} {
		if d := JitteredBackoff(tt.attempt); d < tt.min || d > tt.max {
			t.Errorf("attempt %d: %v not in [%v,%v]", tt.attempt, d, tt.min, tt.max)
		}
	}
}

func TestOnMessageAndUsageEvents(t *testing.T) {
	c := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){
		toolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}), text("done"),
	}}
	var roles []string
	usage := 0
	a := New(Options{Client: c, Tools: fakeTools{},
		OnMessage: func(m llm.Message) { roles = append(roles, string(m.Role)) },
		OnEvent: func(e Event) {
			if e.Kind == EventUsage {
				usage++
			}
		},
	})
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(roles, ",") != "user,assistant,tool,assistant" || usage != 2 {
		t.Fatalf("roles %v usage events %d", roles, usage)
	}
}

// fakeCompressor fires at limit tokens and keeps only a summary plus the
// newest row.
type fakeCompressor struct {
	limit int
	err   error
	seen  []int
	calls int
}

func (f *fakeCompressor) ShouldCompress(n int) bool { f.seen = append(f.seen, n); return n >= f.limit }

func (f *fakeCompressor) Estimate(_ []string, msgs []llm.Message, _ []llm.ToolSpec) int {
	return len(msgs)
}

func (f *fakeCompressor) Compress(_ context.Context, msgs []llm.Message) ([]llm.Message, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return []llm.Message{{Role: llm.RoleUser, Content: "summary"}, msgs[len(msgs)-1]}, nil
}

func (f *fakeCompressor) Note() string { return "NOTE" }

func TestCompressorShrinksHistoryBeforeCall(t *testing.T) {
	call := func(id string, tokens int) func(llm.Request, func(llm.Event)) (llm.Response, error) {
		return func(llm.Request, func(llm.Event)) (llm.Response, error) {
			return llm.Response{
				Message:    llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: "echo", Arguments: json.RawMessage(`{}`)}}},
				StopReason: llm.StopToolUse, Usage: llm.Usage{InputTokens: tokens - 10, CacheReadTokens: 10},
			}, nil
		}
	}
	c := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){
		call("c1", 50), call("c2", 100), text("done"),
	}}
	fc := &fakeCompressor{limit: 100}
	var recorded, compressed int
	a := New(Options{Client: c, Tools: fakeTools{}, System: []string{"sys"}, Compressor: fc,
		OnMessage: func(llm.Message) { recorded++ },
		OnEvent: func(e Event) {
			if e.Kind == EventCompress && e.Err == nil {
				compressed++
			}
		},
	})
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	// The reported prompt (cache reads included) beats the row estimate.
	if fc.calls != 1 || compressed != 1 || fc.seen[2] != 100 {
		t.Fatalf("compress calls %d events %d seen %v", fc.calls, compressed, fc.seen)
	}
	last := c.requests[2]
	if len(last.Messages) != 2 || last.Messages[0].Content != "summary" || last.Messages[1].ToolCallID != "c2" {
		t.Fatalf("compacted request %+v", last.Messages)
	}
	if strings.Join(c.requests[1].System, "|") != "sys" || strings.Join(last.System, "|") != "sys|NOTE" {
		t.Fatalf("system %v then %v", c.requests[1].System, last.System)
	}
	// Persistence sees each new message once and never the summary.
	if recorded != 6 || len(a.History()) != 3 {
		t.Fatalf("recorded %d history %d", recorded, len(a.History()))
	}
}

func TestCompressorFailuresAreCappedAndNonFatal(t *testing.T) {
	var replies []func(llm.Request, func(llm.Event)) (llm.Response, error)
	for i := range 5 {
		replies = append(replies, toolCalls(llm.ToolCall{ID: string(rune('a' + i)), Name: "echo", Arguments: json.RawMessage(`{}`)}))
	}
	c := &scriptClient{replies: append(replies, text("done"))}
	fc := &fakeCompressor{limit: 0, err: errors.New("no")}
	a := New(Options{Client: c, Tools: fakeTools{}, Compressor: fc})
	if res, err := a.Run(context.Background(), "hi"); err != nil || res.Text != "done" {
		t.Fatalf("%+v %v", res, err)
	}
	if fc.calls != MaxCompressions {
		t.Fatalf("compress attempts %d", fc.calls)
	}
}
