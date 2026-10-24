package main

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// tuiClient is the smallest client of the TUI wire: it reads frames, skips
// the ones it is not waiting for and sends requests with increasing ids.
type tuiClient struct {
	t    *testing.T
	conn *websocket.Conn
	id   int
}

func (c *tuiClient) next() map[string]any {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var f map[string]any
	if err := json.Unmarshal(data, &f); err != nil {
		c.t.Fatal(err)
	}
	return f
}

func (c *tuiClient) call(method string, params map[string]any) map[string]any {
	c.t.Helper()
	c.id++
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if err := c.conn.Write(context.Background(), websocket.MessageText, data); err != nil {
		c.t.Fatal(err)
	}
	for {
		if f := c.next(); f["id"] == float64(c.id) {
			res, ok := f["result"].(map[string]any)
			if !ok {
				c.t.Fatalf("%s = %v", method, f)
			}
			return res
		}
	}
}

func (c *tuiClient) event(typ string) map[string]any {
	c.t.Helper()
	for {
		f := c.next()
		if p, _ := f["params"].(map[string]any); f["method"] == "event" && p["type"] == typ {
			payload, _ := p["payload"].(map[string]any)
			return payload
		}
	}
}

func TestTUIServesTheUpstreamWire(t *testing.T) {
	model := deltasModel(t, "Hel", "lo")
	f := newHome(t, modelConfig(model.URL))
	old := version
	version = "1.2.3"
	t.Cleanup(func() { version = old })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	urls := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- runTUI(ctx, tuiFlags{addr: "127.0.0.1:0"}, f.env(), func(u string) { urls <- u })
	}()

	var gatewayURL string
	select {
	case gatewayURL = <-urls:
	case err := <-done:
		t.Fatalf("runTUI returned early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no gateway URL")
	}
	u, err := url.Parse(gatewayURL)
	if err != nil || u.Scheme != "ws" || u.Path != "/api/ws" || len(u.Query().Get("token")) < 32 ||
		!strings.HasPrefix(u.Host, "127.0.0.1:") {
		t.Fatalf("gateway URL = %q", gatewayURL)
	}

	conn, _, err := websocket.Dial(ctx, gatewayURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	c := &tuiClient{t: t, conn: conn}
	c.event("gateway.ready")
	created := c.call("session.create", map[string]any{})
	sid := created["session_id"].(string)
	if info := created["info"].(map[string]any); info["version"] != "1.2.3" || info["model"] != "tiny" {
		t.Errorf("info = %v", info)
	}
	c.call("prompt.submit", map[string]any{"session_id": sid, "text": "hi"})
	if p := c.event("message.delta"); p["text"] != "Hel" {
		t.Errorf("message.delta = %v", p)
	}
	if p := c.event("message.complete"); p["text"] != "Hello" || p["status"] != "complete" {
		t.Errorf("message.complete = %v", p)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runTUI = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runTUI did not stop")
	}
	list, _ := sessions(t, f)
	if len(list) != 1 || list[0].Source != "tui" || list[0].EndReason != "tui_close" || list[0].MessageCount != 2 {
		t.Fatalf("sessions = %+v", list)
	}
}

func TestTUIRejectsAMissingClientBuild(t *testing.T) {
	model := deltasModel(t, "unused")
	f := newHome(t, modelConfig(model.URL))
	code := Run(context.Background(), []string{"tui", "--tui-dir", f.cwd}, f.env())
	if code != 1 || !strings.Contains(f.stderr.String(), "npm run build:ink") {
		t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
	}
}
