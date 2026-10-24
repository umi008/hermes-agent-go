package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/tools"
)

// Executor lists and runs tools; agent.ToolExecutor has the same shape.
type Executor interface {
	Specs() []llm.ToolSpec
	Execute(ctx context.Context, call llm.ToolCall) (string, error)
}

// Options configure Wrap.
type Options struct {
	Specs  []Spec
	Runner *Runner
	// SessionID, Cwd and Profile fill the payload.
	SessionID func() string
	Cwd       string
	Profile   string
	// Approve answers an "approve" directive. Nil blocks the call, as a
	// session with nobody to ask does.
	Approve func(tool, message string) bool
}

// Wrap fires pre_tool_call hooks before next runs a call and
// post_tool_call hooks after. Without hooks it returns next unchanged.
func Wrap(next Executor, o Options) Executor {
	if len(o.Specs) == 0 {
		return next
	}
	if o.Runner == nil {
		o.Runner = &Runner{}
	}
	if o.SessionID == nil {
		o.SessionID = func() string { return "" }
	}
	if o.Profile == "" {
		o.Profile = "default"
	}
	return &executor{next: next, o: o}
}

type executor struct {
	next Executor
	o    Options
}

// Specs lists the wrapped tools unchanged.
func (e *executor) Specs() []llm.ToolSpec { return e.next.Specs() }

func (e *executor) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	var args map[string]any
	if len(bytes.TrimSpace(call.Arguments)) > 0 {
		_ = json.Unmarshal(call.Arguments, &args)
	}
	ids := object{{"task_id", ""}, {"tool_call_id", call.ID}, {"turn_id", ""}, {"api_request_id", ""}}
	block, modified := e.pre(ctx, call.Name, args, ids)
	if block != "" {
		return "", tools.Error(block)
	}
	if modified != nil {
		data, err := json.Marshal(modified)
		if err != nil {
			return "", fmt.Errorf("encode hook-modified arguments: %w", err)
		}
		call.Arguments, args = data, modified
	}
	start := time.Now()
	out, err := e.next.Execute(ctx, call)
	result := out
	if err != nil {
		result = err.Error()
	}
	e.post(ctx, call.Name, args, ids, result, time.Since(start))
	return out, err
}

// pre is upstream _get_pre_tool_call_directive_details plus
// _resolve_block_from_details: every hook runs, modify directives merge
// in order, and block wins over approve.
func (e *executor) pre(ctx context.Context, tool string, args map[string]any, ids object) (string, map[string]any) {
	extra := append(append(object{}, ids...), field{"middleware_trace", []any{}})
	var (
		modified map[string]any
		block    string
		approve  *Directive
	)
	for _, s := range e.o.Specs {
		if s.Event != PreToolCall || !s.matches(tool) {
			continue
		}
		d := e.o.Runner.evaluate(s, e.o.Runner.spawn(ctx, s, e.payload(PreToolCall, tool, args, extra)))
		if d == nil {
			continue
		}
		switch d.Action {
		case "modify":
			if len(d.Args) == 0 {
				continue
			}
			base := modified
			if base == nil {
				base = args
			}
			merged := make(map[string]any, len(base)+len(d.Args))
			for k, v := range base {
				merged[k] = v
			}
			for k, v := range d.Args {
				merged[k] = v
			}
			modified = merged
		case "block":
			if block == "" && d.Message != "" {
				block = d.Message
			}
		case "approve":
			if approve == nil {
				approve = d
			}
		}
	}
	if block != "" || approve == nil {
		return block, modified
	}
	if e.o.Approve != nil && e.o.Approve(tool, approve.Message) {
		return "", modified
	}
	return "BLOCKED: plugin approval required for " + tool, modified
}

func (e *executor) post(ctx context.Context, tool string, args map[string]any, ids object, result string, took time.Duration) {
	status, errType, errMsg := observerFields(result)
	extra := object{{"result", result}}
	extra = append(extra, ids...)
	extra = append(extra, field{"duration_ms", took.Milliseconds()}, field{"status", status},
		field{"error_type", errType}, field{"error_message", errMsg}, field{"middleware_trace", []any{}})
	for _, s := range e.o.Specs {
		if s.Event != PostToolCall || !s.matches(tool) {
			continue
		}
		e.o.Runner.evaluate(s, e.o.Runner.spawn(ctx, s, e.payload(PostToolCall, tool, args, extra)))
	}
}

// observerFields is upstream _tool_result_observer_fields without the
// display heuristics: a JSON result with a truthy "error" is an error.
func observerFields(result string) (string, any, any) {
	var m map[string]any
	if json.Unmarshal([]byte(result), &m) == nil && truthy(m["error"]) {
		msg, ok := m["error"].(string)
		if !ok {
			data, _ := json.Marshal(m["error"])
			msg = string(data)
		}
		return "error", "tool_error", msg
	}
	return "ok", nil, nil
}

// payload is upstream _serialize_payload, keys in wire order.
func (e *executor) payload(event, tool string, args map[string]any, extra object) []byte {
	var input any
	if args != nil {
		input = args
	}
	data, _ := object{
		{"hook_event_name", event}, {"tool_name", tool}, {"tool_input", input},
		{"session_id", e.o.SessionID()}, {"cwd", e.o.Cwd}, {"profile", e.o.Profile}, {"extra", extra},
	}.MarshalJSON()
	return data
}

type field struct {
	k string
	v any
}

// object is a JSON object that keeps its key order.
type object []field

func (o object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.k)
		b.Write(k)
		b.WriteByte(':')
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(f.v); err != nil {
			return nil, err
		}
		b.Truncate(b.Len() - 1)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
