package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// /v1/runs constants, as upstream api_server_runs.
const (
	// runStreamTTL is upstream _RUN_STREAM_TTL: an event buffer nobody
	// subscribes to is dropped this long after the run started.
	runStreamTTL = 300 * time.Second
	// runStatusTTL is upstream _RUN_STATUS_TTL: a terminal status stays
	// pollable this long after its last update.
	runStatusTTL = time.Hour
	// toolPreviewMax is upstream _TOOL_COMPLETED_PREVIEW_MAX_CHARS.
	toolPreviewMax = 500
	// runFailedText is upstream's fallback failure text; the provider error
	// itself is logged, never echoed.
	runFailedText    = "agent run failed"
	runShutdownError = "Gateway shutdown interrupted the run."
)

// terminalStatuses is upstream TERMINAL_STATUSES.
var terminalStatuses = map[string]bool{"completed": true, "failed": true, "cancelled": true, "interrupted": true}

// runRegistry holds the live and pollable runs of this process. Every field
// of every run is guarded by mu.
type runRegistry struct {
	mu   sync.Mutex
	runs map[string]*run
}

type run struct {
	id string
	// status is the pollable record; nil once it expired.
	status  *record
	updated time.Time
	// stream buffers the events; nil once its transport expired.
	stream  *runStream
	created time.Time
	cancel  context.CancelFunc
	active  bool
	// stopping and shutdown say why the run's context was cancelled.
	stopping, shutdown bool
	// started maps a tool call id to its start, for the completed duration.
	started map[string]time.Time
}

func newRunRegistry() *runRegistry { return &runRegistry{runs: map[string]*run{}} }

// setStatus is upstream _set_run_status: the identity fields, created_at
// once, then fields in order. The caller holds mu.
func (rn *run) setStatus(now time.Time, status string, fields ...kv) {
	if rn.status == nil {
		rn.status = newRecord()
	}
	st := rn.status
	st.set("object", "hermes.run")
	st.set("run_id", rn.id)
	st.set("status", status)
	st.set("updated_at", unixSeconds(now))
	created := any(unixSeconds(now))
	rest := make([]kv, 0, len(fields))
	for _, f := range fields {
		if f.k == "created_at" {
			created = f.v
			continue
		}
		rest = append(rest, f)
	}
	st.setDefault("created_at", created)
	for _, f := range rest {
		st.set(f.k, f.v)
	}
	rn.updated = now
}

// push records an event as the run's last_event and sends it to the stream,
// as upstream's tool-progress _push. The caller holds mu.
func (rn *run) push(now time.Time, ev obj) {
	if rn.status != nil {
		status, _ := rn.status.get("status").(string)
		rn.setStatus(now, status, kv{"last_event", ev[0].v})
	}
	rn.send(ev)
}

// send enqueues an event while the run still owns its stream. The caller
// holds mu.
func (rn *run) send(ev obj) {
	if rn.stream != nil {
		rn.stream.put(ev)
	}
}

// sweep expires idle event buffers and old terminal statuses, as upstream
// _sweep_orphaned_runs_once, and forgets runs with neither left.
func (reg *runRegistry) sweep(now time.Time) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for id, rn := range reg.runs {
		if rn.stream != nil && now.Sub(rn.created) > runStreamTTL && rn.stream.subscribers() == 0 {
			rn.stream = nil
		}
		if rn.status != nil {
			switch rn.status.get("status") {
			case "completed", "failed", "cancelled":
				if now.Sub(rn.updated) > runStatusTTL {
					rn.status = nil
				}
			}
		}
		if rn.stream == nil && rn.status == nil && !rn.active {
			delete(reg.runs, id)
		}
	}
}

// interruptAll ends every active run because the server is shutting down.
func (reg *runRegistry) interruptAll() {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, rn := range reg.runs {
		if rn.active {
			rn.shutdown = true
			rn.cancel()
		}
	}
}

// runEvent is upstream _run_event; the key order is wire format.
func runEvent(id, name string, now time.Time, fields ...kv) obj {
	return append(obj{{"event", name}, {"run_id", id}, {"timestamp", unixSeconds(now)}}, fields...)
}

