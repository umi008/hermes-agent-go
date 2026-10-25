package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

const (
	// noResponse is upstream's final text when the turn produced none.
	noResponse = "(No response generated)"
	// runFailed stands in for the agent error text, which may carry provider
	// details the port does not echo to clients.
	runFailed = "The agent run failed."
	// truncationLimit is upstream RESPONSES_AUTO_TRUNCATION_HISTORY_LIMIT, the
	// history kept by truncation: "auto".
	truncationLimit = 100
)

// responsesTurn is one validated POST /v1/responses request, as upstream
// prepare_context resolves it.
type responsesTurn struct {
	id           string
	created      int64
	model        string
	history      []historyEntry
	user         string
	instructions *string
	conversation string
	store        bool
	stream       bool
	// sessionID is the stored session a chained request continues.
	sessionID string
}

// handleResponses answers POST /v1/responses, OpenAI's Responses API.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
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
	t, ok := s.parseResponses(w, r)
	if !ok {
		return
	}
	opts := SessionOptions{Source: sessionSource, ID: t.sessionID, History: toMessages(t.history)}
	if t.instructions != nil {
		opts.Instructions = *t.instructions
	}
	if t.stream {
		s.streamResponse(w, r, t, opts)
		return
	}
	s.completeResponse(w, r, t, opts)
}

// parseResponses validates the body and resolves the chain: the explicit
// conversation_history, else the stored history of previous_response_id (or
// of the conversation's latest response), then the earlier input items. It
// answers the error itself and returns false on a bad request.
func (s *Server) parseResponses(w http.ResponseWriter, r *http.Request) (responsesTurn, bool) {
	ctx := r.Context()
	var body map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&body); err != nil || body == nil {
		writeInvalid(w, "Invalid JSON in request body")
		return responsesTurn{}, false
	}
	bad := func(status int, message string) (responsesTurn, bool) {
		writeAPIError(w, status, "invalid_request_error", "", message)
		return responsesTurn{}, false
	}
	rawInput, has := body["input"]
	if !has || isNull(rawInput) {
		return bad(http.StatusBadRequest, "Missing 'input' field")
	}
	t := responsesTurn{
		id:      "resp_" + randomHex(14),
		created: s.now().Unix(),
		model:   s.backend.Model(),
		store:   coerceBool(body["store"], true),
		stream:  coerceBool(body["stream"], false),
	}
	if v, ok := jsonString(body["instructions"]); ok {
		t.instructions = &v
	}
	if m, ok := jsonString(body["model"]); ok {
		t.model = m
	}
	if truthy(body["conversation"]) && truthy(body["previous_response_id"]) {
		return bad(http.StatusBadRequest, "Cannot use both 'conversation' and 'previous_response_id'")
	}
	previous, _ := jsonString(body["previous_response_id"])
	t.conversation, _ = jsonString(body["conversation"])
	if t.conversation != "" {
		// An unknown name starts a fresh chain.
		id, _, err := s.responses.Conversation(ctx, t.conversation)
		if err != nil {
			return s.storeFailure(w, err)
		}
		previous = id
	}
	messages, msg := parseResponsesInput(rawInput)
	if msg != "" {
		return bad(http.StatusBadRequest, msg)
	}
	if truthy(body["conversation_history"]) {
		if t.history, msg = parseHistory(body["conversation_history"]); msg != "" {
			return bad(http.StatusBadRequest, msg)
		}
	}
	if len(t.history) == 0 && previous != "" {
		rec, found, err := s.responses.Get(ctx, previous)
		if err != nil {
			return s.storeFailure(w, err)
		}
		if !found {
			return bad(http.StatusNotFound, "Previous response not found: "+previous)
		}
		t.history, t.sessionID = append([]historyEntry(nil), rec.ConversationHistory...), rec.SessionID
		if t.instructions == nil {
			t.instructions = rec.Instructions
		}
	}
	if n := len(messages); n > 0 {
		t.history = append(t.history, messages[:n-1]...)
		t.user = messages[n-1].Content
	}
	if strings.TrimSpace(t.user) == "" {
		return bad(http.StatusBadRequest, "No user message found in input")
	}
	if mode, _ := jsonString(body["truncation"]); mode == "auto" && len(t.history) > truncationLimit {
		t.history = t.history[len(t.history)-truncationLimit:]
	}
	return t, true
}

