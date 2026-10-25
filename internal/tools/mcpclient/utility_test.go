package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

// richServer is testServer plus two resources and a prompt, so it
// advertises the resources and prompts capabilities.
func richServer(t *testing.T) *mcp.Server {
	t.Helper()
	s := testServer(t)
	s.AddResource(&mcp.Resource{URI: "file:///a.txt", Name: "a", Description: "first", MIMEType: "text/plain"},
		func(_ context.Context, r *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
				{URI: r.Params.URI, Text: "hello\U000E0041"}, {URI: r.Params.URI, Text: "second"},
			}}, nil
		})
	s.AddResource(&mcp.Resource{URI: "file:///b.bin", Name: "b"},
		func(_ context.Context, r *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
				{URI: r.Params.URI, MIMEType: "application/octet-stream", Blob: []byte{1, 2, 3}},
			}}, nil
		})
	s.AddPrompt(&mcp.Prompt{Name: "greet", Description: "Say hi", Arguments: []*mcp.PromptArgument{
		{Name: "who", Description: "whom", Required: true}, {Name: "tone"},
	}}, func(_ context.Context, r *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Description: "greeting", Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: "hi " + r.Params.Arguments["who"] + " " + r.Params.Arguments["n"]}},
		}}, nil
	})
	s.AddPrompt(&mcp.Prompt{Name: "bare"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{}}, nil
	})
	return s
}

func call(t *testing.T, tool tools.Tool, args string) (string, error) {
	t.Helper()
	if tool.Handler == nil {
		t.Fatalf("tool %q is not registered", tool.Name)
	}
	return tool.Handler(t.Context(), json.RawMessage(args))
}

func TestUtilitySchemas(t *testing.T) {
	ts := byName(connectServer(t, Server{Name: "rich"}, richServer(t)).Tools())
	want := map[string]string{
		"mcp__rich__list_resources": `{"name":"mcp__rich__list_resources","description":"List available resources from MCP server 'rich'","parameters":{"type":"object","properties":{}}}`,
		"mcp__rich__read_resource":  `{"name":"mcp__rich__read_resource","description":"Read a resource by URI from MCP server 'rich'","parameters":{"type":"object","properties":{"uri":{"type":"string","description":"URI of the resource to read"}},"required":["uri"]}}`,
		"mcp__rich__list_prompts":   `{"name":"mcp__rich__list_prompts","description":"List available prompts from MCP server 'rich'","parameters":{"type":"object","properties":{}}}`,
		"mcp__rich__get_prompt":     `{"name":"mcp__rich__get_prompt","description":"Get a prompt by name from MCP server 'rich'","parameters":{"type":"object","properties":{"name":{"type":"string","description":"Name of the prompt to retrieve"},"arguments":{"type":"object","description":"Optional arguments to pass to the prompt","properties":{},"additionalProperties":true}},"required":["name"]}}`,
	}
	if len(ts) != 3+len(want) {
		t.Fatalf("tools = %v", ts)
	}
	for name, schema := range want {
		tool := ts[name]
		got := `{"name":"` + tool.Name + `","description":"` + tool.Description + `","parameters":` + string(tool.Parameters) + `}`
		if got != schema || tool.Toolset != "mcp-rich" {
			t.Errorf("%s:\ngot  %s (toolset %s)\nwant %s", name, got, tool.Toolset, schema)
		}
	}
}

func TestUtilitiesFollowCapabilitiesAndConfig(t *testing.T) {
	if ts := byName(connectMemory(t, Server{Name: "s"}).Tools()); ts["mcp__s__list_resources"].Name != "" || ts["mcp__s__list_prompts"].Name != "" {
		t.Fatalf("utilities registered without the capabilities: %v", ts)
	}
	off := false
	ts := byName(connectServer(t, Server{Name: "s", Resources: &off}, richServer(t)).Tools())
	if ts["mcp__s__read_resource"].Name != "" || ts["mcp__s__get_prompt"].Name == "" {
		t.Fatalf("resources: false: %v", ts)
	}
	ts = byName(connectServer(t, Server{Name: "s", Prompts: &off, Include: []string{}}, richServer(t)).Tools())
	if len(ts) != 2 || ts["mcp__s__list_resources"].Name == "" || ts["mcp__s__read_resource"].Name == "" {
		t.Fatalf("prompts: false with an empty include: %v", ts)
	}
}

func TestNativeToolWinsUtilityName(t *testing.T) {
	srv := richServer(t)
	srv.AddTool(&mcp.Tool{Name: "list_prompts", Description: "native", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "native"}}}, nil
		})
	ts := connectServer(t, Server{Name: "s"}, srv).Tools()
	n := 0
	for _, tool := range ts {
		if tool.Name == "mcp__s__list_prompts" {
			n++
			if tool.Description != "native" {
				t.Errorf("utility replaced the native tool: %+v", tool)
			}
		}
	}
	if n != 1 {
		t.Fatalf("list_prompts registered %d times", n)
	}
}

func TestUtilityHandlers(t *testing.T) {
	ts := byName(connectServer(t, Server{Name: "rich"}, richServer(t)).Tools())
	cases := []struct{ tool, args, want string }{
		{"list_resources", `{}`, `{"resources":[{"uri":"file:///a.txt","name":"a","description":"first","mimeType":"text/plain"},{"uri":"file:///b.bin","name":"b"}]}`},
		{"read_resource", `{"uri":"file:///a.txt"}`, `{"result":"hello\nsecond"}`},
		{"read_resource", `{"uri":"file:///b.bin"}`, `{"result":"[MCP resource omitted: uri=file:///b.bin, mimeType=application/octet-stream, 3 bytes]"}`},
		{"list_prompts", ``, `{"prompts":[{"name":"bare"},{"name":"greet","description":"Say hi","arguments":[{"name":"who","description":"whom","required":true},{"name":"tone","required":false}]}]}`},
		{"get_prompt", `{"name":"greet","arguments":{"who":"Ana","n":2}}`, `{"messages":[{"role":"user","content":"hi Ana 2"}],"description":"greeting"}`},
		{"get_prompt", `{"name":"bare"}`, `{"messages":[]}`},
	}
	for _, tc := range cases {
		got, err := call(t, ts["mcp__rich__"+tc.tool], tc.args)
		if err != nil || got != tc.want {
			t.Errorf("%s %s:\ngot  %s %v\nwant %s", tc.tool, tc.args, got, err, tc.want)
		}
	}
}

func TestUtilityErrors(t *testing.T) {
	ts := byName(connectServer(t, Server{Name: "rich"}, richServer(t)).Tools())
	cases := []struct{ tool, args, want string }{
		{"read_resource", `{}`, "Missing required parameter 'uri'"},
		{"read_resource", `{"uri":""}`, "Missing required parameter 'uri'"},
		{"get_prompt", `{"arguments":{}}`, "Missing required parameter 'name'"},
		{"read_resource", `{"uri":"file:///missing"}`, "MCP call failed: "},
		{"get_prompt", `{"name":"nope"}`, "MCP call failed: "},
	}
	for _, tc := range cases {
		_, err := call(t, ts["mcp__rich__"+tc.tool], tc.args)
		var te *tools.ToolError
		if !errors.As(err, &te) || !strings.Contains(err.Error(), `{"error":"`+tc.want) {
			t.Errorf("%s %s: err = %v, want prefix %q", tc.tool, tc.args, err, tc.want)
		}
	}
}
