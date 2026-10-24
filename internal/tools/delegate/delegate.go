// Package delegate ports upstream delegate_task (tools/delegate_tool*.py):
// the parent agent spawns isolated subagents, each with a fresh conversation
// and its own iteration budget, and gets back only their final summaries.
// Children run synchronously and in parallel, as upstream does for sessions
// without a later-result consumer; there is no background dispatch and no
// live list/steer/stop control.
package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/tools"
)

// Name and Toolset match upstream registration.
const (
	Name    = "delegate_task"
	Toolset = "delegation"
)

// Upstream defaults (hermes_cli/config_defaults.py, delegation.*).
const (
	DefaultMaxChildren     = 10
	DefaultMaxIterations   = 250
	DefaultMaxSummaryChars = 24000
)

// Blocked are the tools a child never gets (DELEGATE_BLOCKED_TOOLS); with
// upstream's default max_spawn_depth of 1 that includes delegate_task.
var Blocked = []string{Name, "clarify", "memory", "send_message", "cronjob_manage"}

// Task is one child's assignment.
type Task struct {
	Index   int
	Goal    string
	Context string
}

// Outcome is what a child run produced.
type Outcome struct {
	// Text is the child's final answer, or the last text it produced.
	Text     string
	Turns    int
	Usage    llm.Usage
	Model    string
	Messages []llm.Message
	// Err is nil on success; ErrBudget, a context error or a failure.
	Err error
}

// ErrBudget reports a child that ran out of iterations.
var ErrBudget = errors.New("subagent iteration budget exhausted")

// Runner runs one child to completion.
type Runner func(ctx context.Context, t Task) Outcome

// Options configures the tool.
type Options struct {
	Run             Runner
	MaxChildren     int
	MaxSummaryChars int
	Now             func() time.Time
}

// Tool builds delegate_task.
func Tool(o Options) tools.Tool {
	if o.MaxChildren <= 0 {
		o.MaxChildren = DefaultMaxChildren
	}
	if o.MaxSummaryChars == 0 {
		o.MaxSummaryChars = DefaultMaxSummaryChars
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return tools.Tool{
		Name:        Name,
		Toolset:     Toolset,
		Description: description,
		Parameters:  parameters(o.MaxChildren),
		Handler:     o.handle,
	}
}

// description is upstream _build_top_level_description for a leaf-only
// tree, minus the background-dispatch and live-control sentences this port
// does not implement.
const description = "Spawn isolated subagents for parallel or context-heavy work. Each gets its own conversation, terminal " +
	"session, and toolset, and only its final summary returns to you. Pass every task in `tasks` — one entry spawns one subagent, several run in parallel " +
	"(limit in the tasks description).\n\n" +
	"Children run in parallel and their results return in this tool call.\n\n" +
	"USE FOR: reasoning-heavy subtasks, work that would flood your context, or independent parallel workstreams.\n" +
	"DO NOT USE FOR (use these instead):\n" +
	"- A single tool call -> call the tool directly\n" +
	"- Tasks needing user interaction -> subagents cannot ask questions\n\n" +
	"RULES:\n" +
	"- Children know nothing of this conversation: pass everything needed via 'context', including any required " +
	"output language, tone, or style (e.g. \"respond in Chinese\").\n" +
	"- Child summaries are SELF-REPORTS, not verified facts: a child claiming \"uploaded successfully\" or " +
	"\"file written\" may be wrong. For external side effects (uploads, remote writes, publishing), require a " +
	"verifiable handle (URL, ID, absolute path) and verify it yourself before telling the user the operation " +
	"succeeded.\n" +
	"- Children cannot call delegate_task, clarify, memory, or cronjob.\n" +
	"- Children inherit the parent model unless pinned via delegation.provider / delegation.model in config.yaml."

func parameters(maxChildren int) json.RawMessage {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tasks": map[string]any{
				"type":     "array",
				"minItems": 1,
				"description": fmt.Sprintf("The task(s), up to %d in parallel for this user (set "+
					"via delegation.max_concurrent_children). Each entry spawns one "+
					"subagent with isolated context and terminal session; a single task "+
					"is a one-entry array. Required when spawning.", maxChildren),
				"items": map[string]any{
					"type":     "object",
					"required": []string{"goal"},
					"properties": map[string]any{
						"goal": map[string]any{
							"type":        "string",
							"description": "What this subagent should accomplish. Be specific and self-contained — it knows nothing about your conversation history.",
						},
						"context": map[string]any{
							"type":        "string",
							"description": "Background THIS child needs: file paths, error messages, constraints. Each child sees only its own context — repeat shared background in every task that needs it.",
						},
					},
				},
			},
		},
		"required": []string{},
	}
	data, _ := json.Marshal(schema)
	return data
}

