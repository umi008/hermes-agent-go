package mcpclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

type field struct {
	k string
	v any
}

type object []field

func (o object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.k)
		b.Write(k)
		b.WriteByte(':')
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(f.v); err != nil {
			return nil, err
		}
		b.Truncate(b.Len() - 1)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// render is upstream _render_call_tool_result. Images and audio are not
// cached to disk in the port: they become an inline notice.
func render(res *mcp.CallToolResult, server string) (string, error) {
	if res.IsError {
		var sb strings.Builder
		for _, c := range res.Content {
			switch b := c.(type) {
			case *mcp.TextContent:
				sb.WriteString(b.Text)
			case *mcp.EmbeddedResource:
				if b.Resource != nil {
					sb.WriteString(b.Resource.Text)
				}
			}
		}
		msg := sb.String()
		if msg == "" {
			msg = "MCP tool returned an error"
		}
		return "", tools.Error(sanitize(msg))
	}
	var parts []string
	usable := 0
	for _, c := range res.Content {
		text, counts := renderBlock(c, server)
		if text != "" {
			parts = append(parts, text)
		}
		if counts {
			usable++
		}
	}
	text := strings.Join(parts, "\n")
	structured := res.StructuredContent
	if structured != nil && usable > 0 && dualEmits(res.Content, structured) {
		structured = nil
	}
	meta := publicMeta(res.Meta)
	if structured == nil && meta == nil {
		return encode(object{{"result", text}})
	}
	var out object
	if text != "" {
		out = append(out, field{"result", text})
		if structured != nil {
			out = append(out, field{"structuredContent", structured})
		}
	} else if structured != nil {
		out = append(out, field{"result", structured})
	}
	if meta != nil {
		out = append(out, field{"_meta", meta})
	}
	if text == "" && structured == nil {
		out = append(out, field{"result", text})
	}
	return encode(out)
}

func encode(o object) (string, error) {
	data, err := o.MarshalJSON()
	if err != nil {
		return "", fmt.Errorf("encode MCP result: %w", err)
	}
	return string(data), nil
}

// renderBlock returns a block's text and whether it counts as real
// content for the structuredContent arbitration.
func renderBlock(c mcp.Content, server string) (string, bool) {
	switch b := c.(type) {
	case *mcp.TextContent:
		if b.Text == "" {
			return "", false
		}
		return stripTags(b.Text), strings.TrimSpace(b.Text) != ""
	case *mcp.ImageContent:
		return fmt.Sprintf("[MCP image omitted: mimeType=%s, %d bytes]", b.MIMEType, len(b.Data)), true
	case *mcp.AudioContent:
		return fmt.Sprintf("[MCP audio omitted: mimeType=%s, %d bytes]", b.MIMEType, len(b.Data)), true
	case *mcp.ResourceLink:
		if b.URI == "" {
			return "", false
		}
		details := "uri=" + b.URI
		if b.Name != "" {
			details += ", name=" + b.Name
		}
		if b.MIMEType != "" {
			details += ", mimeType=" + b.MIMEType
		}
		return "[MCP resource link: " + details + " — fetch it with " + ToolName(server, "read_resource") + "]", true
	case *mcp.EmbeddedResource:
		if b.Resource == nil {
			return "", false
		}
		if b.Resource.Blob == nil {
			return stripTags(b.Resource.Text), true
		}
		return fmt.Sprintf("[MCP resource omitted: uri=%s, mimeType=%s, %d bytes]", b.Resource.URI, b.Resource.MIMEType, len(b.Resource.Blob)), true
	}
	return fmt.Sprintf("[MCP content dropped: unsupported block (type=%T)]", c), false
}

// dualEmits is upstream _content_dual_emits_structured: a text block that
// parses to the same JSON as structuredContent.
func dualEmits(content []mcp.Content, structured any) bool {
	want, ok := normalizeJSON(structured)
	if !ok {
		return false
	}
	for _, c := range content {
		t, ok := c.(*mcp.TextContent)
		if !ok || t.Text == "" {
			continue
		}
		var got any
		if json.Unmarshal([]byte(t.Text), &got) == nil && reflect.DeepEqual(got, want) {
			return true
		}
	}
	return false
}

func normalizeJSON(v any) (any, bool) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var out any
	return out, json.Unmarshal(data, &out) == nil
}

// publicMeta is upstream _strip_reserved_meta_keys: _meta minus keys whose
// prefix has an mcp or modelcontextprotocol label before the last one.
func publicMeta(meta mcp.Meta) map[string]any {
	out := map[string]any{}
	for k, v := range meta {
		if !reservedMetaKey(k) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func reservedMetaKey(k string) bool {
	slash := strings.IndexByte(k, '/')
	if slash <= 0 {
		return false
	}
	labels := strings.Split(k[:slash], ".")
	for i, l := range labels {
		if (l == "modelcontextprotocol" || l == "mcp") && i < len(labels)-1 {
			return true
		}
	}
	return false
}
