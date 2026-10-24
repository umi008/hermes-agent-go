// Package tuigateway serves the upstream Ink TUI (ui-tui) over its WebSocket
// JSON-RPC protocol, so the unmodified upstream client can drive this port's
// agent. The TUI attaches with HERMES_TUI_GATEWAY_URL pointing at /api/ws.
//
// Only the legacy wire is spoken: gateway.ready never advertises
// session_authority, so the client skips the canonical handshake (receipts,
// epochs) and uses session.create, prompt.submit and session events. Startup
// and poll methods the TUI calls are answered with empty results; anything
// else is refused with -32601 as upstream does.
package tuigateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"github.com/umi008/hermes-agent-go/internal/agent"
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

// Info is what session.create reports in its info field.
type Info struct {
	Model    string
	Provider string
	Cwd      string
	// Version must be non-empty: the TUI treats an empty version as a backend
	// that never finished starting.
	Version string
}

// Options configure a Server.
type Options struct {
	// Token is required; clients pass it as the token query parameter.
	Token string
	// NewSession is required.
	NewSession SessionFactory
	Info       Info
	// Logger receives debug and warning lines. Nil discards.
	Logger *slog.Logger
}

const (
	closeReason   = "tui_close"
	defaultSource = "tui"
	readLimit     = 16 << 20

	codeMethodNotFound = -32601
	codeInvalidParams  = 4000
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
		o:        o,
		base:     base,
		stop:     stop,
		sessions: map[string]*liveSession{},
		requests: map[string]*serverRequest{},
		conns:    map[*conn]struct{}{},
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

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) serveConn(ctx context.Context, c *conn) {
	c.event("gateway.ready", "", map[string]any{"heartbeat": true})
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
	for _, ls := range owned {
		ls.close()
	}
}

func (s *Server) dispatch(c *conn, method string, raw json.RawMessage) (any, *rpcError) {
	var params map[string]any
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, &rpcError{codeBadRequest, "params must be an object"}
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
		return map[string]any{
			"pairs": []any{}, "sub": map[string]any{}, "canon": map[string]any{}, "commands": map[string]any{},
			"categories": []any{}, "skills": map[string]any{}, "skill_count": 0, "warning": "",
			"plugin_discovery_error": "", "plugin_commands": map[string]any{},
		}, nil
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
	return nil, &rpcError{codeMethodNotFound, fmt.Sprintf("unknown method: %s — the client and the Hermes backend are out of sync "+
		"(different versions); run `hermes update` and restart both", method)}
}

func (s *Server) session(id string) (*liveSession, *rpcError) {
	if id == "" {
		return nil, &rpcError{codeMissingSession, "session_id required"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ls, ok := s.sessions[id]
	if !ok {
		return nil, &rpcError{codeNoSession, "session not found"}
	}
	return ls, nil
}

func (s *Server) createSession(c *conn, source string) (any, *rpcError) {
	if source == "" {
		source = defaultSource
	}
	ls := &liveSession{srv: s, conn: c, id: randomHex(4)}
	ls.sess = s.o.NewSession(s.base, source, ls.emit)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ls.sess.End(closeReason)
		return nil, &rpcError{codeBadRequest, "server is shutting down"}
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
		ls.close()
	}
	return map[string]any{"closed": ok, "messages": []any{}}
}

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
		return nil, &rpcError{codeInvalidParams, "invalid params for prompt.submit: submission_id: Extra inputs are not permitted" +
			" — the client and the Hermes backend are out of sync (different versions); run `hermes update` and restart both"}
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
