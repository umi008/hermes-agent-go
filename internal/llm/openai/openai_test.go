package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

func sse(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("data: " + l + "\n\n")
	}
	return b.String()
}

func server(t *testing.T, status int, body string, check func(*http.Request, map[string]any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		if check != nil {
			check(r, got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStreamText(t *testing.T) {
	body := sse(
		`{"choices":[{"delta":{"content":"<think>hmm</think>Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`,
		`[DONE]`,
	)
	srv := server(t, 200, body, func(r *http.Request, got map[string]any) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		msgs := got["messages"].([]any)
		first := msgs[0].(map[string]any)
		if first["role"] != "system" || first["content"] != "a\n\nb" {
			t.Errorf("system message = %v", first)
		}
		if got["stream"] != true {
			t.Error("stream not set")
		}
	})
	c := &Client{BaseURL: srv.URL + "/v1/", APIKey: "k"}
	var text, reasoning strings.Builder
	resp, err := c.Stream(context.Background(), llm.Request{
		Model:    "m",
		System:   []string{"a", "b"},
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, func(e llm.Event) {
		switch e.Kind {
		case llm.EventText:
			text.WriteString(e.Text)
		case llm.EventReasoning:
			reasoning.WriteString(e.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "Hello" || text.String() != "Hello" || reasoning.String() != "hmm" {
		t.Fatalf("content=%q text=%q reasoning=%q", resp.Message.Content, text.String(), reasoning.String())
	}
	if resp.StopReason != llm.StopEnd {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if resp.Usage != (llm.Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 4}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestStreamToolCalls(t *testing.T) {
	body := sse(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"x\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"ls","arguments":""}}]},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	)
	srv := server(t, 200, body, func(_ *http.Request, got map[string]any) {
		tools := got["tools"].([]any)
		fn := tools[0].(map[string]any)["function"].(map[string]any)
		if fn["name"] != "read_file" {
			t.Errorf("tools = %v", tools)
		}
		msgs := got["messages"].([]any)
		asst := msgs[1].(map[string]any)
		if asst["content"] != nil {
			t.Errorf("assistant tool-call content should be null, got %v", asst["content"])
		}
		tool := msgs[2].(map[string]any)
		if tool["role"] != "tool" || tool["tool_call_id"] != "old" {
			t.Errorf("tool msg = %v", tool)
		}
	})
	c := &Client{BaseURL: srv.URL}
	var calls []llm.ToolCall
	resp, err := c.Stream(context.Background(), llm.Request{
		Model: "m",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "go"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "old", Name: "ls"}}},
			{Role: llm.RoleTool, ToolCallID: "old", Content: "a.txt"},
		},
		Tools: []llm.ToolSpec{{Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}, func(e llm.Event) {
		if e.Kind == llm.EventToolCall {
			calls = append(calls, *e.ToolCall)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopToolUse || len(resp.Message.ToolCalls) != 2 || len(calls) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "c1" || tc.Name != "read_file" || string(tc.Arguments) != `{"path":"x"}` {
		t.Errorf("call 0 = %+v (%s)", tc, tc.Arguments)
	}
	if string(resp.Message.ToolCalls[1].Arguments) != "{}" {
		t.Errorf("empty args should become {}")
	}
}

func TestStreamErrors(t *testing.T) {
	t.Run("http error is APIError", func(t *testing.T) {
		srv := server(t, 429, `{"error":"slow down"}`, nil)
		_, err := (&Client{BaseURL: srv.URL, Name: "or"}).Stream(context.Background(), llm.Request{Model: "m"}, nil)
		var apiErr *llm.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 429 || !llm.IsRetryable(err) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("retry-after is surfaced", func(t *testing.T) {
		// A rate limiter that says how long to wait: the agent must see it.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "12")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"slow down"}`))
		}))
		defer srv.Close()
		_, err := (&Client{BaseURL: srv.URL, Name: "or"}).Stream(context.Background(), llm.Request{Model: "m"}, nil)
		var apiErr *llm.APIError
		if !errors.As(err, &apiErr) || apiErr.RetryAfter != 12*time.Second {
			t.Fatalf("err = %v, want the provider's Retry-After", err)
		}
	})
	t.Run("truncated stream is retryable", func(t *testing.T) {
		srv := server(t, 200, sse(`{"choices":[{"delta":{"content":"par"}}]}`), nil)
		_, err := (&Client{BaseURL: srv.URL}).Stream(context.Background(), llm.Request{Model: "m"}, nil)
		if !errors.Is(err, llm.ErrStreamInterrupted) || !llm.IsRetryable(err) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("in-stream error", func(t *testing.T) {
		srv := server(t, 200, sse(`{"error":{"message":"overloaded","code":503}}`), nil)
		_, err := (&Client{BaseURL: srv.URL}).Stream(context.Background(), llm.Request{Model: "m"}, nil)
		var apiErr *llm.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 503 {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		srv := server(t, 200, sse(`[DONE]`), nil)
		_, err := (&Client{BaseURL: srv.URL}).Stream(ctx, llm.Request{Model: "m"}, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}
