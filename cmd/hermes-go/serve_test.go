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
	"strconv"
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
	rt, err := newChatRuntime(context.Background(), chatFlags{}, f.env(), logger, noHumans)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.close)
	srv := httptest.NewServer(newAPIServer(rt, "127.0.0.1:0", key, logger, nil).Handler())
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

// TestAPIResponsesChainContinuesSession runs /v1/responses over the real
// runtime: a chained request replays the stored transcript to the model,
// continues the same stored session and layers its instructions on the
// system prompt for that turn only.
func TestAPIResponsesChainContinuesSession(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"reply `+strconv.Itoa(n)+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(model.Close)
	f := newHome(t, modelConfig(model.URL))
	srv := apiTestServer(t, f, "")

	resp := apiDo(t, srv, "POST", "/v1/responses", "", `{"input":"alpha"}`)
	var first struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&first); err != nil || first.Status != "completed" {
		t.Fatalf("first response %+v %v", first, err)
	}
	sid := resp.Header.Get("X-Hermes-Session-Id")
	resp = apiDo(t, srv, "POST", "/v1/responses", "", `{"input":"beta","instructions":"be terse","previous_response_id":"`+first.ID+`"}`)
	if resp.StatusCode != http.StatusOK || sid == "" || resp.Header.Get("X-Hermes-Session-Id") != sid {
		t.Fatalf("chained status %d session %q, want %q", resp.StatusCode, resp.Header.Get("X-Hermes-Session-Id"), sid)
	}
	mu.Lock()
	second := bodies[1]
	mu.Unlock()
	for _, want := range []string{`"alpha"`, `"reply 1"`, `"beta"`, "be terse"} {
		if !strings.Contains(second, want) {
			t.Errorf("chained model request lacks %s: %s", want, second)
		}
	}
	if strings.Contains(bodies[0], "be terse") {
		t.Error("instructions leaked into the first turn")
	}
	_, st := sessions(t, f)
	if got := activeContents(t, st, sid); got != "alpha|reply 1|beta|reply 2" {
		t.Errorf("session messages = %q", got)
	}
}

// TestAPIResponsesToolItems checks the runtime hands the turn's tool calls
// to /v1/responses: output items without streaming, item events with it.
func TestAPIResponsesToolItems(t *testing.T) {
	model, _ := toolCallModel(t, "no_such_tool", `{"a":1}`)
	f := newHome(t, modelConfig(model.URL))
	srv := apiTestServer(t, f, "")
	resp := apiDo(t, srv, "POST", "/v1/responses", "", `{"input":"go"}`)
	raw, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`"type":"function_call","status":"completed","name":"no_such_tool","arguments":"{\"a\":1}","call_id":"c1"`,
		`"type":"function_call_output","status":"completed","call_id":"c1"`, `"text":"done"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("response lacks %s: %s", want, raw)
		}
	}

	model, _ = toolCallModel(t, "no_such_tool", `{"a":1}`)
	f = newHome(t, modelConfig(model.URL))
	srv = apiTestServer(t, f, "")
	resp = apiDo(t, srv, "POST", "/v1/responses", "", `{"input":"go","stream":true}`)
	raw, _ = io.ReadAll(resp.Body)
	var events []string
	for _, line := range strings.Split(string(raw), "\n") {
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, name)
		}
	}
	want := "response.created,response.output_item.added,response.output_item.done,response.output_item.added,response.output_item.done," +
		"response.output_item.added,response.output_text.delta,response.output_text.done,response.output_item.done,response.completed"
	if strings.Join(events, ",") != want || !strings.Contains(string(raw), `"name":"no_such_tool","call_id":"c1"`) {
		t.Fatalf("events = %v\n%s", events, raw)
	}
}

func TestMaxConcurrentRuns(t *testing.T) {
	for yaml, want := range map[string]int{
		"": 10,
		"gateway:\n  api_server:\n    max_concurrent_runs: 3\n":  3,
		"gateway:\n  api_server:\n    max_concurrent_runs: 0\n":  0,
		"gateway:\n  api_server:\n    max_concurrent_runs: -4\n": 0,
	} {
		raw, err := config.Parse([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		if got := maxConcurrentRuns(config.FromMap(raw)); got != want {
			t.Errorf("%q: %d, want %d", yaml, got, want)
		}
	}
}

// runOver posts a run, waits for its event stream to close and returns the
// events and the final status.
func runOver(t *testing.T, srv *httptest.Server, body string) (events, status string) {
	t.Helper()
	resp := apiDo(t, srv, "POST", "/v1/runs", "", body)
	var accepted struct {
		RunID string `json:"run_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/runs: %d %v", resp.StatusCode, err)
	}
	raw, _ := io.ReadAll(apiDo(t, srv, "GET", "/v1/runs/"+accepted.RunID+"/events", "", "").Body)
	st, _ := io.ReadAll(apiDo(t, srv, "GET", "/v1/runs/"+accepted.RunID, "", "").Body)
	return string(raw), string(st)
}

