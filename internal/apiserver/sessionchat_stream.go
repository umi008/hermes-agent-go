package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// The session chat stream: POST /api/sessions/{id}/chat/stream answers one
// turn over a stored session as upstream's typed SSE events, and registers
// the turn as a pollable run so GET /v1/runs/{id} still reports its outcome.

// chatEvent is one stamped SSE event of a session chat stream.
type chatEvent struct {
	name string
	data obj
}

// chatEventStamps stamps the events of one stream: seq counts from one and every
// payload carries the session, the run, its seq and its timestamp, as
// upstream _SessionEventQueue.payload.
type chatEventStamps struct {
	session string
	run     string
	seq     int
	now     func() time.Time
}

func (c *chatEventStamps) event(name string, fields obj) chatEvent {
	c.seq++
	out := append(obj{}, fields...)
	has := func(k string) bool {
		for _, f := range out {
			if f.k == k {
				return true
			}
		}
		return false
	}
	stamps := obj{{"session_id", c.session}, {"run_id", c.run}, {"seq", c.seq}, {"ts", unixSeconds(c.now())}}
	for _, f := range stamps {
		if !has(f.k) {
			out = append(out, f)
		}
	}
	return chatEvent{name: name, data: out}
}

// handleSessionChatStream answers POST /api/sessions/{id}/chat/stream: one
// turn over a stored session, streamed as upstream's typed SSE events.
func (s *Server) handleSessionChatStream(w http.ResponseWriter, r *http.Request) {
	if s.backend == nil {
		writeError(w, http.StatusInternalServerError, "server_error", "internal_error",
			"The API server has no backend.")
		return
	}
	release, ok := s.admit(w)
	if !ok {
		return
	}
	turn, ok := s.prepareSessionChat(w, r)
	if !ok {
		release()
		return
	}
	history, ok := s.sessionHistory(w, r, turn)
	if !ok {
		release()
		return
	}

	messageID, runID := "msg_"+randomHex(16), "run_"+randomHex(16)
	events := make(chan chatEvent, sessionChatQueue)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	rn := s.startChatRun(runID, turn, cancel)
	stream := &chatEventStamps{session: turn.session, run: runID, now: s.now}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Hermes-Session-Id", turn.session)
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	write := func(frame string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(runWriteTimeout))
		if _, err := w.Write([]byte(frame)); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	go s.driveChatStream(ctx, turn, history, rn, stream, events, messageID, release)
	idle := time.NewTimer(sseKeepalive)
	defer idle.Stop()
	for {
		select {
		case ev := <-events:
			if !write(sseNamedEvent(ev.name, ev.data)) || ev.name == "done" {
				return
			}
			idle.Reset(sseKeepalive)
		case <-idle.C:
			if !write(": keepalive\n\n") {
				return
			}
			idle.Reset(sseKeepalive)
		case <-r.Context().Done():
			// The client is gone: stop the turn and let it settle its run.
			cancel()
			return
		}
	}
}

// sseNamedEvent is upstream _sse_frame with a named event and no id line: the seq
// travels inside the payload.
func sseNamedEvent(name string, data any) string {
	b, _ := json.Marshal(data)
	return "event: " + name + "\ndata: " + string(b) + "\n\n"
}

// startChatRun registers the streamed turn as a pollable run, as upstream
// _set_run_status does before the turn starts: GET /v1/runs/{id} reports it,
// POST /v1/runs/{id}/stop cancels it, and its events stay on the response.
func (s *Server) startChatRun(id string, turn *chatTurn, cancel context.CancelFunc) *run {
	now := s.now()
	rn := &run{id: id, created: now, cancel: cancel, active: true, started: map[string]time.Time{}}
	s.runs.mu.Lock()
	rn.setStatus(now, "queued", kv{"created_at", unixSeconds(now)}, kv{"session_id", turn.session},
		kv{"model", turn.statusModel(s.backend.Model())})
	s.runs.runs[id] = rn
	s.runs.mu.Unlock()
	return rn
}

// statusModel is the model the run status reports, as upstream's body model
// with the virtual alias as the default.
func (t *chatTurn) statusModel(served string) string {
	if t.requestedModel != "" {
		return t.requestedModel
	}
	return served
}

// chatDeltas turns the turn's streamed text into assistant.delta events.
type chatDeltas struct {
	emit  func(string, obj)
	msgID string
}

