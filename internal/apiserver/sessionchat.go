package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/state"
)

// /api/sessions/{id}/chat constants, as upstream api_server.
const (
	// sessionChatBase is the completion object name both chat routes answer.
	sessionChatBase = "hermes.session.chat.completion"
	// sessionChatQueue is how many events one stream buffers ahead of the
	// client before the run waits for it.
	sessionChatQueue = 64
)

// chatTurn is one validated /api/sessions/{id}/chat[/stream] request.
type chatTurn struct {
	store   *state.Store
	session string
	message string
	// instructions is the ephemeral system prompt layered on the session's.
	instructions string
	// model is the model this turn serves: the lock's or the request's,
	// empty for the server's own.
	model string
	// provider and requestedModel are reported, never routed: the port
	// serves one configured provider.
	provider, requestedModel, requestedProvider string
	// routeSource is upstream's route_source: global, raw_request or
	// session_model_lock.
	routeSource string
	// lock is set when a confirmed model lock governs the turn.
	lock bool
}

// requested reports whether the client asked for a model or a provider.
func (t *chatTurn) requested() bool { return t.requestedModel != "" || t.requestedProvider != "" }

// handleSessionChat answers POST /api/sessions/{id}/chat: one synchronous turn
// over a stored session, answered as an upstream session-chat completion.
func (s *Server) handleSessionChat(w http.ResponseWriter, r *http.Request) {
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
	turn, ok := s.prepareSessionChat(w, r)
	if !ok {
		return
	}
	history := s.sessionHistory(w, r, turn)
	reply, err := s.runSessionChat(r.Context(), turn, history, nil, nil)
	if err != nil && r.Context().Err() == nil {
		s.logger.Warn("session chat failed", "session", turn.session, "err", err)
	}
	w.Header().Set("X-Hermes-Session-Id", turn.session)
	status := chatStatus(reply, err, r.Context().Err() != nil)
	writeJSON(w, http.StatusOK, obj{
		{"object", sessionChatBase}, {"status", status}, {"session_id", turn.session},
		{"message", obj{{"role", "assistant"}, {"content", reply.Text}}},
		{"usage", runUsage(reply)},
		{"runtime", turn.runtimeMeta(reply, turn.modelLockState(reply))},
	})
}

// prepareSessionChat validates a session chat request and resolves the
// runtime it asks for, as upstream _prepare_session_chat minus the profiles,
// routes and delivery lanes this port does not have. It answers every refused
// request itself.
func (s *Server) prepareSessionChat(w http.ResponseWriter, r *http.Request) (*chatTurn, bool) {
	st, rec, ok := s.existingSession(w, r)
	if !ok {
		return nil, false
	}
	body, ok := readObject(w, r)
	if !ok {
		return nil, false
	}
	message, bad := sessionChatMessage(body)
	if bad != nil {
		apiError(w, http.StatusBadRequest, bad.code, bad.message)
		return nil, false
	}
	instructions, bad := sessionChatInstructions(body)
	if bad != nil {
		apiError(w, http.StatusBadRequest, bad.code, bad.message)
		return nil, false
	}
	turn := &chatTurn{store: st, session: rec.ID, message: message, instructions: instructions}
	if !s.sessionChatRuntime(w, r, body, rec, turn) {
		return nil, false
	}
	return turn, true
}

// sessionChatMessage is upstream _session_chat_user_message: `message`, or
// its `input` alias, flattened to plain text.
func sessionChatMessage(body map[string]json.RawMessage) (string, *badRequest) {
	raw, has := body["message"]
	if !truthy(raw) {
		raw, has = body["input"]
	}
	text := ""
	if has && truthy(raw) {
		text = contentText(raw)
	}
	if strings.TrimSpace(text) == "" {
		return "", &badRequest{code: "missing_message", message: "Missing 'message' field"}
	}
	return text, nil
}

