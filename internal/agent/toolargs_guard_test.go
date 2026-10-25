package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

type guardTools struct {
	mu sync.Mutex
	cs []llm.ToolCall
}

func (g *guardTools) Specs() []llm.ToolSpec {
	return []llm.ToolSpec{{Name: "terminal", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)}}
}

func (g *guardTools) Execute(_ context.Context, c llm.ToolCall) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cs = append(g.cs, c)
	return "ran", nil
}

func (g *guardTools) calls() []llm.ToolCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]llm.ToolCall(nil), g.cs...)
}

func guardCall(id, args, stop, content string) reply {
	return func(_ llm.Request, _ func(llm.Event)) (llm.Response, error) {
		msg := llm.Message{Role: llm.RoleAssistant, Content: content}
		msg.ToolCalls = []llm.ToolCall{{ID: id, Name: "terminal", Arguments: json.RawMessage(args)}}
		return llm.Response{Message: msg, StopReason: stop}, nil
	}
}

// unpairedToolCalls lists ids whose assistant call no tool row answers.
func unpairedToolCalls(msgs []llm.Message) []string {
	var bad []string
	for i, m := range msgs {
		for _, tc := range m.ToolCalls {
			ok := false
			for _, r := range msgs[i+1:] {
				if r.Role == llm.RoleTool && r.ToolCallID == tc.ID {
					ok = true
					break
				}
			}
			if !ok {
				bad = append(bad, tc.ID)
			}
		}
	}
	return bad
}

// S1: a refused call is answered by an error tool row for its own ID.
func TestGuard_RefusalAnswersTheCallID(t *testing.T) {
	c := &scriptClient{replies: []reply{guardCall("c1", `{"command":`, llm.StopToolUse, "x"), text("ok")}}
	tools := &guardTools{}
	a := New(Options{Client: c, Tools: tools, Backoff: noWait})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if n := len(tools.calls()); n != 0 {
		t.Fatalf("executed %d calls with invalid arguments", n)
	}
	var answered bool
	for _, m := range c.requests[1].Messages {
		if m.Role == llm.RoleTool && m.ToolCallID == "c1" && m.IsError {
			answered = true
		}
	}
	if !answered {
		t.Fatalf("no error tool result for c1: %v", c.requests[1].Messages)
	}
	if bad := unpairedToolCalls(c.requests[1].Messages); len(bad) > 0 {
		t.Fatalf("unpaired tool calls %v", bad)
	}
}

// S2: re-sending an unparseable call must not loop forever.
func TestGuard_InvalidArgumentRetriesAreBounded(t *testing.T) {
	reps := make([]reply, 0, 9)
	for i := 0; i < 8; i++ {
		reps = append(reps, guardCall("c1", `{"command":`, llm.StopToolUse, "x"))
	}
	reps = append(reps, text("ok"))
	c := &scriptClient{replies: reps}
	a := New(Options{Client: c, Tools: &guardTools{}, Backoff: noWait})
	_, _ = a.Run(context.Background(), "go")
	if len(c.requests) > MaxLengthContinuations+2 {
		t.Fatalf("requests = %d, unbounded retry of a refused call", len(c.requests))
	}
}

// S3: a cut carrying no text still consumes a continuation and returns.
func TestGuard_TruncatedCallsBoundWithEmptyContent(t *testing.T) {
	reps := make([]reply, 0, 13)
	for i := 0; i < 12; i++ {
		reps = append(reps, guardCall("c1", `{"command":`, llm.StopMaxTokens, ""))
	}
	reps = append(reps, text("done"))
	c := &scriptClient{replies: reps}
	a := New(Options{Client: c, Tools: &guardTools{}, Backoff: noWait})
	res, _ := a.Run(context.Background(), "go")
	if len(c.requests) > MaxLengthContinuations+2 {
		t.Fatalf("requests = %d: the cut branch spun without consuming a continuation", len(c.requests))
	}
	if res.StopReason != llm.StopMaxTokens {
		t.Errorf("stop = %q, want max_tokens", res.StopReason)
	}
}

// S4: a truncated call never executes and the history stays pairable.
func TestGuard_TruncatedCallPairingAndNoExecution(t *testing.T) {
	c := &scriptClient{replies: []reply{guardCall("c1", `{"command":"rm -rf /tmp/x`, llm.StopMaxTokens, "w"), text("done")}}
	tools := &guardTools{}
	a := New(Options{Client: c, Tools: tools, Backoff: noWait})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := tools.calls(); len(got) != 0 {
		t.Fatalf("a truncated call reached the tool: %v", got)
	}
	if len(c.requests) < 2 {
		t.Fatalf("no continuation request: %d", len(c.requests))
	}
	if bad := unpairedToolCalls(c.requests[1].Messages); len(bad) > 0 {
		t.Fatalf("unpaired tool calls %v", bad)
	}
}

