package apiserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

const (
	// sseKeepalive is upstream CHAT_COMPLETIONS_SSE_KEEPALIVE_SECONDS: an
	// idle stream writes a comment frame this often.
	sseKeepalive = 10 * time.Second
	// deltaBatch coalesces text deltas, as upstream's 50ms batch against
	// client re-render storms.
	deltaBatch = 50 * time.Millisecond
)

// itemPrefixes is upstream api_server_response_identity._PREFIXES.
var itemPrefixes = map[string]string{
	"function_call": "fc", "function_call_output": "fco", "reasoning": "rs", "message": "msg",
}

// streamResponse runs one streaming turn: response.created, the output item
// events as text and tools arrive, then the terminal response.<status>.
func (s *Server) streamResponse(w http.ResponseWriter, r *http.Request, t responsesTurn, opts SessionOptions) {
	ctx := r.Context()
	st := &responsesStream{srv: s, w: w, t: t, counts: map[string]int{}, last: time.Now()}
	if f, ok := w.(http.Flusher); ok {
		st.flush = f.Flush
	}
	st.msgID = st.itemID("message", "")
	opts.Out, opts.OnTool = st, st.onTool
	sess := s.backend.NewSession(ctx, opts)
	if sess == nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "", "Internal server error: no session")
		return
	}
	defer sess.End(endReason)
	st.sid = sess.ID()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	if st.sid != "" {
		h.Set("X-Hermes-Session-Id", st.sid)
	}
	w.WriteHeader(http.StatusOK)

	stop := st.keepalive()
	defer stop()
	st.created(ctx)
	reply, err := sess.Run(ctx, t.user)
	if err != nil && ctx.Err() != nil {
		st.abandoned(ctx)
		return
	}
	if err != nil {
		s.logger.Warn("response stream run failed", "err", err)
	}
	st.finish(ctx, reply, err)
}

// responsesStream is upstream _ResponsesStream: the per-request state of one
// SSE response. The agent writes text and tool events on the request
// goroutine while the batch timer and keepalive run on their own, so every
// field and the writer are guarded by mu.
type responsesStream struct {
	srv   *Server
	w     http.ResponseWriter
	flush func()
	t     responsesTurn
	sid   string

	mu          sync.Mutex
	seq         int
	counts      map[string]int
	outputIndex int
	msgID       string
	msgIndex    int
	msgOpened   bool
	text        strings.Builder
	pending     []pendingCall
	emitted     []obj
	calls       int
	batch       strings.Builder
	timer       *time.Timer
	closed      bool
	last        time.Time
}

type pendingCall struct {
	callID, name, arguments, itemID string
	index                           int
}

