// Package tuigateway serves the upstream Ink TUI (ui-tui) over its WebSocket
// JSON-RPC protocol, so the unmodified upstream client can drive this port's
// agent. The TUI attaches with HERMES_TUI_GATEWAY_URL pointing at /api/ws.
//
// The canonical wire is spoken: gateway.ready advertises session_authority, so
// the client runs the canonical handshake (runtime.describe, epochs) and picks
// and resumes conversations through session.list and session.resume. Turns run
// on session.create and prompt.submit, and stream as session events. The
// receipt ledger, prompt.receipt and event replay do not exist here, so the
// snapshot reports zero sequence, revision and generation and an empty queue:
// the client reads that as nothing in flight, which is the truth.
//
// Startup and poll methods the TUI calls are answered with empty results;
// anything else is refused with -32601 and the canonical unknown_method reason,
// as upstream does.
package tuigateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/state"
)

// Session is one live conversation the TUI drives.
type Session interface {
	// ID is the stored session id (stored_session_id on the wire).
	ID() string
	// Run executes one turn and returns the final reply text. The context
	// carries the live session, so approvals and clarify questions asked
	// during the turn reach the client that submitted it.
	Run(ctx context.Context, input string) (string, error)
	// End closes the session with a reason, as upstream "tui_close".
	End(reason string)
}

// SessionFactory opens a session. source is the upstream session source
// ("tui" unless the client names one); emit receives the agent's events while
// a turn runs.
type SessionFactory func(ctx context.Context, source string, emit func(agent.Event)) Session

// ResumeFactory opens a live conversation over an already stored session, so a
// resumed session keeps appending to the same transcript. storedID names the
// rows to continue. A nil factory makes session.resume refuse: answering a
// snapshot whose session cannot accept a turn would be worse than saying no.
type ResumeFactory func(ctx context.Context, source, storedID string, emit func(agent.Event)) Session

// Info is what session.create reports in its info field.
type Info struct {
	Model    string
	Provider string
	Cwd      string
	// Version must be non-empty: the TUI treats an empty version as a backend
	// that never finished starting.
	Version string
	// Profile is the profile id reported to a canonical client. Empty means
	// "default".
	Profile string
}

// Options configure a Server.
type Options struct {
	// Token is required; clients pass it as the token query parameter.
	Token string
	// NewSession is required.
	NewSession SessionFactory
	Info       Info
	// Store is the runtime's state.db. It backs the stored scope of
	// session.list and the resume of a stored session; nil leaves both empty.
	Store *state.Store
	// ResumeSession opens a stored session as a live one. Nil refuses resume.
	ResumeSession ResumeFactory
	// Logger receives debug and warning lines. Nil discards.
	Logger *slog.Logger
}

const (
	closeReason   = "tui_close"
	defaultSource = "tui"
	readLimit     = 16 << 20

	codeMethodNotFound = -32601
	codeInvalidParams  = 4000
	// codeCanonical is upstream's single code for every canonical failure; the
	// data.reason distinguishes invalid params, a missing session and a denied
	// profile.
	codeCanonical      = 4001
	codeNoSession      = 4001
	codeBadRequest     = 4002
	codeMissingSession = 4006
)

// Server is an http.Handler for /api/ws. It also implements clarify.Asker and
// approval.Prompter for the sessions it runs.
type Server struct {
	o Options

	base context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup

	mu       sync.Mutex
	sessions map[string]*liveSession
	requests map[string]*serverRequest
	conns    map[*conn]struct{}
	closed   bool

	// Canonical identity: instance_id names this process, authority_epoch is
	// the epoch a client folds into its own, and replay_epoch changes on every
	// start so a client resets its watermarks. The port keeps no durable epoch
	// (it has no receipts or admission ledger), so the epoch is constant.
	instanceID     string
	authorityEpoch int
	replayEpoch    string
}

