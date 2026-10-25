// Package apiserver serves the Go port's OpenAI-compatible HTTP surface:
// GET /v1/models and POST /v1/chat/completions, streaming included.
//
// The layer knows about conversations, not agents. A Backend opens one
// Session per request; the session streams its text into an io.Writer and
// reports the final reply. cmd/hermes-go backs that seam with its shared chat
// runtime, so every request owns its own session, recorder and output sink and
// never disturbs the REPL's live session.
package apiserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/state"
)

// Reply is the outcome of one turn.
type Reply struct {
	Text  string
	Usage llm.Usage
	// Stop is the normalized provider stop reason (llm.StopEnd,
	// llm.StopMaxTokens, ...), mapped to OpenAI's finish_reason.
	Stop string
	// Turn is this turn's assistant and tool messages, in order, after the
	// user message.
	Turn []llm.Message
	// History is the transcript a follow-up turn replays: the seeded history,
	// the user message and Turn, or the compacted transcript when the turn
	// compressed the context.
	History []llm.Message
	// ExitReason is upstream's turn_exit_reason, set only when the turn ended
	// without finishing (the iteration budget, a failure, an interruption).
	ExitReason string
	// Provider and Model name the runtime that served the turn, a
	// fallback's after a switch.
	Provider, Model string
}

// Session is one conversation. Run sends one user message and streams its text
// into the writer the session was opened with.
type Session interface {
	// ID is the session id echoed in X-Hermes-Session-Id.
	ID() string
	Run(ctx context.Context, input string) (Reply, error)
	// End closes the session with reason, as upstream "api_close".
	End(reason string)
}

// ToolEvent reports a tool call starting (Done false) or finishing.
type ToolEvent struct {
	Done    bool
	Call    llm.ToolCall
	Output  string
	IsError bool
}

// SessionOptions opens one conversation.
type SessionOptions struct {
	// Source is the upstream session source ("api", "api_server").
	Source string
	// Out receives the streamed text; nil discards it.
	Out io.Writer
	// ID continues the stored session with that id; empty opens a new one.
	ID string
	// Resume seeds the conversation with the stored transcript of ID when
	// History is empty.
	Resume bool
	// History seeds the conversation with prior messages.
	History []llm.Message
	// Model pins this session's model on the backend's provider, as a session
	// model lock does upstream; empty serves the backend's model.
	Model string
	// Instructions is an ephemeral system prompt layered on the core one
	// for this session only.
	Instructions string
	// OnTool, when set, sees every tool call start and finish.
	OnTool func(ToolEvent)
}

// Backend opens one session per HTTP request.
type Backend interface {
	// Model is the model id the server advertises and echoes.
	Model() string
	// NewSession opens a conversation. A nil session is a wiring error and
	// answers 500.
	NewSession(ctx context.Context, o SessionOptions) Session
}

// Config wires the server.
type Config struct {
	// Backend opens the sessions. Required.
	Backend Backend
	// APIKey is the accepted bearer token. Empty serves without
	// authentication, so the caller must then bind a loopback address only:
	// see Loopback.
	APIKey string
	// Addr is the listen address Serve binds.
	Addr string
	// Logger receives the request log. Nil discards it.
	Logger *slog.Logger
	// Now stamps the created fields. Nil uses time.Now.
	Now func() time.Time
	// Ready, when set, is called with the bound address before serving.
	Ready func(addr string)
	// MaxConcurrentRuns caps in-flight agent runs, as upstream
	// gateway.api_server.max_concurrent_runs; zero disables the cap.
	MaxConcurrentRuns int
	// Responses keeps the stored /v1/responses records. Nil keeps them in
	// memory for the server's lifetime.
	Responses *ResponseStore
	// Sessions backs /api/sessions. Nil answers those routes 503.
	Sessions *state.Store
}

// Server is the OpenAI-compatible HTTP layer.
type Server struct {
	backend Backend
	apiKey  string
	addr    string
	logger  *slog.Logger
	now     func() time.Time
	ready   func(addr string)
	handler http.Handler

	maxRuns   int
	inflight  atomic.Int64
	responses *ResponseStore
	runs      *runRegistry
	sessions  *state.Store
}

