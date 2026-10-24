package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

func echo(name, toolset string) Tool {
	return Tool{
		Name: name, Toolset: toolset, Parameters: json.RawMessage(`{"type":"object"}`),
		Handler: func(_ context.Context, args json.RawMessage) (string, error) { return string(args), nil },
	}
}

func TestNewRegistryRejects(t *testing.T) {
	if _, err := NewRegistry(echo("a", "x"), echo("a", "y")); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := NewRegistry(Tool{Name: "nohandler"}); err == nil {
		t.Fatal("missing handler accepted")
	}
}

func TestSpecsSortedAndFiltered(t *testing.T) {
	off := echo("off", "x")
	off.Available = func() bool { return false }
	r, err := NewRegistry(echo("zeta", "x"), echo("alpha", "y"), off)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range r.Specs() {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "alpha,zeta" {
		t.Fatalf("specs = %v", names)
	}
	if got := r.Select([]string{"y"}).Specs(); len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("select = %v", got)
	}
	if got := r.Toolsets(); strings.Join(got, ",") != "x,y" {
		t.Fatalf("toolsets = %v", got)
	}
}

func TestExecute(t *testing.T) {
	big := strings.Repeat("é", DefaultMaxResultChars+10)
	r, err := NewRegistry(
		echo("echo", "x"),
		Tool{Name: "fail", Handler: func(context.Context, json.RawMessage) (string, error) { return "", errors.New("no such file") }},
		Tool{Name: "panic", Handler: func(context.Context, json.RawMessage) (string, error) { panic("boom") }},
		Tool{Name: "big", Handler: func(context.Context, json.RawMessage) (string, error) { return big, nil }},
		Tool{Name: "own", MaxResultChars: Unlimited, Handler: func(context.Context, json.RawMessage) (string, error) { return big, nil }},
		Tool{Name: "longerr", Handler: func(context.Context, json.RawMessage) (string, error) {
			return "", Error(strings.Repeat("x", 3000))
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		call    llm.ToolCall
		want    string
		wantErr string
	}{
		{llm.ToolCall{Name: "echo"}, "{}", ""},
		{llm.ToolCall{Name: "echo", Arguments: json.RawMessage(`{"a":1}`)}, `{"a":1}`, ""},
		{llm.ToolCall{Name: "nope"}, "", `{"error":"Unknown tool: nope"}`},
		{llm.ToolCall{Name: "fail"}, "", `{"error":"no such file"}`},
		{llm.ToolCall{Name: "panic"}, "", `{"error":"Tool execution failed: panic: boom"}`},
	}
	for _, tt := range tests {
		got, err := r.Execute(context.Background(), tt.call)
		if tt.wantErr != "" {
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("%s: err = %v", tt.call.Name, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("%s: got %q, %v", tt.call.Name, got, err)
		}
	}
	got, _ := r.Execute(context.Background(), llm.ToolCall{Name: "big"})
	if !strings.HasSuffix(got, "[truncated: result was 100010 chars, limit 100000]") {
		t.Errorf("big not capped: ...%s", got[len(got)-60:])
	}
	if got, _ := r.Execute(context.Background(), llm.ToolCall{Name: "own"}); got != big {
		t.Error("unlimited tool was capped")
	}
	_, err = r.Execute(context.Background(), llm.ToolCall{Name: "longerr"})
	var obj map[string]string
	if json.Unmarshal([]byte(err.Error()), &obj) != nil || len([]rune(obj["error"])) != maxErrorChars+len([]rune("… [truncated]")) {
		t.Errorf("error not capped: %d", len(obj["error"]))
	}
}
