// Package mcpclient ports upstream's MCP client (tools/mcp_tool*.py) on the
// official Go SDK: each configured server is connected over stdio or
// streamable HTTP, and its tools register as mcp__<server>__<tool>.
package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

// Upstream defaults (tools/mcp_tool.py, mcp_tool_common.py).
const (
	DefaultConnectTimeout = 60 * time.Second
	DefaultCallTimeout    = 300 * time.Second
)

var (
	// safeEnv is upstream _SAFE_ENV_KEYS plus the Windows launcher vars: a
	// stdio child sees only these and its own env block, so API keys in
	// the agent's environment do not leak into servers.
	safeEnv = []string{
		"PATH", "HOME", "USER", "LANG", "LC_ALL", "TERM", "SHELL", "TMPDIR",
		"ALLUSERSPROFILE", "APPDATA", "COMMONPROGRAMFILES", "COMMONPROGRAMFILES(X86)",
		"COMMONPROGRAMW6432", "COMPUTERNAME", "COMSPEC", "HOMEDRIVE", "HOMEPATH",
		"LOCALAPPDATA", "NUMBER_OF_PROCESSORS", "OS", "PATHEXT", "PROCESSOR_ARCHITECTURE",
		"PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432", "PUBLIC",
		"SYSTEMDRIVE", "SYSTEMROOT", "TEMP", "TMP", "USERDOMAIN", "USERNAME",
		"USERPROFILE", "WINDIR",
	}
	credentialRE = regexp.MustCompile(`(?i)(?:ghp_[A-Za-z0-9_]{1,255}|sk-[A-Za-z0-9_-](?:\.?[A-Za-z0-9_-]){0,254}|Bearer\s+\S+` +
		`|(?:token|key|API_KEY|password|secret)=[^\s&,;"']{1,255})`)
)

// Server is one mcp_servers entry.
type Server struct {
	Name string
	// Command and Args launch a stdio server.
	Command string
	Args    []string
	Env     map[string]string
	Cwd     string
	// URL selects HTTP: streamable by default, SSE with Transport "sse".
	URL       string
	Headers   map[string]string
	Transport string

	ConnectTimeout, CallTimeout time.Duration
	// Include, when non-nil, is the only tools registered (an empty list
	// registers none) and wins over Exclude. Entries are names or globs.
	Include, Exclude []string
	// Resources and Prompts gate the utility tools (tools.resources and
	// tools.prompts); nil keeps upstream's default of on.
	Resources, Prompts *bool

	// transport overrides the dialed transport in tests; it is called on
	// every dial.
	transport func() (mcp.Transport, error)
}

// Options configure Connect.
type Options struct {
	Servers []Server
	// LookupEnv reads the agent's environment for the safe stdio baseline.
	LookupEnv func(string) (string, bool)
	Version   string
	Logger    *slog.Logger

	// wait sleeps between connect attempts; tests record it instead.
	wait func(context.Context, time.Duration) error
	// respawnWait bounds how long a call waits for a reconnect.
	respawnWait time.Duration
}

// Manager owns the live sessions.
type Manager struct {
	logger *slog.Logger
	conns  []*conn
}

