package apiserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// runBacklog is upstream _RunStream.BACKLOG_LIMIT: the events kept for
	// replay.
	runBacklog = 1000
	// runWriteTimeout is upstream _RUN_STREAM_WRITE_TIMEOUT.
	runWriteTimeout = 5 * time.Second
)

// runEntry is one sequenced event; a nil event ends the stream.
type runEntry struct {
	seq   int
	event obj
}

// runStream sequences one run's events and keeps a bounded backlog that
// subscribers replay from, as upstream _RunStream.
type runStream struct {
	mu       sync.Mutex
	backlog  []runEntry
	next     int
	terminal bool
	// wake is closed and replaced on every event.
	wake chan struct{}
	subs int
}

func newRunStream() *runStream { return &runStream{wake: make(chan struct{})} }

// put appends an event; nil marks the end. Nothing is added after the end.
func (st *runStream) put(ev obj) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.terminal {
		return
	}
	st.backlog = append(st.backlog, runEntry{seq: st.next, event: ev})
	st.next++
	if len(st.backlog) > runBacklog {
		st.backlog = append([]runEntry(nil), st.backlog[len(st.backlog)-runBacklog:]...)
	}
	if ev == nil {
		st.terminal = true
	}
	close(st.wake)
	st.wake = make(chan struct{})
}

// since returns the retained entries after seq and the channel that closes
// on the next event.
func (st *runStream) since(seq int) ([]runEntry, chan struct{}) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []runEntry
	for _, e := range st.backlog {
		if e.seq > seq {
			out = append(out, e)
		}
	}
	return out, st.wake
}

func (st *runStream) attach() { st.mu.Lock(); st.subs++; st.mu.Unlock() }
func (st *runStream) detach() { st.mu.Lock(); st.subs--; st.mu.Unlock() }

func (st *runStream) subscribers() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.subs
}

// handleRunEvents answers GET /v1/runs/{id}/events: the run's events as SSE,
// replayed after Last-Event-ID (or ?last_seq=), as upstream
// _handle_run_events.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.runs.mu.Lock()
	var st *runStream
	if rn := s.runs.runs[id]; rn != nil {
		st = rn.stream
	}
	s.runs.mu.Unlock()
	if st == nil {
		runNotFound(w, id)
		return
	}
	raw, requested := r.Header.Get("Last-Event-ID"), false
	if raw != "" {
		requested = true
	} else if q := r.URL.Query(); q.Has("last_seq") {
		raw, requested = q.Get("last_seq"), true
	}
	last := -1
	if n, err := strconv.Atoi(strings.TrimSpace(raw)); requested && err == nil {
		last = max(-1, n)
	}
	st.attach()
	defer st.detach()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	write := func(b string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(runWriteTimeout))
		if _, err := w.Write([]byte(b)); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	// The comment flushes the head before the first event, so a client that
	// subscribes early still sees the stream open.
	if !write(": open\n\n") {
		return
	}
	entries, wake := st.since(last)
	if len(entries) > 0 && entries[0].seq > last+1 {
		trunc := runEvent(id, "replay.truncated", s.now(), kv{"oldest_retained_seq", entries[0].seq})
		if requested {
			trunc = append(trunc, kv{"requested_seq", last})
		}
		if !write(sseData(trunc, -1)) {
			return
		}
	}
	idle := time.NewTimer(sseKeepalive)
	defer idle.Stop()
	for {
		for _, e := range entries {
			if e.event == nil {
				write(": stream closed\n\n")
				return
			}
			if !write(sseData(append(append(obj{}, e.event...), kv{"seq", e.seq}), e.seq)) {
				return
			}
			last = e.seq
		}
		if len(entries) > 0 {
			idle.Reset(sseKeepalive)
		}
		select {
		case <-wake:
		case <-idle.C:
			if !write(": keepalive\n\n") {
				return
			}
			idle.Reset(sseKeepalive)
		case <-r.Context().Done():
			return
		}
		entries, wake = st.since(last)
		if len(entries) > 0 && entries[0].seq > last+1 {
			// The subscriber fell behind the backlog: close it, as upstream
			// closes a slow subscriber.
			return
		}
	}
}

// sseData is upstream _sse_frame: an id line when seq is not negative, then
// the JSON data line.
func sseData(v any, seq int) string {
	data, _ := json.Marshal(v)
	if seq < 0 {
		return "data: " + string(data) + "\n\n"
	}
	return fmt.Sprintf("id: %d\ndata: %s\n\n", seq, data)
}