// New builds a Server. It listens on nothing; mount it on an http.Server.
func New(o Options) (*Server, error) {
	if o.Token == "" {
		return nil, errors.New("tuigateway: Token is required")
	}
	if o.NewSession == nil {
		return nil, errors.New("tuigateway: NewSession is required")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	base, stop := context.WithCancel(context.Background())
	return &Server{
		o:              o,
		base:           base,
		stop:           stop,
		sessions:       map[string]*liveSession{},
		requests:       map[string]*serverRequest{},
		conns:          map[*conn]struct{}{},
		instanceID:     randomHex(8),
		authorityEpoch: 1,
		replayEpoch:    randomHex(8),
	}, nil
}

// Close disconnects every client, interrupts running turns, ends every
// session with "tui_close" and waits for the turns to return.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	sessions := make([]*liveSession, 0, len(s.sessions))
	for id, ls := range s.sessions {
		sessions = append(sessions, ls)
		delete(s.sessions, id)
	}
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.ws.CloseNow()
	}
	s.stop()
	s.wg.Wait()
	for _, ls := range sessions {
		ls.sess.End(closeReason)
	}
}

// ServeHTTP upgrades /api/ws?token=... and serves the connection until it
// closes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/ws" {
		http.NotFound(w, r)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(s.o.Token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.o.Logger.Warn("tui websocket accept failed", "err", err)
		return
	}
	ws.SetReadLimit(readLimit)
	c := &conn{ws: ws, logger: s.o.Logger}
	defer func() { _ = ws.CloseNow() }()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ws.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	s.serveConn(r.Context(), c)
}

// conn is one attached client. Writes are serialised; reads happen on the
// serving goroutine only.
type conn struct {
	ws     *websocket.Conn
	logger *slog.Logger
	mu     sync.Mutex
}

func (c *conn) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		c.logger.Warn("tui frame encode failed", "err", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ws.Write(context.Background(), websocket.MessageText, data); err != nil {
		c.logger.Debug("tui frame write failed", "err", err)
	}
}

func (c *conn) event(typ, sid string, payload any) {
	params := map[string]any{"type": typ, "payload": payload}
	if sid != "" {
		params["session_id"] = sid
	}
	c.write(map[string]any{"jsonrpc": "2.0", "method": "event", "params": params})
}

// frame is any inbound JSON-RPC message: a request, a notification or a
// response to one of our server requests.
type frame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// rpcError is a JSON-RPC error. Data carries the canonical machine-readable
// reason (upstream contracts/registry.py), so a client never has to parse the
// message text; legacy methods keep their historical codes.
type rpcError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

// Canonical failure reasons, as upstream contracts/registry.py.
const (
	reasonInvalidParams = "invalid_params"
	reasonNotFound      = "not_found"
	reasonUnknownMethod = "unknown_method"
)

// canonErr is a canonical error: upstream answers every canonical failure with
// code 4001 and a data.reason that names what went wrong.
func canonErr(reason, message string) *rpcError {
	return &rpcError{Code: codeCanonical, Message: message, Data: map[string]any{"reason": reason}}
}

func (s *Server) serveConn(ctx context.Context, c *conn) {
	// session_authority puts the client on the canonical wire: it folds the
	// epoch into its own and asks runtime.describe for the rest. It is
	// advertised only because session.list, session.resume and the canonical
	// error contract exist behind it.
	c.event("gateway.ready", "", map[string]any{
		"heartbeat":         true,
		"authority_epoch":   s.authorityEpoch,
		"replay_epoch":      s.replayEpoch,
		"session_authority": true,
		"skin":              nil,
		"change_events":     []any{},
	})
	defer s.detach(c)
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			s.o.Logger.Debug("tui connection closed", "err", err)
			return
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			s.o.Logger.Debug("tui frame ignored", "err", err)
			continue
		}
		if f.Method == "" {
			s.resolveResponse(f)
			continue
		}
		result, rerr := s.dispatch(c, f.Method, f.Params)
		if len(f.ID) == 0 || string(f.ID) == "null" {
			continue
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": f.ID}
		if rerr != nil {
			resp["error"] = rerr
		} else {
			resp["result"] = result
		}
		c.write(resp)
	}
}

