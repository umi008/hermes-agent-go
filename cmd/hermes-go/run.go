package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/home"
	"github.com/umi008/hermes-agent-go/internal/hooks"
	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/llm/anthropic"
	"github.com/umi008/hermes-agent-go/internal/llm/openai"
	"github.com/umi008/hermes-agent-go/internal/provider"
	"github.com/umi008/hermes-agent-go/internal/skills"
	"github.com/umi008/hermes-agent-go/internal/state"
	"github.com/umi008/hermes-agent-go/internal/tools"
	"github.com/umi008/hermes-agent-go/internal/tools/approval"
	"github.com/umi008/hermes-agent-go/internal/tools/clarify"
	"github.com/umi008/hermes-agent-go/internal/tools/memory"
)

// Env is every process fact the CLI reads, so tests never touch the real
// environment or the user's Hermes home.
type Env struct {
	GOOS        string
	Getenv      func(string) string
	LookupEnv   func(string) (string, bool)
	UserHomeDir func() (string, error)
	Getwd       func() (string, error)
	Now         func() time.Time
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	// Interrupt delivers Ctrl-C. It cancels the running turn, and in the
	// interactive chat an idle Ctrl-C exits. Nil never fires.
	Interrupt <-chan struct{}
	// LookPath finds a binary on PATH (doctor). Nil uses exec.LookPath.
	LookPath func(string) (string, error)
}

// version is the release version, set at build time with
// -ldflags "-X main.version=1.2.3" (GoReleaser passes the tag without its
// leading v). Development builds report "dev".
var version = "dev"

// pseudoVersion matches the timestamp-and-commit tail Go stamps on untagged
// builds: a local `go build` in a checkout, or `go install ...@main`.
var pseudoVersion = regexp.MustCompile(`\d{14}-[0-9a-f]{12}`)

// moduleVersion keeps an injected version and otherwise reports the release
// tag Go records for `go install ...@vX.Y.Z`, without its leading v. Untagged
// builds ("(devel)", pseudo-versions, +dirty) stay "dev".
func moduleVersion(injected string, info *debug.BuildInfo, ok bool) string {
	if injected != "dev" || !ok || info == nil {
		return injected
	}
	v := info.Main.Version
	if v == "" || v == "(devel)" || strings.Contains(v, "+") || pseudoVersion.MatchString(v) {
		return injected
	}
	return strings.TrimPrefix(v, "v")
}

const usage = `Usage: hermes-go <command> [flags]

Commands:
  chat     Chat interactively, or send one message with -q
  serve    Serve the OpenAI-compatible HTTP API
  cron     Manage and run scheduled jobs
  gateway  Serve a chat platform (Telegram, Discord)
  tui      Run the upstream Ink TUI against this agent
  doctor   Check the home, config, provider and optional tools
  version  Print the version

Run "hermes-go <command> -h" for that command's flags.
`

// Run executes the CLI and returns the exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = io.WriteString(env.Stderr, usage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "version", "--version":
		_, _ = fmt.Fprintf(env.Stdout, "hermes-go %s\n", version)
		return 0
	case "chat":
		return chat(ctx, args[1:], env)
	case "serve":
		return serve(ctx, args[1:], env)
	case "cron":
		return cronCmd(ctx, args[1:], env)
	case "gateway":
		return gatewayCmd(ctx, args[1:], env)
	case "tui":
		return tuiCmd(ctx, args[1:], env)
	case "doctor":
		return doctorCmd(args[1:], env)
	}
	_, _ = fmt.Fprintf(env.Stderr, "hermes-go: unknown command %q\n\n%s", args[0], usage)
	return 2
}

type chatFlags struct {
	query, profile, model, provider, toolsets string
	verbose, acceptHooks                      bool
	querySet                                  bool // -q given or a positional message: single-query mode
}

func parseChat(args []string, stderr io.Writer) (chatFlags, error) {
	var f chatFlags
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	for _, name := range []string{"q", "query"} {
		fs.StringVar(&f.query, name, "", "message to send (- reads stdin)")
	}
	for _, name := range []string{"p", "profile"} {
		fs.StringVar(&f.profile, name, "", "profile name")
	}
	for _, name := range []string{"m", "model"} {
		fs.StringVar(&f.model, name, "", "model override")
	}
	fs.StringVar(&f.provider, "provider", "", "provider override: "+strings.Join(provider.Names(), ", "))
	fs.StringVar(&f.toolsets, "toolsets", "", "comma-separated toolsets to enable (default all)")
	fs.BoolVar(&f.verbose, "v", false, "log debug output to stderr")
	fs.BoolVar(&f.acceptHooks, "accept-hooks", false, "run shell hooks from config.yaml without the allowlist")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if f.query == "" && fs.NArg() > 0 {
		f.query = strings.Join(fs.Args(), " ")
	}
	fs.Visit(func(fl *flag.Flag) { f.querySet = f.querySet || fl.Name == "q" || fl.Name == "query" })
	f.querySet = f.querySet || fs.NArg() > 0
	return f, nil
}

