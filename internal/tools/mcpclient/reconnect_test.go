package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// waits records the backoff delays instead of sleeping; then runs after
// each recorded delay.
type waits struct {
	mu   sync.Mutex
	got  []time.Duration
	then func(n int)
}

func (w *waits) wait(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	w.got = append(w.got, d)
	n := len(w.got)
	then := w.then
	w.mu.Unlock()
	if then != nil {
		then(n)
	}
	return ctx.Err()
}

func (w *waits) delays() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.got...)
}

func (m *memory) setFail(err error) {
	m.mu.Lock()
	m.fail = err
	m.mu.Unlock()
}

func (m *memory) drop(i int) {
	m.mu.Lock()
	ss := m.sessions[i]
	m.mu.Unlock()
	_ = ss.Close()
}

func dialMemory(t *testing.T, s Server, srv *mcp.Server, w *waits) (*Manager, *memory) {
	t.Helper()
	mem := &memory{t: t, srv: srv}
	s.transport = mem.transport
	m := Connect(t.Context(), Options{Servers: []Server{s}, wait: w.wait, respawnWait: 5 * time.Second})
	t.Cleanup(func() { _ = m.Close() })
	return m, mem
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// within reports whether d is base with upstream's +/-20% jitter.
func within(d, base time.Duration) bool {
	return d >= base*8/10 && d <= base*12/10
}

func echo(t *testing.T, m *Manager, server string) (string, error) {
	t.Helper()
	return byName(m.Tools())[ToolName(server, "echo")].Handler(t.Context(), json.RawMessage(`{"text":"x"}`))
}

func TestReconnectAfterDrop(t *testing.T) {
	w := &waits{}
	m, mem := dialMemory(t, Server{Name: "s"}, testServer(t), w)
	mem.drop(0)
	eventually(t, "a second dial", func() bool { return mem.dials() == 2 })
	out, err := echo(t, m, "s")
	if err != nil || !strings.HasPrefix(out, `{"result":"echo: x"`) {
		t.Fatalf("after reconnect: %s %v", out, err)
	}
	if d := w.delays(); len(d) != 1 || !within(d[0], time.Second) {
		t.Fatalf("delays = %v, want one ~1s backoff", d)
	}
}

func TestReconnectBackoffParksThenProbes(t *testing.T) {
	w := &waits{}
	m, mem := dialMemory(t, Server{Name: "s"}, testServer(t), w)
	mem.setFail(errors.New("connection refused"))
	// The park probe waits for the test to see the parked state.
	parked, release := make(chan struct{}), make(chan struct{})
	w.then = func(n int) {
		if n == 6 {
			close(parked)
			<-release
			mem.setFail(nil)
		}
	}
	mem.drop(0)
	<-parked
	_, err := echo(t, m, "s")
	if err == nil || !strings.Contains(err.Error(), "MCP server 's' is not connected") {
		t.Fatalf("parked call err = %v", err)
	}
	close(release)
	eventually(t, "the probe reconnect", func() bool { return mem.dials() == 2 })
	if _, err := echo(t, m, "s"); err != nil {
		t.Fatalf("after probe: %v", err)
	}
	d := w.delays()
	for i, base := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second} {
		if !within(d[i], base) {
			t.Errorf("delay %d = %v, want ~%v", i, d[i], base)
		}
	}
	if d[5] != 300*time.Second {
		t.Errorf("park probe = %v, want 300s", d[5])
	}
}

func TestProvenSessionResetsBackoff(t *testing.T) {
	w := &waits{}
	m, mem := dialMemory(t, Server{Name: "s"}, testServer(t), w)
	mem.drop(0)
	eventually(t, "a second dial", func() bool { return mem.dials() == 2 })
	if _, err := echo(t, m, "s"); err != nil {
		t.Fatal(err)
	}
	mem.drop(1)
	eventually(t, "a third dial", func() bool { return mem.dials() == 3 })
	if d := w.delays(); len(d) != 2 || !within(d[1], time.Second) {
		t.Fatalf("delays = %v, want the budget reset to ~1s", d)
	}
}