// detach ends the sessions a closed connection owned and withdraws its open
// requests, so no turn waits on a client that is gone.
func (s *Server) detach(c *conn) {
	s.mu.Lock()
	delete(s.conns, c)
	var owned []*liveSession
	for id, ls := range s.sessions {
		if ls.conn == c {
			owned = append(owned, ls)
			delete(s.sessions, id)
		}
	}
	for _, req := range s.requests {
		if req.conn == c {
			s.finish(req, nil)
		}
	}
	s.mu.Unlock()
	// Closing waits for each running turn; do it off the read loop so a turn
	// slow to honour cancellation never stalls the connection.
	for _, ls := range owned {
		go ls.close()
	}
}

func (s *Server) dispatch(c *conn, method string, raw json.RawMessage) (any, *rpcError) {
	var params map[string]any
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, &rpcError{Code: codeBadRequest, Message: "params must be an object"}
		}
	}
	str := func(k string) string { v, _ := params[k].(string); return v }
	switch method {
	case "session.create":
		return s.createSession(c, str("source"))
	case "prompt.submit":
		return s.submit(params)
	case "session.interrupt":
		ls, rerr := s.session(str("session_id"))
		if rerr != nil {
			return nil, rerr
		}
		if ls.interrupt() {
			return map[string]any{"status": "interrupted", "interrupted": true}, nil
		}
		return map[string]any{"status": "not_interrupted", "interrupted": false}, nil
	case "session.close":
		return s.closeSession(str("session_id")), nil
	case "clarify.lock":
		return s.lockClarify(str("request_id"), str("question_id"), params["answer"])
	case "session.active_list":
		return s.activeList(c), nil
	case "runtime.describe":
		return s.describe(params)
	case "session.list":
		return s.listSessions(c, params)
	case "session.resume":
		return s.resumeSession(c, params)
	case "terminal.resize":
		cols, _ := params["cols"].(float64)
		return map[string]any{"cols": int(cols)}, nil
	case "config.get":
		switch str("key") {
		case "full":
			return map[string]any{"config": map[string]any{}}, nil
		case "mtime":
			return map[string]any{"mtime": 0}, nil
		}
		return map[string]any{"value": nil}, nil
	case "setup.status":
		return map[string]any{"provider_configured": true}, nil
	case "client.capabilities":
		return map[string]any{"server_requests": []string{"approval", "clarify"}, "declines_not_shown": false}, nil
	case "gateway.ping", "shared_metrics.startup_latency":
		return map[string]any{"ok": true}, nil
	case "wake.start":
		return map[string]any{"started": false, "reason": "unsupported"}, nil
	case "commands.catalog":
		return s.commandsCatalog(), nil
	case "complete.slash":
		return s.completeSlash(params)
	case "slash.exec":
		return s.slashExec(c, params)
	case "session.control.read":
		return map[string]any{"control": map[string]any{
			"goal": nil, "loop": nil, "heartbeat": nil, "revision": "", "updated_at": 0,
		}}, nil
	case "subagent.list":
		return map[string]any{"subagents": []any{}, "delegations": []any{}}, nil
	case "process.list":
		return map[string]any{"processes": []any{}}, nil
	case "system.battery":
		return map[string]any{"available": false}, nil
	case "reload.mcp":
		return map[string]any{"status": "reloaded"}, nil
	}
	// Upstream's exact refusal: the client keys its compatibility fallbacks on
	// this code and never on the message text. The version hint is genuinely
	// useful when the two drift, so it goes to the log instead of the wire.
	s.o.Logger.Warn("tui client asked for an unsupported method", "method", method)
	return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown method: " + method,
		Data: map[string]any{"reason": reasonUnknownMethod}}
}

