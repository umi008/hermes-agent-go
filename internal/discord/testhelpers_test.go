package discord

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/gateway"
)

const testTimeout = 10 * time.Second

// fakeGWConn is the server side of one test WebSocket connection: it reads
// masked client frames and writes unmasked server frames.
type fakeGWConn struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	req  *http.Request
}

func (c *fakeGWConn) sendFrame(op byte, payload []byte, fin bool) {
	c.t.Helper()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := writeFrame(c.conn, op, payload, fin, false); err != nil {
		c.t.Errorf("fake gateway: write frame 0x%x: %v", op, err)
	}
}

func (c *fakeGWConn) sendJSON(v any) {
	c.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		c.t.Errorf("fake gateway: encode payload: %v", err)
		return
	}
	c.sendFrame(wsOpText, data, true)
}

func (c *fakeGWConn) sendDispatch(event string, seq int64, data any) {
	c.sendJSON(map[string]any{"op": gwOpDispatch, "t": event, "s": seq, "d": data})
}

func (c *fakeGWConn) sendClose(code int, reason string) {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, uint16(code))
	copy(payload[2:], reason)
	c.sendFrame(wsOpClose, payload, true)
}

func (c *fakeGWConn) next(timeout time.Duration) (wsFrame, bool) {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	frame, err := readFrame(c.br, true)
	if err != nil {
		c.t.Logf("fake gateway: read frame: %v", err)
		return wsFrame{}, false
	}
	return frame, true
}

// nextJSON reads text frames until one arrives and decodes it.
func (c *fakeGWConn) nextJSON(timeout time.Duration) (map[string]any, bool) {
	c.t.Helper()
	for {
		frame, ok := c.next(timeout)
		if !ok {
			return nil, false
		}
		if frame.opcode != wsOpText {
			c.t.Logf("fake gateway: skipping frame with opcode 0x%x", frame.opcode)
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(frame.payload, &payload); err != nil {
			c.t.Errorf("fake gateway: decode frame payload: %v", err)
			return nil, false
		}
		return payload, true
	}
}

// expectOp reads messages until the next gateway op matches op.
func (c *fakeGWConn) expectOp(op int, timeout time.Duration) (map[string]any, bool) {
	c.t.Helper()
	for {
		payload, ok := c.nextJSON(timeout)
		if !ok {
			return nil, false
		}
		if got := opOf(payload); got != op {
			c.t.Logf("fake gateway: skipping op %d while waiting for op %d", got, op)
			continue
		}
		return payload, true
	}
}

func opOf(payload map[string]any) int {
	value, _ := payload["op"].(float64)
	return int(value)
}

func objectOf(payload map[string]any, key string) map[string]any {
	value, _ := payload[key].(map[string]any)
	return value
}

// holdConnection blocks until the client closes the connection.
func (c *fakeGWConn) holdConnection() {
	c.t.Helper()
	for {
		if _, ok := c.next(5 * time.Second); !ok {
			return
		}
	}
}

// fakeGateway is a raw TCP server that performs the WebSocket upgrade by hand
// and then runs handler per connection in order.
type fakeGateway struct {
	t   *testing.T
	ln  net.Listener
	url string

	mu       sync.Mutex
	accepted int
	wg       sync.WaitGroup
}

func startFakeGateway(t *testing.T, handler func(idx int, c *fakeGWConn)) *fakeGateway {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gw := &fakeGateway{t: t, ln: ln, url: "ws://" + ln.Addr().String()}
	go gw.serve(handler)
	t.Cleanup(func() {
		_ = ln.Close()
		gw.wg.Wait()
	})
	return gw
}

func (g *fakeGateway) serve(handler func(idx int, c *fakeGWConn)) {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		g.mu.Lock()
		idx := g.accepted
		g.accepted++
		g.mu.Unlock()
		g.wg.Add(1)
		go func(conn net.Conn, idx int) {
			defer g.wg.Done()
			defer func() { _ = conn.Close() }()
			c, err := upgradeFakeConn(conn)
			if err != nil {
				g.t.Errorf("fake gateway: upgrade connection %d: %v", idx, err)
				return
			}
			c.t = g.t
			handler(idx, c)
		}(conn, idx)
	}
}

func (g *fakeGateway) acceptedCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.accepted
}