// sessionChatInstructions reads the ephemeral system prompt, as upstream
// `body.get("system_message") or body.get("instructions")`.
func sessionChatInstructions(body map[string]json.RawMessage) (string, *badRequest) {
	for _, key := range []string{"system_message", "instructions"} {
		if !truthy(body[key]) {
			continue
		}
		text, isString := jsonString(body[key])
		if !isString {
			return "", &badRequest{code: "invalid_system_message", message: "system_message must be a string"}
		}
		return text, nil
	}
	return "", nil
}

// sessionChatRuntime resolves and, for a newly requested lock, persists the
// turn's runtime, as upstream _effective_session_runtime_request,
// _runtime_lock_error and _persist_session_runtime_lock. A request that names
// no model falls back to the session's stored lock.
func (s *Server) sessionChatRuntime(w http.ResponseWriter, r *http.Request, body map[string]json.RawMessage,
	rec state.Record, turn *chatTurn) bool {
	rawModel := cleanRuntimeID(firstString(body["model"], body["model_id"]))
	rawProvider := cleanRuntimeID(firstString(body["provider"], body["provider_id"]))
	provider, model := splitProviderModel(rawModel)
	if rawProvider != "" {
		provider = rawProvider
	}
	if model == "" {
		model = rawModel
	}
	// The virtual model alias is not a provider model id.
	if model == s.backend.Model() {
		model = ""
	}
	turn.provider, turn.requestedModel, turn.requestedProvider = provider, model, provider
	lock := coerceBool(body["require_model_lock"], false)
	if model != "" && provider == "" {
		turn.routeSource = "raw_request"
	}
	if model == "" && provider == "" {
		// No selection in the body: a confirmed stored lock governs instead.
		if stored, ok := storedModelLock(rec); ok {
			turn.model, turn.provider, turn.requestedModel, turn.requestedProvider = stored.Model, stored.Provider, stored.Model, stored.Provider
			turn.routeSource, turn.lock = "session_model_lock", true
		}
	} else {
		turn.model = model
	}
	if !lock {
		return true
	}
	if turn.requestedModel == "" && turn.requestedProvider == "" {
		apiError(w, http.StatusBadRequest, "missing_model",
			"require_model_lock was set but no model/provider was provided")
		return false
	}
	if provider != "" {
		// The port serves one configured provider: a locked provider cannot
		// be routed, and falling back to the global one silently is worse.
		apiError(w, http.StatusConflict, "model_lock_unavailable",
			"Requested Browser model lock cannot be routed; refusing silent global fallback")
		return false
	}
	turn.lock = true
	if turn.routeSource == "session_model_lock" {
		return true // a reused lock is never rewritten
	}
	modelOptions := json.RawMessage(`{}`)
	if raw := body["model_options"]; isObject(raw) {
		modelOptions = raw
	}
	err := s.sessions.SetModelLock(r.Context(), rec.ID, state.ModelLock{
		Model: turn.model, Provider: provider, ModelOpts: modelOptions,
		RouteSource: turn.routeSource, Confirmed: true,
	})
	if err != nil {
		s.logger.Warn("persist session model lock", "session", rec.ID, "err", err)
		apiError(w, http.StatusInternalServerError, "model_lock_persistence_failed",
			"Could not persist the requested session model lock")
		return false
	}
	return true
}