func (s *Server) session(id string) (*liveSession, *rpcError) {
	if id == "" {
		return nil, &rpcError{Code: codeMissingSession, Message: "session_id required",
			Data: map[string]any{"reason": reasonInvalidParams}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ls, ok := s.sessions[id]
	if !ok {
		return nil, &rpcError{Code: codeNoSession, Message: "session not found",
			Data: map[string]any{"reason": reasonNotFound}}
	}
	return ls, nil
}

func (s *Server) createSession(c *conn, source string) (any, *rpcError) {
	if source == "" {
		source = defaultSource
	}
	ls := &liveSession{srv: s, conn: c, id: randomHex(4), started: time.Now()}
	ls.sess = s.o.NewSession(s.base, source, ls.emit)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ls.sess.End(closeReason)
		return nil, &rpcError{Code: codeBadRequest, Message: "server is shutting down"}
	}
	s.sessions[ls.id] = ls
	s.mu.Unlock()
	return map[string]any{
		"session_id":        ls.id,
		"stored_session_id": ls.sess.ID(),
		"message_count":     0,
		"messages":          []any{},
		"info":              s.info(ls),
	}, nil
}

func (s *Server) info(ls *liveSession) map[string]any {
	return map[string]any{
		"model":             s.o.Info.Model,
		"provider":          s.o.Info.Provider,
		"cwd":               s.o.Info.Cwd,
		"version":           s.o.Info.Version,
		"stored_session_id": ls.sess.ID(),
		"approval_mode":     "manual",
		"tools":             map[string]any{},
		"skills":            map[string]any{},
		"running":           false,
	}
}

func (s *Server) closeSession(id string) map[string]any {
	s.mu.Lock()
	ls, ok := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if ok {
		go ls.close()
	}
	return map[string]any{"closed": ok, "messages": []any{}}
}

// describe is the canonical handshake result (upstream
// gateway/session_controls.py): the identity a client folds into its own, the
// closed capability list it validates features against, and the launch
// parameters this backend honours. It takes no parameters.
func (s *Server) describe(params map[string]any) (any, *rpcError) {
	if len(params) > 0 {
		return nil, canonErr(reasonInvalidParams, "runtime.describe takes no parameters")
	}
	profile := s.o.Info.Profile
	if profile == "" {
		profile = "default"
	}
	return map[string]any{
		"instance_id":     s.instanceID,
		"profile_id":      profile,
		"authority_epoch": s.authorityEpoch,
		// Only the capabilities this port really implements: a client that
		// trusts an advertised feature would call a method that is not served.
		"capabilities": []any{"local-cli-create-v1"},
		"session_create": map[string]any{
			"sources":    []any{"tui", "cli"},
			"parameters": []any{},
		},
		"skin": nil,
	}, nil
}

// resumeParams is the closed parameter set of session.resume (upstream
// contracts/canonical_sessions.py SessionResumeParams). A key outside it is
// refused by name, so a client learns which field it sent wrong.
var resumeParams = map[string]bool{
	"profile": true, "session_id": true, "title": true, "latest": true, "workspace": true,
	"source": true, "editor": true, "api_key": true, "defer_history": true,
	"omit_messages": true, "cols": true, "display_projection": true,
}

// resumeSession is session.resume: it attaches the caller to a live session,
// or opens the stored one, and answers the canonical snapshot the client
// replays into its renderer.
func (s *Server) resumeSession(c *conn, params map[string]any) (any, *rpcError) {
	if rerr := checkResumeParams(params); rerr != nil {
		return nil, rerr
	}
	omit, _ := params["omit_messages"].(bool)
	deferHistory, _ := params["defer_history"].(bool)
	source, _ := params["source"].(string)
	if source == "" {
		source = defaultSource
	}
	sid, _ := params["session_id"].(string)
	title := ""
	if raw, ok := params["title"].(string); ok {
		title = strings.TrimSpace(raw)
	}

	// A live session is the resume: attaching must not fork the conversation.
	// The client resumes by the stored id it was given, so both the live id
	// and the stored id resolve to the same conversation.
	if sid != "" {
		if ls, err := s.session(sid); err == nil && ls.conn == c {
			return s.snapshot(ls, omit || deferHistory), nil
		}
		if ls := s.liveByStoredID(c, sid); ls != nil {
			return s.snapshot(ls, omit || deferHistory), nil
		}
	}
	storedID := sid
	if storedID == "" && title != "" {
		resolved, rerr := s.storedIDByTitle(title)
		if rerr != nil {
			return nil, rerr
		}
		storedID = resolved
	}
	if storedID == "" {
		return nil, canonErr(reasonInvalidParams, "session_id or title is required")
	}
	if s.o.Store == nil {
		return nil, canonErr("unavailable", "session database unavailable")
	}
	if _, err := s.o.Store.SessionRecord(s.base, storedID); err != nil {
		return nil, canonErr(reasonNotFound, "session not found")
	}
	if s.o.ResumeSession == nil {
		return nil, canonErr("unsupported", "this gateway cannot resume a stored session")
	}
	ls := &liveSession{srv: s, conn: c, id: randomHex(4), started: time.Now()}
	ls.sess = s.o.ResumeSession(s.base, source, storedID, ls.emit)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ls.sess.End(closeReason)
		return nil, canonErr("unavailable", "server is shutting down")
	}
	s.sessions[ls.id] = ls
	s.mu.Unlock()
	return s.snapshot(ls, omit || deferHistory), nil
}