func chat(ctx context.Context, args []string, env Env) int {
	f, err := parseChat(args, env.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	if err := runChat(ctx, f, env); err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "hermes-go: %v\n", err)
		return 1
	}
	return 0
}

// chatRuntime is what one chat process shares across sessions: config,
// endpoint, tools and the session store. The agent and the recorder are
// per session; /new replaces both.
type chatRuntime struct {
	env      Env
	f        chatFlags
	logger   *slog.Logger
	cfg      *config.Config
	lookup   config.LookupFunc
	ep       provider.Endpoint
	hermes   string
	cwd      string
	userHome string
	client   llm.Client
	// fallbacks is the resolved fallback_providers chain.
	fallbacks []agent.Route
	store     *state.Store
	memory    *memory.Store
	skills    *skills.Catalog
	reg       *tools.Registry
	// Per-session hook wiring: the specs and the runner are shared, the
	// session id is not, so each session wraps the registry itself.
	hookSpecs  []hooks.Spec
	hookRunner *hooks.Runner
	approve    func(tool, message string) bool
	closeTools func()

	mu    sync.Mutex
	cur   *chatSession  // the REPL's live session; nil in a server runtime
	lines <-chan string // interactive input, nil in single-query mode
}

// sessionID is the live session id, read by tools and hooks.
func (rt *chatRuntime) sessionID() string {
	if s := rt.currentSession(); s != nil {
		return s.id
	}
	return ""
}

func runChat(ctx context.Context, f chatFlags, env Env) error {
	level := slog.LevelWarn
	if f.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: level}))

	interactive := !f.querySet
	query := f.query
	if query == "-" {
		data, err := io.ReadAll(env.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		query = string(data)
	}
	if !interactive && strings.TrimSpace(query) == "" {
		return errors.New("chat needs a message: -q \"...\"")
	}

	humans := noHumans
	if interactive {
		humans = terminalHumans
	}
	rt, err := newChatRuntime(ctx, f, env, logger, humans)
	if err != nil {
		return err
	}
	defer rt.close()
	rt.startSession(ctx)
	if interactive {
		return rt.repl(ctx)
	}
	// Ctrl-C cancels the single query.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-env.Interrupt:
			cancel()
		case <-ctx.Done():
		}
	}()
	return rt.turn(ctx, query)
}

// humansFunc returns who answers clarify and dangerous-command approvals for a
// runtime. A nil asker or prompter means nobody can be asked: clarify fails
// closed and approvals follow approvals.single_query_mode. An asker also
// marks the runtime interactive, which registers skill_manage as upstream.
type humansFunc func(rt *chatRuntime) (clarify.Asker, approval.Prompter)

// noHumans is the runtime of an unattended command (-q, serve, cron).
func noHumans(*chatRuntime) (clarify.Asker, approval.Prompter) { return nil, nil }

// terminalHumans asks on the interactive chat's own input lines.
func terminalHumans(rt *chatRuntime) (clarify.Asker, approval.Prompter) {
	return &lineAsker{rt: rt}, &linePrompter{rt: rt}
}

func newChatRuntime(ctx context.Context, f chatFlags, env Env, logger *slog.Logger, humans humansFunc) (*chatRuntime, error) {
	h, cfg, lookup, err := loadRuntimeConfig(f, env, logger)
	if err != nil {
		return nil, err
	}
	return buildRuntime(ctx, f, env, logger, h, cfg, lookup, humans)
}

// loadRuntimeConfig resolves the profile and reads .env and config.yaml. The
// gateways call it on their own because they need gateway.* before a runtime
// exists.
func loadRuntimeConfig(f chatFlags, env Env, logger *slog.Logger) (home.Home, *config.Config, config.LookupFunc, error) {
	h, err := home.Resolver{GOOS: env.GOOS, Getenv: env.Getenv, UserHomeDir: env.UserHomeDir}.Resolve(f.profile)
	if err != nil {
		return home.Home{}, nil, nil, err
	}
	dotenv, err := config.LoadDotenv(filepath.Join(h.Dir, ".env"))
	if err != nil {
		return home.Home{}, nil, nil, err
	}
	lookup := config.Overlay(dotenv, env.LookupEnv)
	cfg, err := config.Load(filepath.Join(h.Dir, "config.yaml"), config.Options{Lookup: lookup, Logger: logger})
	if err != nil {
		return home.Home{}, nil, nil, err
	}
	return h, cfg, lookup, nil
}

