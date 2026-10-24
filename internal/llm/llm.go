// Package llm defines provider-neutral chat types and the Client interface
// that the agent loop talks to. Provider packages adapt these types to a
// concrete wire format.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Role is a message author.
type Role string

// Message roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one conversation turn. System text travels in Request.System,
// never as a message.
type Message struct {
	Role    Role
	Content string
	// ToolCalls is set on assistant messages that call tools.
	ToolCalls []ToolCall
	// ToolCallID and IsError are set on tool messages.
	ToolCallID string
	IsError    bool
	// Native is the provider's own form of an assistant reply, kept so the
	// same provider can replay it verbatim (Anthropic thinking blocks carry
	// signatures that must round-trip). Other providers ignore it.
	Native any
}

// ToolCall is a model request to run a tool.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolSpec describes a tool to the model. Parameters is a JSON Schema object.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Request is one model call.
type Request struct {
	Model string
	// System holds the system prompt in tiers, most stable first. Providers
	// that cache prompts place a breakpoint after the first tier and after the
	// last one; others join the tiers with a blank line.
	System    []string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int
	// CacheTTL is "" for the provider default or "1h".
	CacheTTL string
}

// SystemText joins the system tiers the way upstream does.
func (r Request) SystemText() string {
	out := ""
	for _, part := range r.System {
		if part == "" {
			continue
		}
		if out != "" {
			out += "\n\n"
		}
		out += part
	}
	return out
}

// EventKind classifies a stream event.
type EventKind int

// Stream event kinds.
const (
	EventText EventKind = iota + 1
	EventReasoning
	EventToolCall
)

// Event is an incremental piece of a streamed response.
type Event struct {
	Kind EventKind
	// Text is the delta for EventText and EventReasoning.
	Text string
	// ToolCall is set for EventToolCall once a call is complete.
	ToolCall *ToolCall
}

// Usage reports token counts. Cache fields stay zero when unsupported.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// Add accumulates u2 into u.
func (u *Usage) Add(u2 Usage) {
	u.InputTokens += u2.InputTokens
	u.OutputTokens += u2.OutputTokens
	u.CacheReadTokens += u2.CacheReadTokens
	u.CacheWriteTokens += u2.CacheWriteTokens
}

// Stop reasons, normalized across providers.
const (
	StopEnd       = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
)

// Response is a complete model reply.
type Response struct {
	Message    Message
	StopReason string
	Usage      Usage
}

// Client streams one model call. onEvent runs on the calling goroutine, in
// order, and may be nil.
type Client interface {
	Stream(ctx context.Context, req Request, onEvent func(Event)) (Response, error)
}

// APIError is an HTTP-level failure from a provider.
type APIError struct {
	Provider string
	Status   int
	Body     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Status, e.Body)
}

// Retryable reports whether the request may succeed if sent again.
func (e *APIError) Retryable() bool {
	return e.Status == 408 || e.Status == 409 || e.Status == 429 || e.Status >= 500
}

// IsRetryable reports whether err is a transient provider failure. Context
// cancellation is never retryable.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	var transient interface{ Temporary() bool }
	if errors.As(err, &transient) {
		return transient.Temporary()
	}
	return errors.Is(err, ErrStreamInterrupted)
}

// ErrStreamInterrupted reports a stream that ended before its terminal event.
var ErrStreamInterrupted = errors.New("stream ended before completion")