// TestAPIRunsResumeSession checks a run with session_id replays that stored
// session and reports the served runtime.
func TestAPIRunsResumeSession(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"reply `+strconv.Itoa(n)+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(model.Close)
	f := newHome(t, modelConfig(model.URL))
	srv := apiTestServer(t, f, "")

	events, status := runOver(t, srv, `{"input":"alpha","session_id":"s-run"}`)
	if !strings.Contains(events, `"event":"message.delta"`) || !strings.Contains(events, `"event":"run.completed"`) {
		t.Fatalf("events %s", events)
	}
	for _, want := range []string{`"status":"completed"`, `"session_id":"s-run"`, `"output":"reply 1"`,
		`"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4,"cache_read_tokens":0,"cache_write_tokens":0}`,
		`"runtime":{"provider":"custom","model":"tiny","route_source":"global"}`} {
		if !strings.Contains(status, want) {
			t.Errorf("status lacks %s: %s", want, status)
		}
	}
	runOver(t, srv, `{"input":"beta","session_id":"s-run"}`)
	mu.Lock()
	second := bodies[1]
	mu.Unlock()
	for _, want := range []string{`"alpha"`, `"reply 1"`, `"beta"`} {
		if !strings.Contains(second, want) {
			t.Errorf("resumed model request lacks %s: %s", want, second)
		}
	}
	_, st := sessions(t, f)
	if got := activeContents(t, st, "s-run"); got != "alpha|reply 1|beta|reply 2" {
		t.Errorf("session messages = %q", got)
	}
}

// TestAPIRunsToolEvents checks the runtime's tool calls reach the run stream.
func TestAPIRunsToolEvents(t *testing.T) {
	model, _ := toolCallModel(t, "no_such_tool", `{"query":"q"}`)
	f := newHome(t, modelConfig(model.URL))
	srv := apiTestServer(t, f, "")
	events, _ := runOver(t, srv, `{"input":"go"}`)
	for _, want := range []string{`"event":"tool.started","run_id"`, `"tool":"no_such_tool","preview":"q","tool_call_id":"c1"`,
		`"event":"tool.completed"`, `"event":"run.completed"`} {
		if !strings.Contains(events, want) {
			t.Errorf("events lack %s: %s", want, events)
		}
	}
}

// TestAPISessionsReadTheRuntimeStore checks /api/sessions serves the same
// state.db the agent turns write.
func TestAPISessionsReadTheRuntimeStore(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"pong"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(model.Close)
	srv := apiTestServer(t, newHome(t, modelConfig(model.URL)), "")
	runOver(t, srv, `{"input":"ping","session_id":"s-api"}`)

	list, _ := io.ReadAll(apiDo(t, srv, "GET", "/api/sessions", "", "").Body)
	if !strings.Contains(string(list), `"id":"s-api"`) || !strings.Contains(string(list), `"preview":"ping"`) {
		t.Fatalf("list = %s", list)
	}
	msgs, _ := io.ReadAll(apiDo(t, srv, "GET", "/api/sessions/s-api/messages?order=oldest&limit=10", "", "").Body)
	if !strings.Contains(string(msgs), `"content":"ping"`) || !strings.Contains(string(msgs), `"content":"pong"`) {
		t.Fatalf("messages = %s", msgs)
	}
}