// S5: upstream's dropped-tools nudge, byte for byte.
func TestGuard_DroppedToolsNudgeText(t *testing.T) {
	want := "[System: Your previous tool call (write_file) was too large and the stream timed out before it " +
		"could be delivered. Do NOT retry the same tool call with the same large content. Instead, break the content " +
		"into multiple smaller tool calls (e.g. use multiple patch calls or write smaller files). Each tool call's " +
		"arguments must be under ~8K tokens to avoid stream timeouts. The cut was a transport interruption, not a " +
		"capability change — your tools remain fully available.]"
	if got := droppedToolsNudge([]llm.ToolCall{{Name: "write_file"}}); got != want {
		t.Fatalf("nudge = %q", got)
	}
}

// S6: valid arguments keep executing, in order, with no refusal rows.
func TestGuard_ValidArgumentsStillExecute(t *testing.T) {
	two := func(_ llm.Request, _ func(llm.Event)) (llm.Response, error) {
		msg := llm.Message{Role: llm.RoleAssistant}
		msg.ToolCalls = []llm.ToolCall{
			{ID: "a", Name: "terminal", Arguments: json.RawMessage(`{"command":"ls"}`)},
			{ID: "b", Name: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)},
		}
		return llm.Response{Message: msg, StopReason: llm.StopToolUse}, nil
	}
	c := &scriptClient{replies: []reply{two, text("done")}}
	tools := &guardTools{}
	a := New(Options{Client: c, Tools: tools, Backoff: noWait})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	// Calls run concurrently, so only the set is a contract here: the results
	// come back in call order, which TestParallelToolsKeepOrderAndErrors pins.
	got := tools.calls()
	ran := map[string]bool{}
	for _, call := range got {
		ran[call.ID] = true
	}
	if len(got) != 2 || !ran["a"] || !ran["b"] {
		t.Fatalf("executed = %v", got)
	}
	for _, m := range c.requests[1].Messages {
		if m.Role == llm.RoleTool && strings.Contains(m.Content, "not valid JSON") {
			t.Fatalf("valid calls were refused: %v", m)
		}
	}
}

// TestGuard_RefusalCeilingIsExact pins the ceiling: an unusable call stops at
// exactly MaxLengthContinuations+1 requests, and every request stays pairable.
func TestGuard_RefusalCeilingIsExact(t *testing.T) {
	var reps []reply
	for i := 0; i < 12; i++ {
		reps = append(reps, guardCall("c1", `{"command":`, llm.StopToolUse, "trying"))
	}
	c := &scriptClient{replies: reps}
	tools := &guardTools{}
	a := New(Options{Client: c, Tools: tools, Backoff: noWait})
	if _, err := a.Run(context.Background(), "go"); !errors.Is(err, ErrUnusableToolCalls) {
		t.Fatalf("err = %v, want the turn to fail rather than report an answer", err)
	}
	if got := len(c.requests); got != MaxLengthContinuations+1 {
		t.Fatalf("requests = %d, want %d", got, MaxLengthContinuations+1)
	}
	if n := len(tools.calls()); n != 0 {
		t.Fatalf("executed %d refused calls", n)
	}
	for i, req := range c.requests {
		if bad := unpairedToolCalls(req.Messages); len(bad) > 0 {
			t.Fatalf("request %d unpaired %v", i+1, bad)
		}
	}
}

// TestGuard_TruncationCeilingIsExact pins the same ceiling for a cut that
// carries no text.
func TestGuard_TruncationCeilingIsExact(t *testing.T) {
	var reps []reply
	for i := 0; i < 12; i++ {
		reps = append(reps, guardCall("c1", `{"command":`, llm.StopMaxTokens, ""))
	}
	c := &scriptClient{replies: reps}
	a := New(Options{Client: c, Tools: &guardTools{}, Backoff: noWait})
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(c.requests); got != MaxLengthContinuations+1 {
		t.Fatalf("requests = %d, want %d", got, MaxLengthContinuations+1)
	}
	if res.StopReason != llm.StopMaxTokens {
		t.Errorf("stop = %q", res.StopReason)
	}
}

// TestGuard_MixedRoundPairsInCallOrder pins a response with one usable and one
// refused call: the usable one runs, and both get rows in call order.
func TestGuard_MixedRoundPairsInCallOrder(t *testing.T) {
	mixed := func(_ llm.Request, _ func(llm.Event)) (llm.Response, error) {
		msg := llm.Message{Role: llm.RoleAssistant}
		msg.ToolCalls = []llm.ToolCall{
			{ID: "good", Name: "terminal", Arguments: json.RawMessage(`{"command":"ls"}`)},
			{ID: "bad", Name: "terminal", Arguments: json.RawMessage(`{"command":`)},
		}
		return llm.Response{Message: msg, StopReason: llm.StopToolUse}, nil
	}
	c := &scriptClient{replies: []reply{mixed, text("ok")}}
	tools := &guardTools{}
	a := New(Options{Client: c, Tools: tools, Backoff: noWait})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	ran := tools.calls()
	if len(ran) != 1 || ran[0].ID != "good" {
		t.Fatalf("executed = %v", ran)
	}
	var ids []string
	for _, m := range c.requests[1].Messages {
		if m.Role == llm.RoleTool {
			ids = append(ids, m.ToolCallID)
		}
	}
	if strings.Join(ids, ",") != "good,bad" {
		t.Fatalf("rows = %v", ids)
	}
	if bad := unpairedToolCalls(c.requests[1].Messages); len(bad) > 0 {
		t.Fatalf("unpaired %v", bad)
	}
}

