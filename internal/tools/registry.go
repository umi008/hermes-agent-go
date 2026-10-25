// Package tools holds the model tool registry and its result conventions,
// ported from upstream tools/registry.py: results are JSON strings, errors
// are {"error": "..."} objects the model can read.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// DefaultMaxResultChars is upstream's per-result cap (tools/budget_config.py).
const DefaultMaxResultChars = 100_000

// Unlimited disables the result cap for a tool that enforces its own budget.
const Unlimited = -1

// maxErrorChars caps error text like upstream _MAX_ERROR_CHARS.
const maxErrorChars = 2048

// Handler runs a tool. A returned error becomes {"error": err.Error()}.
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

// Tool is one model-callable tool.
type Tool struct {
	Name        string
	Toolset     string
	Description string
	// Parameters is a JSON Schema object.
	Parameters json.RawMessage
	Handler    Handler
	// Available reports whether the tool can run (keys present, binary on
	// PATH). Nil means always available. It is checked once per Specs call.
	Available func() bool
	// MaxResultChars caps the result; zero means DefaultMaxResultChars.
	MaxResultChars int
}

// Registry implements agent.ToolExecutor over a set of tools.
type Registry struct {
	tools map[string]Tool
}

// Inventory groups the tools that can run by toolset, with names sorted, so a
// client can show what the agent actually offers. A tool whose Available
// reports false is left out, as Specs leaves it out.
func (r *Registry) Inventory() map[string][]string {
	if r == nil {
		return nil
	}
	out := map[string][]string{}
	for name, t := range r.tools {
		if t.Available != nil && !t.Available() {
			continue
		}
		out[t.Toolset] = append(out[t.Toolset], name)
	}
	for _, names := range out {
		sort.Strings(names)
	}
	return out
}

// NewRegistry registers tools, rejecting duplicates and missing fields.
func NewRegistry(tools ...Tool) (*Registry, error) {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range tools {
		switch {
		case t.Name == "" || t.Handler == nil:
			return nil, fmt.Errorf("tools: tool %q needs a name and a handler", t.Name)
		case r.tools[t.Name].Name != "":
			return nil, fmt.Errorf("tools: duplicate tool %q", t.Name)
		}
		r.tools[t.Name] = t
	}
	return r, nil
}

// Select returns a registry restricted to the given toolsets. An empty list
// keeps every tool.
func (r *Registry) Select(toolsets []string) *Registry {
	if len(toolsets) == 0 {
		return r
	}
	want := map[string]bool{}
	for _, ts := range toolsets {
		want[ts] = true
	}
	out := &Registry{tools: map[string]Tool{}}
	for name, t := range r.tools {
		if want[t.Toolset] || want[name] {
			out.tools[name] = t
		}
	}
	return out
}

// Toolsets lists the toolset names present, sorted.
func (r *Registry) Toolsets() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range r.tools {
		if !seen[t.Toolset] {
			seen[t.Toolset] = true
			out = append(out, t.Toolset)
		}
	}
	sort.Strings(out)
	return out
}

// Specs returns the available tools sorted by name, so the tool block of
// the prompt is byte-stable across turns.
func (r *Registry) Specs() []llm.ToolSpec {
	names := make([]string, 0, len(r.tools))
	for name, t := range r.tools {
		if t.Available == nil || t.Available() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	specs := make([]llm.ToolSpec, 0, len(names))
	for _, name := range names {
		t := r.tools[name]
		specs = append(specs, llm.ToolSpec{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return specs
}

// Execute runs one call. Failures come back as an error whose text is the
// JSON error object, so the loop forwards it to the model as an error result.
func (r *Registry) Execute(ctx context.Context, call llm.ToolCall) (out string, err error) {
	t, ok := r.tools[call.Name]
	if !ok {
		return "", Error("Unknown tool: " + call.Name)
	}
	defer func() {
		if p := recover(); p != nil {
			out, err = "", Error(fmt.Sprintf("Tool execution failed: panic: %v", p))
		}
	}()
	args := call.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	out, err = t.Handler(ctx, args)
	if err != nil {
		var te *ToolError
		if errors.As(err, &te) {
			return "", te
		}
		return "", Error(err.Error())
	}
	return capResult(out, t.MaxResultChars), nil
}

// ToolError is a model-facing error. Its text is the JSON error object.
type ToolError struct{ json string }

func (e *ToolError) Error() string { return e.json }

// Error builds a ToolError with upstream's {"error": msg} shape.
func Error(msg string) error {
	return ErrorWith(msg, nil)
}

// ErrorWith adds extra fields next to "error".
func ErrorWith(msg string, extra map[string]any) error {
	obj := map[string]any{}
	for k, v := range extra {
		obj[k] = v
	}
	obj["error"] = truncateRunes(msg, maxErrorChars, "… [truncated]")
	data, _ := json.Marshal(obj)
	return &ToolError{json: string(data)}
}

// Result marshals v as the tool result.
func Result(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode result: %w", err)
	}
	return string(data), nil
}

func capResult(out string, limit int) string {
	if limit == Unlimited {
		return out
	}
	if limit == 0 {
		limit = DefaultMaxResultChars
	}
	n := utf8.RuneCountInString(out)
	if n <= limit {
		return out
	}
	return truncateRunes(out, limit, fmt.Sprintf("\n\n... [truncated: result was %d chars, limit %d]", n, limit))
}

func truncateRunes(s string, limit int, marker string) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	return string(r[:limit]) + marker
}

// Decode unmarshals args into v, wrapping failures as a ToolError.
func Decode(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return Error("Invalid arguments: " + err.Error())
	}
	return nil
}

// Without returns a registry minus the named tools.
func (r *Registry) Without(names ...string) *Registry {
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
	}
	out := &Registry{tools: map[string]Tool{}}
	for name, t := range r.tools {
		if !drop[name] {
			out.tools[name] = t
		}
	}
	return out
}

// Has reports whether the registry holds the named tool.
func (r *Registry) Has(name string) bool { return r.tools[name].Name != "" }

// With returns a registry holding r's tools plus extra; a name clash is an
// error, as in NewRegistry.
func (r *Registry) With(extra ...Tool) (*Registry, error) {
	all := make([]Tool, 0, len(r.tools)+len(extra))
	for _, t := range r.tools {
		all = append(all, t)
	}
	return NewRegistry(append(all, extra...)...)
}