// buildRuntime resolves the endpoint and opens the store, memory, skills and
// tools every session of the process shares.
func buildRuntime(ctx context.Context, f chatFlags, env Env, logger *slog.Logger,
	h home.Home, cfg *config.Config, lookup config.LookupFunc, humans humansFunc) (_ *chatRuntime, err error) {

	ep, err := provider.Resolve(cfg, lookup, provider.Overrides{Provider: f.provider, Model: f.model})
	if err != nil {
		return nil, err
	}
	logger.Debug("resolved endpoint", "home", h.Dir, "provider", ep.Provider, "model", ep.Model, "base_url", ep.BaseURL)

	cwd, err := env.Getwd()
	if err != nil {
		return nil, fmt.Errorf("working directory: %w", err)
	}
	userHome, err := env.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home: %w", err)
	}
	rt := &chatRuntime{
		env: env, f: f, logger: logger, cfg: cfg, lookup: lookup, ep: ep,
		hermes: h.Dir, cwd: cwd, userHome: userHome, client: newClient(ep),
	}
	chain, warns := provider.FallbackChain(cfg, lookup)
	for _, w := range warns {
		logger.Warn(w)
	}
	for _, fb := range chain {
		rt.fallbacks = append(rt.fallbacks, agent.Route{Client: newClient(fb), Provider: fb.Provider, Model: fb.Model, BaseURL: fb.BaseURL})
		logger.Debug("fallback provider", "provider", fb.Provider, "model", fb.Model, "base_url", fb.BaseURL)
	}
	rt.store = openStore(ctx, h.Dir, env.Now, logger)
	rt.memory = openMemory(h.Dir, cfg, logger)
	rt.skills = openSkills(h.Dir, userHome, cfg, lookup, logger)
	defer func() {
		if err != nil {
			rt.close()
		}
	}()

	deps := toolDeps{
		cfg: cfg, lookup: lookup, homeDir: userHome, hermes: h.Dir, cwd: cwd,
		toolsets: f.toolsets, logger: logger, memory: rt.memory, skills: rt.skills, profile: f.profile,
		store: rt.store, sessionID: rt.sessionID,
		route: agent.Route{Client: rt.client, Provider: ep.Provider, Model: ep.Model},
	}
	deps.asker, deps.prompter = humans(rt)
	reg, closeTools, err := buildTools(ctx, deps)
	if err != nil {
		return nil, err
	}
	rt.reg, rt.closeTools = reg, closeTools
	rt.hookSpecs = hookSpecs(cfg, h.Dir, f.acceptHooks, lookup, logger)
	rt.hookRunner = &hooks.Runner{GOOS: env.GOOS, Home: userHome, Logger: logger}
	rt.approve = func(string, string) bool {
		return cfg.String("approvals", "mode") == "off" || cfg.String("approvals", "single_query_mode") == "approve"
	}
	if err := rt.withDelegation(); err != nil {
		return nil, err
	}
	return rt, nil
}

// startSession opens a fresh session for the REPL: a new id, a new recorder
// and an agent with a freshly built system prompt, as upstream new_session.
func (rt *chatRuntime) startSession(ctx context.Context) {
	s := rt.newSession(ctx, "cli", rt.env.Stdout)
	rt.mu.Lock()
	rt.cur = s
	rt.mu.Unlock()
}

// currentSession is the REPL’s live session. It is nil in a server runtime,
// where every session has its own owner and never touches rt.cur.
func (rt *chatRuntime) currentSession() *chatSession {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.cur
}

// endSession closes the current session with reason.
func (rt *chatRuntime) endSession(reason string) {
	rt.currentSession().end(reason)
}

func (rt *chatRuntime) close() {
	rt.currentSession().end("cli_close")
	if rt.closeTools != nil {
		rt.closeTools()
	}
	if rt.store != nil {
		if err := rt.store.Close(); err != nil {
			rt.logger.Warn("close session store", "err", err)
		}
	}
}

// turn runs one user message through the current session and ends the reply
// line, so the prompt starts on its own line.
func (rt *chatRuntime) turn(ctx context.Context, input string) error {
	s := rt.currentSession()
	if s == nil {
		return errors.New("no live session")
	}
	res, err := s.run(ctx, input)
	if res.Text != "" && !strings.HasSuffix(res.Text, "\n") {
		_, _ = io.WriteString(rt.env.Stdout, "\n")
	}
	if err != nil {
		return err
	}
	rt.logger.Debug("usage", "turns", res.Turns, "input", res.Usage.InputTokens, "output", res.Usage.OutputTokens,
		"cache_read", res.Usage.CacheReadTokens, "cache_write", res.Usage.CacheWriteTokens)
	return nil
}

func newClient(ep provider.Endpoint) llm.Client {
	if ep.APIMode == provider.ModeAnthropicMessages {
		return anthropic.New(anthropic.Options{BaseURL: ep.BaseURL, APIKey: ep.APIKey})
	}
	return &openai.Client{BaseURL: ep.BaseURL, APIKey: ep.APIKey, Name: ep.Provider}
}

func cacheTTL(cfg *config.Config) string {
	if cfg.String("prompt_caching", "cache_ttl") == "1h" {
		return "1h"
	}
	return ""
}