// handleRuns answers POST /v1/runs: it admits the run, starts it in the
// background and answers 202 with its id at once.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	if s.backend == nil {
		writeError(w, http.StatusInternalServerError, "server_error", "internal_error",
			"The API server has no backend.")
		return
	}
	s.runs.sweep(s.now())
	var body map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&body); err != nil || body == nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "", "Invalid JSON")
		return
	}
	user, msg := runUserMessage(body["input"])
	if msg != "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "", msg)
		return
	}
	history, instructions, storedSession, msg := s.runHistory(r.Context(), body)
	if msg != "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "", msg)
		return
	}
	release, ok := s.admit(w)
	if !ok {
		return
	}
	id := "run_" + randomHex(16)
	sessionID, _ := jsonString(body["session_id"])
	if sessionID == "" {
		sessionID = storedSession
	}
	previous, _ := jsonString(body["previous_response_id"])
	// A selected session replays its stored transcript unless the request
	// brought its own history or continues a response chain.
	resume := sessionID != "" && len(history) == 0 && previous == ""
	if sessionID == "" {
		sessionID = id
	}
	model := any(s.backend.Model())
	if raw, ok := body["model"]; ok {
		model = raw
	}

	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	now := s.now()
	rn := &run{id: id, stream: newRunStream(), created: now, cancel: cancel, active: true, started: map[string]time.Time{}}
	s.runs.mu.Lock()
	s.runs.runs[id] = rn
	rn.setStatus(now, "queued", kv{"created_at", unixSeconds(now)}, kv{"session_id", sessionID}, kv{"model", model})
	s.runs.mu.Unlock()

	opts := SessionOptions{
		Source:  sessionSource,
		Out:     runDeltas{s: s, rn: rn},
		ID:      sessionID,
		History: toMessages(history),
		Resume:  resume,
		OnTool:  func(e ToolEvent) { s.runTool(rn, e) },
	}
	if instructions != nil {
		opts.Instructions = *instructions
	}
	go s.executeRun(ctx, rn, opts, user, release)
	writeJSON(w, http.StatusAccepted, obj{{"run_id", id}, {"status", "started"}, {"replayed", false}})
}

// runUserMessage is upstream _run_user_message: the input string, or the
// content of the last input item.
func runUserMessage(raw json.RawMessage) (string, string) {
	if !truthy(raw) {
		return "", "Missing 'input' field"
	}
	text, ok := jsonString(raw)
	if !ok {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) == nil && len(items) > 0 && isObject(items[len(items)-1]) {
			var last struct {
				Content json.RawMessage `json:"content"`
			}
			_ = json.Unmarshal(items[len(items)-1], &last)
			text = contentText(last.Content)
		}
	}
	if text == "" {
		return "", "No user message found in input"
	}
	return text, ""
}