// TestGuard_CeilingKeepsEveryFragment pins that the budget running out keeps
// the text that did arrive.
func TestGuard_CeilingKeepsEveryFragment(t *testing.T) {
	var reps []reply
	want := ""
	for i := 1; i <= 12; i++ {
		reps = append(reps, guardCall("c1", `{"command":"ls`, llm.StopMaxTokens, fmt.Sprintf("p%d", i)))
		if i <= MaxLengthContinuations+1 {
			want += fmt.Sprintf("p%d", i)
		}
	}
	c := &scriptClient{replies: reps}
	a := New(Options{Client: c, Tools: &guardTools{}, Backoff: noWait})
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
}

// TestGuard_CounterSurvivesProductiveRounds pins that a productive round does
// not reset the budget and re-arm unbounded retries.
func TestGuard_CounterSurvivesProductiveRounds(t *testing.T) {
	var reps []reply
	for i := 0; i < 30; i++ {
		if i%2 == 0 {
			reps = append(reps, guardCall(fmt.Sprintf("c%d", i), `{"command":`, llm.StopToolUse, ""))
			continue
		}
		reps = append(reps, toolCalls(llm.ToolCall{ID: fmt.Sprintf("ok%d", i), Name: "terminal",
			Arguments: json.RawMessage(`{"command":"ls"}`)}))
	}
	c := &scriptClient{replies: reps}
	a := New(Options{Client: c, Tools: &guardTools{}, Backoff: noWait})
	if _, err := a.Run(context.Background(), "go"); err != nil && !errors.Is(err, ErrUnusableToolCalls) {
		t.Fatal(err)
	}
	if got, want := len(c.requests), 2*MaxLengthContinuations+1; got != want {
		t.Fatalf("requests = %d, want %d", got, want)
	}
}

// TestGuard_MultiCutCallsGetOrderedRows pins several cut calls in one response:
// one row each, in call order, nudge last.
func TestGuard_MultiCutCallsGetOrderedRows(t *testing.T) {
	two := func(_ llm.Request, _ func(llm.Event)) (llm.Response, error) {
		msg := llm.Message{Role: llm.RoleAssistant}
		msg.ToolCalls = []llm.ToolCall{
			{ID: "x", Name: "terminal", Arguments: json.RawMessage(`{"command":"a`)},
			{ID: "y", Name: "terminal", Arguments: json.RawMessage(`{"command":"b`)},
		}
		return llm.Response{Message: msg, StopReason: llm.StopMaxTokens}, nil
	}
	c := &scriptClient{replies: []reply{two, text("ok")}}
	a := New(Options{Client: c, Tools: &guardTools{}, Backoff: noWait})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range c.requests[1].Messages {
		if m.Role == llm.RoleTool {
			ids = append(ids, m.ToolCallID)
		}
	}
	if strings.Join(ids, ",") != "x,y" {
		t.Fatalf("rows = %v", ids)
	}
	msgs := c.requests[1].Messages
	if n := msgs[len(msgs)-1]; n.Role != llm.RoleUser || !strings.HasPrefix(n.Content, droppedToolsNudgePrefix) {
		t.Fatalf("nudge not last: %+v", n)
	}
}

// TestGuard_SteerDrainedOnRefusedRound pins that a refused round still drains a
// pending steer, in order after the tool row it answers.
func TestGuard_SteerDrainedOnRefusedRound(t *testing.T) {
	steer := make(chan string, 1)
	first := func(_ llm.Request, _ func(llm.Event)) (llm.Response, error) {
		steer <- "also do y"
		msg := llm.Message{Role: llm.RoleAssistant}
		msg.ToolCalls = []llm.ToolCall{{ID: "c1", Name: "terminal", Arguments: json.RawMessage(`{"cmd":`)}}
		return llm.Response{Message: msg, StopReason: llm.StopToolUse}, nil
	}
	c := &scriptClient{replies: []reply{first, text("ok")}}
	a := New(Options{Client: c, Tools: &guardTools{}, Steer: steer, Backoff: noWait})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	msgs := c.requests[1].Messages
	toolAt, steerAt := -1, -1
	for i, m := range msgs {
		if m.Role == llm.RoleTool && m.ToolCallID == "c1" {
			toolAt = i
		}
		if m.Role == llm.RoleUser && m.Content == "also do y" {
			steerAt = i
		}
	}
	if toolAt < 0 || steerAt < toolAt {
		t.Fatalf("steer at %d, tool row at %d: %v", steerAt, toolAt, msgs)
	}
}
