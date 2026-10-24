package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

func call(t *testing.T, o Options, args string) (map[string]any, error) {
	t.Helper()
	out, err := Tool(o).Handler(context.Background(), json.RawMessage(args))
	if err != nil {
		return nil, err
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, out)
	}
	return got, nil
}

func results(t *testing.T, got map[string]any) []map[string]any {
	t.Helper()
	raw, _ := got["results"].([]any)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i] = r.(map[string]any)
	}
	return out
}

func TestBatchRunsEveryTaskAndKeepsOrder(t *testing.T) {
	var mu sync.Mutex
	var seen []Task
	o := Options{Run: func(_ context.Context, task Task) Outcome {
		mu.Lock()
		seen = append(seen, task)
		mu.Unlock()
		return Outcome{Text: "done " + task.Goal, Turns: 2, Model: "m", Usage: llm.Usage{InputTokens: 10, CacheReadTokens: 5, OutputTokens: 3}}
	}}
	got, err := call(t, o, `{"tasks":[{"goal":"a","context":"ctx a"},{"goal":" b "}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("ran %d children", len(seen))
	}
	rs := results(t, got)
	if len(rs) != 2 || rs[0]["summary"] != "done a" || rs[1]["summary"] != "done b" {
		t.Fatalf("results = %+v", rs)
	}
	for i, r := range rs {
		if r["task_index"].(float64) != float64(i) || r["status"] != "completed" || r["exit_reason"] != "completed" ||
			r["api_calls"].(float64) != 2 || r["model"] != "m" || r["truncated"] != false || r["cost_status"] != "unknown" {
			t.Errorf("entry %d = %+v", i, r)
		}
		tokens := r["tokens"].(map[string]any)
		if tokens["input"].(float64) != 15 || tokens["output"].(float64) != 3 {
			t.Errorf("tokens = %+v", tokens)
		}
	}
	if _, ok := got["total_duration_seconds"]; !ok {
		t.Error("missing total_duration_seconds")
	}
	for _, s := range seen {
		if s.Goal == "a" && s.Context != "ctx a" {
			t.Errorf("context not passed: %+v", s)
		}
	}
}

func TestLegacyGoalIsOneTask(t *testing.T) {
	var got Task
	o := Options{Run: func(_ context.Context, task Task) Outcome { got = task; return Outcome{Text: "ok"} }}
	if _, err := call(t, o, `{"goal":"g","context":"c"}`); err != nil {
		t.Fatal(err)
	}
	if got.Goal != "g" || got.Context != "c" {
		t.Errorf("task = %+v", got)
	}
}

func TestValidation(t *testing.T) {
	o := Options{MaxChildren: 2, Run: func(context.Context, Task) Outcome { t.Fatal("must not run"); return Outcome{} }}
	cases := map[string]string{
		`{}`: "Provide either 'goal' (single task) or 'tasks' (batch).",
		`{"tasks":[{"goal":"a"},{"goal":"b"},{"goal":"c"}]}`: "Too many tasks: 3 provided, but max_concurrent_children is 2.",
		`{"tasks":[{"goal":"a"},{"context":"x"}]}`:           "Task 1 is missing a 'goal'.",
		`{"action":"list"}`:                                  "not supported",
	}
	for args, want := range cases {
		if _, err := call(t, o, args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", args, err, want)
		}
	}
}

func TestStatusRules(t *testing.T) {
	outcomes := []Outcome{
		{Text: "partial", Err: ErrBudget},
		{Text: "", Err: nil},
		{Text: "boom", Err: errors.New("provider down")},
		{Text: "so far", Err: context.Canceled},
	}
	o := Options{Run: func(_ context.Context, task Task) Outcome { return outcomes[task.Index] }}
	got, err := call(t, o, `{"tasks":[{"goal":"a"},{"goal":"b"},{"goal":"c"},{"goal":"d"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	rs := results(t, got)
	want := []struct{ status, exit, err string }{
		{"completed", "max_iterations", ""},
		{"failed", "completed", "Subagent did not produce a response."},
		{"failed", "error", "provider down"},
		{"interrupted", "interrupted", "Operation interrupted."},
	}
	for i, w := range want {
		e, _ := rs[i]["error"].(string)
		if rs[i]["status"] != w.status || rs[i]["exit_reason"] != w.exit || e != w.err {
			t.Errorf("entry %d = %+v, want %+v", i, rs[i], w)
		}
	}
	if rs[0]["truncated"] != true {
		t.Error("max_iterations must mark truncated")
	}
}

func TestToolTraceSummarizesWithoutRawData(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go","offset":3}`)},
			{ID: "2", Name: "web_extract", Arguments: json.RawMessage(`{"url":"https://u:p@x.example/p?q=secret"}`)},
		}},
		{Role: llm.RoleTool, ToolCallID: "1", Content: "content"},
		{Role: llm.RoleTool, ToolCallID: "2", Content: `{"error":"nope"}`},
	}
	o := Options{Run: func(context.Context, Task) Outcome { return Outcome{Text: "ok", Messages: msgs} }}
	got, err := call(t, o, `{"goal":"g"}`)
	if err != nil {
		t.Fatal(err)
	}
	trace := results(t, got)[0]["tool_trace"].([]any)
	first, second := trace[0].(map[string]any), trace[1].(map[string]any)
	if first["tool"] != "read_file" || first["status"] != "ok" || first["result_bytes"].(float64) != 7 {
		t.Errorf("first = %+v", first)
	}
	sum := first["input_summary"].(map[string]any)
	if keys := sum["argument_keys"].([]any); len(keys) != 2 || keys[0] != "offset" || keys[1] != "path" {
		t.Errorf("keys = %v", keys)
	}
	if sum["targets"].(map[string]any)["path"] != "a.go" {
		t.Errorf("targets = %v", sum["targets"])
	}
	if second["status"] != "error" {
		t.Errorf("second status = %v", second["status"])
	}
	if u := second["input_summary"].(map[string]any)["targets"].(map[string]any)["url"]; u != "https://x.example/p" {
		t.Errorf("url target = %v", u)
	}
}

func TestLooksLikeError(t *testing.T) {
	for in, want := range map[string]bool{
		`{"status":"failed"}`: true, "Error: x": true, "Traceback (most recent": true,
		`{"error":null}`: false, "fine": false, "": false,
	} {
		if got := looksLikeError(in); got != want {
			t.Errorf("looksLikeError(%q) = %v", in, got)
		}
	}
}

func TestSummaryCapKeepsHeadAndTail(t *testing.T) {
	long := strings.Repeat("h", 80) + strings.Repeat("t", 40)
	o := Options{MaxSummaryChars: 40, Run: func(context.Context, Task) Outcome { return Outcome{Text: long} }}
	got, err := call(t, o, `{"goal":"g"}`)
	if err != nil {
		t.Fatal(err)
	}
	r := results(t, got)[0]
	s := r["summary"].(string)
	if r["summary_truncated"] != true || !strings.HasPrefix(s, strings.Repeat("h", 30)+"\n\n[... middle omitted — see footer ...]\n\n"+strings.Repeat("t", 10)) ||
		!strings.Contains(s, "Showing 30 chars (head) + 10 chars (tail) of 120 total") {
		t.Errorf("summary = %q", s)
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int]string{7: "7", 1234: "1,234", 1234567: "1,234,567"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %s", n, got)
		}
	}
}