// New builds the server. Call Serve to bind it, or Handler to embed it.
func New(cfg Config) *Server {
	s := &Server{
		backend: cfg.Backend,
		apiKey:  cfg.APIKey,
		addr:    cfg.Addr,
		logger:  cfg.Logger,
		now:     cfg.Now,
		ready:   cfg.Ready,
		maxRuns: max(cfg.MaxConcurrentRuns, 0),

		responses: cfg.Responses,
		runs:      newRunRegistry(),
		sessions:  cfg.Sessions,
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.responses == nil {
		s.responses = MemoryResponseStore()
	}
	s.handler = s.routes()
	return s
}

// Handler is the HTTP surface, for embedding in another listener.
func (s *Server) Handler() http.Handler { return s.handler }

// Serve binds the configured address and serves until ctx is cancelled. It
// returns nil after a clean shutdown.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	bound := ln.Addr().String()
	if s.ready != nil {
		s.ready(bound)
	}
	s.logger.Info("api server listening", "addr", bound)
	hs := &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.runs.interruptAll()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := hs.Shutdown(shutdownCtx); err != nil {
			return err
		}
		if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/models", s.authenticated(http.HandlerFunc(s.handleModels)))
	mux.Handle("POST /v1/chat/completions", s.authenticated(http.HandlerFunc(s.handleChat)))
	mux.Handle("POST /v1/responses", s.authenticated(http.HandlerFunc(s.handleResponses)))
	mux.Handle("GET /v1/responses/{id}", s.authenticated(http.HandlerFunc(s.handleGetResponse)))
	mux.Handle("DELETE /v1/responses/{id}", s.authenticated(http.HandlerFunc(s.handleDeleteResponse)))
	mux.Handle("POST /v1/runs", s.authenticated(http.HandlerFunc(s.handleRuns)))
	mux.Handle("GET /v1/runs/{id}", s.authenticated(http.HandlerFunc(s.handleGetRun)))
	mux.Handle("GET /v1/runs/{id}/events", s.authenticated(http.HandlerFunc(s.handleRunEvents)))
	mux.Handle("POST /v1/runs/{id}/stop", s.authenticated(http.HandlerFunc(s.handleStopRun)))
	mux.Handle("GET /api/sessions", s.authenticated(http.HandlerFunc(s.handleListSessions)))
	mux.Handle("POST /api/sessions", s.authenticated(http.HandlerFunc(s.handleCreateSession)))
	mux.Handle("GET /api/sessions/{id}", s.authenticated(http.HandlerFunc(s.handleGetSession)))
	mux.Handle("PATCH /api/sessions/{id}", s.authenticated(http.HandlerFunc(s.handlePatchSession)))
	mux.Handle("DELETE /api/sessions/{id}", s.authenticated(http.HandlerFunc(s.handleDeleteSession)))
	mux.Handle("GET /api/sessions/{id}/messages", s.authenticated(http.HandlerFunc(s.handleSessionMessages)))
	mux.Handle("POST /api/sessions/{id}/fork", s.authenticated(http.HandlerFunc(s.handleForkSession)))
	mux.Handle("POST /api/sessions/{id}/chat", s.authenticated(http.HandlerFunc(s.handleSessionChat)))
	return s.logRequests(mux)
}

// Loopback reports whether addr binds only the loopback interface. An address
// with no host (":8787") listens on every interface, so it is not loopback.
func Loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// authenticated enforces the bearer token. With no configured key the server
// serves unauthenticated, which the caller may only do on loopback.
func (s *Server) authenticated(next http.Handler) http.Handler {
	if s.apiKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authorized(r) {
			next.ServeHTTP(w, r)
			return
		}
		// The attempted token is client input: log the path, never the header.
		s.logger.Warn("api server rejected a request", "path", r.URL.Path, "reason", "invalid bearer token")
		writeError(w, http.StatusUnauthorized, "gateway_auth_error", "gateway_auth_failed",
			"Invalid API key (HERMES_API_KEY).")
	})
}

func (s *Server) authorized(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(s.apiKey)) == 1
}

// logRequests records method, path, status and duration at debug level. It
// never records the body, the message content or the bearer token.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		s.logger.Debug("request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status(), "duration", time.Since(start))
	})
}

// handleModels answers GET /v1/models with the configured model.
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	if s.backend == nil {
		writeError(w, http.StatusInternalServerError, "server_error", "internal_error",
			"The API server has no backend.")
		return
	}
	model := s.backend.Model()
	writeJSON(w, http.StatusOK, modelList{Object: "list", Data: []modelItem{{
		ID: model, Object: "model", Created: s.now().Unix(), OwnedBy: "hermes",
	}}})
}

// modelList is OpenAI's GET /v1/models envelope.
type modelList struct {
	Object string      `json:"object"`
	Data   []modelItem `json:"data"`
}

type modelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// errorBody is OpenAI's error envelope.
type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, typ, code, message string) {
	var body errorBody
	body.Error.Message, body.Error.Type, body.Error.Code = message, typ, code
	writeJSON(w, status, body)
}

// openAIError is upstream _openai_error: the envelope with a param slot. A
// nil code encodes as null.
type openAIError struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    *string `json:"code"`
	} `json:"error"`
}

// writeAPIError is upstream _error_response; an empty code encodes as null.
func writeAPIError(w http.ResponseWriter, status int, typ, code, message string) {
	var body openAIError
	body.Error.Message, body.Error.Type = message, typ
	if code != "" {
		body.Error.Code = &code
	}
	writeJSON(w, status, body)
}

// writeInvalid is upstream _invalid_request: 400 with the bare {message,
// type} envelope.
func writeInvalid(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusBadRequest, map[string]map[string]string{
		"error": {"message": message, "type": "invalid_request_error"},
	})
}

// admit reserves one agent run under the concurrency cap. It answers 429 and
// returns false when the cap is reached; otherwise the caller must call the
// returned release once the run ends.
func (s *Server) admit(w http.ResponseWriter) (release func(), ok bool) {
	n := s.inflight.Add(1)
	if s.maxRuns > 0 && n > int64(s.maxRuns) {
		s.inflight.Add(-1)
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded",
			fmt.Sprintf("Too many concurrent runs (max %d)", s.maxRuns))
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(func() { s.inflight.Add(-1) }) }, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// statusWriter remembers the status code for the request log and forwards
// Flush so streaming responses are not buffered.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