// firstString is the first value that is a JSON string, already trimmed away
// from whitespace-only text.
func firstString(raws ...json.RawMessage) string {
	for _, raw := range raws {
		if text, ok := jsonString(raw); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

// cleanRuntimeID is upstream _clean_runtime_id: a trimmed id with no control
// characters and no CR/LF, or "".
func cleanRuntimeID(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 200 || strings.ContainsAny(text, "\r\n\x00") {
		return ""
	}
	return text
}

// splitProviderModel is upstream _split_provider_prefixed_model: "provider::model"
// splits when the prefix looks like a provider name.
func splitProviderModel(text string) (provider, model string) {
	prefix, rest, ok := strings.Cut(text, "::")
	if !ok || !providerName(prefix) || strings.TrimSpace(rest) == "" {
		return "", text
	}
	return prefix, strings.TrimSpace(rest)
}

// providerName is upstream's `^[a-zA-Z0-9_.-]{2,64}$` provider check.
func providerName(text string) bool {
	if len(text) < 2 || len(text) > 64 {
		return false
	}
	for _, c := range text {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// storedModelLock is upstream _runtime_request_from_persisted_session_lock: a
// confirmed lock persisted on the session row.
func storedModelLock(rec state.Record) (state.ModelLock, bool) {
	var cfg map[string]json.RawMessage
	if json.Unmarshal([]byte(rec.ModelConfig), &cfg) != nil {
		return state.ModelLock{}, false
	}
	var lock state.ModelLock
	if json.Unmarshal(cfg["browser_model_lock"], &lock) != nil || !lock.Confirmed {
		return state.ModelLock{}, false
	}
	if lock.Model == "" && lock.Provider == "" {
		return state.ModelLock{}, false
	}
	if lock.RouteSource == "model_routes" {
		// The port has no route table: a route-backed lock cannot be replayed.
		return state.ModelLock{}, false
	}
	return lock, true
}

// runtimeMeta is upstream _sanitize_runtime_metadata over a finished turn.
func (t *chatTurn) runtimeMeta(reply Reply, modelLock string) obj {
	out := obj{{"provider", cleanRuntimeID(reply.Provider)}, {"model", cleanRuntimeID(reply.Model)},
		{"route_source", t.routeSourceOf()}}
	if t.requested() {
		out = append(out, kv{"requested", obj{{"provider", t.requestedProvider}, {"model", t.requestedModel}}})
	}
	if modelLock != "" {
		out = append(out, kv{"model_lock", modelLock})
	}
	return out
}

// routeSourceOf is the route the turn was selected through, defaulting to the
// global one as upstream does.
func (t *chatTurn) routeSourceOf() string {
	if t.routeSource == "" {
		return "global"
	}
	return t.routeSource
}

// modelLockState is upstream _model_lock_state: "confirmed" once a runtime was
// observed under a lock, "accepted" before that, and "" without a lock.
func (t *chatTurn) modelLockState(reply Reply) string {
	if !t.lock {
		return ""
	}
	if reply.Provider != "" || reply.Model != "" {
		return "confirmed"
	}
	return "accepted"
}

// chatStatus is upstream _response_status: an unfinished turn is incomplete,
// a failed one failed, an interrupted one cancelled.
func chatStatus(reply Reply, err error, interrupted bool) string {
	switch {
	case interrupted:
		return "cancelled"
	case err != nil:
		return "failed"
	case reply.ExitReason != "":
		return "incomplete"
	}
	return "completed"
}

// sessionHistory loads the conversation a session chat replays: every active
// stored row, as the agent's seed history.
func (s *Server) sessionHistory(w http.ResponseWriter, r *http.Request, turn *chatTurn) []llm.Message {
	rows, err := turn.store.ActiveMessages(r.Context(), turn.session)
	if err != nil {
		s.sessionFailure(w, err)
		return nil
	}
	entries := make([]historyEntry, 0, len(rows))
	for _, m := range rows {
		e := historyEntry{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		if m.ToolCalls != "" {
			_ = json.Unmarshal([]byte(m.ToolCalls), &e.ToolCalls)
		}
		entries = append(entries, e)
	}
	return toMessages(entries)
}

// runSessionChat runs one turn over the stored session. out, onTool and
// onComment may be nil.
func (s *Server) runSessionChat(ctx context.Context, turn *chatTurn, history []llm.Message,
	out io.Writer, onTool func(ToolEvent)) (Reply, error) {
	sess := s.backend.NewSession(ctx, SessionOptions{
		Source: sessionSource, Out: out, ID: turn.session, History: history,
		Instructions: turn.instructions, Model: turn.model, OnTool: onTool,
	})
	if sess == nil {
		return Reply{}, errors.New("apiserver: backend opened no session")
	}
	// A session chat continues a stored conversation, so the session row is
	// never ended here, as upstream.
	return sess.Run(ctx, turn.message)
}