type taskArg struct {
	Goal    string `json:"goal"`
	Context string `json:"context"`
}

type args struct {
	Tasks   []json.RawMessage `json:"tasks"`
	Goal    string            `json:"goal"`
	Context string            `json:"context"`
	Action  string            `json:"action"`
}

// parseTasks normalizes the call as upstream delegate_tool_tasks.py: the
// `tasks` array, or the legacy top-level goal/context as one task.
func (o Options) parseTasks(raw json.RawMessage) ([]Task, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, tools.Error("Invalid arguments: " + err.Error())
	}
	if a.Action != "" && a.Action != "spawn" {
		return nil, tools.Error(fmt.Sprintf("action %q is not supported: subagents run synchronously and return in this call", a.Action))
	}
	if len(a.Tasks) == 0 {
		if strings.TrimSpace(a.Goal) == "" {
			return nil, tools.Error("Provide either 'goal' (single task) or 'tasks' (batch).")
		}
		return []Task{{Goal: strings.TrimSpace(a.Goal), Context: a.Context}}, nil
	}
	if len(a.Tasks) > o.MaxChildren {
		return nil, tools.Error(fmt.Sprintf("Too many tasks: %d provided, but max_concurrent_children is %d. "+
			"Either reduce the task count, split into multiple delegate_task calls, or increase "+
			"delegation.max_concurrent_children in config.yaml.", len(a.Tasks), o.MaxChildren))
	}
	out := make([]Task, 0, len(a.Tasks))
	for i, rawTask := range a.Tasks {
		var t taskArg
		if err := json.Unmarshal(rawTask, &t); err != nil {
			return nil, tools.Error(fmt.Sprintf("Task %d must be an object with a 'goal'.", i))
		}
		if strings.TrimSpace(t.Goal) == "" {
			return nil, tools.Error(fmt.Sprintf("Task %d is missing a 'goal'.", i))
		}
		out = append(out, Task{Index: i, Goal: strings.TrimSpace(t.Goal), Context: t.Context})
	}
	return out, nil
}

func (o Options) handle(ctx context.Context, raw json.RawMessage) (string, error) {
	tasks, err := o.parseTasks(raw)
	if err != nil {
		return "", err
	}
	start := o.Now()
	entries := make([]map[string]any, len(tasks))
	var wg sync.WaitGroup
	for i, t := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			began := o.Now()
			out := o.Run(ctx, t)
			entries[i] = resultEntry(t.Index, out, seconds(o.Now().Sub(began)))
		}()
	}
	wg.Wait()
	for _, e := range entries {
		capSummary(e, o.MaxSummaryChars)
	}
	data, err := json.Marshal(map[string]any{"results": entries, "total_duration_seconds": seconds(o.Now().Sub(start))})
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func seconds(d time.Duration) float64 { return math.Round(d.Seconds()*100) / 100 }

// resultEntry is upstream _build_result_entry for one child.
func resultEntry(index int, out Outcome, duration float64) map[string]any {
	summary := out.Text
	usable := strings.TrimSpace(summary) != "" && strings.TrimSpace(summary) != "(empty)"
	var status, exit, errText string
	switch {
	case out.Err != nil && isCancel(out.Err):
		status, exit = "interrupted", "interrupted"
		if usable {
			errText = "Operation interrupted."
		}
	case out.Err != nil && !errors.Is(out.Err, ErrBudget):
		status, exit, errText = "failed", "error", out.Err.Error()
	default:
		exit = "completed"
		if errors.Is(out.Err, ErrBudget) {
			exit = "max_iterations"
		}
		status = "completed"
		if !usable {
			status, errText = "failed", "Subagent did not produce a response."
		}
	}
	entry := map[string]any{
		"task_index":       index,
		"status":           status,
		"summary":          summary,
		"api_calls":        out.Turns,
		"duration_seconds": duration,
		"model":            out.Model,
		"exit_reason":      exit,
		"truncated":        exit == "max_iterations",
		"tokens":           map[string]int{"input": out.Usage.InputTokens + out.Usage.CacheReadTokens + out.Usage.CacheWriteTokens, "output": out.Usage.OutputTokens},
		"tool_trace":       toolTrace(out.Messages),
		"cost_usd":         0.0,
		"cost_status":      "unknown",
	}
	if errText != "" {
		entry["error"] = errText
	}
	return entry
}