// TestAPISessionChatTurn checks /api/sessions/{id}/chat runs a real turn over
// the stored session: the transcript seeds it, the reply and the served
// runtime come back, and the next turn replays the conversation.
func TestAPISessionChatTurn(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(model.Close)
	f := newHome(t, modelConfig(model.URL))
	srv := apiTestServer(t, f, "")

	created := apiDo(t, srv, "POST", "/api/sessions", "", `{"id":"s-chat"}`)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create session = %d", created.StatusCode)
	}
	resp := apiDo(t, srv, "POST", "/api/sessions/s-chat/chat", "", `{"message":"ping"}`)
	body, _ := io.ReadAll(resp.Body)
	want := `{"object":"hermes.session.chat.completion","status":"completed","session_id":"s-chat",` +
		`"message":{"role":"assistant","content":"pong"},` +
		`"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4,"cache_read_tokens":0,"cache_write_tokens":0},` +
		`"runtime":{"provider":"custom","model":"tiny","route_source":"global"}}`
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != want {
		t.Fatalf("chat = %d %s", resp.StatusCode, body)
	}
	_, st := sessions(t, f)
	if got := activeContents(t, st, "s-chat"); got != "ping|pong" {
		t.Fatalf("stored turn = %q", got)
	}

	// The next turn replays the stored conversation and appends to it.
	second := apiDo(t, srv, "POST", "/api/sessions/s-chat/chat", "", `{"message":"again"}`)
	if _, err := io.Copy(io.Discard, second.Body); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	request := bodies[len(bodies)-1]
	mu.Unlock()
	for _, want := range []string{`"ping"`, `"pong"`, `"again"`} {
		if !strings.Contains(request, want) {
			t.Errorf("second turn lacks %s: %s", want, request)
		}
	}
	if got := activeContents(t, st, "s-chat"); got != "ping|pong|again|pong" {
		t.Errorf("stored turns = %q", got)
	}
}

// TestAPISessionChatModelLock checks a locked session model reaches the
// provider on this turn and on the next one, which names no model.
func TestAPISessionChatModelLock(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(model.Close)
	f := newHome(t, modelConfig(model.URL))
	srv := apiTestServer(t, f, "")

	created := apiDo(t, srv, "POST", "/api/sessions", "", `{"id":"s-lock"}`)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create session = %d", created.StatusCode)
	}
	locked := apiDo(t, srv, "POST", "/api/sessions/s-lock/chat", "",
		`{"message":"hi","require_model_lock":true,"model":"locked-model"}`)
	body, _ := io.ReadAll(locked.Body)
	if locked.StatusCode != http.StatusOK ||
		!strings.Contains(string(body), `"model":"locked-model"`) ||
		!strings.Contains(string(body), `"model_lock":"confirmed"`) {
		t.Fatalf("locked chat = %d %s", locked.StatusCode, body)
	}
	plain := apiDo(t, srv, "POST", "/api/sessions/s-lock/chat", "", `{"message":"again"}`)
	plainBody, _ := io.ReadAll(plain.Body)
	if plain.StatusCode != http.StatusOK || !strings.Contains(string(plainBody), `"route_source":"session_model_lock"`) {
		t.Fatalf("reused lock = %d %s", plain.StatusCode, plainBody)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, request := range bodies {
		if !strings.Contains(request, `"model":"locked-model"`) {
			t.Errorf("turn %d did not use the locked model: %s", i+1, request)
		}
	}
}
