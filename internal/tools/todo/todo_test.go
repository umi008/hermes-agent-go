package todo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/tools"
)

func call(t *testing.T, r *tools.Registry, args string) string {
	t.Helper()
	out, err := r.Execute(context.Background(), llm.ToolCall{Name: "todo_list", Arguments: json.RawMessage(args)})
	if err != nil {
		return err.Error()
	}
	return out
}

func TestTodoTool(t *testing.T) {
	s := New()
	r, err := tools.NewRegistry(s.Tools()...)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, args, want string }{
		{"empty read", `{}`, `{"todos":[],"revision":0,"summary":{"total":0,"pending":0,"in_progress":0,"completed":0,"cancelled":0}}`},
		{"write lifts in_progress", `{"todos":[{"id":"1","content":"a","status":"pending"},{"id":"2","content":" b ","status":"IN_PROGRESS"}]}`,
			`{"todos":[{"id":"2","content":"b","status":"in_progress"},{"id":"1","content":"a","status":"pending"}],"revision":1,"summary":{"total":2,"pending":1,"in_progress":1,"completed":0,"cancelled":0}}`},
		{"same write keeps revision", `{"todos":[{"id":"2","content":"b","status":"in_progress"},{"id":"1","content":"a","status":"pending"}]}`, `"revision":1`},
		{"merge updates and appends", `{"merge":true,"todos":[{"id":"2","status":"completed"},{"id":"3","content":"c","status":"bogus","parent":"1"}]}`,
			`{"todos":[{"id":"2","content":"b","status":"completed"},{"id":"1","content":"a","status":"pending"},{"id":"3","content":"c","status":"pending","parent":"1"}],"revision":2`},
		{"empty content rejected", `{"todos":[{"id":"9","content":"  ","status":"pending"}]}`, `{"error":"Todo item '9' has empty or missing content. Each item must have a non-empty description."}`},
		{"rejected write leaves list", `{}`, `"revision":2`},
		{"not a list", `{"todos":{"id":"1"}}`, `{"error":"todos must be a list, got dict"}`},
		{"dedupe and dangling parent", `{"todos":[{"id":"1","content":"x","status":"pending","parent":"zz"},{"id":"1","content":"y","status":"completed"}]}`,
			`{"todos":[{"id":"1","content":"y","status":"completed"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := call(t, r, tt.args); !strings.Contains(got, tt.want) {
				t.Fatalf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestCapsAndInjection(t *testing.T) {
	s := New()
	long := strings.Repeat("é", MaxContentChars+10)
	items, err := s.Write([]map[string]any{
		{"id": "p", "content": "parent", "status": "completed"},
		{"id": "c", "content": long, "status": "in_progress", "parent": "p"},
		{"id": "d", "content": "done", "status": "completed"},
		{"id": "x", "content": "cycle a", "status": "pending", "parent": "y"},
		{"id": "y", "content": "cycle b", "status": "pending", "parent": "x"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if c := []rune(items[1].Content); len(c) != MaxContentChars || !strings.HasSuffix(items[1].Content, truncMarker) {
		t.Fatalf("content not capped: %d", len(c))
	}
	if items[3].Parent == "y" && items[4].Parent == "x" {
		t.Fatal("cycle kept")
	}
	got := s.FormatForInjection()
	if !strings.HasPrefix(got, InjectionHeader+"\n- [x] p. parent (completed)\n  - [>] c. ") || strings.Contains(got, "done") {
		t.Fatalf("injection:\n%s", got)
	}
	big := make([]map[string]any, MaxItems+5)
	for i := range big {
		big[i] = map[string]any{"id": float64(i), "content": "t", "status": "pending"}
	}
	if items, _ = s.Write(big, false); len(items) != MaxItems || items[1].ID != "1" {
		t.Fatalf("len %d", len(items))
	}
}
