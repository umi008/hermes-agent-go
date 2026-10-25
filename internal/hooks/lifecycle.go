package hooks

import (
	"context"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/llm"
)

// Lifecycle events the port fires besides the tool events.
const (
	PreLLMCall     = "pre_llm_call"
	PostLLMCall    = "post_llm_call"
	OnSessionStart = "on_session_start"
	OnSessionEnd   = "on_session_end"
	SubagentStart  = "subagent_start"
	SubagentStop   = "subagent_stop"
)

// Lifecycle fires the non-tool events of one session. Every hook of an
// event runs in config order; only pre_llm_call reads an answer.
type Lifecycle struct {
	o Options
	// Platform is the upstream platform kwarg ("cli", "tui", "api", ...).
	Platform string
}

// NewLifecycle returns nil when no spec listens to a lifecycle event.
func NewLifecycle(o Options, platform string) *Lifecycle {
	l := &Lifecycle{o: o, Platform: platform}
	for _, event := range []string{PreLLMCall, PostLLMCall, OnSessionStart, OnSessionEnd, SubagentStart, SubagentStop} {
		if l.has(event) {
			if l.o.Runner == nil {
				l.o.Runner = &Runner{}
			}
			if l.o.SessionID == nil {
				l.o.SessionID = func() string { return "" }
			}
			if l.o.Profile == "" {
				l.o.Profile = "default"
			}
			return l
		}
	}
	return nil
}

func (l *Lifecycle) has(event string) bool {
	if l == nil {
		return false
	}
	for _, s := range l.o.Specs {
		if s.Event == event {
			return true
		}
	}
	return false
}

// Turns is the lifecycle as agent turn hooks, nil when no turn event has
// a hook.
func (l *Lifecycle) Turns() agent.TurnHooks {
	if !l.has(PreLLMCall) && !l.has(PostLLMCall) && !l.has(OnSessionStart) && !l.has(OnSessionEnd) {
		return nil
	}
	return l
}

func (l *Lifecycle) fire(ctx context.Context, event, sessionID string, extra object) []*Directive {
	var out []*Directive
	for _, s := range l.o.Specs {
		if s.Event != event {
			continue
		}
		data := payload(event, nil, nil, sessionID, l.o.Cwd, l.o.Profile, extra)
		if d := l.o.Runner.evaluate(s, l.o.Runner.spawn(ctx, s, data)); d != nil {
			out = append(out, d)
		}
	}
	return out
}

// SessionStart fires on_session_start.
func (l *Lifecycle) SessionStart(ctx context.Context, model string) {
	if l.has(OnSessionStart) {
		l.fire(ctx, OnSessionStart, l.o.SessionID(), object{{"model", model}, {"platform", l.Platform}})
	}
}

// PreLLMCall fires pre_llm_call and joins the hooks' context, as upstream
// _collect_pre_llm_call_context.
func (l *Lifecycle) PreLLMCall(ctx context.Context, t agent.TurnInfo) string {
	if !l.has(PreLLMCall) {
		return ""
	}
	extra := object{
		{"task_id", ""}, {"turn_id", ""}, {"user_message", t.UserMessage},
		{"conversation_history", history(t.History)}, {"is_first_turn", t.FirstTurn},
		{"model", t.Model}, {"platform", l.Platform}, {"sender_id", ""},
	}
	var parts []string
	for _, d := range l.fire(ctx, PreLLMCall, l.o.SessionID(), extra) {
		if d.Context != "" {
			parts = append(parts, d.Context)
		}
	}
	return strings.Join(parts, "\n\n")
}

// PostLLMCall fires post_llm_call.
func (l *Lifecycle) PostLLMCall(ctx context.Context, t agent.TurnInfo, response string) {
	if !l.has(PostLLMCall) {
		return
	}
	l.fire(ctx, PostLLMCall, l.o.SessionID(), object{
		{"task_id", ""}, {"turn_id", ""}, {"user_message", t.UserMessage}, {"assistant_response", response},
		{"conversation_history", history(t.History)}, {"model", t.Model}, {"platform", l.Platform},
	})
}

// TurnEnd fires upstream's per-turn on_session_end.
func (l *Lifecycle) TurnEnd(ctx context.Context, t agent.TurnInfo, end agent.TurnEnd) {
	if !l.has(OnSessionEnd) {
		return
	}
	l.fire(ctx, OnSessionEnd, l.o.SessionID(), object{
		{"task_id", ""}, {"turn_id", ""}, {"completed", end.Completed}, {"failed", end.Failed},
		{"interrupted", end.Interrupted}, {"turn_exit_reason", end.Reason}, {"model", t.Model}, {"platform", l.Platform},
	})
}

// Subagent describes a delegate_task child for the subagent events.
type Subagent struct {
	ParentSessionID string
	SessionID       string
	// ID is upstream's child_subagent_id, "sa-<index>-<hex>".
	ID   string
	Goal string
}

// SubagentStart fires subagent_start. Upstream passes no session_id, so
// the payload's session_id is the parent's.
func (l *Lifecycle) SubagentStart(ctx context.Context, c Subagent) {
	if !l.has(SubagentStart) {
		return
	}
	l.fire(ctx, SubagentStart, c.ParentSessionID, object{
		{"parent_turn_id", ""}, {"parent_subagent_id", nil}, {"child_session_id", nullable(c.SessionID)},
		{"child_subagent_id", c.ID}, {"child_role", "leaf"}, {"child_goal", c.Goal},
	})
}

// SubagentStop fires subagent_stop with the child's result entry fields.
func (l *Lifecycle) SubagentStop(ctx context.Context, c Subagent, summary, status any, toolHistory any, durationMS int64) {
	if !l.has(SubagentStop) {
		return
	}
	l.fire(ctx, SubagentStop, c.ParentSessionID, object{
		{"parent_turn_id", ""}, {"child_session_id", nullable(c.SessionID)}, {"child_role", "leaf"},
		{"child_summary", summary}, {"child_status", status}, {"tool_call_history", toolHistory},
		{"duration_ms", durationMS},
	})
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// history renders messages in the OpenAI chat shape upstream's
// conversation_history carries.
func history(msgs []llm.Message) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		o := object{{"role", string(m.Role)}, {"content", m.Content}}
		if len(m.ToolCalls) > 0 {
			calls := make([]any, len(m.ToolCalls))
			for i, c := range m.ToolCalls {
				calls[i] = object{{"id", c.ID}, {"type", "function"},
					{"function", object{{"name", c.Name}, {"arguments", string(c.Arguments)}}}}
			}
			o = append(o, field{"tool_calls", calls})
		}
		if m.ToolCallID != "" {
			o = append(o, field{"tool_call_id", m.ToolCallID})
		}
		out = append(out, o)
	}
	return out
}