// itemID is upstream next_item_id: a stable digest of the response id, the
// item kind, its call id and its occurrence.
func (st *responsesStream) itemID(kind, callID string) string {
	key := kind + "\x00" + callID
	n := st.counts[key]
	st.counts[key] = n + 1
	var call any
	if callID != "" {
		call = callID
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode([]any{st.t.id, kind, call, false, n})
	sum := sha256.Sum256(bytes.TrimRight(b.Bytes(), "\n"))
	return itemPrefixes[kind] + "_" + hex.EncodeToString(sum[:])[:24]
}

func (st *responsesStream) nextIndex() int {
	i := st.outputIndex
	st.outputIndex++
	return i
}

// event writes one frame with its sequence number; st.mu must be held.
func (st *responsesStream) event(typ string, data obj) {
	data = append(data, kv{"sequence_number", st.seq})
	st.seq++
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(st.w, "event: %s\ndata: %s\n\n", typ, payload)
	st.sent()
}

func (st *responsesStream) sent() {
	st.last = time.Now()
	if st.flush != nil {
		st.flush()
	}
}

// keepalive writes a comment frame whenever the stream idles for
// sseKeepalive; the returned func stops it.
func (st *responsesStream) keepalive() func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				st.mu.Lock()
				if !st.closed && time.Since(st.last) >= sseKeepalive {
					_, _ = fmt.Fprint(st.w, ": keepalive\n\n")
					st.sent()
				}
				st.mu.Unlock()
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

func (st *responsesStream) envelope(status string) obj {
	return obj{{"id", st.t.id}, {"object", "response"}, {"status", status}, {"created_at", st.t.created}, {"model", st.t.model}}
}

func (st *responsesStream) created(ctx context.Context) {
	env := append(st.envelope("in_progress"), kv{"output", []any{}})
	st.mu.Lock()
	st.event("response.created", obj{{"type", "response.created"}, {"response", env}})
	st.mu.Unlock()
	st.srv.storeResponse(ctx, st.t, env, st.t.turnHistory(""), st.sid)
}

// Write implements io.Writer: text deltas are buffered and flushed as one
// output_text.delta per batch window.
func (st *responsesStream) Write(p []byte) (int, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed || len(p) == 0 {
		return len(p), nil
	}
	st.batch.Write(p)
	if st.timer == nil {
		st.timer = time.AfterFunc(deltaBatch, func() {
			st.mu.Lock()
			defer st.mu.Unlock()
			st.timer = nil
			if !st.closed {
				st.flushBatch()
			}
		})
	}
	return len(p), nil
}

// flushBatch emits the buffered text; st.mu must be held.
func (st *responsesStream) flushBatch() {
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	if st.batch.Len() == 0 {
		return
	}
	text := st.batch.String()
	st.batch.Reset()
	st.textDelta(text)
}

func (st *responsesStream) textDelta(text string) {
	if !st.msgOpened {
		st.msgOpened, st.msgIndex = true, st.nextIndex()
		st.event("response.output_item.added", obj{
			{"type", "response.output_item.added"}, {"output_index", st.msgIndex},
			{"item", obj{{"id", st.msgID}, {"type", "message"}, {"status", "in_progress"}, {"role", "assistant"}, {"content", []any{}}}},
		})
	}
	st.text.WriteString(text)
	st.event("response.output_text.delta", obj{
		{"type", "response.output_text.delta"}, {"item_id", st.msgID}, {"output_index", st.msgIndex},
		{"content_index", 0}, {"delta", text}, {"logprobs", []any{}},
	})
}

// onTool maps tool events to function_call items. Tool events flush the
// pending text first, as upstream.
func (st *responsesStream) onTool(e ToolEvent) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return
	}
	st.flushBatch()
	if e.Done {
		st.toolCompleted(e)
		return
	}
	st.calls++
	callID := e.Call.ID
	if callID == "" {
		callID = fmt.Sprintf("call_%s_%d", strings.TrimPrefix(st.t.id, "resp_"), st.calls)
	}
	args := string(e.Call.Arguments)
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	p := pendingCall{callID: callID, name: e.Call.Name, arguments: args, itemID: st.itemID("function_call", callID)}
	p.index = st.nextIndex()
	st.pending = append(st.pending, p)
	st.emitted = append(st.emitted, obj{{"type", "function_call"}, {"name", p.name}, {"arguments", args}, {"call_id", callID}})
	st.event("response.output_item.added", obj{
		{"type", "response.output_item.added"}, {"output_index", p.index},
		{"item", obj{{"id", p.itemID}, {"type", "function_call"}, {"status", "in_progress"}, {"name", p.name}, {"call_id", callID}, {"arguments", args}}},
	})
}

func (st *responsesStream) toolCompleted(e ToolEvent) {
	at := -1
	for i, p := range st.pending {
		if e.Call.ID != "" && p.callID == e.Call.ID {
			at = i
			break
		}
	}
	if at < 0 {
		return // an orphan completion, as upstream skips
	}
	p := st.pending[at]
	st.pending = append(st.pending[:at], st.pending[at+1:]...)
	st.event("response.output_item.done", obj{
		{"type", "response.output_item.done"}, {"output_index", p.index},
		{"item", obj{{"id", p.itemID}, {"type", "function_call"}, {"status", "completed"}, {"name", p.name}, {"call_id", p.callID}, {"arguments", p.arguments}}},
	})
	parts := []any{obj{{"type", "input_text"}, {"text", e.Output}}}
	item := obj{{"id", st.itemID("function_call_output", p.callID)}, {"type", "function_call_output"}, {"call_id", p.callID}, {"output", parts}, {"status", "completed"}}
	idx := st.nextIndex()
	st.emitted = append(st.emitted, obj{{"type", "function_call_output"}, {"call_id", p.callID}, {"output", []any{obj{{"type", "input_text"}, {"text", e.Output}}}}})
	for _, typ := range []string{"response.output_item.added", "response.output_item.done"} {
		st.event(typ, obj{{"type", typ}, {"output_index", idx}, {"item", item}})
	}
}

