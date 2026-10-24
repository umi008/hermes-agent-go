package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/config"
)

// deltasModel answers every request with the given text, one SSE delta per
// part, so the server's streaming path really reassembles several frames.
func deltasModel(t *testing.T, parts ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, part := range parts {
			chunk, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{"delta": map[string]string{"content": part}}},
			})
			_, _ = io.WriteString(w, "data: "+string(chunk)+"\n\n")
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withAPIKey adds the api_server config section to a model config.
func withAPIKey(configYAML, key string) string {
	return configYAML + "api_server:\n  api_key: " + key + "\n"
}

func apiTestLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// apiTestServer starts the OpenAI-compatible handler over an isolated chat
// runtime, the way `hermes-go serve` does, and returns its base URL.
func apiTestServer(t *testing.T, f *fakeEnv, key string) *httptest.Server {
	t.Helper()
	return apiTestServerWith(t, f, key, apiTestLogger(io.Discard, slog.LevelWarn))
}

func apiTestServerWith(t *testing.T, f *fakeEnv, key string, logger *slog.Logger) *httptest.Server {
	t.Helper()
	rt, err := newChatRuntime(context.Background(), chatFlags{}, f.env(), logger, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.close)
	srv := httptest.NewServer(newAPIServer(rt, "127.0.0.1:0", key, logger).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// apiDo sends one request; an empty body sends none and an empty token omits
// the Authorization header.
func apiDo(t *testing.T, srv *httptest.Server, method, path, token, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestAPIListModels(t *testing.T) {
	model := deltasModel(t, "unused")
	f := newHome(t, modelConfig(model.URL))
	srv := apiTestServer(t, f, "")

	resp := apiDo(t, srv, "GET", "/v1/models", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if got.Object != "list" || len(got.Data) != 1 {
		t.Fatalf("models = %+v, want one entry in a list", got)
	}
	entry := got.Data[0]
	if entry.ID != "tiny" || entry.Object != "model" || entry.OwnedBy != "hermes" || entry.Created == 0 {
		t.Errorf("model entry = %+v, want the configured model tiny", entry)
	}
}

func TestAPIChatCompletion(t *testing.T) {
	model := deltasModel(t, "hello", " there")
	f := newHome(t, withAPIKey(modelConfig(model.URL), "sekrit"))
	srv := apiTestServer(t, f, "sekrit")

	body := `{"model":"tiny","temperature":0.2,"messages":[` +
		`{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`
	resp := apiDo(t, srv, "POST", "/v1/chat/completions", "sekrit", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/chat/completions status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q, want application/json", ct)
	}
	sid := resp.Header.Get("X-Hermes-Session-Id")
	if sid == "" {
		t.Fatal("response carries no X-Hermes-Session-Id")
	}
	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if !strings.HasPrefix(got.ID, "chatcmpl-") || got.Object != "chat.completion" || got.Model != "tiny" {
		t.Errorf("completion envelope = %+v", got)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(got.Choices))
	}
	choice := got.Choices[0]
	if choice.Message.Role != "assistant" || choice.Message.Content != "hello there" || choice.FinishReason != "stop" {
		t.Errorf("choice = %+v, want assistant/hello there/stop", choice)
	}
	if got.Usage.PromptTokens != 5 || got.Usage.CompletionTokens != 2 || got.Usage.TotalTokens != 7 {
		t.Errorf("usage = %+v, want 5/2/7", got.Usage)
	}

	// One request, one session: it is persisted and closed with api_close.
	all, st := sessions(t, f)
	if len(all) != 1 {
		t.Fatalf("sessions = %d, want 1", len(all))
	}
	if all[0].ID != sid || all[0].Source != "api" || all[0].EndReason != "api_close" {
		t.Errorf("session = %+v, want %s source api end_reason api_close", all[0], sid)
	}
	if contents := activeContents(t, st, sid); contents != "hi|hello there" {
		t.Errorf("session messages = %q, want %q", contents, "hi|hello there")
	}
}

func TestAPIStreamingChatCompletion(t *testing.T) {
	model := deltasModel(t, "hel", "lo", "!")
	f := newHome(t, modelConfig(model.URL))
	srv := apiTestServer(t, f, "")

	body := `{"model":"tiny","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp := apiDo(t, srv, "POST", "/v1/chat/completions", "", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("streaming status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content type = %q, want text/event-stream", ct)
	}
	sid := resp.Header.Get("X-Hermes-Session-Id")
	if sid == "" {
		t.Fatal("streaming response carries no X-Hermes-Session-Id")
	}

	var (
		content       strings.Builder
		contentFrames int
		finish        string
		done          bool
		afterDone     bool
	)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		if done {
			afterDone = true
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("decode chunk %q: %v", data, err)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("chunk object = %q, want chat.completion.chunk", chunk.Object)
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				content.WriteString(c.Delta.Content)
				contentFrames++
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !done {
		t.Fatal("stream did not end with data: [DONE]")
	}
	if afterDone {
		t.Error("stream sent chunks after [DONE]")
	}
	if contentFrames != len([]string{"hel", "lo", "!"}) {
		t.Errorf("content frames = %d, want 3", contentFrames)
	}
	if got := content.String(); got != "hello!" {
		t.Errorf("reassembled content = %q, want %q", got, "hello!")
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop", finish)
	}

	all, _ := sessions(t, f)
	if len(all) != 1 || all[0].ID != sid || all[0].EndReason != "api_close" {
		t.Fatalf("sessions = %+v, want one session %s closed with api_close", all, sid)
	}
}

func TestAPIAuth(t *testing.T) {
	model, seen := echoModel(t)
	f := newHome(t, withAPIKey(modelConfig(model.URL), "sekrit"))
	srv := apiTestServer(t, f, "sekrit")
	body := `{"messages":[{"role":"user","content":"hi"}]}`

	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "nope", http.StatusUnauthorized},
		{"right token", "sekrit", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := apiDo(t, srv, "POST", "/v1/chat/completions", tc.token, body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want != http.StatusUnauthorized {
				return
			}
			var env struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if env.Error.Message == "" || env.Error.Type == "" || env.Error.Code == "" {
				t.Errorf("error envelope = %+v, want message, type and code", env.Error)
			}
		})
	}

	if resp := apiDo(t, srv, "GET", "/v1/models", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /v1/models status = %d, want 401", resp.StatusCode)
	}
	if calls := len(seen()); calls != 1 {
		t.Errorf("model calls = %d, want 1: unauthenticated requests must not run a turn", calls)
	}
}

func TestAPIBadRequest(t *testing.T) {
	model, seen := echoModel(t)
	f := newHome(t, modelConfig(model.URL))
	srv := apiTestServer(t, f, "")

	for _, tc := range []struct{ name, body string }{
		{"malformed json", `{"messages":`},
		{"not an object", `[]`},
		{"missing messages", `{"model":"tiny"}`},
		{"no user message", `{"messages":[{"role":"system","content":"be brief"}]}`},
		{"blank user message", `{"messages":[{"role":"user","content":"   "}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := apiDo(t, srv, "POST", "/v1/chat/completions", "", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var env struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if env.Error.Message == "" || env.Error.Type != "invalid_request_error" || env.Error.Code == "" {
				t.Errorf("error envelope = %+v, want an invalid_request_error with message and code", env.Error)
			}
		})
	}

	if calls := len(seen()); calls != 0 {
		t.Errorf("model calls = %d, want 0: a rejected request must not run a turn", calls)
	}
	if all, _ := sessions(t, f); len(all) != 0 {
		t.Errorf("rejected requests created %d sessions, want 0", len(all))
	}
}

func TestAPIConcurrentSessions(t *testing.T) {
	model, _ := echoModel(t)
	f := newHome(t, modelConfig(model.URL))
	srv := apiTestServer(t, f, "")

	prompts := []string{"alpha", "beta"}
	ids := make([]string, len(prompts))
	replies := make([]string, len(prompts))
	var wg sync.WaitGroup
	for i, prompt := range prompts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"messages":[{"role":"user","content":"` + prompt + `"}]}`
			req, err := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
			if err != nil {
				t.Errorf("request %q: %v", prompt, err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Errorf("post %q: %v", prompt, err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status for %q = %d, want 200", prompt, resp.StatusCode)
				return
			}
			ids[i] = resp.Header.Get("X-Hermes-Session-Id")
			var got struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Errorf("decode %q: %v", prompt, err)
				return
			}
			if len(got.Choices) == 1 {
				replies[i] = got.Choices[0].Message.Content
			}
		}()
	}
	wg.Wait()

	if ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Fatalf("session ids = %q, %q; want two distinct ids", ids[0], ids[1])
	}
	for i, prompt := range prompts {
		if want := "re: " + prompt; replies[i] != want {
			t.Errorf("reply for %q = %q, want %q", prompt, replies[i], want)
		}
	}

	all, st := sessions(t, f)
	if len(all) != 2 {
		t.Fatalf("sessions = %d, want 2", len(all))
	}
	for i, id := range ids {
		found := false
		for _, sess := range all {
			if sess.ID == id {
				found = true
				if sess.EndReason != "api_close" {
					t.Errorf("session %s end_reason = %q, want api_close", id, sess.EndReason)
				}
			}
		}
		if !found {
			t.Errorf("session %s was not persisted", id)
		}
		if contents, want := activeContents(t, st, id), prompts[i]+"|re: "+prompts[i]; contents != want {
			t.Errorf("session %s messages = %q, want %q", id, contents, want)
		}
	}
}

// TestAPIRequestLogging checks the debug request log: method, path, status
// and duration, and never the message content or the token.
func TestAPIRequestLogging(t *testing.T) {
	model := deltasModel(t, "pong")
	f := newHome(t, withAPIKey(modelConfig(model.URL), "topsecret"))
	var logs lockedBuffer
	srv := apiTestServerWith(t, f, "topsecret", apiTestLogger(&logs, slog.LevelDebug))

	body := `{"messages":[{"role":"user","content":"zebra-question"}]}`
	resp := apiDo(t, srv, "POST", "/v1/chat/completions", "topsecret", body)
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	out := logs.String()
	for _, want := range []string{`method=POST`, `path=/v1/chat/completions`, `status=200`, "duration="} {
		if !strings.Contains(out, want) {
			t.Errorf("request log %q does not contain %q", out, want)
		}
	}
	for _, secret := range []string{"zebra-question", "pong", "topsecret"} {
		if strings.Contains(out, secret) {
			t.Errorf("request log leaked %q: %s", secret, out)
		}
	}
}

func TestAPIKeyResolution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("api_server:\n  api_key: from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, config.Options{})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := config.Load(filepath.Join(dir, "missing.yaml"), config.Options{})
	if err != nil {
		t.Fatal(err)
	}
	none := func(string) (string, bool) { return "", false }
	env := func(name string) (string, bool) {
		if name == "HERMES_API_KEY" {
			return "from-env", true
		}
		return "", false
	}

	if got := apiKey(cfg, none); got != "from-file" {
		t.Errorf("config key = %q, want from-file", got)
	}
	if got := apiKey(cfg, env); got != "from-file" {
		t.Errorf("config key with env set = %q, want from-file (config wins)", got)
	}
	if got := apiKey(empty, env); got != "from-env" {
		t.Errorf("env key = %q, want from-env", got)
	}
	if got := apiKey(empty, none); got != "" {
		t.Errorf("no key = %q, want empty", got)
	}
}

func TestServeAuthPolicy(t *testing.T) {
	var logs lockedBuffer
	logger := apiTestLogger(&logs, slog.LevelWarn)

	if err := serveAuth("0.0.0.0:8787", "", logger); err == nil {
		t.Error("serveAuth allowed a non-loopback address without an API key")
	}
	if err := serveAuth("[::]:8787", "", logger); err == nil {
		t.Error("serveAuth allowed a wildcard IPv6 address without an API key")
	}
	if err := serveAuth("127.0.0.1:8787", "", logger); err != nil {
		t.Errorf("serveAuth refused a loopback address without an API key: %v", err)
	}
	if !strings.Contains(logs.String(), "without an API key") {
		t.Errorf("loopback start without a key did not warn: %q", logs.String())
	}

	logs = lockedBuffer{}
	if err := serveAuth("0.0.0.0:8787", "sekrit", logger); err != nil {
		t.Errorf("serveAuth refused an authenticated non-loopback address: %v", err)
	}
	if strings.Contains(logs.String(), "without an API key") {
		t.Errorf("authenticated start warned about a missing key: %q", logs.String())
	}
}

func TestServeRefusesNonLoopbackWithoutKey(t *testing.T) {
	model := deltasModel(t, "unused")
	f := newHome(t, modelConfig(model.URL))

	code := serve(context.Background(), []string{"--addr", "0.0.0.0:0"}, f.env())
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(f.stderr.String(), "API key") {
		t.Errorf("stderr = %q, want a refusal mentioning the API key", f.stderr.String())
	}
}

func TestServeFlags(t *testing.T) {
	f, err := parseServe(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if f.addr != "127.0.0.1:8787" {
		t.Errorf("default addr = %q, want 127.0.0.1:8787", f.addr)
	}
	f, err = parseServe([]string{"--addr", "127.0.0.1:9", "-p", "work", "-m", "big", "--provider", "custom", "--toolsets", "web", "-v"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if f.addr != "127.0.0.1:9" || f.profile != "work" || f.model != "big" || f.provider != "custom" ||
		f.toolsets != "web" || !f.verbose {
		t.Errorf("parsed flags = %+v", f)
	}
}
