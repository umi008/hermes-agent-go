package apiserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/state"
)

const (
	// maxSessionIDLen is upstream _MAX_SESSION_HEADER_LEN.
	maxSessionIDLen = 256
	// messagePageMax caps one /messages page, as upstream.
	messagePageMax = 500
)

// sessionSources are the sources POST /api/sessions keeps, as upstream
// _normalize_session_source; anything else becomes api_server.
var sessionSources = []string{"api_server", "hermes_browser", "browser", "cli", "telegram", "discord", "slack", "desktop", "dashboard"}

func apiError(w http.ResponseWriter, status int, code, message string) {
	writeAPIError(w, status, "invalid_request_error", code, message)
}

// catalog returns the session store, or answers 503 when there is none.
func (s *Server) catalog(w http.ResponseWriter) (*state.Store, bool) {
	if s.sessions == nil {
		apiError(w, http.StatusServiceUnavailable, "session_db_unavailable", "Session database unavailable")
		return nil, false
	}
	return s.sessions, true
}

// existingSession loads the path's session or answers 404, as upstream
// _get_existing_session_or_404.
func (s *Server) existingSession(w http.ResponseWriter, r *http.Request) (*state.Store, state.Record, bool) {
	st, ok := s.catalog(w)
	if !ok {
		return nil, state.Record{}, false
	}
	id := r.PathValue("id")
	rec, err := st.SessionRecord(r.Context(), id)
	if errors.Is(err, state.ErrNotFound) {
		apiError(w, http.StatusNotFound, "session_not_found", "Session not found: "+id)
		return nil, state.Record{}, false
	}
	if err != nil {
		s.sessionFailure(w, err)
		return nil, state.Record{}, false
	}
	return st, rec, true
}

func (s *Server) sessionFailure(w http.ResponseWriter, err error) {
	s.logger.Error("session store failed", "err", err)
	writeAPIError(w, http.StatusInternalServerError, "server_error", "", "Internal server error")
}

// readObject is upstream _read_json_body: the body must be a JSON object.
func readObject(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		apiError(w, http.StatusBadRequest, "", "Invalid JSON in request body")
		return nil, false
	}
	var body map[string]json.RawMessage
	if !isObject(raw) || json.Unmarshal(raw, &body) != nil {
		apiError(w, http.StatusBadRequest, "", "Request body must be a JSON object")
		return nil, false
	}
	return body, true
}

// bodySessionID is upstream `body.get("id") or body.get("session_id")`, or
// a fresh api_<epoch>_<hex8> id.
func (s *Server) bodySessionID(body map[string]json.RawMessage) string {
	for _, k := range []string{"id", "session_id"} {
		if truthy(body[k]) {
			return strings.TrimSpace(pyStr(body[k]))
		}
	}
	return fmt.Sprintf("api_%d_%s", s.now().Unix(), randomHex(4))
}