// finish closes the message item and writes the terminal event: failed
// when the run errored, else completed or incomplete.
func (st *responsesStream) finish(ctx context.Context, reply Reply, runErr error) {
	st.mu.Lock()
	st.flushBatch()
	if reply.Text != "" && st.text.Len() == 0 {
		// The provider streamed no deltas: emit the final text as one.
		st.textDelta(reply.Text)
	}
	final := st.text.String()
	if final == "" {
		final = reply.Text
	}
	if st.msgOpened {
		st.event("response.output_text.done", obj{
			{"type", "response.output_text.done"}, {"item_id", st.msgID}, {"output_index", st.msgIndex},
			{"content_index", 0}, {"text", final}, {"logprobs", []any{}},
		})
		st.event("response.output_item.done", obj{
			{"type", "response.output_item.done"}, {"output_index", st.msgIndex},
			{"item", obj{{"id", st.msgID}, {"type", "message"}, {"status", "completed"}, {"role", "assistant"},
				{"content", []any{obj{{"type", "output_text"}, {"text", final}}}}}},
		})
	}
	status := responseStatus(reply, runErr)
	text := final
	if runErr != nil && text == "" {
		text = runFailed
	}
	items := append(trimToolItems(st.emitted), messageItem(text))
	env := append(st.envelope(status), kv{"output", items})
	if runErr != nil {
		env = append(env, kv{"error", obj{{"message", runFailed}, {"type", "server_error"}}})
	}
	env = append(env, kv{"usage", responsesUsage(reply.Usage)})
	history := st.t.turnHistory(text)
	if runErr == nil && len(reply.History) > 0 {
		history = fromMessages(reply.History)
	}
	st.mu.Unlock()

	// Persist before the terminal event, so a client that chains on it
	// finds the record.
	st.srv.storeResponse(ctx, st.t, env, history, st.sid)
	st.mu.Lock()
	st.event("response."+status, obj{{"type", "response." + status}, {"response", env}})
	st.closed = true
	st.mu.Unlock()
}

// abandoned persists an incomplete snapshot when the client disconnects, so
// GET and chaining still see what the turn produced.
func (st *responsesStream) abandoned(ctx context.Context) {
	st.mu.Lock()
	st.closed = true
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	text := st.text.String() + st.batch.String()
	items := make([]any, 0, len(st.emitted)+1)
	for _, it := range st.emitted {
		items = append(items, it)
	}
	if text != "" {
		items = append(items, messageItem(text))
	}
	env := append(st.envelope("incomplete"), kv{"output", items}, kv{"usage", responsesUsage(llm.Usage{})})
	st.mu.Unlock()
	st.srv.storeResponse(ctx, st.t, env, st.t.turnHistory(text), st.sid)
}

// trimToolItems is upstream _trim_tool_items: large tool payloads shrink in
// the terminal event, since clients already saw them in full.
func trimToolItems(items []obj) []any {
	out := make([]any, 0, len(items)+1)
	for _, it := range items {
		out = append(out, trimToolItem(it))
	}
	return out
}

func trimToolItem(it obj) obj {
	cp := append(obj(nil), it...)
	switch field(cp, "type") {
	case "function_call":
		raw, _ := field(cp, "arguments").(string)
		var args map[string]any
		if json.Unmarshal([]byte(raw), &args) != nil || args == nil {
			return cp
		}
		changed := false
		for _, k := range []string{"content", "query", "pattern", "old_string", "new_string"} {
			if v, ok := args[k].(string); ok && utf8.RuneCountInString(v) > 500 {
				args[k] = "[" + strconv.Itoa(utf8.RuneCountInString(v)) + " chars — truncated for response.completed]"
				changed = true
			}
		}
		if changed {
			data, _ := json.Marshal(args)
			setField(cp, "arguments", string(data))
		}
	case "function_call_output":
		parts, _ := field(cp, "output").([]any)
		if len(parts) == 0 {
			return cp
		}
		first, ok := parts[0].(obj)
		if !ok || field(first, "type") != "input_text" {
			return cp
		}
		text, _ := field(first, "text").(string)
		if n := utf8.RuneCountInString(text); n > 1000 {
			runes := []rune(text)
			capped := string(runes[:500]) + "...[" + strconv.Itoa(n-500) + " more chars]"
			setField(cp, "output", []any{obj{{"type", "input_text"}, {"text", capped}}})
		}
	}
	return cp
}

func field(o obj, k string) any {
	for _, f := range o {
		if f.k == k {
			return f.v
		}
	}
	return nil
}

func setField(o obj, k string, v any) {
	for i := range o {
		if o[i].k == k {
			o[i].v = v
			return
		}
	}
}