// Connect dials every server at once. A server that fails to connect is
// logged and skipped; the agent runs without its tools.
func Connect(ctx context.Context, o Options) *Manager {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.LookupEnv == nil {
		o.LookupEnv = func(string) (string, bool) { return "", false }
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	if o.wait == nil {
		o.wait = sleep
	}
	if o.respawnWait <= 0 {
		o.respawnWait = respawnWait
	}
	m := &Manager{logger: o.Logger}
	results := make([]*conn, len(o.Servers))
	var wg sync.WaitGroup
	for i, s := range o.Servers {
		wg.Go(func() {
			c, err := dial(ctx, s, o)
			if err != nil {
				o.Logger.Warn("MCP server unavailable", "server", s.Name, "err", sanitize(err.Error()))
				return
			}
			results[i] = c
		})
	}
	wg.Wait()
	for _, c := range results {
		if c != nil {
			m.conns = append(m.conns, c)
		}
	}
	return m
}

// dial connects one server, retrying transient failures like upstream's
// initial connect, lists its tools and starts the reconnect supervisor.
func dial(ctx context.Context, s Server, o Options) (*conn, error) {
	c := newConn(ctx, s, o)
	var session *mcp.ClientSession
	var stop context.CancelFunc
	for attempt := 0; ; attempt++ {
		var err error
		session, stop, err = c.open()
		if err == nil {
			break
		}
		if permanent(err) || attempt >= maxInitialRetries {
			c.cancel()
			return nil, err
		}
		o.Logger.Warn("MCP server connect failed, retrying", "server", s.Name, "attempt", attempt+1,
			"err", sanitize(err.Error()))
		if werr := o.wait(ctx, jittered(backoff(attempt))); werr != nil {
			c.cancel()
			return nil, err
		}
	}
	if err := c.listTools(session); err != nil {
		_ = session.Close()
		stop()
		c.cancel()
		return nil, err
	}
	c.publish(session, stop)
	go c.supervise()
	o.Logger.Debug("MCP server connected", "server", s.Name, "tools", len(c.tools))
	return c, nil
}

// open connects one session. The session context outlives open: an SSE
// stream is a GET bound to it, so the connect timeout cancels it only
// while connecting.
func (c *conn) open() (*mcp.ClientSession, context.CancelFunc, error) {
	t, err := transportFor(c.server, c.opts.LookupEnv)
	if err != nil {
		return nil, nil, err
	}
	timeout := c.connectTimeout()
	sctx, stop := context.WithCancel(c.ctx)
	timer := time.AfterFunc(timeout, stop)
	client := mcp.NewClient(&mcp.Implementation{Name: "hermes-agent-go", Version: c.opts.Version}, nil)
	session, err := client.Connect(sctx, t, nil)
	if !timer.Stop() && err == nil {
		_ = session.Close()
		err = context.DeadlineExceeded
	}
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	return session, stop, nil
}

func (c *conn) connectTimeout() time.Duration {
	if c.server.ConnectTimeout > 0 {
		return c.server.ConnectTimeout
	}
	return DefaultConnectTimeout
}

// listTools records the first session's capabilities and filtered tools;
// later reconnects keep these registrations.
func (c *conn) listTools(session *mcp.ClientSession) error {
	if init := session.InitializeResult(); init != nil {
		c.caps = init.Capabilities
	}
	if c.caps != nil && c.caps.Tools == nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(c.ctx, c.connectTimeout())
	defer cancel()
	for tool, err := range session.Tools(cctx, nil) {
		if err != nil {
			return fmt.Errorf("list tools: %w", err)
		}
		if (filter{include: c.server.Include, includeSet: c.server.Include != nil, exclude: c.server.Exclude}).allows(tool.Name) {
			c.tools = append(c.tools, tool)
		}
	}
	return nil
}

func transportFor(s Server, lookup func(string) (string, bool)) (mcp.Transport, error) {
	if s.transport != nil {
		return s.transport()
	}
	switch {
	case s.Command != "":
		cmd := exec.Command(s.Command, s.Args...)
		cmd.Dir = s.Cwd
		cmd.Env = stdioEnv(s.Env, lookup)
		return &mcp.CommandTransport{Command: cmd}, nil
	case s.URL != "":
		hc := &http.Client{Transport: headerTransport{headers: s.Headers, base: http.DefaultTransport}}
		if s.Transport == "sse" {
			return &mcp.SSEClientTransport{Endpoint: s.URL, HTTPClient: hc}, nil
		}
		return &mcp.StreamableClientTransport{Endpoint: s.URL, HTTPClient: hc}, nil
	}
	return nil, fmt.Errorf("MCP server '%s' has %w", s.Name, errNoEndpoint)
}

// stdioEnv is upstream _build_safe_env: the safe baseline plus the
// server's own env block.
func stdioEnv(user map[string]string, lookup func(string) (string, bool)) []string {
	env := map[string]string{}
	for _, k := range safeEnv {
		if v, ok := lookup(k); ok {
			env[k] = v
		}
	}
	for k, v := range user {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if len(h.headers) > 0 {
		r = r.Clone(r.Context())
		for k, v := range h.headers {
			r.Header.Set(k, v)
		}
	}
	return h.base.RoundTrip(r)
}

// Close ends every session.
func (m *Manager) Close() error {
	var errs []error
	for _, c := range m.conns {
		if err := c.close(); err != nil {
			errs = append(errs, fmt.Errorf("close MCP server %s: %w", c.server.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Servers lists the connected server names.
func (m *Manager) Servers() []string {
	out := make([]string, 0, len(m.conns))
	for _, c := range m.conns {
		out = append(out, c.server.Name)
	}
	return out
}

// Tools returns one registry tool per server tool, in toolset
// mcp-<server> like upstream, then the server's resource and prompt
// utilities. A later duplicate name is dropped, so a native tool wins
// over a utility of the same name.
func (m *Manager) Tools() []tools.Tool {
	var out []tools.Tool
	seen := map[string]bool{}
	for _, c := range m.conns {
		for _, t := range c.tools {
			name := ToolName(c.server.Name, t.Name)
			if seen[name] {
				m.logger.Warn("MCP tool name collision, keeping the first", "name", name, "server", c.server.Name)
				continue
			}
			seen[name] = true
			desc := t.Description
			if desc == "" {
				desc = fmt.Sprintf("MCP tool %s from %s", t.Name, c.server.Name)
			}
			out = append(out, tools.Tool{
				Name:        name,
				Toolset:     "mcp-" + c.server.Name,
				Description: stripTags(desc),
				Parameters:  NormalizeSchema(t.InputSchema),
				Handler:     c.handler(t),
			})
		}
		for _, u := range c.utilities() {
			if seen[u.Name] {
				m.logger.Warn("MCP utility shadowed by a server tool", "name", u.Name, "server", c.server.Name)
				continue
			}
			seen[u.Name] = true
			out = append(out, u)
		}
	}
	return out
}

func (c *conn) handler(tool *mcp.Tool) tools.Handler {
	readOnly := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		args := map[string]any{}
		if len(bytes.TrimSpace(raw)) > 0 && string(raw) != "null" {
			if err := tools.Decode(raw, &args); err != nil {
				return "", err
			}
		}
		var res *mcp.CallToolResult
		err := c.call(ctx, readOnly, func(ctx context.Context, s *mcp.ClientSession) error {
			var err error
			res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: args})
			return err
		})
		if err != nil {
			return "", err
		}
		return render(res, c.server.Name)
	}
}

func (c *conn) callTimeout() time.Duration {
	if c.server.CallTimeout > 0 {
		return c.server.CallTimeout
	}
	return DefaultCallTimeout
}

func sanitize(s string) string { return credentialRE.ReplaceAllString(s, "[REDACTED]") }