func (s *Server) storeFailure(w http.ResponseWriter, err error) (responsesTurn, bool) {
	s.logger.Warn("response store failed", "err", err)
	writeAPIError(w, http.StatusInternalServerError, "server_error", "", "Internal server error: response store unavailable")
	return responsesTurn{}, false
}

// parseResponsesInput is upstream _parse_responses_input: a string is one user
// message; an array holds strings and message objects, skipping echoed
// reasoning items.
func parseResponsesInput(raw json.RawMessage) ([]historyEntry, string) {
	if text, ok := jsonString(raw); ok {
		return []historyEntry{{Role: "user", Content: text}}, ""
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, "'input' must be a string or array"
	}
	out := make([]historyEntry, 0, len(items))
	for _, item := range items {
		if text, ok := jsonString(item); ok {
			out = append(out, historyEntry{Role: "user", Content: text})
			continue
		}
		var m struct {
			Type    string          `json:"type"`
			Role    *string         `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if !isObject(item) || json.Unmarshal(item, &m) != nil || m.Type == "reasoning" {
			continue
		}
		role := "user"
		if m.Role != nil {
			role = *m.Role
		}
		out = append(out, historyEntry{Role: role, Content: contentText(m.Content)})
	}
	return out, ""
}

// parseHistory is upstream _parse_conversation_history.
func parseHistory(raw json.RawMessage) ([]historyEntry, string) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, "'conversation_history' must be an array of message objects"
	}
	out := make([]historyEntry, 0, len(items))
	for i, item := range items {
		var m map[string]json.RawMessage
		if isObject(item) && json.Unmarshal(item, &m) == nil {
			if typ, _ := jsonString(m["type"]); typ == "reasoning" {
				continue
			}
		}
		role, hasRole := m["role"]
		content, hasContent := m["content"]
		if m == nil || !hasRole || !hasContent {
			return nil, fmt.Sprintf("conversation_history[%d] must have 'role' and 'content' fields", i)
		}
		name, ok := jsonString(role)
		if !ok {
			name = strings.Trim(string(role), `"`)
		}
		out = append(out, historyEntry{Role: name, Content: contentText(content)})
	}
	return out, ""
}

// completeResponse runs one non-streaming turn and answers the response
// object.
func (s *Server) completeResponse(w http.ResponseWriter, r *http.Request, t responsesTurn, opts SessionOptions) {
	ctx := r.Context()
	sess := s.backend.NewSession(ctx, opts)
	if sess == nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "", "Internal server error: no session")
		return
	}
	defer sess.End(endReason)
	sid := sess.ID()
	reply, err := sess.Run(ctx, t.user)
	if err != nil && ctx.Err() != nil {
		return // the client is gone; there is nobody to answer
	}
	if err != nil {
		s.logger.Warn("response run failed", "err", err)
	}
	status := responseStatus(reply, err)
	final := reply.Text
	switch {
	case final != "":
	case err != nil:
		final = runFailed
	default:
		final = noResponse
	}
	resp := obj{
		{"id", t.id}, {"object", "response"}, {"status", status}, {"created_at", t.created},
		{"model", t.model}, {"output", outputItems(reply.Turn, final)}, {"usage", responsesUsage(reply.Usage)},
	}
	if status == "failed" {
		resp = append(resp, kv{"error", obj{{"code", "agent_error"}, {"message", runFailed}}})
	}
	history := t.turnHistory(final)
	if err == nil && len(reply.History) > 0 {
		history = fromMessages(reply.History)
	}
	s.storeResponse(ctx, t, resp, history, sid)
	if sid != "" {
		w.Header().Set("X-Hermes-Session-Id", sid)
	}
	writeJSON(w, http.StatusOK, resp)
}