func isCancel(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// toolTrace pairs each tool call with its result by call id, as upstream
// _build_tool_trace, without raw arguments or output.
func toolTrace(msgs []llm.Message) []map[string]any {
	trace := []map[string]any{}
	byID := map[string]map[string]any{}
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleAssistant:
			for _, c := range m.ToolCalls {
				e := map[string]any{"tool": c.Name, "args_bytes": len(c.Arguments), "input_summary": inputSummary(c.Arguments)}
				trace = append(trace, e)
				if c.ID != "" {
					byID[c.ID] = e
				}
			}
		case llm.RoleTool:
			status := "ok"
			if m.IsError || looksLikeError(m.Content) {
				status = "error"
			}
			target := byID[m.ToolCallID]
			if target == nil && len(trace) > 0 {
				target = trace[len(trace)-1]
			}
			if target != nil {
				target["result_bytes"] = len(m.Content)
				target["status"] = status
			}
		}
	}
	return trace
}

// looksLikeError is upstream _looks_like_error_output.
func looksLikeError(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if json.Unmarshal([]byte(trimmed), &obj) == nil {
			if e, ok := obj["error"]; ok && e != nil && e != "" && e != false {
				return true
			}
			if s, ok := obj["status"].(string); ok {
				switch strings.ToLower(strings.TrimSpace(s)) {
				case "error", "failed", "failure", "timeout":
					return true
				}
			}
		}
	}
	first := strings.ToLower(strings.TrimSpace(strings.SplitN(trimmed, "\n", 2)[0]))
	for _, p := range []string{"error:", "failed:", "traceback ", "exception:"} {
		if strings.HasPrefix(first, p) {
			return true
		}
	}
	return false
}

var targetKeys = map[string]bool{
	"cwd": true, "destination_path": true, "directory": true, "dst": true, "endpoint": true, "file_path": true,
	"new_path": true, "old_path": true, "path": true, "source_path": true, "src": true, "target_path": true,
	"url": true, "urls": true,
}

var urlKeys = map[string]bool{"endpoint": true, "url": true, "urls": true}

// inputSummary is upstream _summarize_tool_arguments: the argument names and
// the side-effect targets, with URL credentials and queries dropped.
func inputSummary(raw json.RawMessage) map[string]any {
	keys := []string{}
	targets := map[string]any{}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 64 {
			keys = keys[:64]
		}
		for k, v := range obj {
			key := strings.ToLower(k)
			if !targetKeys[key] {
				continue
			}
			if clean := sanitizeTarget(key, v); clean != nil {
				targets[key] = clean
			}
		}
	}
	return map[string]any{"argument_keys": keys, "targets": targets}
}

func sanitizeTarget(key string, v any) any {
	if list, ok := v.([]any); ok {
		var out []any
		for i, item := range list {
			if i >= 16 {
				break
			}
			if c := sanitizeTarget(key, item); c != nil {
				out = append(out, c)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	if r := []rune(s); len(r) > 1024 {
		s = string(r[:1024])
	}
	if urlKeys[key] {
		if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
			if u.Hostname() == "" {
				return nil
			}
			return (&url.URL{Scheme: u.Scheme, Host: u.Host[strings.LastIndex(u.Host, "@")+1:], Path: u.Path}).String()
		}
	}
	return s
}

// capSummary trims an over-long summary to a head and tail window with
// upstream's footer. The port keeps no spill file.
func capSummary(entry map[string]any, limit int) {
	summary, _ := entry["summary"].(string)
	runes := []rune(summary)
	if limit <= 0 || len(runes) <= limit {
		return
	}
	headBudget := int(float64(limit) * 0.75)
	tailBudget := limit - headBudget
	head, tail := string(runes[:headBudget]), string(runes[len(runes)-tailBudget:])
	if nl := strings.LastIndex(head, "\n"); nl >= 0 && float64(len([]rune(head[:nl]))) > float64(headBudget)*0.5 {
		head = head[:nl]
	}
	if nl := strings.Index(tail, "\n"); nl >= 0 && float64(len([]rune(tail[:nl]))) < float64(tailBudget)*0.5 {
		tail = tail[nl+1:]
	}
	footer := strings.Join([]string{
		"", strings.Repeat("─", 8) + " [SUMMARY TRUNCATED] " + strings.Repeat("─", 8),
		fmt.Sprintf("Showing %s chars (head) + %s chars (tail) of %s total — trimmed to protect the parent's context window.",
			thousands(len([]rune(head))), thousands(len([]rune(tail))), thousands(len(runes))),
		"Full output could not be stored to disk; the head+tail above is all that was preserved.",
		strings.Repeat("─", 37),
	}, "\n")
	entry["summary"] = head + "\n\n[... middle omitted — see footer ...]\n\n" + tail + footer
	entry["summary_truncated"] = true
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
