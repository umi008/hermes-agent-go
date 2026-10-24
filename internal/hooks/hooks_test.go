package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/tools"
)

// TestMain lets the test binary act as a hook command:
// <exe> fake-hook <mode> [file].
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "fake-hook" {
		in, _ := io.ReadAll(os.Stdin)
		switch os.Args[2] {
		case "record":
			f, _ := os.OpenFile(os.Args[3], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			_, _ = f.Write(append(in, '\n'))
			_ = f.Close()
		case "block":
			_, _ = os.Stderr.WriteString("no shell today\n")
			os.Exit(BlockExitCode)
		case "modify":
			_, _ = os.Stdout.WriteString(`{"action":"modify","args":{"command":"ls"}}`)
		case "approve":
			_, _ = os.Stdout.WriteString(`{"action":"approve","message":"check"}`)
		case "sleep":
			time.Sleep(5 * time.Second)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type fixture struct {
	Split []struct {
		Line    string
		Posix   []string
		Windows []string
	}
	Evaluate []struct {
		Event      string
		FailClosed bool `json:"fail_closed"`
		Name       string
		Result     struct {
			Returncode *int
			Stdout     string
			Stderr     string
			TimedOut   bool `json:"timed_out"`
			Error      *string
		}
		Want map[string]any
	}
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "upstream.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSplitCommandMatchesUpstream(t *testing.T) {
	for _, c := range loadFixture(t).Split {
		for _, mode := range []struct {
			windows bool
			want    []string
		}{{false, c.Posix}, {true, c.Windows}} {
			got, err := SplitCommand(c.Line, mode.windows)
			if mode.want == nil {
				if err == nil {
					t.Errorf("%q windows=%v: got %q, want an error", c.Line, mode.windows, got)
				}
				continue
			}
			if err != nil || !reflect.DeepEqual(got, mode.want) {
				t.Errorf("%q windows=%v: got %q %v, want %q", c.Line, mode.windows, got, err, mode.want)
			}
		}
	}
}

// wire renders a directive in upstream's dict shape.
func wire(d *Directive) map[string]any {
	switch {
	case d == nil:
		return nil
	case d.Context != "":
		return map[string]any{"context": d.Context}
	case d.Action == "modify":
		return map[string]any{"action": "modify", "args": d.Args}
	case d.Action == "approve":
		out := map[string]any{"action": "approve"}
		if d.Message != "" {
			out["message"] = d.Message
		}
		if d.RuleKey != "" {
			out["rule_key"] = d.RuleKey
		}
		return out
	}
	return map[string]any{"action": d.Action, "message": d.Message}
}

func TestEvaluateMatchesUpstream(t *testing.T) {
	r := &Runner{}
	for _, c := range loadFixture(t).Evaluate {
		res := result{stdout: c.Result.Stdout, stderr: c.Result.Stderr, timedOut: c.Result.TimedOut}
		if c.Result.Returncode != nil {
			res.exitCode = *c.Result.Returncode
		}
		if c.Result.Error != nil {
			res.err = *c.Result.Error
		}
		s := Spec{Event: c.Event, Command: "hook.sh", Timeout: DefaultTimeout, FailClosed: c.FailClosed}
		got := wire(r.evaluate(s, res))
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(c.Want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("%s/%s fail_closed=%v:\n got  %s\n want %s", c.Event, c.Name, c.FailClosed, gotJSON, wantJSON)
		}
	}
}

func TestParse(t *testing.T) {
	raw := map[string]any{
		"pre_tool_call": []any{
			map[string]any{"command": " a.sh ", "matcher": " terminal|patch ", "timeout": 999, "failClosed": true},
			map[string]any{"command": "b.sh", "timeout": "x", "fail_closed": "yes"},
			map[string]any{"command": ""},
			"nope",
		},
		"post_tool_call": []any{map[string]any{"command": "c.sh", "fail_closed": true, "timeout": 0, "matcher": "("}},
		"pre_llm_call":   []any{map[string]any{"command": "d.sh"}},
		"bogus":          []any{map[string]any{"command": "e.sh"}},
		"output_spill":   map[string]any{"x": 1},
		"on_session_end": nil,
	}
	got := Parse(raw, nil)
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	c, a, b := got[0], got[1], got[2]
	if c.Event != PostToolCall || c.FailClosed || c.Timeout != DefaultTimeout || c.Matcher != "(" || c.matcher != nil {
		t.Errorf("c = %+v", c)
	}
	if a.Command != "a.sh" || a.Matcher != "terminal|patch" || a.Timeout != MaxTimeout || !a.FailClosed {
		t.Errorf("a = %+v", a)
	}
	if b.Timeout != DefaultTimeout || b.FailClosed {
		t.Errorf("b = %+v", b)
	}
	if !a.matches("patch") || a.matches("patch2") || a.matches("read_file") || !c.matches("(") || c.matches("x") {
		t.Error("matcher")
	}
	if Parse("not a map", nil) != nil || Parse(nil, nil) != nil {
		t.Error("bad hooks: block")
	}
}

func TestConsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shell-hooks-allowlist.json")
	s := Spec{Event: PreToolCall, Command: "a.sh"}
	if LoadConsent(path, false).Allows(s) || !LoadConsent(path, true).Allows(s) {
		t.Fatal("missing file")
	}
	body := "\uFEFF" + `{"approvals":[{"event":"pre_tool_call","command":"a.sh"},"junk",{"event":"post_tool_call"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c := LoadConsent(path, false)
	if !c.Allows(s) || c.Allows(Spec{Event: PostToolCall, Command: "a.sh"}) {
		t.Fatal("allowlist")
	}
}

type recordExec struct {
	calls []llm.ToolCall
	out   string
	err   error
}

func (r *recordExec) Execute(_ context.Context, call llm.ToolCall) (string, error) {
	r.calls = append(r.calls, call)
	return r.out, r.err
}

func hookCmd(t *testing.T, args ...string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parts := []string{`"` + exe + `"`, "fake-hook"}
	for _, a := range args {
		parts = append(parts, `"`+a+`"`)
	}
	return strings.Join(parts, " ")
}

func readPayloads(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestWrapPayloadsAndModify(t *testing.T) {
	log := filepath.Join(t.TempDir(), "payloads.jsonl")
	next := &recordExec{out: "<b>done</b>"}
	ex := Wrap(next, Options{
		Specs: []Spec{
			{Event: PreToolCall, Command: hookCmd(t, "modify"), Timeout: 10 * time.Second},
			{Event: PreToolCall, Command: hookCmd(t, "record", log), Timeout: 10 * time.Second},
			{Event: PostToolCall, Command: hookCmd(t, "record", log), Timeout: 10 * time.Second, Matcher: "terminal"},
			{Event: PostToolCall, Command: hookCmd(t, "block"), Timeout: 10 * time.Second, Matcher: "other"},
		},
		Runner:    &Runner{GOOS: runtime.GOOS},
		SessionID: func() string { return "s1" },
		Cwd:       "/work",
	})
	out, err := ex.Execute(t.Context(), llm.ToolCall{ID: "c1", Name: "terminal", Arguments: json.RawMessage(`{"command":"rm -rf /","timeout":5}`)})
	if err != nil || out != "<b>done</b>" {
		t.Fatalf("%q %v", out, err)
	}
	if got := string(next.calls[0].Arguments); got != `{"command":"ls","timeout":5}` {
		t.Errorf("modified args = %s", got)
	}
	p := readPayloads(t, log)
	if len(p) != 2 {
		t.Fatalf("payloads = %v", p)
	}
	data, _ := os.ReadFile(log)
	first := strings.SplitN(string(data), "\n", 2)[0]
	want := `{"hook_event_name":"pre_tool_call","tool_name":"terminal","tool_input":{"command":"rm -rf /","timeout":5},` +
		`"session_id":"s1","cwd":"/work","profile":"default","extra":{"task_id":"","tool_call_id":"c1","turn_id":"","api_request_id":"","middleware_trace":[]}}`
	if first != want {
		t.Errorf("pre payload\n got  %s\n want %s", first, want)
	}
	post := p[1]
	extra := post["extra"].(map[string]any)
	if post["hook_event_name"] != PostToolCall || extra["result"] != "<b>done</b>" || extra["status"] != "ok" ||
		post["tool_input"].(map[string]any)["command"] != "ls" || extra["error_type"] != nil {
		t.Errorf("post payload = %v", post)
	}
}

func TestWrapBlocks(t *testing.T) {
	tests := map[string]struct {
		specs   []Spec
		approve func(string, string) bool
		want    string
	}{
		"exit 2":         {specs: []Spec{{Event: PreToolCall, Command: hookCmd(t, "block")}}, want: "no shell today"},
		"block wins":     {specs: []Spec{{Event: PreToolCall, Command: hookCmd(t, "approve")}, {Event: PreToolCall, Command: hookCmd(t, "block")}}, want: "no shell today"},
		"approve denied": {specs: []Spec{{Event: PreToolCall, Command: hookCmd(t, "approve")}}, want: "BLOCKED: plugin approval required for terminal"},
		"approve ok":     {specs: []Spec{{Event: PreToolCall, Command: hookCmd(t, "approve")}}, approve: func(tool, msg string) bool { return tool == "terminal" && msg == "check" }},
		"timeout open":   {specs: []Spec{{Event: PreToolCall, Command: hookCmd(t, "sleep"), Timeout: 200 * time.Millisecond}}},
		"timeout closed": {specs: []Spec{{Event: PreToolCall, Command: hookCmd(t, "sleep"), Timeout: time.Second, FailClosed: true}}, want: "hook " + hookCmd(t, "sleep") + " failed closed: timed out after 1s"},
		"missing closed": {specs: []Spec{{Event: PreToolCall, Command: "hermes-go-no-such-hook", FailClosed: true}}, want: "hook hermes-go-no-such-hook failed closed: command not found"},
		"other tool":     {specs: []Spec{{Event: PreToolCall, Command: hookCmd(t, "block"), Matcher: "patch"}}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			for i := range tc.specs {
				if tc.specs[i].Timeout == 0 {
					tc.specs[i].Timeout = 10 * time.Second
				}
			}
			next := &recordExec{out: "ran"}
			ex := Wrap(next, Options{Specs: tc.specs, Runner: &Runner{GOOS: runtime.GOOS}, Approve: tc.approve})
			out, err := ex.Execute(t.Context(), llm.ToolCall{Name: "terminal", Arguments: json.RawMessage(`{}`)})
			if tc.want == "" {
				if err != nil || out != "ran" {
					t.Fatalf("%q %v", out, err)
				}
				return
			}
			var te *tools.ToolError
			want := tools.Error(tc.want).Error()
			if !errors.As(err, &te) || err.Error() != want || len(next.calls) != 0 {
				t.Fatalf("got %v (calls %d), want %s", err, len(next.calls), want)
			}
		})
	}
}

func TestWrapWithoutHooks(t *testing.T) {
	next := &recordExec{}
	if Wrap(next, Options{}) != Executor(next) {
		t.Fatal("no hooks must return next")
	}
}

func TestObserverFields(t *testing.T) {
	if s, et, em := observerFields(`{"error":"bad"}`); s != "error" || et != "tool_error" || em != "bad" {
		t.Error(s, et, em)
	}
	if s, _, _ := observerFields(`{"error":""}`); s != "ok" {
		t.Error(s)
	}
	if s, _, _ := observerFields(`plain`); s != "ok" {
		t.Error(s)
	}
}