// pathUnsafe is upstream gateway.session._is_path_unsafe (strict).
func pathUnsafe(id string) bool {
	if strings.Contains(id, "..") || strings.ContainsAny(id, `/\`) {
		return true
	}
	return len(id) >= 2 && id[1] == ':' && (id[0]|0x20 >= 'a' && id[0]|0x20 <= 'z')
}

func normalizeSource(raw json.RawMessage) string {
	src := "api_server"
	if truthy(raw) {
		src = strings.ToLower(strings.TrimSpace(pyStr(raw)))
	}
	if !slices.Contains(sessionSources, src) {
		return "api_server"
	}
	if src == "browser" {
		return "hermes_browser"
	}
	return src
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilIfZero(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

// sessionObject is upstream _session_response over a row: the client-safe
// keys in order, the flags as booleans, and only the presence of the
// system prompt and model config.
func sessionObject(r state.Record) obj {
	o := obj{
		{"id", r.ID}, {"source", r.Source}, {"user_id", nilIfEmpty(r.UserID)}, {"model", nilIfEmpty(r.Model)},
		{"title", nilIfEmpty(r.Title)}, {"started_at", r.StartedAt}, {"ended_at", nilIfZero(r.EndedAt)},
		{"end_reason", nilIfEmpty(r.EndReason)}, {"message_count", r.MessageCount}, {"tool_call_count", r.ToolCallCount},
		{"input_tokens", r.InputTokens}, {"output_tokens", r.OutputTokens}, {"cache_read_tokens", r.CacheRead},
		{"cache_write_tokens", r.CacheWrite}, {"reasoning_tokens", r.ReasoningTokens},
		{"estimated_cost_usd", r.EstimatedCost}, {"actual_cost_usd", r.ActualCost}, {"api_call_count", r.APICallCount},
		{"parent_session_id", nilIfEmpty(r.ParentID)},
	}
	if r.HasPreview {
		o = append(o, kv{"last_active", r.LastActive}, kv{"preview", r.Preview})
	}
	o = append(o, kv{"pinned", r.Pinned}, kv{"archived", r.Archived}, kv{"hidden", r.Hidden},
		kv{"has_system_prompt", r.SystemPrompt != ""}, kv{"has_model_config", r.ModelConfig != ""})
	var cfg map[string]json.RawMessage
	internal := json.Unmarshal([]byte(r.ModelConfig), &cfg) == nil && cfg["_delegate_from"] != nil &&
		string(cfg["_delegate_from"]) != "null"
	return append(o, kv{"is_internal_child", internal})
}

// messageObject is upstream _message_response over a messages row.
func messageObject(m state.ClientMessage) obj {
	var calls any
	if m.ToolCalls != "" {
		calls = []any{}
		var decoded any
		if json.Unmarshal([]byte(m.ToolCalls), &decoded) == nil {
			calls = decoded
		}
	}
	var tokens any
	if m.TokenCount != 0 {
		tokens = m.TokenCount
	}
	return obj{
		{"id", m.ID}, {"session_id", m.SessionID}, {"role", m.Role}, {"content", nilIfEmpty(m.Content)},
		{"tool_call_id", nilIfEmpty(m.ToolCallID)}, {"tool_calls", calls}, {"tool_name", nilIfEmpty(m.ToolName)},
		{"timestamp", m.Timestamp}, {"token_count", tokens}, {"finish_reason", nilIfEmpty(m.FinishReason)},
		{"reasoning", nilIfEmpty(m.Reasoning)}, {"reasoning_content", nilIfEmpty(m.ReasoningContent)},
		{"display_kind", nilIfEmpty(m.DisplayKind)},
	}
}

func writeSession(w http.ResponseWriter, status int, r state.Record) {
	writeJSON(w, status, obj{{"object", "hermes.session"}, {"session", sessionObject(r)}})
}

// queryBool is upstream _coerce_request_bool over a query value.
func queryBool(q map[string][]string, key string) bool {
	v, ok := q[key]
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v[0])) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// nonNegative is upstream _parse_nonnegative_int.
func nonNegative(raw string, def, maxN int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	if n < 0 {
		return def
	}
	return min(n, maxN)
}

// handleListSessions answers GET /api/sessions.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	st, ok := s.catalog(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := nonNegative(q.Get("limit"), 50, 200)
	offset := nonNegative(q.Get("offset"), 0, 1_000_000)
	title := strings.TrimSpace(q.Get("title"))
	rows, err := st.ListSessions(r.Context(), state.ListOptions{
		Limit: limit, Offset: offset, Source: q.Get("source"),
		IncludeChildren: queryBool(q, "include_children"),
		// include_hidden counts only with a title filter.
		IncludeHidden: title != "" && queryBool(q, "include_hidden"),
		Search:        title,
	})
	if err != nil {
		s.sessionFailure(w, err)
		return
	}
	data, windowed := []obj{}, 0
	for _, row := range rows {
		if title != "" && strings.TrimSpace(row.Title) != title {
			continue
		}
		data = append(data, sessionObject(row))
		if !row.Pinned {
			windowed++
		}
	}
	writeJSON(w, http.StatusOK, obj{{"object", "list"}, {"data", data}, {"limit", limit}, {"offset", offset},
		{"has_more", windowed >= limit}})
}

// handleCreateSession answers POST /api/sessions: an empty session row.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	body, ok := readObject(w, r)
	if !ok {
		return
	}
	st, ok := s.catalog(w)
	if !ok {
		return
	}
	id := s.bodySessionID(body)
	if id == "" || strings.ContainsAny(id, "\r\n\x00") || pathUnsafe(id) {
		apiError(w, http.StatusBadRequest, "invalid_session_id", "Invalid session ID")
		return
	}
	if len(id) > maxSessionIDLen {
		apiError(w, http.StatusBadRequest, "invalid_session_id", "Session ID too long")
		return
	}
	prompt := ""
	if raw, ok := body["system_prompt"]; ok && string(raw) != "null" {
		text, isStr := jsonString(raw)
		if !isStr {
			apiError(w, http.StatusBadRequest, "invalid_system_prompt", "system_prompt must be a string")
			return
		}
		prompt = text
	}
	n := state.NewAPISession{ID: id, Source: normalizeSource(body["source"]), SystemPrompt: prompt}
	if raw, ok := body["title"]; ok && string(raw) != "null" {
		n.Title = pyStr(raw)
	}
	var titleErr *state.TitleError
	switch err := st.CreateAPISession(r.Context(), n); {
	case errors.Is(err, state.ErrSessionExists):
		apiError(w, http.StatusConflict, "session_exists", "Session already exists: "+id)
		return
	case errors.As(err, &titleErr):
		apiError(w, http.StatusBadRequest, "invalid_title", titleErr.Error())
		return
	case err != nil:
		s.sessionFailure(w, err)
		return
	}
	rec, err := st.SessionRecord(r.Context(), id)
	if err != nil {
		s.sessionFailure(w, err)
		return
	}
	writeSession(w, http.StatusCreated, rec)
}

// handleGetSession answers GET /api/sessions/{id}.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	if _, rec, ok := s.existingSession(w, r); ok {
		writeSession(w, http.StatusOK, rec)
	}
}

// patchFields are the session fields PATCH may set.
var patchFields = []string{"title", "end_reason", "pinned", "archived", "hidden", "unread"}

// handlePatchSession answers PATCH /api/sessions/{id}.
func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	st, rec, ok := s.existingSession(w, r)
	if !ok {
		return
	}
	body, ok := readObject(w, r)
	if !ok {
		return
	}
	var unknown []string
	for k := range body {
		if !slices.Contains(patchFields, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		apiError(w, http.StatusBadRequest, "unsupported_session_field", "Unsupported session fields: "+strings.Join(unknown, ", "))
		return
	}
	flags := map[string]bool{}
	for _, f := range []string{"pinned", "archived", "hidden", "unread"} {
		raw, ok := body[f]
		if !ok {
			continue
		}
		var b bool
		if t := strings.TrimSpace(string(raw)); (t != "true" && t != "false") || json.Unmarshal(raw, &b) != nil {
			apiError(w, http.StatusBadRequest, "invalid_session_field", fmt.Sprintf("'%s' must be a boolean", f))
			return
		}
		flags[f] = b
	}
	ctx, id := r.Context(), rec.ID
	if raw, ok := body["title"]; ok {
		title := ""
		if string(raw) != "null" {
			title = pyStr(raw)
		}
		var titleErr *state.TitleError
		if err := st.RenameSession(ctx, id, title); errors.As(err, &titleErr) {
			apiError(w, http.StatusBadRequest, "invalid_title", titleErr.Error())
			return
		} else if err != nil {
			s.sessionFailure(w, err)
			return
		}
	}
	// Pinned last: a pin clears hidden, so it wins over an explicit hidden.
	steps := []struct {
		field string
		flag  state.Flag
		on    func(bool) bool
	}{
		{"archived", state.FlagArchived, func(b bool) bool { return b }},
		{"hidden", state.FlagHidden, func(b bool) bool { return b }},
		{"pinned", state.FlagPinned, func(b bool) bool { return b }},
		{"unread", state.FlagRead, func(b bool) bool { return !b }},
	}
	for _, step := range steps {
		if b, ok := flags[step.field]; ok {
			if err := st.SetFlag(ctx, id, step.flag, step.on(b)); err != nil {
				s.sessionFailure(w, err)
				return
			}
		}
	}
	if raw := body["end_reason"]; truthy(raw) {
		if err := st.EndSession(ctx, id, pyStr(raw)); err != nil {
			s.sessionFailure(w, err)
			return
		}
	}
	if fresh, err := st.SessionRecord(ctx, id); err == nil {
		rec = fresh
	}
	writeSession(w, http.StatusOK, rec)
}

// activeTurn reports whether a /v1/runs run is still driving the session.
func (s *Server) activeTurn(id string) bool {
	s.runs.mu.Lock()
	defer s.runs.mu.Unlock()
	for _, rn := range s.runs.runs {
		if rn.active && rn.status.get("session_id") == id {
			return true
		}
	}
	return false
}

// handleDeleteSession answers DELETE /api/sessions/{id}.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	st, rec, ok := s.existingSession(w, r)
	if !ok {
		return
	}
	if s.activeTurn(rec.ID) {
		apiError(w, http.StatusConflict, "session_active_turn",
			fmt.Sprintf("session '%s' (or a delegate child) has an active turn lease or compression lock", rec.ID))
		return
	}
	deleted, err := st.DeleteSession(r.Context(), rec.ID)
	if err != nil {
		s.sessionFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, obj{{"object", "hermes.session.deleted"}, {"id", rec.ID}, {"deleted", deleted}})
}

// handleSessionMessages answers GET /api/sessions/{id}/messages: one page,
// the latest by default.
func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	st, rec, ok := s.existingSession(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	order := q.Get("order")
	if q.Has("order") && order != "oldest" && order != "latest" {
		apiError(w, http.StatusBadRequest, "invalid_pagination", "order must be one of: oldest, latest")
		return
	}
	offset, limit := 0, -1
	var err error
	if q.Has("offset") {
		offset, err = strconv.Atoi(strings.TrimSpace(q.Get("offset")))
	}
	if err == nil && q.Has("limit") {
		limit, err = strconv.Atoi(strings.TrimSpace(q.Get("limit")))
		if err == nil && limit < 0 {
			err = errors.New("negative")
		}
	}
	if err != nil || offset < 0 {
		apiError(w, http.StatusBadRequest, "invalid_pagination", "limit and offset must be non-negative integers")
		return
	}
	defaultPage := !q.Has("limit")
	latest := order == "latest" || (order == "" && defaultPage)
	if defaultPage {
		limit = messagePageMax
	}
	limit = min(limit, messagePageMax)
	msgs, err := st.MessagePage(r.Context(), rec.ID, limit, offset, latest)
	if err != nil {
		s.sessionFailure(w, err)
		return
	}
	data := make([]obj, 0, len(msgs))
	for _, m := range msgs {
		data = append(data, messageObject(m))
	}
	if order == "" {
		order = "oldest"
		if defaultPage {
			order = "latest"
		}
	}
	writeJSON(w, http.StatusOK, obj{{"object", "list"}, {"session_id", rec.ID}, {"data", data},
		{"pagination", obj{{"limit", limit}, {"offset", offset}, {"order", order}, {"returned", len(msgs)}}}})
}

// handleForkSession answers POST /api/sessions/{id}/fork, upstream's CLI
// /branch over the API.
func (s *Server) handleForkSession(w http.ResponseWriter, r *http.Request) {
	st, src, ok := s.existingSession(w, r)
	if !ok {
		return
	}
	body, ok := readObject(w, r)
	if !ok {
		return
	}
	forkID := s.bodySessionID(body)
	if forkID == "" || strings.ContainsAny(forkID, "\r\n\x00") {
		apiError(w, http.StatusBadRequest, "invalid_session_id", "Invalid session ID")
		return
	}
	ctx := r.Context()
	switch err := st.ForkSession(ctx, src.ID, forkID); {
	case errors.Is(err, state.ErrSessionExists):
		apiError(w, http.StatusConflict, "session_exists", "Session already exists: "+forkID)
		return
	case err != nil:
		s.sessionFailure(w, err)
		return
	}
	var title string
	if raw, ok := body["title"]; ok && string(raw) != "null" {
		title = pyStr(raw)
	} else {
		base := src.Title
		if base == "" {
			base = "fork"
		}
		title = base + " fork"
		if next, err := st.NextTitleInLineage(ctx, base); err == nil {
			title = next
		}
	}
	var titleErr *state.TitleError
	if err := st.RenameSession(ctx, forkID, title); errors.As(err, &titleErr) {
		apiError(w, http.StatusBadRequest, "invalid_title", titleErr.Error())
		return
	} else if err != nil {
		s.sessionFailure(w, err)
		return
	}
	fork, err := st.SessionRecord(ctx, forkID)
	if err != nil {
		s.sessionFailure(w, err)
		return
	}
	writeSession(w, http.StatusCreated, fork)
}
