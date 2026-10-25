package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

type hookCall struct {
	event    string
	turn     TurnInfo
	response string
	end      TurnEnd
}

type recordingHooks struct {
	calls   []hookCall
	context string
}

func (h *recordingHooks) SessionStart(_ context.Context, model string) {
	h.calls = append(h.calls, hookCall{event: "start", turn: TurnInfo{Model: model}})
}

func (h *recordingHooks) PreLLMCall(_ context.Context, t TurnInfo) string {
	h.calls = append(h.calls, hookCall{event: "pre", turn: t})
	return h.context
}

func (h *recordingHooks) PostLLMCall(_ context.Context, t TurnInfo, response string) {
	h.calls = append(h.calls, hookCall{event: "post", turn: t, response: response})
}

func (h *recordingHooks) TurnEnd(_ context.Context, t TurnInfo, end TurnEnd) {
	h.calls = append(h.calls, hookCall{event: "end", turn: t, end: end})
}

func (h *recordingHooks) events() string {
	var out []string
	for _, c := range h.calls {
		out = append(out, c.event)
	}
	return strings.Join(out, ",")
}

func TestTurnHooksOrderAndContextInjection(t *testing.T) {
	client := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){text("one"), text("two")}}
	h := &recordingHooks{context: "CTX"}
	var persisted []llm.Message
	a := New(Options{Client: client, Model: "m1", Hooks: h, Backoff: noWait,
		OnMessage: func(m llm.Message) { persisted = append(persisted, m) }})

	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got := h.events(); got != "start,pre,post,end" {
		t.Fatalf("first turn events = %q", got)
	}
	pre := h.calls[1].turn
	if pre.UserMessage != "hi" || !pre.FirstTurn || pre.Model != "m1" || len(pre.History) != 1 || pre.History[0].Content != "hi" {
		t.Fatalf("pre_llm_call turn = %+v", pre)
	}
	if got := client.requests[0].Messages[0].Content; got != "hi\n\nCTX" {
		t.Fatalf("sent user content = %q, want the hook context appended", got)
	}
	if persisted[0].Content != "hi" {
		t.Fatalf("persisted user content = %q, want the original text", persisted[0].Content)
	}
	post := h.calls[2]
	if post.response != "one" || len(post.turn.History) != 2 {
		t.Fatalf("post_llm_call = %+v", post)
	}
	end := h.calls[3].end
	if end != (TurnEnd{Completed: true, Reason: "text_response(finish_reason=stop)"}) {
		t.Fatalf("turn end = %+v", end)
	}

	h.calls = nil
	if _, err := a.Run(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if got := h.events(); got != "pre,post,end" {
		t.Fatalf("second turn events = %q, want no session start", got)
	}
	if h.calls[0].turn.FirstTurn {
		t.Fatal("second turn reported as first")
	}
	if got := client.requests[1].Messages[0].Content; got != "hi\n\nCTX" {
		t.Fatalf("replayed first user message = %q, want the bytes sent before", got)
	}
}

func TestTurnHooksEmptyContextLeavesMessage(t *testing.T) {
	client := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){text("ok")}}
	a := New(Options{Client: client, Hooks: &recordingHooks{}, Backoff: noWait})
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got := client.requests[0].Messages[0].Content; got != "hi" {
		t.Fatalf("sent = %q", got)
	}
}

func TestTurnEndReasons(t *testing.T) {
	cases := []struct {
		name  string
		reply func(llm.Request, func(llm.Event)) (llm.Response, error)
		opts  func(*Options)
		ctx   func() context.Context
		want  TurnEnd
	}{
		{
			name:  "max turns",
			reply: toolCalls(llm.ToolCall{ID: "1", Name: "echo"}),
			opts:  func(o *Options) { o.MaxTurns = 1; o.Tools = fakeTools{} },
			want:  TurnEnd{Reason: "max_iterations_reached(1/1)"},
		},
		{
			name:  "failure",
			reply: fail(errors.New("bad request")),
			opts:  func(o *Options) { o.MaxAttempts = 1 },
			want:  TurnEnd{Failed: true, Reason: "all_retries_exhausted_no_response"},
		},
		{
			name:  "interrupted",
			reply: fail(context.Canceled),
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			want: TurnEnd{Interrupted: true, Reason: "interrupted_by_user"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){tc.reply}}
			h := &recordingHooks{}
			o := Options{Client: client, Hooks: h, Backoff: noWait}
			if tc.opts != nil {
				tc.opts(&o)
			}
			ctx := context.Background()
			if tc.ctx != nil {
				ctx = tc.ctx()
			}
			_, _ = New(o).Run(ctx, "hi")
			last := h.calls[len(h.calls)-1]
			if last.event != "end" || last.end != tc.want {
				t.Fatalf("last hook = %+v, want end %+v", last, tc.want)
			}
		})
	}
}

func TestRewindReturnsTypedTextAfterInjection(t *testing.T) {
	client := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){text("ok")}}
	a := New(Options{Client: client, Hooks: &recordingHooks{context: "CTX"}, Backoff: noWait})
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got, _, ok := a.Rewind(1); !ok || got != "hi" {
		t.Fatalf("rewind = %q %v, want the typed text", got, ok)
	}
}
