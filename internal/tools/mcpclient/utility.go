package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

// utility is one of upstream's _UTILITY_TOOL_SPECS: a resource or prompt
// tool registered per server next to its native tools.
type utility struct {
	name, description, parameters string
	resources                     bool // the resources family, else prompts
	required                      string
	run                           func(ctx context.Context, s *mcp.ClientSession, args map[string]any, server string) (object, error)
}

// utilities are upstream's frozen schemas, in registration order.
var utilities = []utility{
	{
		name: "list_resources", description: "List available resources from MCP server '%s'", resources: true,
		parameters: `{"type":"object","properties":{}}`,
		run:        listResources,
	},
	{
		name: "read_resource", description: "Read a resource by URI from MCP server '%s'", resources: true,
		parameters: `{"type":"object","properties":{"uri":{"type":"string","description":"URI of the resource to read"}},"required":["uri"]}`,
		required:   "uri", run: readResource,
	},
	{
		name: "list_prompts", description: "List available prompts from MCP server '%s'",
		parameters: `{"type":"object","properties":{}}`,
		run:        listPrompts,
	},
	{
		name: "get_prompt", description: "Get a prompt by name from MCP server '%s'",
		parameters: `{"type":"object","properties":{"name":{"type":"string","description":"Name of the prompt to retrieve"},` +
			`"arguments":{"type":"object","description":"Optional arguments to pass to the prompt","properties":{},"additionalProperties":true}},"required":["name"]}`,
		required: "name", run: getPrompt,
	},
}

// utilities is upstream _select_utility_schemas: a family registers only
// when the server advertises it and the config leaves it on. The tools
// include/exclude filters do not apply.
func (c *conn) utilities() []tools.Tool {
	if c.caps == nil {
		return nil
	}
	var out []tools.Tool
	for _, u := range utilities {
		advertised, enabled := c.caps.Prompts != nil, c.server.Prompts
		if u.resources {
			advertised, enabled = c.caps.Resources != nil, c.server.Resources
		}
		if !advertised || (enabled != nil && !*enabled) {
			continue
		}
		out = append(out, tools.Tool{
			Name:        ToolName(c.server.Name, u.name),
			Toolset:     "mcp-" + c.server.Name,
			Description: fmt.Sprintf(u.description, c.server.Name),
			Parameters:  json.RawMessage(u.parameters),
			Handler:     c.utilityHandler(u),
		})
	}
	return out
}

func (c *conn) utilityHandler(u utility) tools.Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		args := map[string]any{}
		if len(bytes.TrimSpace(raw)) > 0 && string(raw) != "null" {
			if err := tools.Decode(raw, &args); err != nil {
				return "", err
			}
		}
		if u.required != "" && !truthy(args[u.required]) {
			return "", tools.Error(fmt.Sprintf("Missing required parameter '%s'", u.required))
		}
		var out object
		err := c.call(ctx, true, func(ctx context.Context, s *mcp.ClientSession) error {
			var err error
			out, err = u.run(ctx, s, args, c.server.Name)
			return err
		})
		if err != nil {
			return "", err
		}
		return encode(out)
	}
}

func listResources(ctx context.Context, s *mcp.ClientSession, _ map[string]any, _ string) (object, error) {
	list := []any{}
	for r, err := range s.Resources(ctx, nil) {
		if err != nil {
			return nil, err
		}
		entry := object{{"uri", r.URI}, {"name", r.Name}}
		if r.Description != "" {
			entry = append(entry, field{"description", r.Description})
		}
		if r.MIMEType != "" {
			entry = append(entry, field{"mimeType", r.MIMEType})
		}
		list = append(list, entry)
	}
	return object{{"resources", list}}, nil
}

// readResource is upstream _render_read_resource: text blocks joined by
// newlines, a blob rendered like an embedded resource block.
func readResource(ctx context.Context, s *mcp.ClientSession, args map[string]any, server string) (object, error) {
	res, err := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: str(args["uri"])})
	if err != nil {
		return nil, err
	}
	parts := ""
	for i, b := range res.Contents {
		if i > 0 {
			parts += "\n"
		}
		if b.Blob != nil {
			text, _ := renderBlock(&mcp.EmbeddedResource{Resource: b}, server)
			parts += text
			continue
		}
		parts += stripTags(b.Text)
	}
	return object{{"result", parts}}, nil
}

func listPrompts(ctx context.Context, s *mcp.ClientSession, _ map[string]any, _ string) (object, error) {
	list := []any{}
	for p, err := range s.Prompts(ctx, nil) {
		if err != nil {
			return nil, err
		}
		entry := object{{"name", p.Name}}
		if p.Description != "" {
			entry = append(entry, field{"description", p.Description})
		}
		if len(p.Arguments) > 0 {
			params := make([]any, len(p.Arguments))
			for i, a := range p.Arguments {
				arg := object{{"name", a.Name}}
				if a.Description != "" {
					arg = append(arg, field{"description", a.Description})
				}
				params[i] = append(arg, field{"required", a.Required})
			}
			entry = append(entry, field{"arguments", params})
		}
		list = append(list, entry)
	}
	return object{{"prompts", list}}, nil
}

// getPrompt is upstream _render_get_prompt. Prompt arguments are strings
// on the wire, so other JSON values are sent encoded.
func getPrompt(ctx context.Context, s *mcp.ClientSession, args map[string]any, server string) (object, error) {
	params := &mcp.GetPromptParams{Name: str(args["name"])}
	if a, ok := args["arguments"].(map[string]any); ok && len(a) > 0 {
		params.Arguments = make(map[string]string, len(a))
		for k, v := range a {
			if sv, ok := v.(string); ok {
				params.Arguments[k] = sv
				continue
			}
			data, _ := json.Marshal(v)
			params.Arguments[k] = string(data)
		}
	}
	res, err := s.GetPrompt(ctx, params)
	if err != nil {
		return nil, err
	}
	msgs := make([]any, 0, len(res.Messages))
	for _, m := range res.Messages {
		entry := object{{"role", string(m.Role)}}
		if m.Content != nil {
			text := ""
			if t, ok := m.Content.(*mcp.TextContent); ok {
				text = t.Text
			} else {
				text, _ = renderBlock(m.Content, server)
			}
			entry = append(entry, field{"content", stripTags(text)})
		}
		msgs = append(msgs, entry)
	}
	out := object{{"messages", msgs}}
	if res.Description != "" {
		out = append(out, field{"description", res.Description})
	}
	return out, nil
}

// truthy is Python truthiness for a decoded JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// str renders a decoded JSON scalar as Python str() would for the common
// cases.
func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
