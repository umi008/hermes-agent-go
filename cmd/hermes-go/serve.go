package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/apiserver"
	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/provider"
)

// serveFlags are the `hermes-go serve` options: the address plus the chat
// runtime knobs the server shares with every other interface.
type serveFlags struct {
	chatFlags
	addr string
}

func parseServe(args []string, stderr io.Writer) (serveFlags, error) {
	var f serveFlags
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.addr, "addr", "127.0.0.1:8787", "listen address")
	for _, name := range []string{"p", "profile"} {
		fs.StringVar(&f.profile, name, "", "profile name")
	}
	for _, name := range []string{"m", "model"} {
		fs.StringVar(&f.model, name, "", "model override")
	}
	fs.StringVar(&f.provider, "provider", "", "provider override: "+strings.Join(provider.Names(), ", "))
	fs.StringVar(&f.toolsets, "toolsets", "", "comma-separated toolsets to enable (default all)")
	fs.BoolVar(&f.verbose, "v", false, "log debug output to stderr")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if fs.NArg() > 0 {
		return f, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return f, nil
}

// serve runs the OpenAI-compatible API server until ctx is cancelled.
func serve(ctx context.Context, args []string, env Env) int {
	f, err := parseServe(args, env.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	if err := runServe(ctx, f, env); err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "hermes-go: %v\n", err)
		return 1
	}
	return 0
}

func runServe(ctx context.Context, f serveFlags, env Env) error {
	level := slog.LevelWarn
	if f.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: level}))

	rt, err := newChatRuntime(ctx, f.chatFlags, env, logger, noHumans)
	if err != nil {
		return err
	}
	defer rt.close()

	key := apiKey(rt.cfg, rt.lookup)
	if err := serveAuth(f.addr, key, logger); err != nil {
		return err
	}
	// Ctrl-C shuts the server down the same way a cancelled context does.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-env.Interrupt:
			cancel()
		case <-ctx.Done():
		}
	}()
	responses, err := apiserver.OpenResponseStore(ctx, responseStorePath(rt.hermes))
	if err != nil {
		logger.Warn("response store unavailable; stored responses live in memory", "err", err)
		responses = nil
	} else {
		defer func() { _ = responses.Close() }()
	}
	return newAPIServer(rt, f.addr, key, logger, responses).Serve(ctx)
}

// responseStorePath is the Go port's own /v1/responses store, next to its
// state.db; upstream keeps response_store.db under HERMES_HOME.
func responseStorePath(hermes string) string {
	return filepath.Join(hermes, "hermes-go", "response_store.db")
}

// maxConcurrentRuns is upstream gateway.api_server.max_concurrent_runs:
// default 10, 0 disables the cap, negatives count as 0.
func maxConcurrentRuns(cfg *config.Config) int {
	n, ok := cfg.Int("gateway", "api_server", "max_concurrent_runs")
	if !ok {
		return 10
	}
	return max(n, 0)
}

// apiKey resolves the bearer token: api_server.api_key from config.yaml wins,
// then HERMES_API_KEY from the environment (or the profile's .env).
func apiKey(cfg *config.Config, lookup config.LookupFunc) string {
	if key := strings.TrimSpace(cfg.String("api_server", "api_key")); key != "" {
		return key
	}
	if value, ok := lookup("HERMES_API_KEY"); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

// serveAuth enforces the startup rule: never serve a non-loopback address
// without a token. A loopback bind may run unauthenticated, with a warning.
func serveAuth(addr, key string, logger *slog.Logger) error {
	if key != "" {
		return nil
	}
	if !apiserver.Loopback(addr) {
		return fmt.Errorf("refusing to serve %s without an API key: set api_server.api_key in config.yaml or HERMES_API_KEY", addr)
	}
	logger.Warn("serving without an API key on a loopback address; anyone on this machine can run agent turns", "addr", addr)
	return nil
}

// newAPIServer builds the HTTP layer over the shared chat runtime: one session
// per request, its text streamed to the response, and no REPL session touched.
func newAPIServer(rt *chatRuntime, addr, key string, logger *slog.Logger, responses *apiserver.ResponseStore) *apiserver.Server {
	return apiserver.New(apiserver.Config{
		Backend:           runtimeBackend{rt: rt},
		APIKey:            key,
		Addr:              addr,
		Logger:            logger,
		Now:               rt.env.Now,
		MaxConcurrentRuns: maxConcurrentRuns(rt.cfg),
		Responses:         responses,
		Ready: func(bound string) {
			_, _ = fmt.Fprintf(rt.env.Stderr, "hermes-go: api server listening on http://%s\n", bound)
		},
	})
}

// runtimeBackend adapts the shared chat runtime to the server's seam: the model
// id it advertises and the session factory.
type runtimeBackend struct{ rt *chatRuntime }

func (b runtimeBackend) Model() string { return b.rt.ep.Model }

func (b runtimeBackend) NewSession(ctx context.Context, o apiserver.SessionOptions) apiserver.Session {
	spec := sessionSpec{source: o.Source, out: o.Out, id: o.ID, history: o.History, instructions: o.Instructions}
	if o.OnTool != nil {
		spec.observe = func(e agent.Event) {
			if e.ToolCall == nil {
				return
			}
			switch e.Kind {
			case agent.EventToolStart:
				o.OnTool(apiserver.ToolEvent{Call: *e.ToolCall})
			case agent.EventToolResult:
				o.OnTool(apiserver.ToolEvent{Done: true, Call: *e.ToolCall, Output: e.Text, IsError: e.IsError})
			}
		}
	}
	return runtimeSession{s: b.rt.openSession(ctx, spec)}
}

// runtimeSession is one API request's conversation.
type runtimeSession struct{ s *chatSession }

func (s runtimeSession) ID() string { return s.s.id }

func (s runtimeSession) Run(ctx context.Context, input string) (apiserver.Reply, error) {
	res, err := s.s.run(ctx, input)
	history, turn := s.s.transcript()
	reply := apiserver.Reply{Text: res.Text, Usage: res.Usage, Stop: res.StopReason, Turn: turn, History: history}
	if errors.Is(err, agent.ErrMaxTurns) {
		// Upstream ships a budget-exhausted turn as a soft partial: the text it
		// managed plus finish_reason "length", not a transport error.
		reply.Stop = llm.StopMaxTokens
		return reply, nil
	}
	return reply, err
}

func (s runtimeSession) End(reason string) { s.s.end(reason) }
