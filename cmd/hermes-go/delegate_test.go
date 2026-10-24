package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/tools/approval"
	"github.com/umi008/hermes-agent-go/internal/tools/delegate"
)

// delegatingModel plays both sides: the parent asks for delegate_task once
// and then answers "parent done"; a child (recognized by its system prompt)
// answers "child summary".
func delegatingModel(t *testing.T, args string) (*httptest.Server, func() (parent, child []map[string]any)) {
	t.Helper()
	var (
		mu            sync.Mutex
		parent, child []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		msgs, _ := body["messages"].([]any)
		sys, _ := msgs[0].(map[string]any)["content"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		text := func(s string) {
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"`+s+`"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(sys, "You are a focused subagent") {
			child = append(child, body)
			text("child summary")
			return
		}
		parent = append(parent, body)
		if len(parent) == 1 {
			call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function",
					"function": map[string]any{"name": delegate.Name, "arguments": args}}}}}}})
			_, _ = io.WriteString(w, "data: "+string(call)+"\n\ndata: [DONE]\n\n")
			return
		}
		text("parent done")
	}))
	t.Cleanup(srv.Close)
	return srv, func() ([]map[string]any, []map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		return parent, child
	}
}

func toolNames(body map[string]any) map[string]bool {
	names := map[string]bool{}
	list, _ := body["tools"].([]any)
	for _, tl := range list {
		fn, _ := tl.(map[string]any)["function"].(map[string]any)
		name, _ := fn["name"].(string)
		names[name] = true
	}
	return names
}

func TestDelegateTaskRunsAChildAndReturnsItsSummary(t *testing.T) {
	srv, record := delegatingModel(t, `{"tasks":[{"goal":"count the files","context":"repo at /src"}]}`)
	f := newHome(t, modelConfig(srv.URL))
	code := Run(context.Background(), []string{"chat", "-q", "go"}, f.env())
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
	}
	if f.stdout.String() != "parent done\n" {
		t.Fatalf("stdout = %q", f.stdout.String())
	}
	parent, child := record()
	if len(parent) != 2 || len(child) != 1 {
		t.Fatalf("parent calls %d, child calls %d", len(parent), len(child))
	}
	if !toolNames(parent[0])[delegate.Name] {
		t.Error("parent must see delegate_task")
	}
	childTools := toolNames(child[0])
	for _, blocked := range delegate.Blocked {
		if childTools[blocked] {
			t.Errorf("child sees blocked tool %s", blocked)
		}
	}
	msgs := child[0]["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "\nCONTEXT:\nrepo at /src") || !strings.Contains(sys, "\nWORKSPACE PATH:\n") ||
		!strings.Contains(sys, "Keep your final summary tight") {
		t.Errorf("child system prompt = %q", sys)
	}
	if first := msgs[1].(map[string]any); first["role"] != "user" || first["content"] != "count the files" || len(msgs) != 2 {
		t.Errorf("child conversation = %v", msgs)
	}

	result := lastToolResult(t, parent)
	var got struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("tool result %q: %v", result, err)
	}
	if len(got.Results) != 1 || got.Results[0]["summary"] != "child summary" || got.Results[0]["status"] != "completed" ||
		got.Results[0]["model"] != "tiny" {
		t.Fatalf("results = %+v", got.Results)
	}

	all, _ := sessions(t, f)
	var parentID, childParent string
	for _, s := range all {
		switch s.Source {
		case "cli":
			parentID = s.ID
		case "subagent":
			childParent = s.ParentID
		}
	}
	if parentID == "" || childParent != parentID {
		t.Errorf("child session parent = %q, want %q (sessions %+v)", childParent, parentID, all)
	}
}

func TestDelegateTaskLeftOutByToolsets(t *testing.T) {
	srv, record := toolCallModel(t, "todo", `{}`)
	f := newHome(t, modelConfig(srv.URL))
	if code := Run(context.Background(), []string{"chat", "--toolsets", "todo", "-q", "go"}, f.env()); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
	}
	if toolNames(record()[0])[delegate.Name] {
		t.Error("delegate_task must follow the --toolsets filter")
	}
}

func TestChildPrompterDeniesUnlessAutoApprove(t *testing.T) {
	rt := &chatRuntime{logger: slog.New(slog.DiscardHandler)}
	if c, _ := (childPrompter{rt: rt}).Approve(context.Background(), approval.Request{Command: "rm -rf x"}); c != approval.Deny {
		t.Errorf("default = %v, want deny", c)
	}
	if c, _ := (childPrompter{rt: rt, approve: true}).Approve(context.Background(), approval.Request{}); c != approval.Once {
		t.Errorf("auto approve = %v, want once", c)
	}
}
