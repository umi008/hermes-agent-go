package tuigateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/umi008/hermes-agent-go/internal/tools/approval"
	"github.com/umi008/hermes-agent-go/internal/tools/clarify"
)

// serverRequest is a server→client request waiting for its answer. Its
// fields after creation are guarded by Server.mu.
type serverRequest struct {
	id     string
	method string
	conn   *conn
	// qids and locked track a clarify batch: the question ids, and the answers
	// clarify.lock accepted so far.
	qids   []string
	locked map[string]any
	// done receives the result once; nil means nobody answered.
	done chan json.RawMessage
}

// finish settles the request and forgets it; Server.mu is held.
func (s *Server) finish(req *serverRequest, result json.RawMessage) {
	if _, open := s.requests[req.id]; !open {
		return
	}
	delete(s.requests, req.id)
	req.done <- result
}

var errNoSession = errors.New("tuigateway: no TUI session in context")

// request sends method to the client that runs the session in ctx and waits
// for its answer. Cancelling ctx withdraws the request with request.cancel.
func (s *Server) request(ctx context.Context, method string, params map[string]any, qids []string) (json.RawMessage, error) {
	ls, ok := sessionFrom(ctx)
	if !ok {
		return nil, errNoSession
	}
	req := &serverRequest{
		id: "srq-" + randomHex(6), method: method, conn: ls.conn,
		qids: qids, locked: map[string]any{}, done: make(chan json.RawMessage, 1),
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("tuigateway: server closed")
	}
	s.requests[req.id] = req
	s.mu.Unlock()

	wire := map[string]any{"session_id": ls.id}
	for k, v := range params {
		wire[k] = v
	}
	ls.conn.write(map[string]any{"jsonrpc": "2.0", "id": req.id, "method": method, "params": wire})

	select {
	case res := <-req.done:
		if res == nil {
			return nil, fmt.Errorf("tuigateway: the client did not answer %s", method)
		}
		return res, nil
	case <-ctx.Done():
		s.mu.Lock()
		s.finish(req, nil)
		s.mu.Unlock()
		ls.conn.event("request.cancel", ls.id, map[string]any{"id": req.id, "method": method, "reason": "cancelled"})
		return nil, ctx.Err()
	}
}

// resolveResponse settles a request from the client's response frame. An
// error response means the client cannot answer it.
func (s *Server) resolveResponse(f frame) {
	var id string
	if err := json.Unmarshal(f.ID, &id); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.requests[id]
	if !ok {
		return
	}
	switch {
	case len(f.Error) > 0 && string(f.Error) != "null":
		s.finish(req, nil)
	case len(f.Result) == 0 || string(f.Result) == "null":
		s.finish(req, json.RawMessage(`{}`))
	default:
		s.finish(req, f.Result)
	}
}

// Approve asks the client running the turn in ctx. Permanent approval is not
// offered, as in the rest of this port. A smart deny reduces the offer to
// once/deny: the guardian already judged the command dangerous, so a scope the
// gate would discard must never be presented.
func (s *Server) Approve(ctx context.Context, r approval.Request) (approval.Choice, error) {
	params := map[string]any{
		"request_id":      randomHex(6),
		"command":         r.Command,
		"description":     r.Description,
		"choices":         []string{"once", "session", "deny"},
		"allow_permanent": false,
	}
	if r.SmartDenied {
		params["choices"] = []string{"once", "deny"}
		params["smart_denied"] = true
	}
	res, err := s.request(ctx, "approval", params, nil)
	if err != nil {
		return approval.Deny, err
	}
	var answer struct {
		Choice string `json:"choice"`
	}
	_ = json.Unmarshal(res, &answer)
	if r.SmartDenied && answer.Choice != "once" {
		// An answer the offer did not include resolves as a denial.
		return approval.Deny, nil
	}
	switch answer.Choice {
	case "once":
		return approval.Once, nil
	case "session":
		return approval.Session, nil
	case "always":
		return approval.Always, nil
	}
	return approval.Deny, nil
}

// Ask sends one clarify request for the batch. Answers arrive one at a time
// through clarify.lock, or all at once in the response frame; a response
// without answers cancels the batch.
func (s *Server) Ask(ctx context.Context, qs []clarify.Question) (clarify.Reply, error) {
	wire := make([]map[string]any, 0, len(qs))
	qids := make([]string, 0, len(qs))
	for _, q := range qs {
		w := map[string]any{"qid": q.ID, "question": q.Text, "multi_select": q.MultiSelect}
		if len(q.Choices) > 0 {
			w["choices"] = q.Choices
		}
		wire = append(wire, w)
		qids = append(qids, q.ID)
	}
	res, err := s.request(ctx, "clarify", map[string]any{"questions": wire}, qids)
	if err != nil {
		if ctx.Err() != nil {
			return clarify.Reply{}, err
		}
		return clarify.Reply{Outcome: "undelivered"}, nil
	}
	var answer struct {
		Answers map[string]any `json:"answers"`
		Outcome string         `json:"outcome"`
	}
	_ = json.Unmarshal(res, &answer)
	if answer.Answers == nil {
		return clarify.Reply{Outcome: "cancelled"}, nil
	}
	answers := make(map[string]any, len(answer.Answers))
	for k, v := range answer.Answers {
		answers[k] = normalizeAnswer(v)
	}
	outcome := answer.Outcome
	if outcome == "" {
		outcome = "submitted"
	}
	return clarify.Reply{Answers: answers, Outcome: outcome}, nil
}

// lockClarify records one answer of an open clarify batch. The lock that
// leaves nothing remaining resolves the request; a lock on a request that
// already ended is "expired", not an error.
func (s *Server) lockClarify(requestID, qid string, answer any) (any, *rpcError) {
	if requestID == "" || qid == "" {
		return nil, &rpcError{Code: codeBadRequest, Message: "request_id and question_id required"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.requests[requestID]
	if !ok {
		return map[string]any{"status": "expired"}, nil
	}
	known := false
	for _, id := range req.qids {
		known = known || id == qid
	}
	if req.method != "clarify" || !known {
		return nil, &rpcError{Code: codeBadRequest, Message: fmt.Sprintf("unknown question %q for request %s", qid, requestID)}
	}
	req.locked[qid] = normalizeAnswer(answer)
	remaining := []string{}
	for _, id := range req.qids {
		if _, done := req.locked[id]; !done {
			remaining = append(remaining, id)
		}
	}
	if len(remaining) == 0 {
		data, _ := json.Marshal(map[string]any{"answers": req.locked})
		s.finish(req, data)
	}
	return map[string]any{"status": "ok", "remaining": remaining}, nil
}

// normalizeAnswer keeps strings and nil (skipped), turns a list into
// []string for multi-select questions and JSON-encodes anything else, as
// upstream does with non-string answers.
func normalizeAnswer(v any) any {
	switch a := v.(type) {
	case nil, string:
		return a
	case []any:
		out := make([]string, 0, len(a))
		for _, item := range a {
			if s, ok := item.(string); ok {
				out = append(out, s)
			} else {
				data, _ := json.Marshal(item)
				out = append(out, string(data))
			}
		}
		return out
	}
	data, _ := json.Marshal(v)
	return string(data)
}
