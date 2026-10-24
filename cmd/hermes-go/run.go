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
	"sync"
	"time"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/home"
	"github.com/umi008/hermes-agent-go/internal/hooks"
	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/llm/anthropic"
	"github.com/umi008/hermes-agent-go/internal/llm/openai"
	"github.com/umi008/hermes-agent-go/internal/prompt"
	"github.com/umi008/hermes-agent-go/internal/provider"
	"github.com/umi008/hermes-agent-go/internal/skills"
	"github.com/umi008/hermes-agent-go/internal/state"
	"github.com/umi008/hermes-agent-go/internal/tools"
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
}

const usage = `Usage: hermes-go <command> [flags]

Commands:
  chat    Chat interactively, or send one message with -q

Run "hermes-go chat -h" for chat flags.
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
	if args[0] == "chat" {
		return chat(ctx, args[1:], env)
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
	env        Env
	f          chatFlags
	logger     *slog.Logger
	cfg        *config.Config
	lookup     config.LookupFunc
	ep         provider.Endpoint
	hermes     string
	cwd        string
	userHome   string
	client     llm.Client
	store      *state.Store
	memory     *memory.Store
	skills     *skills.Catalog
	reg        *tools.Registry
	tools      agent.ToolExecutor
	closeTools func()

	mu      sync.Mutex
	rec     *recorder
	agent   *agent.Agent
	started time.Time
	usage   llm.Usage     // this session's token totals
	lines   <-chan string // interactive input, nil in single-query mode
}

// sessionID is the live session id, read by tools and hooks.
func (rt *chatRuntime) sessionID() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.rec == nil {
		return ""
	}
	return rt.rec.id
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

	rt, err := newChatRuntime(ctx, f, env, logger, interactive)
	if err != nil {
		return err
	}
	defer rt.close()
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

func newChatRuntime(ctx context.Context, f chatFlags, env Env, logger *slog.Logger, interactive bool) (_ *chatRuntime, err error) {
	h, err := home.Resolver{GOOS: env.GOOS, Getenv: env.Getenv, UserHomeDir: env.UserHomeDir}.Resolve(f.profile)
	if err != nil {
		return nil, err
	}
	dotenv, err := config.LoadDotenv(filepath.Join(h.Dir, ".env"))
	if err != nil {
		return nil, err
	}
	lookup := config.Overlay(dotenv, env.LookupEnv)
	cfg, err := config.Load(filepath.Join(h.Dir, "config.yaml"), config.Options{Lookup: lookup, Logger: logger})
	if err != nil {
		return nil, err
	}
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
	}
	if interactive {
		deps.asker = &lineAsker{rt: rt}
	}
	reg, closeTools, err := buildTools(ctx, deps)
	if err != nil {
		return nil, err
	}
	rt.reg, rt.closeTools = reg, closeTools
	rt.tools = hooks.Wrap(reg, hooks.Options{
		Specs:     hookSpecs(cfg, h.Dir, f.acceptHooks, lookup, logger),
		Runner:    &hooks.Runner{GOOS: env.GOOS, Home: userHome, Logger: logger},
		SessionID: rt.sessionID,
		Cwd:       cwd,
		Profile:   f.profile,
		Approve: func(string, string) bool {
			return cfg.String("approvals", "mode") == "off" || cfg.String("approvals", "single_query_mode") == "approve"
		},
	})
	rt.startSession(ctx)
	return rt, nil
}

// startSession opens a fresh session: a new id, a new recorder and an
// agent with a freshly built system prompt, as upstream new_session.
func (rt *chatRuntime) startSession(ctx context.Context) {
	started := rt.env.Now()
	rec := newRecorder(ctx, rt.store, state.Session{
		ID: state.NewSessionID(started), Source: "cli", Model: rt.ep.Model, Cwd: rt.cwd,
	}, rt.logger)

	var guidance, memBlocks []string
	if hasTool(rt.reg, "memory") {
		guidance = append(guidance, memory.Guidance)
	}
	if hasTool(rt.reg, "session_search") {
		guidance = append(guidance, sessionSearchGuidance)
	}
	if rt.memory != nil {
		memBlocks = rt.memory.PromptBlocks()
	}
	maxChars, _ := rt.cfg.Int("context_file_max_chars")
	sys := prompt.Build(prompt.Inputs{
		HomeDir:      rt.hermes,
		Cwd:          rt.cwd,
		MaxFileChars: maxChars,
		Model:        rt.ep.Model,
		Provider:     rt.ep.Provider,
		Platform:     "cli",
		Now:          started,
		ToolGuidance: guidance,
		Skills:       skillsIndex(rt.skills, rt.reg, rt.lookup),
		Memory:       memBlocks,
		Logger:       rt.logger,
	})
	maxTurns, _ := rt.cfg.Int("agent", "max_turns")
	a := agent.New(agent.Options{
		Client:     rt.client,
		Compressor: newCompressor(rt.cfg, rt.client, rt.ep.Model, rt.env.Now, rt.logger),
		Tools:      rt.tools,
		Model:      rt.ep.Model,
		System:     sys.Tiers(),
		CacheTTL:   cacheTTL(rt.cfg),
		MaxTurns:   maxTurns,
		Logger:     rt.logger,
		OnMessage:  func(m llm.Message) { rec.message(ctx, m) },
		OnEvent: func(e agent.Event) {
			switch {
			case e.Kind == agent.EventUsage:
				rt.usage.Add(e.Usage)
				rec.usage(ctx, e.Usage)
			case e.Kind == agent.EventText:
				_, _ = io.WriteString(rt.env.Stdout, e.Text)
			case e.Kind == agent.EventToolStart && rt.f.verbose && e.ToolCall != nil:
				_, _ = fmt.Fprintf(rt.env.Stderr, "[tool] %s %s\n", e.ToolCall.Name, e.ToolCall.Arguments)
			}
		},
	})
	rt.mu.Lock()
	rt.rec, rt.agent, rt.started, rt.usage = rec, a, started, llm.Usage{}
	rt.mu.Unlock()
}

// endSession closes the current session with reason.
func (rt *chatRuntime) endSession(reason string) {
	rt.mu.Lock()
	rec := rt.rec
	rt.mu.Unlock()
	rec.end(reason)
}

func (rt *chatRuntime) close() {
	if rt.agent != nil {
		rt.endSession("cli_close")
	}
	if rt.closeTools != nil {
		rt.closeTools()
	}
	if rt.store != nil {
		if err := rt.store.Close(); err != nil {
			rt.logger.Warn("close session store", "err", err)
		}
	}
}

// turn runs one user message through the agent and ends the reply line.
func (rt *chatRuntime) turn(ctx context.Context, input string) error {
	res, err := rt.agent.Run(ctx, input)
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
