package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

func testServer(t *testing.T) *mcp.Server {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{
		Name: "echo", Description: "Echo the text.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"text": map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}, "description": "what to say"},
		}, "required": []any{"text", "ghost"}},
	}, func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct{ Text string }
		_ = json.Unmarshal(r.Params.Arguments, &a)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + a.Text}}}, nil
	})
	s.AddTool(&mcp.Tool{Name: "boom", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "bad key=hunter2"}}}, nil
		})
	s.AddTool(&mcp.Tool{Name: "data", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: `{"n": 1}`}},
				StructuredContent: map[string]any{"n": 1},
				Meta:              mcp.Meta{"modelcontextprotocol.io/x": 1, "com.example/y": "z"},
			}, nil
		})
	return s
}

func connectMemory(t *testing.T, s Server) *Manager {
	t.Helper()
	srvT, cliT := mcp.NewInMemoryTransports()
	ss, err := testServer(t).Connect(t.Context(), srvT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	s.transport = cliT
	m := Connect(t.Context(), Options{Servers: []Server{s}})
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func byName(ts []tools.Tool) map[string]tools.Tool {
	out := map[string]tools.Tool{}
	for _, t := range ts {
		out[t.Name] = t
	}
	return out
}

func TestToolsRegisterAndCall(t *testing.T) {
	m := connectMemory(t, Server{Name: "my-srv"})
	ts := byName(m.Tools())
	echo, ok := ts["mcp__my_srv__echo"]
	if !ok || len(ts) != 3 {
		t.Fatalf("tools = %v", ts)
	}
	if echo.Toolset != "mcp-my-srv" || echo.Description != "Echo the text." {
		t.Fatalf("%+v", echo)
	}
	if got := string(echo.Parameters); got != `{"properties":{"text":{"description":"what to say","nullable":true,"type":"string"}},"required":["text"],"type":"object"}` {
		t.Fatalf("schema = %s", got)
	}
	if ts["mcp__my_srv__boom"].Description != "MCP tool boom from my-srv" {
		t.Fatal(ts["mcp__my_srv__boom"].Description)
	}
	out, err := echo.Handler(t.Context(), json.RawMessage(`{"text":"hi <b>"}`))
	if err != nil || !strings.HasPrefix(out, `{"result":"echo: hi <b>"`) {
		t.Fatalf("%s %v", out, err)
	}
	_, err = ts["mcp__my_srv__boom"].Handler(t.Context(), nil)
	var te *tools.ToolError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "bad [REDACTED]") {
		t.Fatalf("err %v", err)
	}
	out, err = ts["mcp__my_srv__data"].Handler(t.Context(), json.RawMessage(`{}`))
	if err != nil || !strings.HasPrefix(out, `{"result":"{\"n\": 1}","_meta":{"com.example/y":"z"`) || strings.Contains(out, "modelcontextprotocol.io/x") || strings.Contains(out, "structuredContent") {
		t.Fatalf("%s %v", out, err)
	}
}

func TestFilterIncludeExclude(t *testing.T) {
	m := connectMemory(t, Server{Name: "s", Exclude: []string{"b*"}})
	if ts := byName(m.Tools()); len(ts) != 2 || ts["mcp__s__boom"].Name != "" {
		t.Fatalf("exclude: %v", ts)
	}
	m = connectMemory(t, Server{Name: "s", Include: []string{"echo"}, Exclude: []string{"echo"}})
	if ts := byName(m.Tools()); len(ts) != 1 || ts["mcp__s__echo"].Name == "" {
		t.Fatalf("include: %v", ts)
	}
	m = connectMemory(t, Server{Name: "s", Include: []string{}})
	if ts := m.Tools(); len(ts) != 0 {
		t.Fatalf("empty include: %v", ts)
	}
}

func TestStreamableHTTPWithHeaders(t *testing.T) {
	var auth string
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		auth = r.Header.Get("Authorization")
		return testServer(t)
	}, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	m := Connect(t.Context(), Options{Servers: []Server{{Name: "web", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer abc"}}}})
	t.Cleanup(func() { _ = m.Close() })
	ts := byName(m.Tools())
	out, err := ts["mcp__web__echo"].Handler(t.Context(), json.RawMessage(`{"text":"x"}`))
	if err != nil || !strings.HasPrefix(out, `{"result":"echo: x"`) || auth != "Bearer abc" {
		t.Fatalf("%s %v auth=%q", out, err, auth)
	}
}

func TestUnreachableServerIsSkipped(t *testing.T) {
	m := Connect(t.Context(), Options{Servers: []Server{
		{Name: "gone", Command: "hermes-go-no-such-binary"},
		{Name: "empty"},
	}})
	if len(m.Tools()) != 0 || len(m.Servers()) != 0 {
		t.Fatal("dead servers registered")
	}
}

func TestToolNameClamp(t *testing.T) {
	long := strings.Repeat("x", 80)
	n := ToolName("srv.v2", long)
	if len(n) != 64 || !strings.HasPrefix(n, "mcp__srv_v2__xxx") || n != ToolName("srv.v2", long) {
		t.Fatalf("%q", n)
	}
	if ToolName("a-b", "c.d") != "mcp__a_b__c_d" {
		t.Fatal(ToolName("a-b", "c.d"))
	}
}

func TestNormalizeSchema(t *testing.T) {
	tests := map[string]string{
		`null`: `{"type":"object","properties":{}}`,
		`{"properties":{"definitions":{"$ref":"#/definitions/X"}},"definitions":{"X":{"type":"string"}}}`: `{"$defs":{"X":{"type":"string"}},"properties":{"definitions":{"$ref":"#/$defs/X"}},"required":[],"type":"object"}`,
		`{"type":"object","properties":{"o":{"type":"object","required":["a"]}}}`:                         `{"properties":{"o":{"properties":{},"required":[],"type":"object"}},"required":[],"type":"object"}`,
		`{"type":"object","oneOf":[{"required":["a"]},{"required":["b"]}],"properties":{"a":{},"b":{}}}`:  `{"oneOf":[{"required":["a"]},{"required":["b"]}],"properties":{"a":{},"b":{}},"required":[],"type":"object"}`,
	}
	for in, want := range tests {
		var v any
		_ = json.Unmarshal([]byte(in), &v)
		if got := string(NormalizeSchema(v)); got != want {
			t.Errorf("%s\ngot  %s\nwant %s", in, got, want)
		}
	}
}

func TestPublicMeta(t *testing.T) {
	for k, reserved := range map[string]bool{"modelcontextprotocol.io/a": true, "mcp.tools.com/b": true, "io.modelcontextprotocol/s": false, "com.example.mcp/c": false, "plain": false} {
		if reservedMetaKey(k) != reserved {
			t.Errorf("%s", k)
		}
	}
}

func TestSSETransport(t *testing.T) {
	srv := httptest.NewServer(mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return testServer(t) }, nil))
	t.Cleanup(srv.Close)
	m := Connect(t.Context(), Options{Servers: []Server{{Name: "old", URL: srv.URL, Transport: "sse"}}})
	t.Cleanup(func() { _ = m.Close() })
	ts := byName(m.Tools())
	out, err := ts["mcp__old__echo"].Handler(t.Context(), json.RawMessage(`{"text":"s"}`))
	if err != nil || !strings.HasPrefix(out, `{"result":"echo: s"`) {
		t.Fatalf("%s %v", out, err)
	}
}
