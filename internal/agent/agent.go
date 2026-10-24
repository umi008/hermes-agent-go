// Package agent runs the Hermes turn loop: call the model, run the tools it
// asks for, feed results back, and stop when it answers without tools. The
// conversation history is append-only so the provider prefix cache holds.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// DefaultMaxAttempts matches upstream agent.api_max_retries (3 attempts).
const DefaultMaxAttempts = 3

// ErrMaxTurns reports that the iteration budget ran out before a final answer.
var ErrMaxTurns = errors.New("agent: iteration budget exhausted")

// ToolExecutor runs model tool calls. A returned error becomes an error
// tool result the model can read; it never aborts the turn.
type ToolExecutor interface {
	Specs() []llm.ToolSpec
	Execute(ctx context.Context, call llm.ToolCall) (string, error)
}

// EventKind classifies loop events.
type EventKind int

// Loop event kinds.
const (
	EventText EventKind = iota + 1
	EventReasoning
	EventToolStart
	EventToolResult
	EventRetry
	EventSteer
	// EventUsage reports the token usage of one model call.
	EventUsage
)

// Event is a typed notification from the loop, delivered in order on the
// goroutine that called Run.
type Event struct {
	Kind     EventKind
	Text     string
	ToolCall *llm.ToolCall
	IsError  bool
	Attempt  int
	Err      error
	Usage    llm.Usage
}

// Options configures an Agent.
type Options struct {
	Client    llm.Client
	Tools     ToolExecutor
	Model     string
	System    []string
	MaxTokens int
	CacheTTL  string
	// MaxTurns caps model calls per Run; zero means unlimited, as upstream.
	MaxTurns int
	// MaxAttempts per model call; zero means DefaultMaxAttempts.
	MaxAttempts int
	// Backoff returns the wait before retry attempt n (1-based); nil uses
	// upstream jittered exponential backoff.
	Backoff func(attempt int) time.Duration
	// Steer delivers user messages injected between iterations.
	Steer   <-chan string
	OnEvent func(Event)
	// OnMessage sees every message as it joins the history, for persistence.
	OnMessage func(llm.Message)
	Logger    *slog.Logger
}

// Result summarizes one Run.
type Result struct {
	Text       string
	Turns      int
	StopReason string
	Usage      llm.Usage
}

// Agent holds one conversation. It is not safe for concurrent Runs.
type Agent struct {
	opts    Options
	history []llm.Message
}

// New builds an agent.
func New(o Options) *Agent {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	if o.Backoff == nil {
		o.Backoff = JitteredBackoff
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Agent{opts: o}
}

// History returns a copy of the conversation so far.
func (a *Agent) History() []llm.Message {
	return append([]llm.Message(nil), a.history...)
}

func (a *Agent) record(msgs ...llm.Message) {
	a.history = append(a.history, msgs...)
	if a.opts.OnMessage != nil {
		for _, m := range msgs {
			a.opts.OnMessage(m)
		}
	}
}

func (a *Agent) emit(e Event) {
	if a.opts.OnEvent != nil {
		a.opts.OnEvent(e)
	}
}

// Run sends one user message and loops until the model answers without
// tool calls, the budget runs out, or ctx is done.
func (a *Agent) Run(ctx context.Context, input string) (Result, error) {
	a.record(llm.Message{Role: llm.RoleUser, Content: input})
	var res Result
	var specs []llm.ToolSpec
	if a.opts.Tools != nil {
		specs = a.opts.Tools.Specs()
	}
	for {
		if a.opts.MaxTurns > 0 && res.Turns >= a.opts.MaxTurns {
			return res, ErrMaxTurns
		}
		res.Turns++
		resp, err := a.call(ctx, specs)
		if err != nil {
			return res, err
		}
		res.Usage.Add(resp.Usage)
		a.emit(Event{Kind: EventUsage, Usage: resp.Usage})
		res.StopReason = resp.StopReason
		res.Text = resp.Message.Content
		a.record(resp.Message)

		calls := resp.Message.ToolCalls
		if len(calls) > 0 {
			a.record(a.runTools(ctx, calls)...)
		}
		steered := a.drainSteer()
		if len(calls) == 0 && !steered {
			return res, nil
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
}

func (a *Agent) drainSteer() bool {
	steered := false
	for a.opts.Steer != nil {
		select {
		case msg, ok := <-a.opts.Steer:
			if !ok {
				return steered
			}
			a.record(llm.Message{Role: llm.RoleUser, Content: msg})
			a.emit(Event{Kind: EventSteer, Text: msg})
			steered = true
		default:
			return steered
		}
	}
	return steered
}

func (a *Agent) call(ctx context.Context, specs []llm.ToolSpec) (llm.Response, error) {
	req := llm.Request{
		Model:     a.opts.Model,
		System:    a.opts.System,
		Messages:  a.history,
		Tools:     specs,
		MaxTokens: a.opts.MaxTokens,
		CacheTTL:  a.opts.CacheTTL,
	}
	onEvent := func(e llm.Event) {
		switch e.Kind {
		case llm.EventText:
			a.emit(Event{Kind: EventText, Text: e.Text})
		case llm.EventReasoning:
			a.emit(Event{Kind: EventReasoning, Text: e.Text})
		}
	}
	for attempt := 1; ; attempt++ {
		resp, err := a.opts.Client.Stream(ctx, req, onEvent)
		if err == nil {
			return resp, nil
		}
		if attempt >= a.opts.MaxAttempts || !llm.IsRetryable(err) {
			return llm.Response{}, fmt.Errorf("agent: model call: %w", err)
		}
		wait := a.opts.Backoff(attempt)
		a.opts.Logger.Warn("retrying model call", "attempt", attempt, "wait", wait, "err", err)
		a.emit(Event{Kind: EventRetry, Attempt: attempt, Err: err})
		if err := sleep(ctx, wait); err != nil {
			return llm.Response{}, err
		}
	}
}

// runTools executes calls concurrently and returns results in call order.
func (a *Agent) runTools(ctx context.Context, calls []llm.ToolCall) []llm.Message {
	for i := range calls {
		a.emit(Event{Kind: EventToolStart, ToolCall: &calls[i]})
	}
	out := make([]llm.Message, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = a.execute(ctx, call)
		}()
	}
	wg.Wait()
	for i := range out {
		a.emit(Event{Kind: EventToolResult, ToolCall: &calls[i], Text: out[i].Content, IsError: out[i].IsError})
	}
	return out
}

func (a *Agent) execute(ctx context.Context, call llm.ToolCall) llm.Message {
	msg := llm.Message{Role: llm.RoleTool, ToolCallID: call.ID}
	if a.opts.Tools == nil {
		msg.Content, msg.IsError = fmt.Sprintf("unknown tool %q", call.Name), true
		return msg
	}
	content, err := a.opts.Tools.Execute(ctx, call)
	if err != nil {
		msg.Content, msg.IsError = err.Error(), true
		return msg
	}
	msg.Content = content
	return msg
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// JitteredBackoff is upstream jittered_backoff: min(5s * 2^(n-1), 120s) plus
// up to 50% random jitter.
func JitteredBackoff(attempt int) time.Duration {
	const base, maxDelay = 5 * time.Second, 120 * time.Second
	delay := maxDelay
	if exp := attempt - 1; exp < 6 {
		delay = min(base<<max(exp, 0), maxDelay)
	}
	return delay + time.Duration(rand.Float64()*0.5*float64(delay))
}