// liveByStoredID is this connection's live session over storedID, if any.
func (s *Server) liveByStoredID(c *conn, storedID string) *liveSession {
	if storedID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ls := range s.sessions {
		if ls.conn == c && ls.sess.ID() == storedID {
			return ls
		}
	}
	return nil
}

// checkResumeParams refuses an unknown key by name.
func checkResumeParams(params map[string]any) *rpcError {
	var unknown []string
	for key := range params {
		if !resumeParams[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	fields := make([]any, 0, len(unknown))
	for _, key := range unknown {
		fields = append(fields, key)
	}
	return &rpcError{Code: codeCanonical, Message: "unknown session.resume fields: " + strings.Join(unknown, ", "),
		Data: map[string]any{"reason": reasonInvalidParams, "fields": fields}}
}

// storedIDByTitle resolves an exact stored title to its session id.
func (s *Server) storedIDByTitle(title string) (string, *rpcError) {
	if s.o.Store == nil {
		return "", canonErr("unavailable", "session database unavailable")
	}
	rows, err := s.o.Store.ListSessions(s.base, state.ListOptions{Limit: 200, IncludeHidden: true, Search: title})
	if err != nil {
		s.o.Logger.Warn("tui resume title lookup failed", "err", err)
		return "", canonErr("unavailable", "session database unavailable")
	}
	for _, row := range rows {
		if strings.TrimSpace(row.Title) == title {
			return row.ID, nil
		}
	}
	return "", canonErr(reasonNotFound, "session not found: "+title)
}

// snapshot is the canonical session snapshot (upstream CanonicalSnapshot). The
// port has no receipt ledger, event replay or pending admissions, so the
// sequence, revision, generation and queue fields are zero and empty: a client
// reads them as "nothing in flight", which is the truth here.
func (s *Server) snapshot(ls *liveSession, omitMessages bool) map[string]any {
	ls.mu.Lock()
	running := ls.running
	ls.mu.Unlock()
	storedID := ls.sess.ID()
	messages := []any{}
	count := 0
	if s.o.Store != nil && storedID != "" {
		rows, err := s.o.Store.ActiveMessages(s.base, storedID)
		if err != nil {
			s.o.Logger.Warn("tui resume history failed", "session", storedID, "err", err)
		} else {
			count = len(rows)
			if !omitMessages {
				messages = projectTranscript(rows)
			}
		}
	}
	return map[string]any{
		"session_id": ls.id, "stored_session_id": storedID,
		"messages": messages, "display_messages": nil, "message_count": count, "running": running,
		"authority_epoch": s.authorityEpoch, "replay_epoch": s.replayEpoch,
		"last_sequence": 0, "subscription_id": randomHex(6), "revision": 0, "execution_generation": 0,
		"pending": []any{}, "prompts": []any{}, "info": s.info(ls),
		"pending_connection": nil, "todo_state": nil, "open_requests": nil,
	}
}

// projectTranscript projects stored rows as upstream's TranscriptMessage: the
// renderable text plus the durable row id and the display and tool fields.
func projectTranscript(rows []state.ClientMessage) []any {
	out := make([]any, 0, len(rows))
	for _, m := range rows {
		msg := map[string]any{
			"role": m.Role, "text": m.Content, "timestamp": m.Timestamp, "row_id": m.ID,
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
			msg["content"] = m.Content
		}
		if m.ToolName != "" {
			msg["name"] = m.ToolName
		}
		if m.DisplayKind != "" {
			msg["display_kind"] = m.DisplayKind
		}
		if m.Reasoning != "" {
			msg["reasoning"] = m.Reasoning
		}
		out = append(out, msg)
	}
	return out
}

// listSessions is the canonical session picker (upstream
// gateway/session_controls.py / contracts/canonical_sessions.py): the live
// sessions this connection owns, or the stored rows of state.db. A title
// filter searches stored rows only, so it always answers the stored scope.
func (s *Server) listSessions(c *conn, params map[string]any) (any, *rpcError) {
	scope, rerr := listScope(params)
	if rerr != nil {
		return nil, rerr
	}
	limit, rerr := listLimit(params)
	if rerr != nil {
		return nil, rerr
	}
	title, rerr := listTitle(params)
	if rerr != nil {
		return nil, rerr
	}
	includeHidden, rerr := listIncludeHidden(params)
	if rerr != nil {
		return nil, rerr
	}
	if title != "" {
		if _, scoped := params["scope"]; scoped && params["scope"] != nil {
			return nil, canonErr(reasonInvalidParams, "title cannot be combined with scope")
		}
		rows, rerr := s.storedRows(limit, includeHidden, title)
		return map[string]any{"sessions": rows, "scope": "stored"}, rerr
	}
	if scope == "stored" {
		rows, rerr := s.storedRows(limit, includeHidden, "")
		return map[string]any{"sessions": rows, "scope": "stored"}, rerr
	}
	return map[string]any{"sessions": s.liveRows(c, limit), "scope": "live"}, nil
}

// listScope validates the scope parameter, defaulting to live.
func listScope(params map[string]any) (string, *rpcError) {
	raw, ok := params["scope"]
	if !ok || raw == nil {
		return "live", nil
	}
	text, isString := raw.(string)
	if !isString || (text != "live" && text != "stored") {
		return "", canonErr(reasonInvalidParams, "scope must be 'live' or 'stored'")
	}
	return text, nil
}

// listLimit validates the page size, defaulting to 200 as upstream does.
func listLimit(params map[string]any) (int, *rpcError) {
	raw, ok := params["limit"]
	if !ok || raw == nil {
		return 200, nil
	}
	value, isNumber := raw.(float64)
	if !isNumber || value != math.Trunc(value) || value < 1 || value > 200 {
		return 0, canonErr(reasonInvalidParams, "limit must be an integer between 1 and 200")
	}
	return int(value), nil
}

// listTitle reads the exact-title filter.
func listTitle(params map[string]any) (string, *rpcError) {
	raw, ok := params["title"]
	if !ok || raw == nil {
		return "", nil
	}
	text, isString := raw.(string)
	if !isString {
		return "", canonErr(reasonInvalidParams, "title must be a string")
	}
	return strings.TrimSpace(text), nil
}

// listIncludeHidden reads the hidden-row opt-in.
func listIncludeHidden(params map[string]any) (bool, *rpcError) {
	raw, ok := params["include_hidden"]
	if !ok || raw == nil {
		return false, nil
	}
	value, isBool := raw.(bool)
	if !isBool {
		return false, canonErr(reasonInvalidParams, "include_hidden must be a boolean")
	}
	return value, nil
}

// liveRows are this connection's sessions, newest first. A session belongs to
// the connection that created it, so another client's conversation is never
// listed here.
func (s *Server) liveRows(c *conn, limit int) []any {
	s.mu.Lock()
	live := make([]*liveSession, 0, len(s.sessions))
	for _, ls := range s.sessions {
		if ls.conn == c {
			live = append(live, ls)
		}
	}
	s.mu.Unlock()
	sort.Slice(live, func(i, j int) bool { return live[i].started.After(live[j].started) })
	if len(live) > limit {
		live = live[:limit]
	}
	rows := make([]any, 0, len(live))
	for _, ls := range live {
		ls.mu.Lock()
		running := ls.running
		ls.mu.Unlock()
		rows = append(rows, map[string]any{
			"session_id": ls.id, "id": ls.id, "title": "",
			"source": "tui", "started_at": unixSeconds(ls.started), "message_count": 0,
			"running": running, "model": s.o.Info.Model,
			"resolved_id": ls.sess.ID(), "root_title": nil,
		})
	}
	return rows
}

// storedRows are the state.db rows of the stored scope.
func (s *Server) storedRows(limit int, includeHidden bool, title string) ([]any, *rpcError) {
	if s.o.Store == nil {
		return []any{}, nil
	}
	records, err := s.o.Store.ListSessions(s.base, state.ListOptions{
		Limit: limit, IncludeHidden: includeHidden, Search: title,
	})
	if err != nil {
		s.o.Logger.Warn("tui session list failed", "err", err)
		return nil, canonErr("unavailable", "session database unavailable")
	}
	rows := make([]any, 0, len(records))
	for _, r := range records {
		// A title filter is exact on this surface, as the picker's is.
		if title != "" && strings.TrimSpace(r.Title) != title {
			continue
		}
		rows = append(rows, map[string]any{
			"session_id": r.ID, "id": r.ID, "title": r.Title, "source": r.Source,
			"started_at": r.StartedAt, "message_count": r.MessageCount, "running": false,
			"model": nilIfEmpty(r.Model), "resolved_id": nil, "root_title": nil,
		})
	}
	return rows, nil
}

// nilIfEmpty keeps an absent value null on the wire instead of an empty string.
func nilIfEmpty(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// unixSeconds is a time as Python time.time().
func unixSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func (s *Server) activeList(c *conn) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := []any{}
	for _, ls := range s.sessions {
		if ls.conn != c {
			continue
		}
		items = append(items, map[string]any{
			"current": false, "id": ls.id, "last_active": 0, "message_count": 0,
			"model": s.o.Info.Model, "preview": "", "session_key": ls.sess.ID(),
		})
	}
	return map[string]any{"sessions": items}
}

// submit starts a turn, or queues the input behind the running one, as
// upstream's busy path does. submission_id belongs to the canonical wire:
// upstream's legacy contract refuses it as an unknown key with 4000, and the
// TUI then retries without it.
func (s *Server) submit(params map[string]any) (any, *rpcError) {
	if _, ok := params["submission_id"]; ok {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params for prompt.submit: submission_id: Extra inputs are not permitted" +
			" — the client and the Hermes backend are out of sync (different versions); run `hermes update` and restart both",
			Data: map[string]any{"reason": reasonInvalidParams}}
	}
	sid, _ := params["session_id"].(string)
	ls, rerr := s.session(sid)
	if rerr != nil {
		return nil, rerr
	}
	var text string
	switch v := params["text"].(type) {
	case string:
		text = v
	case nil:
	default:
		data, _ := json.Marshal(v)
		text = string(data)
	}
	if ls.start(text) {
		return map[string]any{"status": "streaming"}, nil
	}
	return map[string]any{"status": "queued"}, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