// turnHistory is the request history plus the user message and, when there
// is one, the assistant text.
func (t responsesTurn) turnHistory(assistant string) []historyEntry {
	h := append(append([]historyEntry(nil), t.history...), historyEntry{Role: "user", Content: t.user})
	if assistant != "" {
		h = append(h, historyEntry{Role: "assistant", Content: assistant})
	}
	return h
}

// storeResponse persists the record when the request asked to store it and
// advances the named conversation. A store failure is logged, not answered:
// the turn already ran.
func (s *Server) storeResponse(ctx context.Context, t responsesTurn, resp obj, history []historyEntry, sid string) {
	if !t.store {
		return
	}
	ctx = context.WithoutCancel(ctx)
	data, err := json.Marshal(resp)
	if err == nil {
		err = s.responses.Put(ctx, t.id, StoredResponse{Response: data, ConversationHistory: history, Instructions: t.instructions, SessionID: sid})
	}
	if err == nil && t.conversation != "" {
		err = s.responses.SetConversation(ctx, t.conversation, t.id)
	}
	if err != nil {
		s.logger.Warn("storing response failed", "response", t.id, "err", err)
	}
}

// responseStatus is upstream _response_status for a finished turn.
func responseStatus(reply Reply, err error) string {
	switch {
	case err != nil:
		return "failed"
	case reply.Stop == llm.StopMaxTokens:
		return "incomplete"
	}
	return "completed"
}

// outputItems is upstream _extract_output_items: a function_call per
// assistant tool call, a function_call_output per tool message, then the
// final message.
func outputItems(turn []llm.Message, final string) []any {
	items := []any{}
	for _, m := range turn {
		switch {
		case m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0:
			for _, tc := range m.ToolCalls {
				items = append(items, obj{
					{"id", "fc_" + randomHex(12)}, {"type", "function_call"}, {"status", "completed"},
					{"name", tc.Name}, {"arguments", string(tc.Arguments)}, {"call_id", tc.ID},
				})
			}
		case m.Role == llm.RoleTool:
			items = append(items, obj{
				{"id", "fco_" + randomHex(12)}, {"type", "function_call_output"}, {"status", "completed"},
				{"call_id", m.ToolCallID}, {"output", m.Content},
			})
		}
	}
	return append(items, messageItem(final))
}

// messageItem is upstream _message_item.
func messageItem(text string) obj {
	return obj{{"type", "message"}, {"role", "assistant"}, {"content", []any{obj{{"type", "output_text"}, {"text", text}}}}}
}

func responsesUsage(u llm.Usage) obj {
	return obj{{"input_tokens", u.InputTokens}, {"output_tokens", u.OutputTokens}, {"total_tokens", u.InputTokens + u.OutputTokens}}
}

// handleGetResponse answers GET /v1/responses/{id} with the stored response.
func (s *Server) handleGetResponse(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, found, err := s.responses.Get(r.Context(), id)
	if err != nil {
		s.storeFailure(w, err)
		return
	}
	if !found {
		writeAPIError(w, http.StatusNotFound, "invalid_request_error", "", "Response not found: "+id)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(rec.Response, '\n'))
}

// handleDeleteResponse answers DELETE /v1/responses/{id}.
func (s *Server) handleDeleteResponse(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	deleted, err := s.responses.Delete(r.Context(), id)
	if err != nil {
		s.storeFailure(w, err)
		return
	}
	if !deleted {
		writeAPIError(w, http.StatusNotFound, "invalid_request_error", "", "Response not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, obj{{"id", id}, {"object", "response"}, {"deleted", true}})
}

// jsonString decodes a JSON string; false for any other value.
func jsonString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func isNull(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "null" }

func isObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return strings.HasPrefix(trimmed, "{")
}

// truthy is Python truthiness of a JSON value; absent is false.
func truthy(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}
