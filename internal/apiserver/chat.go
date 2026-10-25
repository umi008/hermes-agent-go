package apiserver

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

const (
	// maxBodyBytes caps one request body. Real clients send whole
	// conversations, so the cap is generous; it only stops unbounded reads.
	maxBodyBytes = 16 << 20
	// sessionSource is the session source recorded in the store, as upstream
	// "api_server" sessions carry an API origin.
	sessionSource = "api"
	// endReason closes the per-request session once the request finishes,
	// including when the client disconnects.
	endReason = "api_close"
)

// handleChat answers POST /v1/chat/completions.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if s.backend == nil {
		writeError(w, http.StatusInternalServerError, "server_error", "internal_error",
			"The API server has no backend.")
		return
	}
	release, ok := s.admit(w)
	if !ok {
		return
	}
	defer release()
	input, stream, bad := parseChatRequest(w, r)
	if bad != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", bad.code, bad.message)
		return
	}
	if stream {
		s.streamChat(w, r, input)
		return
	}
	s.completeChat(w, r, input)
}

// chatRequest is the part of the OpenAI request the port acts on. Every other
// field (temperature, tools, top_p, ...) is tolerated and ignored.
type chatRequest struct {
	Model    string          `json:"model"`
	Messages json.RawMessage `json:"messages"`
	Stream   json.RawMessage `json:"stream"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// badRequest is a client error the handler answers with 400.
type badRequest struct {
	code    string
	message string
}

// parseChatRequest validates the request and returns the last user message,
// which is the one turn this server runs, plus the stream flag.
func parseChatRequest(w http.ResponseWriter, r *http.Request) (input string, stream bool, bad *badRequest) {
	var req chatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		return "", false, &badRequest{"invalid_json", "Invalid JSON in request body"}
	}
	var messages []wireMessage
	if len(req.Messages) == 0 || json.Unmarshal(req.Messages, &messages) != nil || len(messages) == 0 {
		return "", false, &badRequest{"missing_messages", "Missing or invalid 'messages' field"}
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		if text := contentText(messages[i].Content); strings.TrimSpace(text) != "" {
			return text, coerceBool(req.Stream, false), nil
		}
		break
	}
	return "", false, &badRequest{"no_user_message", "No user message found in messages"}
}

// contentText flattens OpenAI message content: a plain string, or an array of
// parts of which only the text survives (text, input_text, output_text). This
// port does not take images.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Text != "" && textPart(part.Type) {
			out = append(out, part.Text)
		}
	}
	return strings.Join(out, "\n")
}

// textPart is upstream _TEXT_PART_TYPES, plus an untyped part.
func textPart(typ string) bool {
	switch typ {
	case "", "text", "input_text", "output_text":
		return true
	}
	return false
}

// coerceBool normalizes boolean-like payload values, as upstream
// _coerce_request_bool: some frontends send "false", which is truthy as a Go
// string, so only explicit bool-ish scalars count.
func coerceBool(raw json.RawMessage, def bool) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return def
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
		return def
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n != 0
	}
	return def
}

// completeChat runs one non-streaming turn: one session, one user message, and
// a whole chat.completion object.
func (s *Server) completeChat(w http.ResponseWriter, r *http.Request, input string) {
	ctx := r.Context()
	sess := s.backend.NewSession(ctx, SessionOptions{Source: sessionSource})
	if sess == nil {
		writeError(w, http.StatusInternalServerError, "server_error", "internal_error",
			"The API server could not open a session.")
		return
	}
	defer sess.End(endReason)
	if id := sess.ID(); id != "" {
		w.Header().Set("X-Hermes-Session-Id", id)
	}
	reply, err := sess.Run(ctx, input)
	if err != nil {
		s.logger.Warn("chat completion failed", "err", err)
		if ctx.Err() != nil {
			return // the client is gone; there is nobody to answer
		}
		writeError(w, http.StatusInternalServerError, "server_error", "internal_error", "The agent run failed.")
		return
	}
	writeJSON(w, http.StatusOK, completionResponse{
		ID:      newCompletionID(),
		Object:  "chat.completion",
		Created: s.now().Unix(),
		Model:   s.backend.Model(),
		Choices: []completionChoice{{
			Message:      completionMessage{Role: "assistant", Content: reply.Text},
			FinishReason: finishReason(reply.Stop),
		}},
		Usage: usageOf(reply.Usage),
	})
}

// streamChat runs one streaming turn: the session writes its text deltas into
// the SSE writer, then the stream closes with a finish chunk and [DONE].
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, input string) {
	ctx := r.Context()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	out := newSSEWriter(w, newCompletionID(), s.backend.Model(), s.now().Unix())
	sess := s.backend.NewSession(ctx, SessionOptions{Source: sessionSource, Out: out})
	if sess == nil {
		writeError(w, http.StatusInternalServerError, "server_error", "internal_error",
			"The API server could not open a session.")
		return
	}
	defer sess.End(endReason)
	if id := sess.ID(); id != "" {
		h.Set("X-Hermes-Session-Id", id)
	}
	out.start()
	reply, err := sess.Run(ctx, input)
	switch {
	case err != nil && ctx.Err() != nil:
		return // the client disconnected; the deferred End still closes the session
	case err != nil:
		s.logger.Warn("chat completion stream failed", "err", err)
		out.finish("error", llm.Usage{}, true)
	default:
		out.finish(finishReason(reply.Stop), reply.Usage, false)
	}
	out.done()
}

// finishReason maps the agent's stop reason to OpenAI's vocabulary.
func finishReason(stop string) string {
	if stop == llm.StopMaxTokens {
		return "length"
	}
	return "stop"
}

// completionResponse is OpenAI's non-streaming chat.completion object.
type completionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   wireUsage          `json:"usage"`
}

type completionChoice struct {
	Index        int               `json:"index"`
	Message      completionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type completionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// wireUsage is OpenAI's usage block.
type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func usageOf(u llm.Usage) wireUsage {
	return wireUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
}

// newCompletionID mirrors upstream's chatcmpl-<hex> ids.
func newCompletionID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// sseWriter turns the session's text deltas into chat.completion.chunk frames.
// A failed write is not an error the turn acts on: a gone client surfaces
// through the request context, which cancels the run.
type sseWriter struct {
	w       http.ResponseWriter
	flush   func()
	id      string
	model   string
	created int64
}

func newSSEWriter(w http.ResponseWriter, id, model string, created int64) *sseWriter {
	sw := &sseWriter{w: w, id: id, model: model, created: created}
	if f, ok := w.(http.Flusher); ok {
		sw.flush = f.Flush
	}
	return sw
}

// Write implements io.Writer: every delta becomes one content chunk.
func (s *sseWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := s.emit(s.envelope(deltaBody{Content: string(p)}, nil)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// start opens the stream with the assistant role chunk.
func (s *sseWriter) start() {
	_ = s.emit(s.envelope(deltaBody{Role: "assistant"}, nil))
}

// finish closes the stream with the empty-delta chunk carrying the
// finish_reason and the usage.
func (s *sseWriter) finish(reason string, usage llm.Usage, failed bool) {
	env := s.envelope(deltaBody{}, &reason)
	u := usageOf(usage)
	env.Usage = &u
	if failed {
		env.Error = &streamError{Message: "The agent run failed.", Type: "agent_error"}
	}
	_ = s.emit(env)
}

// done terminates the stream, as every OpenAI SSE client expects.
func (s *sseWriter) done() {
	_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	s.flushNow()
}

func (s *sseWriter) envelope(delta deltaBody, finish *string) chunkEnvelope {
	return chunkEnvelope{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []chunkChoice{{Delta: delta, FinishReason: finish}},
	}
}

func (s *sseWriter) emit(env chunkEnvelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "data: %s\n\n", data)
	s.flushNow()
	return err
}

func (s *sseWriter) flushNow() {
	if s.flush != nil {
		s.flush()
	}
}

// chunkEnvelope is one OpenAI chat.completion.chunk frame.
type chunkEnvelope struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *wireUsage    `json:"usage,omitempty"`
	Error   *streamError  `json:"error,omitempty"`
}

type chunkChoice struct {
	Index        int       `json:"index"`
	Delta        deltaBody `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type deltaBody struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// streamError rides on the finish chunk when the run failed after the stream
// had already started and the status could no longer change.
type streamError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}
