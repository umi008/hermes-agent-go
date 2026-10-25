package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/umi008/hermes-agent-go/internal/config"
)

// fakeMCPEnv makes the test binary act as a stdio MCP server, so the CLI
// test launches a real child process.
const fakeMCPEnv = "HERMES_GO_FAKE_MCP"

func TestMain(m *testing.M) {
	// fake-hook-dump records the hook payload, so a test can prove which
	// session a tool call was attributed to.
	if len(os.Args) > 1 && os.Args[1] == "fake-hook-dump" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(1)
		}
		f, err := os.CreateTemp(os.Getenv("HERMES_HOOK_DUMP"), "payload-*.json")
		if err != nil {
			os.Exit(1)
		}
		_, _ = f.Write(data)
		_ = f.Close()
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "fake-hook-context" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		_, _ = os.Stdout.WriteString(`{"context":"PLUGIN NOTE"}`)
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "fake-hook" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		_, _ = os.Stderr.WriteString("todo is off limits\n")
		os.Exit(2)
	}
	if os.Getenv(fakeMCPEnv) == "1" {
		s := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
		s.AddTool(&mcp.Tool{Name: "echo", Description: "Echo.", InputSchema: map[string]any{"type": "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}}}},
			func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var a struct{ Text string }
				_ = json.Unmarshal(r.Params.Arguments, &a)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + a.Text}}}, nil
			})
		s.AddTool(&mcp.Tool{Name: "hidden", InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
		if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestChatCallsMCPTool(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	srv, record := toolCallModel(t, "mcp__demo__echo", `{"text":"hey"}`)

	exeYAML, _ := json.Marshal(exe) // a JSON string is a valid YAML scalar
	f := newHome(t, "model:\n  provider: custom\n  default: tiny\n  base_url: "+srv.URL+"\n"+
		"mcp_servers:\n"+
		"  demo:\n    command: "+string(exeYAML)+"\n    env: {"+fakeMCPEnv+": '1'}\n    tools: {exclude: [hidden]}\n"+
		"  off:\n    command: nope\n    enabled: false\n")
	if code := Run(context.Background(), []string{"chat", "-q", "hi"}, f.env()); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
	}
	calls := record()
	var names []string
	for _, tl := range calls[0]["tools"].([]any) {
		names = append(names, tl.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	got := strings.Join(names, ",")
	if !strings.Contains(got, "mcp__demo__echo") || strings.Contains(got, "hidden") || strings.Contains(got, "mcp__off") {
		t.Errorf("tools = %s", got)
	}
	if res := lastToolResult(t, calls); !strings.HasPrefix(res, `{"result":"echo: hey"`) {
		t.Errorf("tool result = %s", res)
	}
}

func TestMCPServersConfig(t *testing.T) {
	cfg := config.FromMap(map[string]any{"mcp_servers": map[string]any{
		"b": map[string]any{"url": "http://x", "headers": map[string]any{"A": "1"}, "transport": "SSE", "timeout": 2.5, "connect_timeout": "3",
			"tools": map[string]any{"include": "one", "exclude": []any{"two"}}},
		"a":    map[string]any{"command": "srv", "args": []any{"--x", 1}, "env": map[string]any{"K": "v"}, "cwd": "/w", "tools": map[string]any{"include": []any{}}},
		"off":  map[string]any{"command": "srv", "enabled": "no"},
		"bad":  "nope",
		"none": map[string]any{"command": "srv", "tools": map[string]any{"include": nil}},
	}})
	none := func(string) (string, bool) { return "", false }
	got := mcpServers(cfg, none, slog.New(slog.DiscardHandler))
	if len(got) != 3 || got[0].Name != "a" || got[1].Name != "b" || got[2].Name != "none" {
		t.Fatalf("%+v", got)
	}
	a, b := got[0], got[1]
	if a.Command != "srv" || strings.Join(a.Args, " ") != "--x 1" || a.Env["K"] != "v" || a.Cwd != "/w" || a.Include == nil || len(a.Include) != 0 {
		t.Errorf("a = %+v", a)
	}
	if b.URL != "http://x" || b.Headers["A"] != "1" || b.Transport != "sse" || b.CallTimeout != 2500*time.Millisecond ||
		b.ConnectTimeout != 3*time.Second || strings.Join(b.Include, ",") != "one" || strings.Join(b.Exclude, ",") != "two" {
		t.Errorf("b = %+v", b)
	}
	if got[2].Include != nil {
		t.Error("null include must keep every tool")
	}
	safe := func(k string) (string, bool) { return "1", k == "HERMES_SAFE_MODE" }
	if got := mcpServers(cfg, safe, slog.New(slog.DiscardHandler)); got != nil {
		t.Errorf("safe mode: %+v", got)
	}
}

// toolCallModel is a fake model that asks for one tool call, then answers
// "done". record returns every request body it saw.
func toolCallModel(t *testing.T, tool, args string) (srv *httptest.Server, record func() []map[string]any) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []map[string]any
	)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		calls = append(calls, body)
		n := len(calls)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function",
					"function": map[string]any{"name": tool, "arguments": args}}}}}}})
			_, _ = io.WriteString(w, "data: "+string(call)+"\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

// lastToolResult is the tool message the second model request carried.
func lastToolResult(t *testing.T, calls []map[string]any) string {
	t.Helper()
	if len(calls) != 2 {
		t.Fatalf("model calls = %d", len(calls))
	}
	msgs := calls[1]["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" {
		t.Fatalf("last message = %v", last)
	}
	s, _ := last["content"].(string)
	return s
}