func TestUnprovenSessionKeepsBackoff(t *testing.T) {
	w := &waits{}
	_, mem := dialMemory(t, Server{Name: "s"}, testServer(t), w)
	mem.drop(0)
	eventually(t, "a second dial", func() bool { return mem.dials() == 2 })
	mem.drop(1)
	eventually(t, "a third dial", func() bool { return mem.dials() == 3 })
	if d := w.delays(); len(d) != 2 || !within(d[1], 2*time.Second) {
		t.Fatalf("delays = %v, want the second drop at ~2s", d)
	}
}

// dropper is a server whose tool cuts the client transport on its first
// call, so the client sees the connection die mid-call.
func dropper(t *testing.T, readOnly bool) (*Manager, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	var mem *memory
	s := mcp.NewServer(&mcp.Implementation{Name: "dropper", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if calls.Add(1) == 1 {
				mem.mu.Lock()
				c := mem.clients[0]
				mem.mu.Unlock()
				_ = c.Close()
				return nil, errors.New("gone")
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: x"}}}, nil
		})
	m, mem := dialMemory(t, Server{Name: "s"}, s, &waits{})
	return m, &calls
}

func TestMidCallDropIsNotReplayed(t *testing.T) {
	m, calls := dropper(t, false)
	_, err := echo(t, m, "s")
	if err == nil || !strings.Contains(err.Error(), "outcome is UNKNOWN") || !strings.Contains(err.Error(), "been re-established") {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("server saw %d calls, want no replay", calls.Load())
	}
}

func TestMidCallDropRetriesReadOnlyTool(t *testing.T) {
	m, calls := dropper(t, true)
	out, err := echo(t, m, "s")
	if err != nil || !strings.HasPrefix(out, `{"result":"echo: x"`) || calls.Load() != 2 {
		t.Fatalf("out %s err %v calls %d", out, err, calls.Load())
	}
}

func TestInitialConnectRetriesTransientFailures(t *testing.T) {
	w := &waits{}
	mem := &memory{t: t, srv: testServer(t), fail: errors.New("connection refused")}
	w.then = func(n int) {
		if n == 2 {
			mem.setFail(nil)
		}
	}
	m := Connect(t.Context(), Options{Servers: []Server{{Name: "s", transport: mem.transport}}, wait: w.wait})
	t.Cleanup(func() { _ = m.Close() })
	if len(m.Servers()) != 1 {
		t.Fatal("server not connected after transient failures")
	}
	d := w.delays()
	if len(d) != 2 || !within(d[0], time.Second) || !within(d[1], 2*time.Second) {
		t.Fatalf("delays = %v", d)
	}

	w = &waits{}
	mem = &memory{t: t, srv: testServer(t), fail: errors.New("connection refused")}
	m = Connect(t.Context(), Options{Servers: []Server{{Name: "s", transport: mem.transport}}, wait: w.wait})
	if len(m.Servers()) != 0 || len(w.delays()) != 3 {
		t.Fatalf("servers %v after %d retries, want skipped after 3", m.Servers(), len(w.delays()))
	}
}

func TestInitialConnectPermanentFailureDoesNotRetry(t *testing.T) {
	w := &waits{}
	m := Connect(t.Context(), Options{Servers: []Server{{Name: "gone", Command: "hermes-go-no-such-binary"}, {Name: "empty"}}, wait: w.wait})
	if len(m.Servers()) != 0 || len(w.delays()) != 0 {
		t.Fatalf("servers %v, delays %v", m.Servers(), w.delays())
	}
	for _, msg := range []string{"401 Unauthorized", "Forbidden"} {
		if !permanent(errors.New("connect: " + msg)) {
			t.Errorf("%q is not permanent", msg)
		}
	}
	if permanent(errors.New("connection refused")) {
		t.Error("connection refused is permanent")
	}
}

func TestCloseStopsReconnect(t *testing.T) {
	w := &waits{}
	m, mem := dialMemory(t, Server{Name: "s"}, testServer(t), w)
	mem.setFail(errors.New("connection refused"))
	mem.drop(0)
	eventually(t, "a backoff", func() bool { return len(w.delays()) > 0 })
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	n := len(w.delays())
	time.Sleep(20 * time.Millisecond)
	if len(w.delays()) != n {
		t.Fatal("reconnect kept running after Close")
	}
}
