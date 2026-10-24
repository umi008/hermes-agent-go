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
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// Reply is the outcome of one turn.
type Reply struct {
	Text  string
	Usage llm.Usage
	// Stop is the normalized provider stop reason (llm.StopEnd,
	// llm.StopMaxTokens, ...), mapped to OpenAI's finish_reason.
	Stop string
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

// Backend opens one session per HTTP request.
type Backend interface {
	// Model is the model id the server advertises and echoes.
	Model() string
	// NewSession opens a conversation whose streamed text goes to out. A nil
	// session is a wiring error and answers 500.
	NewSession(ctx context.Context, source string, out io.Writer) Session
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
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
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
