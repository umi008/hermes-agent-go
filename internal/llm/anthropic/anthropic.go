// Package anthropic adapts the official Anthropic Go SDK to llm.Client. It
// applies the upstream prompt caching plan (agent/prompt_caching.py): up to
// four cache_control breakpoints over the system prompt and the last
// messages.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// DefaultMaxTokens is used when a request leaves MaxTokens at zero.
const DefaultMaxTokens = 64000

// maxBreakpoints is the API limit on cache_control markers per request.
const maxBreakpoints = 4

// Client calls the Anthropic Messages API, or a compatible proxy.
type Client struct {
	sdk sdk.Client
}

var _ llm.Client = (*Client)(nil)

// Options configures New.
type Options struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// New builds a client. Retries are disabled: the agent loop owns retry
// policy for every provider.
func New(o Options) *Client {
	opts := []option.RequestOption{option.WithMaxRetries(0)}
	if o.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(o.BaseURL))
	}
	if strings.HasPrefix(o.APIKey, "sk-ant-oat") {
		opts = append(opts, option.WithAuthToken(o.APIKey))
	} else if o.APIKey != "" {
		opts = append(opts, option.WithAPIKey(o.APIKey))
	}
	if o.HTTP != nil {
		opts = append(opts, option.WithHTTPClient(o.HTTP))
	}
	return &Client{sdk: sdk.NewClient(opts...)}
}

// Stream implements llm.Client.
func (c *Client) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event)) (llm.Response, error) {
	params, err := BuildParams(req)
	if err != nil {
		return llm.Response{}, err
	}
	emit := func(e llm.Event) {
		if onEvent != nil {
			onEvent(e)
		}
	}
	stream := c.sdk.Messages.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	var msg sdk.Message
	stopped := false
	for stream.Next() {
		event := stream.Current()
		if err := msg.Accumulate(event); err != nil {
			return llm.Response{}, fmt.Errorf("anthropic: accumulate stream: %w", err)
		}
		switch ev := event.AsAny().(type) {
		case sdk.ContentBlockDeltaEvent:
			switch d := ev.Delta.AsAny().(type) {
			case sdk.TextDelta:
				emit(llm.Event{Kind: llm.EventText, Text: d.Text})
			case sdk.ThinkingDelta:
				emit(llm.Event{Kind: llm.EventReasoning, Text: d.Thinking})
			}
		case sdk.MessageStopEvent:
			stopped = true
		}
	}
	if err := stream.Err(); err != nil {
		return llm.Response{}, convertError(ctx, err)
	}
	if ctx.Err() != nil {
		return llm.Response{}, ctx.Err()
	}
	if !stopped {
		return llm.Response{}, fmt.Errorf("anthropic: %w", llm.ErrStreamInterrupted)
	}
	resp := toResponse(msg)
	for i := range resp.Message.ToolCalls {
		emit(llm.Event{Kind: llm.EventToolCall, ToolCall: &resp.Message.ToolCalls[i]})
	}
	return resp, nil
}

func convertError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		// The limiter's own wait, when it sends one, beats the local backoff.
		var after time.Duration
		if apiErr.Response != nil {
			after, _ = llm.ParseRetryAfter(apiErr.Response.Header.Get("Retry-After"))
		}
		return &llm.APIError{Provider: "anthropic", Status: apiErr.StatusCode, Body: apiErr.Error(), RetryAfter: after}
	}
	return fmt.Errorf("anthropic: %w: %w", llm.ErrStreamInterrupted, err)
}

func toResponse(msg sdk.Message) llm.Response {
	out := llm.Message{Role: llm.RoleAssistant, Native: msg.ToParam()}
	var text strings.Builder
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case sdk.TextBlock:
			text.WriteString(b.Text)
		case sdk.ToolUseBlock:
			args := json.RawMessage(b.JSON.Input.Raw())
			if len(strings.TrimSpace(string(args))) == 0 {
				args = json.RawMessage("{}")
			}
			out.ToolCalls = append(out.ToolCalls, llm.ToolCall{ID: b.ID, Name: b.Name, Arguments: args})
		}
	}
	out.Content = text.String()
	stop := llm.StopEnd
	switch msg.StopReason {
	case sdk.StopReasonToolUse:
		stop = llm.StopToolUse
	case sdk.StopReasonMaxTokens:
		stop = llm.StopMaxTokens
	}
	if len(out.ToolCalls) > 0 {
		stop = llm.StopToolUse
	}
	return llm.Response{
		Message:    out,
		StopReason: stop,
		Usage: llm.Usage{
			InputTokens:      int(msg.Usage.InputTokens),
			OutputTokens:     int(msg.Usage.OutputTokens),
			CacheReadTokens:  int(msg.Usage.CacheReadInputTokens),
			CacheWriteTokens: int(msg.Usage.CacheCreationInputTokens),
		},
	}
}