// runHistory is upstream _resolve_conversation_history: conversation_history
// wins, then the previous_response_id chain (with its session and
// instructions), then the input items before the last.
func (s *Server) runHistory(ctx context.Context, body map[string]json.RawMessage) (history []historyEntry, instructions *string, session, msg string) {
	if text, ok := jsonString(body["instructions"]); ok {
		instructions = &text
	}
	if raw := body["conversation_history"]; truthy(raw) {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return nil, nil, "", "'conversation_history' must be an array of message objects"
		}
		for i, item := range items {
			var m map[string]json.RawMessage
			if !isObject(item) || json.Unmarshal(item, &m) != nil {
				return nil, nil, "", fmt.Sprintf("conversation_history[%d] must have 'role' and 'content' fields", i)
			}
			role, hasRole := m["role"]
			content, hasContent := m["content"]
			if !hasRole || !hasContent {
				return nil, nil, "", fmt.Sprintf("conversation_history[%d] must have 'role' and 'content' fields", i)
			}
			history = append(history, historyEntry{Role: pyStr(role), Content: pyStr(content)})
		}
	}
	if previous, _ := jsonString(body["previous_response_id"]); len(history) == 0 && previous != "" {
		rec, found, err := s.responses.Get(ctx, previous)
		switch {
		case err != nil:
			s.logger.Warn("response store read failed", "err", err)
		case found:
			history, session = rec.ConversationHistory, rec.SessionID
			if instructions == nil {
				instructions = rec.Instructions
			}
		}
	}
	var items []json.RawMessage
	if len(history) == 0 && json.Unmarshal(body["input"], &items) == nil && len(items) > 1 {
		for _, item := range items[:len(items)-1] {
			var m struct {
				Role    json.RawMessage `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if !isObject(item) || json.Unmarshal(item, &m) != nil || !truthy(m.Role) || !truthy(m.Content) {
				continue
			}
			history = append(history, historyEntry{Role: pyStr(m.Role), Content: flattenText(m.Content)})
		}
	}
	return history, instructions, session, ""
}

// flattenText joins the text parts of a content list with spaces, as
// upstream's runs history; any other content is its string form.
func flattenText(raw json.RawMessage) string {
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return pyStr(raw)
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		var part struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if isObject(p) && json.Unmarshal(p, &part) == nil && part.Type == "text" {
			out = append(out, part.Text)
		}
	}
	return strings.Join(out, " ")
}

// pyStr is a JSON value as Python str(): a string's text, else its JSON.
func pyStr(raw json.RawMessage) string {
	if text, ok := jsonString(raw); ok {
		return text
	}
	return strings.TrimSpace(string(raw))
}

// executeRun drives one admitted run and publishes its terminal status and
// event, as upstream _execute_run.
func (s *Server) executeRun(ctx context.Context, rn *run, opts SessionOptions, input string, release func()) {
	defer release()
	defer rn.cancel()
	s.runs.mu.Lock()
	if rn.shutdown || rn.stopping {
		// Stopped before its first tick: nothing ran, so no outcome fields.
		status, fields := "cancelled", obj(nil)
		if rn.shutdown {
			status, fields = "interrupted", obj{{"error", runShutdownError}}
		}
		s.closeRunLocked(rn, status, fields, nil)
		s.runs.mu.Unlock()
		return
	}
	rn.setStatus(s.now(), "running")
	s.runs.mu.Unlock()

	var reply Reply
	var err error
	if sess := s.backend.NewSession(ctx, opts); sess == nil {
		err = fmt.Errorf("apiserver: backend opened no session")
	} else {
		reply, err = sess.Run(ctx, input)
		sess.End(endReason)
	}
	if err != nil && ctx.Err() == nil {
		s.logger.Warn("run failed", "run_id", rn.id, "err", err)
	}
	s.runs.mu.Lock()
	status, fields, extra := runOutcome(rn, reply, err)
	s.closeRunLocked(rn, status, fields, extra)
	s.runs.mu.Unlock()
}

// runOutcome maps a finished turn to its terminal status, its outcome fields
// and the terminal flags, as upstream terminal_run_status and _execute_run.
// The caller holds mu.
func runOutcome(rn *run, reply Reply, err error) (status string, fields, extra obj) {
	switch {
	case rn.shutdown:
		status, fields = "interrupted", obj{{"error", runShutdownError}}
	case rn.stopping && (err != nil || reply.ExitReason != ""):
		status, extra = "cancelled", terminalFields(false, true, reply.ExitReason)
	case err != nil:
		status, fields, extra = "failed", obj{{"error", runFailedText}}, terminalFields(false, false, reply.ExitReason)
	default:
		finished := reply.ExitReason == ""
		status = "failed"
		if finished {
			status = "completed"
		}
		fields = obj{{"output", reply.Text}, {"usage", runUsage(reply)}, {"runtime", obj{
			{"provider", reply.Provider}, {"model", reply.Model}, {"route_source", "global"},
		}}}
		extra = terminalFields(finished, false, reply.ExitReason)
	}
	return status, fields, extra
}

// closeRunLocked is upstream _finish plus the stream close: the terminal
// status, the run.<status> event, then the end of the stream. The caller
// holds mu.
func (s *Server) closeRunLocked(rn *run, status string, fields, extra obj) {
	now := s.now()
	name := "run." + status
	statusFields := append(append(append(obj{}, fields...), kv{"last_event", name}), extra...)
	rn.setStatus(now, status, statusFields...)
	rn.send(runEvent(rn.id, name, now, append(append(obj{}, fields...), extra...)...))
	rn.send(nil)
	rn.active = false
}

// terminalFields are the flags every terminal event and status carries.
func terminalFields(completed, interrupted bool, reason string) obj {
	out := obj{{"completed", completed}, {"partial", false}, {"interrupted", interrupted}}
	if !completed && reason != "" {
		out = append(out, kv{"turn_exit_reason", reason})
	}
	return out
}

// runUsage is upstream _run_usage.
func runUsage(r Reply) obj {
	u := r.Usage
	return obj{
		{"input_tokens", u.InputTokens}, {"output_tokens", u.OutputTokens},
		{"total_tokens", u.InputTokens + u.OutputTokens},
		{"cache_read_tokens", u.CacheReadTokens}, {"cache_write_tokens", u.CacheWriteTokens},
	}
}

// runDeltas turns the streamed text into message.delta events.
type runDeltas struct {
	s  *Server
	rn *run
}

func (d runDeltas) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	now := d.s.now()
	d.s.runs.mu.Lock()
	d.rn.send(runEvent(d.rn.id, "message.delta", now, kv{"delta", string(p)}))
	d.s.runs.mu.Unlock()
	return len(p), nil
}

// runTool publishes tool.started and tool.completed, as upstream's
// _FIXED_EVENT_FIELDS.
func (s *Server) runTool(rn *run, e ToolEvent) {
	now := s.now()
	s.runs.mu.Lock()
	defer s.runs.mu.Unlock()
	callID := e.Call.ID
	if !e.Done {
		rn.started[callID] = now
		fields := obj{{"tool", e.Call.Name}, {"preview", toolPreview(e.Call.Name, e.Call.Arguments)}}
		if callID != "" {
			fields = append(fields, kv{"tool_call_id", callID})
		}
		rn.push(now, runEvent(rn.id, "tool.started", now, fields...))
		return
	}
	duration := 0.0
	if t0, ok := rn.started[callID]; ok {
		duration = math.Round(now.Sub(t0).Seconds()*1000) / 1000
		delete(rn.started, callID)
	}
	fields := obj{{"tool", e.Call.Name}, {"duration", duration}, {"error", e.IsError}}
	if callID != "" {
		fields = append(fields, kv{"tool_call_id", callID})
	}
	fields = append(fields, kv{"preview", completedPreview(e.Output)})
	rn.push(now, runEvent(rn.id, "tool.completed", now, fields...))
}

// primaryArgs is upstream _PRIMARY_ARGS: the argument a tool preview shows.
var primaryArgs = map[string]string{
	"terminal": "command", "web_search": "query", "web_extract": "urls", "read_file": "path",
	"write_file": "path", "patch": "path", "search_files": "pattern", "browser_navigate": "url",
	"browser_click": "ref", "browser_type": "text", "image_generate": "prompt", "text_to_speech": "text",
	"vision_analyze": "question", "skill_view": "name", "skills_list": "category", "cronjob_manage": "action",
	"execute_code": "code", "browser_exec": "code", "delegate_task": "goal",
	"skill_manage": "name",
}

// fallbackPreviewKeys is upstream _FALLBACK_PREVIEW_KEYS.
var fallbackPreviewKeys = []string{"query", "text", "command", "path", "name", "prompt", "code", "goal"}

// toolPreview is upstream _primary_arg_preview with no length cap: the
// tool's primary argument on one line.
func toolPreview(name string, raw json.RawMessage) string {
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil || len(args) == 0 {
		return ""
	}
	key := primaryArgs[name]
	if key == "" {
		for _, k := range fallbackPreviewKeys {
			if _, ok := args[k]; ok {
				key = k
				break
			}
		}
	}
	value, ok := args[key]
	if key == "" || !ok {
		return ""
	}
	var list []json.RawMessage
	if json.Unmarshal(value, &list) == nil {
		if len(list) == 0 {
			return ""
		}
		value = list[0]
	}
	return strings.Join(strings.Fields(pyStr(value)), " ")
}

// completedPreview is upstream _tool_completed_preview's bound: the result
// cut to 500 characters with "...".
func completedPreview(out string) string {
	runes := []rune(out)
	if len(runes) <= toolPreviewMax {
		return out
	}
	return string(runes[:toolPreviewMax-3]) + "..."
}

// runNotFound is upstream _run_not_found.
func runNotFound(w http.ResponseWriter, id string) {
	writeAPIError(w, http.StatusNotFound, "invalid_request_error", "run_not_found", "Run not found: "+id)
}

// handleGetRun answers GET /v1/runs/{id} with the pollable status.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	s.runs.sweep(s.now())
	id := r.PathValue("id")
	s.runs.mu.Lock()
	var snap obj
	if rn := s.runs.runs[id]; rn != nil && rn.status != nil {
		snap = rn.status.snapshot()
	}
	s.runs.mu.Unlock()
	if snap == nil {
		runNotFound(w, id)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleStopRun answers POST /v1/runs/{id}/stop: it interrupts an active run,
// as upstream _handle_stop_run.
func (s *Server) handleStopRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.runs.mu.Lock()
	rn := s.runs.runs[id]
	if rn == nil || rn.status == nil {
		s.runs.mu.Unlock()
		runNotFound(w, id)
		return
	}
	if status, _ := rn.status.get("status").(string); terminalStatuses[status] {
		snap := rn.status.snapshot()
		s.runs.mu.Unlock()
		writeJSON(w, http.StatusOK, snap)
		return
	}
	if !rn.active {
		s.runs.mu.Unlock()
		writeAPIError(w, http.StatusConflict, "invalid_request_error", "run_not_active",
			"Run is not active in this gateway process: "+id)
		return
	}
	rn.setStatus(s.now(), "stopping", kv{"last_event", "run.stopping"})
	rn.stopping = true
	rn.cancel()
	s.runs.mu.Unlock()
	writeJSON(w, http.StatusOK, obj{{"run_id", id}, {"status", "stopping"}})
}
