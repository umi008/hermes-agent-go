// Package openai is a streaming client for OpenAI-compatible chat
// completions endpoints (OpenRouter, Ollama, LM Studio, vLLM, DeepSeek, ...).
// It speaks plain net/http so it tolerates the loose schemas those servers
// return.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// Client calls one OpenAI-compatible endpoint.
type Client struct {
	BaseURL string
	APIKey  string
	// Name labels errors, for example "openrouter".
	Name string
	HTTP *http.Client
	// Headers are extra request headers.
	Headers map[string]string
}

var _ llm.Client = (*Client)(nil)

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string          `json:"type"`
	Function wireToolFuncDef `json:"function"`
}

type wireToolFuncDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireRequest struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Tools         []wireTool     `json:"tools,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions map[string]any `json:"stream_options,omitempty"`
}

type wireChunk struct {
	Choices []struct {
		Delta struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			Reasoning        string         `json:"reasoning"`
			ToolCalls        []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func strPtr(s string) *string { return &s }

// buildRequest converts a neutral request to the wire format.
func buildRequest(req llm.Request) wireRequest {
	w := wireRequest{
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		Stream:        true,
		StreamOptions: map[string]any{"include_usage": true},
	}
	if sys := req.SystemText(); sys != "" {
		w.Messages = append(w.Messages, wireMessage{Role: "system", Content: strPtr(sys)})
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: string(m.Role), Content: strPtr(m.Content), ToolCallID: m.ToolCallID}
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
			if m.Content == "" {
				wm.Content = nil
			}
			for _, tc := range m.ToolCalls {
				args := string(tc.Arguments)
				if args == "" {
					args = "{}"
				}
				wm.ToolCalls = append(wm.ToolCalls, wireToolCall{ID: tc.ID, Type: "function", Function: wireFunction{Name: tc.Name, Arguments: args}})
			}
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{Type: "function", Function: wireToolFuncDef{Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
	}
	return w
}

// Stream implements llm.Client.
func (c *Client) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event)) (llm.Response, error) {
	body, err := json.Marshal(buildRequest(req))
	if err != nil {
		return llm.Response{}, fmt.Errorf("encode request: %w", err)
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return llm.Response{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	for k, v := range c.Headers {
		httpReq.Header.Set(k, v)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(httpReq)
	if err != nil {
		return llm.Response{}, fmt.Errorf("%s: send request: %w", c.label(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		// The limiter's own wait, when it sends one, beats the local backoff.
		after, _ := llm.ParseRetryAfter(resp.Header.Get("Retry-After"))
		return llm.Response{}, &llm.APIError{Provider: c.label(), Status: resp.StatusCode,
			Body: strings.TrimSpace(string(data)), RetryAfter: after}
	}
	return c.readStream(ctx, resp.Body, onEvent)
}

func (c *Client) label() string {
	if c.Name != "" {
		return c.Name
	}
	return "openai-compatible"
}

type pendingCall struct {
	id, name string
	args     strings.Builder
}

func (c *Client) readStream(ctx context.Context, r io.Reader, onEvent func(llm.Event)) (llm.Response, error) {
	emit := func(e llm.Event) {
		if onEvent != nil {
			onEvent(e)
		}
	}
	var (
		text      strings.Builder
		scrub     = llm.NewThinkScrubber()
		calls     = map[int]*pendingCall{}
		finish    string
		usage     llm.Usage
		sawDone   bool
		sawFinish bool
	)
	addText := func(delta string) {
		visible, hidden := scrub.Feed(delta)
		if hidden != "" {
			emit(llm.Event{Kind: llm.EventReasoning, Text: hidden})
		}
		if visible != "" {
			text.WriteString(visible)
			emit(llm.Event{Kind: llm.EventText, Text: visible})
		}
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			sawDone = true
			break
		}
		if data == "" {
			continue
		}
		var chunk wireChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return llm.Response{}, fmt.Errorf("%s: decode stream chunk: %w", c.label(), err)
		}
		if chunk.Error != nil {
			return llm.Response{}, &llm.APIError{Provider: c.label(), Status: errorStatus(chunk.Error.Code), Body: chunk.Error.Message}
		}
		if chunk.Usage != nil {
			usage.InputTokens = chunk.Usage.PromptTokens
			usage.OutputTokens = chunk.Usage.CompletionTokens
			if d := chunk.Usage.PromptTokensDetails; d != nil {
				usage.CacheReadTokens = d.CachedTokens
			}
		}
		for _, ch := range chunk.Choices {
			if r := ch.Delta.ReasoningContent + ch.Delta.Reasoning; r != "" {
				emit(llm.Event{Kind: llm.EventReasoning, Text: r})
			}
			if ch.Delta.Content != "" {
				addText(ch.Delta.Content)
			}
			for i, tc := range ch.Delta.ToolCalls {
				idx := i
				if tc.Index != nil {
					idx = *tc.Index
				}
				p := calls[idx]
				if p == nil {
					p = &pendingCall{}
					calls[idx] = p
				}
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name += tc.Function.Name
				}
				p.args.WriteString(tc.Function.Arguments)
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
				sawFinish = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return llm.Response{}, ctx.Err()
		}
		return llm.Response{}, fmt.Errorf("%s: read stream: %w: %w", c.label(), llm.ErrStreamInterrupted, err)
	}
	if ctx.Err() != nil {
		return llm.Response{}, ctx.Err()
	}
	if !sawDone && !sawFinish {
		return llm.Response{}, fmt.Errorf("%s: %w", c.label(), llm.ErrStreamInterrupted)
	}
	if tail := scrub.Flush(); tail != "" {
		text.WriteString(tail)
		emit(llm.Event{Kind: llm.EventText, Text: tail})
	}

	msg := llm.Message{Role: llm.RoleAssistant, Content: text.String()}
	indexes := make([]int, 0, len(calls))
	for i := range calls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		p := calls[i]
		args := strings.TrimSpace(p.args.String())
		if args == "" {
			args = "{}"
		}
		tc := llm.ToolCall{ID: p.id, Name: p.name, Arguments: json.RawMessage(args)}
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d", i)
		}
		msg.ToolCalls = append(msg.ToolCalls, tc)
		emit(llm.Event{Kind: llm.EventToolCall, ToolCall: &tc})
	}
	return llm.Response{Message: msg, StopReason: stopReason(finish, len(msg.ToolCalls) > 0), Usage: usage}, nil
}

func stopReason(finish string, hasTools bool) string {
	switch {
	case hasTools || finish == "tool_calls" || finish == "function_call":
		return llm.StopToolUse
	case finish == "length":
		return llm.StopMaxTokens
	default:
		return llm.StopEnd
	}
}

// errorStatus maps an in-stream error code to an HTTP-like status so retry
// classification works; unknown codes count as a server error.
func errorStatus(code any) int {
	switch v := code.(type) {
	case float64:
		return int(v)
	case string:
		if v == "rate_limit_exceeded" {
			return http.StatusTooManyRequests
		}
	}
	return http.StatusBadGateway
}