func (d chatDeltas) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	d.emit("assistant.delta", obj{{"message_id", d.msgID}, {"delta", string(p)}})
	return len(p), nil
}

// driveChatStream runs one turn and publishes its events, as upstream
// _run_and_signal: the opening frames, the deltas and tool events, the
// terminal frames, then the end of the stream. It owns the run's status and
// the concurrency slot.
func (s *Server) driveChatStream(ctx context.Context, turn *chatTurn, history []llm.Message, rn *run,
	stream *chatEventStamps, events chan<- chatEvent, messageID string, release func()) {
	defer release()
	// emit stamps an event and hands it to the writer; the stamping stays in
	// this goroutine so seq order is event order.
	emit := func(name string, fields obj) {
		select {
		case events <- stream.event(name, fields):
		case <-ctx.Done():
		}
	}
	defer func() {
		emit("done", obj{})
		close(events)
	}()
	started := s.now()
	s.runs.mu.Lock()
	rn.stopping = rn.stopping || ctx.Err() != nil
	s.runs.mu.Unlock()
	emit("run.started", obj{
		{"user_message", obj{{"role", "user"}, {"content", turn.message}}},
		{"runtime", turn.runtimeMeta(Reply{}, "")},
	})
	emit("message.started", obj{{"message", obj{{"id", messageID}, {"role", "assistant"}}}})
	s.runs.mu.Lock()
	if rn.stream == nil {
		rn.setStatus(started, "running", kv{"last_event", "message.started"})
	}
	s.runs.mu.Unlock()

	onTool := func(e ToolEvent) {
		name := "tool.started"
		fields := obj{{"message_id", messageID}, {"tool_name", e.Call.Name}}
		if !e.Done {
			fields = append(fields, kv{"preview", toolPreview(e.Call.Name, e.Call.Arguments)},
				kv{"args", rawArguments(e.Call.Arguments)})
		} else {
			if e.IsError {
				name = "tool.failed"
			}
			fields = append(fields, kv{"preview", completedPreview(e.Output)}, kv{"args", nil})
		}
		emit(name, fields)
	}
	reply, err := s.runSessionChat(ctx, turn, history, chatDeltas{emit: emit, msgID: messageID}, onTool)
	if err != nil && ctx.Err() == nil {
		s.logger.Warn("session chat stream failed", "session", turn.session, "run_id", rn.id, "err", err)
	}
	interrupted := ctx.Err() != nil
	status, fields := chatTerminal(reply, err, interrupted)
	meta := turn.runtimeMeta(reply, turn.modelLockState(reply))
	s.closeChatRun(rn, status, reply, meta, fields)
	if err != nil && !interrupted {
		emit("error", obj{{"message", runFailedText}})
		return
	}
	emit("assistant.completed", append(append(obj{
		{"session_id", turn.session}, {"message_id", messageID}, {"content", reply.Text},
	}, fields...), kv{"runtime", meta}))
	emit("run."+status, append(append(obj{
		{"session_id", turn.session}, {"message_id", messageID},
	}, fields...), kv{"messages", fromMessages(reply.Turn)}, kv{"usage", runUsage(reply)}, kv{"runtime", meta}))
}

// chatTerminal is upstream terminal_run_status: the run status the stream
// closes with and the flags every terminal frame carries.
func chatTerminal(reply Reply, err error, interrupted bool) (string, obj) {
	if interrupted {
		return "cancelled", terminalFields(false, true, reply.ExitReason)
	}
	if err != nil {
		return "failed", terminalFields(false, false, reply.ExitReason)
	}
	finished := reply.ExitReason == ""
	if !finished {
		return "failed", terminalFields(false, false, reply.ExitReason)
	}
	return "completed", terminalFields(true, false, "")
}

// closeChatRun publishes the run's terminal status, so a caller whose stream
// died can still read the outcome from GET /v1/runs/{id}.
func (s *Server) closeChatRun(rn *run, status string, reply Reply, meta obj, fields obj) {
	now := s.now()
	statusFields := obj{{"output", reply.Text}, {"usage", runUsage(reply)}, {"runtime", meta}}
	statusFields = append(statusFields, fields...)
	statusFields = append(statusFields, kv{"last_event", "run." + status})
	s.runs.mu.Lock()
	defer s.runs.mu.Unlock()
	rn.setStatus(now, status, statusFields...)
	rn.active = false
}

// rawArguments decodes a tool call's arguments for the stream's args field;
// anything that is not JSON stays nil.
func rawArguments(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}
