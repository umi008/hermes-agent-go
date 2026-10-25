package anthropic

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

func event(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

const toolStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":12,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":7}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Reading."}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tu_1","name":"read_file","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"a.txt\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30}}

event: message_stop
data: {"type":"message_stop"}

`

func serve(t *testing.T, status int, body string, check func(*http.Request, map[string]any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var got map[string]any
		_ = json.Unmarshal(raw, &got)
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

func TestStreamToolUse(t *testing.T) {
	srv := serve(t, 200, toolStream, func(r *http.Request, got map[string]any) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "sk-ant-api-test" {
			t.Errorf("x-api-key = %q", r.Header.Get("X-Api-Key"))
		}
		if r.Header.Get("Anthropic-Version") == "" {
			t.Error("missing anthropic-version")
		}
		if got["stream"] != true {
			t.Error("stream not set")
		}
	})
	c := New(Options{BaseURL: srv.URL, APIKey: "sk-ant-api-test"})
	var text, reasoning strings.Builder
	var calls int
	resp, err := c.Stream(context.Background(), llm.Request{
		Model:    "claude-opus-5-5",
		System:   []string{"stable", "volatile"},
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "read a.txt"}},
		Tools:    []llm.ToolSpec{{Name: "read_file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}},
	}, func(e llm.Event) {
		switch e.Kind {
		case llm.EventText:
			text.WriteString(e.Text)
		case llm.EventReasoning:
			reasoning.WriteString(e.Text)
		case llm.EventToolCall:
			calls++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "Reading." || reasoning.String() != "plan" || calls != 1 {
		t.Fatalf("text=%q reasoning=%q calls=%d", text.String(), reasoning.String(), calls)
	}
	if resp.StopReason != llm.StopToolUse || resp.Message.Content != "Reading." {
		t.Fatalf("resp = %+v", resp)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "tu_1" || tc.Name != "read_file" {
		t.Fatalf("tool call = %+v", tc)
	}
	var args map[string]string
	if err := json.Unmarshal(tc.Arguments, &args); err != nil || args["path"] != "a.txt" {
		t.Fatalf("args = %s (%v)", tc.Arguments, err)
	}
	if resp.Usage.CacheReadTokens != 100 || resp.Usage.CacheWriteTokens != 7 || resp.Usage.OutputTokens != 30 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Message.Native == nil {
		t.Error("native replay form missing")
	}
}

func TestReplayKeepsThinkingAndMergesToolResults(t *testing.T) {
	srv := serve(t, 200, toolStream, nil)
	c := New(Options{BaseURL: srv.URL, APIKey: "k"})
	first, err := c.Stream(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	history := []llm.Message{
		{Role: llm.RoleUser, Content: "go"},
		first.Message,
		{Role: llm.RoleTool, ToolCallID: "tu_1", Content: "data"},
		{Role: llm.RoleTool, ToolCallID: "tu_2", Content: "boom", IsError: true},
	}
	p, err := BuildParams(llm.Request{Model: "m", Messages: history})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	var body struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("want 3 turns, got %d: %s", len(body.Messages), raw)
	}
	asst := body.Messages[1]
	if asst.Content[0]["type"] != "thinking" || asst.Content[0]["signature"] != "sig" {
		t.Errorf("thinking block not replayed: %v", asst.Content[0])
	}
	results := body.Messages[2]
	if results.Role != "user" || len(results.Content) != 2 || results.Content[1]["is_error"] != true {
		t.Errorf("tool results = %v", results)
	}
	// The stored history must not pick up cache markers from this request.
	again, _ := json.Marshal(first.Message.Native)
	if strings.Contains(string(again), "cache_control") {
		t.Error("cache marker leaked into stored native message")
	}
}

func TestCacheBreakpoints(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "1"},
		{Role: llm.RoleAssistant, Content: "2"},
		{Role: llm.RoleUser, Content: "3"},
		{Role: llm.RoleAssistant, Content: "4"},
		{Role: llm.RoleUser, Content: "5"},
	}
	tests := []struct {
		name        string
		system      []string
		ttl         string
		wantSystem  []bool
		wantMarkers []bool
	}{
		{"static prefix", []string{"a", "b", "c"}, "", []bool{true, false, true}, []bool{false, false, false, true, true}},
		{"single system", []string{"a"}, "1h", []bool{true}, []bool{false, false, true, true, true}},
		{"no system", nil, "", nil, []bool{false, true, true, true, true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := BuildParams(llm.Request{Model: "m", System: tt.system, Messages: msgs, CacheTTL: tt.ttl})
			if err != nil {
				t.Fatal(err)
			}
			total := 0
			for i, s := range p.System {
				has := s.CacheControl.Type != ""
				if has != tt.wantSystem[i] {
					t.Errorf("system[%d] marker = %v", i, has)
				}
				if has {
					total++
					if tt.ttl == "1h" && s.CacheControl.TTL != "1h" {
						t.Errorf("ttl = %q", s.CacheControl.TTL)
					}
				}
			}
			for i, m := range p.Messages {
				has := m.Content[len(m.Content)-1].GetCacheControl().Type != ""
				if has != tt.wantMarkers[i] {
					t.Errorf("message[%d] marker = %v", i, has)
				}
				if has {
					total++
				}
			}
			if total > maxBreakpoints {
				t.Errorf("%d breakpoints, limit %d", total, maxBreakpoints)
			}
		})
	}
}

func TestStreamErrors(t *testing.T) {
	t.Run("overloaded is retryable APIError", func(t *testing.T) {
		srv := serve(t, 529, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, nil)
		_, err := New(Options{BaseURL: srv.URL, APIKey: "k"}).Stream(context.Background(), llm.Request{Model: "m"}, nil)
		var apiErr *llm.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 529 || !llm.IsRetryable(err) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("retry-after is surfaced", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "9")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"busy"}}`))
		}))
		defer srv.Close()
		_, err := New(Options{BaseURL: srv.URL, APIKey: "k"}).Stream(context.Background(), llm.Request{Model: "m"}, nil)
		var apiErr *llm.APIError
		if !errors.As(err, &apiErr) || apiErr.RetryAfter != 9*time.Second {
			t.Fatalf("err = %v, want the provider's Retry-After", err)
		}
	})
	t.Run("bad request is not retryable", func(t *testing.T) {
		srv := serve(t, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`, nil)
		_, err := New(Options{BaseURL: srv.URL, APIKey: "k"}).Stream(context.Background(), llm.Request{Model: "m"}, nil)
		if err == nil || llm.IsRetryable(err) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("stream without message_stop", func(t *testing.T) {
		body := event("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
		srv := serve(t, 200, body, nil)
		_, err := New(Options{BaseURL: srv.URL, APIKey: "k"}).Stream(context.Background(), llm.Request{Model: "m"}, nil)
		if !errors.Is(err, llm.ErrStreamInterrupted) {
			t.Fatalf("err = %v", err)
		}
	})
}