// upgradeFakeConn reads the client's HTTP upgrade, answers with a handshake
// whose Sec-WebSocket-Accept is computed the way RFC 6455 requires, and returns
// the frame connection.
func upgradeFakeConn(conn net.Conn) (*fakeGWConn, error) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return nil, fmt.Errorf("read upgrade request: %w", err)
	}
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("upgrade method = %s, want GET", req.Method)
	}
	if !headerHasToken(req.Header.Get("Connection"), "upgrade") {
		return nil, fmt.Errorf("missing Connection: Upgrade")
	}
	if got := req.Header.Get("Upgrade"); !equalFold(got, "websocket") {
		return nil, fmt.Errorf("Upgrade = %q, want websocket", got)
	}
	if got := req.Header.Get("Sec-WebSocket-Version"); got != "13" {
		return nil, fmt.Errorf("Sec-WebSocket-Version = %q, want 13", got)
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAccept(key) + "\r\n\r\n"
	if _, err := io.WriteString(conn, response); err != nil {
		return nil, fmt.Errorf("write upgrade response: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return &fakeGWConn{conn: conn, br: br, req: req}, nil
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// restCall is one recorded POST /channels/{id}/messages request.
type restCall struct {
	Path          string
	Content       string
	Authorization string
	ContentType   string
}

// restServer is an httptest server that records message posts and can answer
// the first request with 429.
type restServer struct {
	srv *httptest.Server

	mu             sync.Mutex
	calls          []restCall
	seen           int
	rateLimitFirst float64
}

func newRESTServer(t *testing.T) *restServer {
	t.Helper()
	r := &restServer{}
	r.srv = httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *restServer) handle(w http.ResponseWriter, req *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(req.Body, maxErrorBodyRead))
	var payload struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(raw, &payload)

	r.mu.Lock()
	r.seen++
	requestNumber := r.seen
	r.calls = append(r.calls, restCall{
		Path:          req.URL.Path,
		Content:       payload.Content,
		Authorization: req.Header.Get("Authorization"),
		ContentType:   req.Header.Get("Content-Type"),
	})
	rateLimit := r.rateLimitFirst
	r.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if rateLimit > 0 && requestNumber == 1 {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message":     "You are being rate limited.",
			"retry_after": rateLimit,
			"global":      false,
		})
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"id":"1","channel_id":"c"}`)
}

func (r *restServer) recorded() []restCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]restCall, len(r.calls))
	copy(out, r.calls)
	return out
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestAdapter(t *testing.T, opts Options) *Adapter {
	t.Helper()
	if opts.Token == "" {
		opts.Token = "test-token"
	}
	if opts.Logger == nil {
		opts.Logger = testLogger(t)
	}
	adapter, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return adapter
}

// adapterRun drives Adapter.Run for the duration of the test.
type adapterRun struct {
	cancel context.CancelFunc
	done   chan error
}

func startAdapter(t *testing.T, adapter *Adapter, handle func(gateway.Inbound) error) *adapterRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &adapterRun{cancel: cancel, done: make(chan error, 1)}
	go func() { run.done <- adapter.Run(ctx, handle) }()
	t.Cleanup(func() { run.stop(t) })
	return run
}

func (r *adapterRun) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Errorf("Run returned an error after cancel: %v", err)
		}
	case <-time.After(testTimeout):
		t.Errorf("Run did not return after the context was cancelled")
	}
}

// channelInbox collects inbound messages for a test.
func channelInbox() chan gateway.Inbound {
	return make(chan gateway.Inbound, 16)
}

func receiveInbound(t *testing.T, inbox <-chan gateway.Inbound) gateway.Inbound {
	t.Helper()
	select {
	case in := <-inbox:
		return in
	case <-time.After(testTimeout):
		t.Fatalf("timed out waiting for an inbound message")
		return gateway.Inbound{}
	}
}

func expectNoInbound(t *testing.T, inbox <-chan gateway.Inbound) {
	t.Helper()
	select {
	case in := <-inbox:
		t.Fatalf("unexpected inbound message: %+v", in)
	case <-time.After(200 * time.Millisecond):
	}
}

// messagePayload builds a MESSAGE_CREATE payload.
func messagePayload(opts messageOptions) map[string]any {
	author := map[string]any{
		"id":       opts.authorID,
		"username": opts.username,
		"bot":      opts.bot,
	}
	if opts.globalName != "" {
		author["global_name"] = opts.globalName
	}
	payload := map[string]any{
		"id":         opts.id,
		"channel_id": opts.channelID,
		"content":    opts.content,
		"type":       opts.messageType,
		"author":     author,
	}
	if opts.guildID != "" {
		payload["guild_id"] = opts.guildID
	}
	if opts.nick != "" {
		payload["member"] = map[string]any{"nick": opts.nick}
	}
	if opts.mentions != nil {
		payload["mentions"] = opts.mentions
	}
	return payload
}

type messageOptions struct {
	id          string
	channelID   string
	guildID     string
	content     string
	authorID    string
	username    string
	globalName  string
	nick        string
	bot         bool
	messageType int
	mentions    []map[string]any
}

// decodeMessage mirrors the production path: a Gateway event's data field is
// unmarshalled into discordMessage.
func decodeMessage(t *testing.T, payload map[string]any) discordMessage {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode message payload: %v", err)
	}
	var msg discordMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("decode message payload: %v", err)
	}
	return msg
}