// BuildParams converts a neutral request, including cache breakpoints.
func BuildParams(req llm.Request) (sdk.MessageNewParams, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	p := sdk.MessageNewParams{Model: req.Model, MaxTokens: int64(maxTokens)}
	for _, part := range req.System {
		if part != "" {
			p.System = append(p.System, sdk.TextBlockParam{Text: part})
		}
	}
	msgs, err := convertMessages(req.Messages)
	if err != nil {
		return p, err
	}
	p.Messages = msgs
	for _, t := range req.Tools {
		tool, err := convertTool(t)
		if err != nil {
			return p, err
		}
		p.Tools = append(p.Tools, sdk.ToolUnionParam{OfTool: &tool})
	}
	applyCacheControl(&p, cacheMarker(req.CacheTTL))
	return p, nil
}

func cacheMarker(ttl string) sdk.CacheControlEphemeralParam {
	m := sdk.NewCacheControlEphemeralParam()
	if ttl == "1h" {
		m.TTL = sdk.CacheControlEphemeralTTLTTL1h
	}
	return m
}

// applyCacheControl places breakpoints like upstream: the stable system
// prefix and the end of the system prompt, then the last messages, for at
// most four markers.
func applyCacheControl(p *sdk.MessageNewParams, marker sdk.CacheControlEphemeralParam) {
	used := 0
	if n := len(p.System); n > 0 {
		if n > 1 {
			p.System[0].CacheControl = marker
			used++
		}
		p.System[n-1].CacheControl = marker
		used++
	}
	for i := len(p.Messages) - 1; i >= 0 && used < maxBreakpoints; i-- {
		content := p.Messages[i].Content
		if len(content) == 0 {
			continue
		}
		if cc := content[len(content)-1].GetCacheControl(); cc != nil {
			*cc = marker
			used++
		}
	}
}

func convertTool(t llm.ToolSpec) (sdk.ToolParam, error) {
	tool := sdk.ToolParam{Name: t.Name}
	if t.Description != "" {
		tool.Description = sdk.String(t.Description)
	}
	schema := struct {
		Properties any      `json:"properties"`
		Required   []string `json:"required"`
	}{}
	if len(t.Parameters) > 0 {
		if err := json.Unmarshal(t.Parameters, &schema); err != nil {
			return tool, fmt.Errorf("tool %s: decode parameters: %w", t.Name, err)
		}
	}
	if schema.Properties == nil {
		schema.Properties = map[string]any{}
	}
	tool.InputSchema = sdk.ToolInputSchemaParam{Properties: schema.Properties, Required: schema.Required}
	return tool, nil
}

// convertMessages maps neutral messages to Anthropic turns. Tool results
// become user tool_result blocks, and consecutive turns with the same role
// merge so parallel tool results share one user message.
func convertMessages(in []llm.Message) ([]sdk.MessageParam, error) {
	var out []sdk.MessageParam
	push := func(role sdk.MessageParamRole, blocks []sdk.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, sdk.MessageParam{Role: role, Content: blocks})
	}
	for _, m := range in {
		switch m.Role {
		case llm.RoleUser:
			if m.Content != "" {
				push(sdk.MessageParamRoleUser, []sdk.ContentBlockParamUnion{sdk.NewTextBlock(m.Content)})
			}
		case llm.RoleTool:
			push(sdk.MessageParamRoleUser, []sdk.ContentBlockParamUnion{sdk.NewToolResultBlock(m.ToolCallID, m.Content, m.IsError)})
		case llm.RoleAssistant:
			if native, ok := m.Native.(sdk.MessageParam); ok {
				push(sdk.MessageParamRoleAssistant, cloneBlocks(native.Content))
				continue
			}
			var blocks []sdk.ContentBlockParamUnion
			if m.Content != "" {
				blocks = append(blocks, sdk.NewTextBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				var input any = map[string]any{}
				if len(tc.Arguments) > 0 {
					if err := json.Unmarshal(tc.Arguments, &input); err != nil {
						return nil, fmt.Errorf("tool call %s: decode arguments: %w", tc.ID, err)
					}
				}
				blocks = append(blocks, sdk.NewToolUseBlock(tc.ID, input, tc.Name))
			}
			push(sdk.MessageParamRoleAssistant, blocks)
		default:
			return nil, fmt.Errorf("unsupported role %q", m.Role)
		}
	}
	return out, nil
}

// cloneBlocks copies the union values and the blocks they point to, so cache
// markers placed on one request never leak into stored history.
func cloneBlocks(in []sdk.ContentBlockParamUnion) []sdk.ContentBlockParamUnion {
	out := make([]sdk.ContentBlockParamUnion, len(in))
	for i, b := range in {
		switch {
		case b.OfText != nil:
			v := *b.OfText
			b.OfText = &v
		case b.OfToolUse != nil:
			v := *b.OfToolUse
			b.OfToolUse = &v
		case b.OfToolResult != nil:
			v := *b.OfToolResult
			b.OfToolResult = &v
		}
		out[i] = b
	}
	return out
}
