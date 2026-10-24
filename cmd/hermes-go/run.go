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
	"time"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/home"
	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/llm/anthropic"
	"github.com/umi008/hermes-agent-go/internal/llm/openai"
	"github.com/umi008/hermes-agent-go/internal/prompt"
	"github.com/umi008/hermes-agent-go/internal/provider"
	"github.com/umi008/hermes-agent-go/internal/state"
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
}

const usage = `Usage: hermes-go <command> [flags]

Commands:
  chat    Send one message and stream the reply

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
	verbose                                   bool
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
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if f.query == "" && fs.NArg() > 0 {
		f.query = strings.Join(fs.Args(), " ")
	}
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

func runChat(ctx context.Context, f chatFlags, env Env) error {
	level := slog.LevelWarn
	if f.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: level}))

	query := f.query
	if query == "-" {
		data, err := io.ReadAll(env.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		query = string(data)
	}
	if strings.TrimSpace(query) == "" {
		return errors.New("chat needs a message: -q \"...\"")
	}

	h, err := home.Resolver{GOOS: env.GOOS, Getenv: env.Getenv, UserHomeDir: env.UserHomeDir}.Resolve(f.profile)
	if err != nil {
		return err
	}
	dotenv, err := config.LoadDotenv(filepath.Join(h.Dir, ".env"))
	if err != nil {
		return err
	}
	lookup := config.Overlay(dotenv, env.LookupEnv)
	cfg, err := config.Load(filepath.Join(h.Dir, "config.yaml"), config.Options{Lookup: lookup, Logger: logger})
	if err != nil {
		return err
	}
	ep, err := provider.Resolve(cfg, lookup, provider.Overrides{Provider: f.provider, Model: f.model})
	if err != nil {
		return err
	}
	logger.Debug("resolved endpoint", "home", h.Dir, "provider", ep.Provider, "model", ep.Model, "base_url", ep.BaseURL)

	cwd, err := env.Getwd()
	if err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	userHome, err := env.UserHomeDir()
	if err != nil {
		return fmt.Errorf("user home: %w", err)
	}
	started := env.Now()
	rec := openRecorder(ctx, h.Dir, state.Session{
		ID: state.NewSessionID(started), Source: "cli", Model: ep.Model, Cwd: cwd,
	}, env.Now, logger)
	defer rec.close()
	mem := openMemory(h.Dir, cfg, logger)

	deps := toolDeps{
		cfg: cfg, lookup: lookup, homeDir: userHome, hermes: h.Dir, cwd: cwd,
		toolsets: f.toolsets, logger: logger, memory: mem, profile: f.profile,
	}
	if rec != nil {
		deps.store, deps.sessionID = rec.store, rec.id
	}
	reg, closeTools, err := buildTools(ctx, deps)
	if err != nil {
		return err
	}
	defer closeTools()

	var guidance, memBlocks []string
	if hasTool(reg, "memory") {
		guidance = append(guidance, memory.Guidance)
	}
	if hasTool(reg, "session_search") {
		guidance = append(guidance, sessionSearchGuidance)
	}
	if mem != nil {
		memBlocks = mem.PromptBlocks()
	}
	maxChars, _ := cfg.Int("context_file_max_chars")
	sys := prompt.Build(prompt.Inputs{
		HomeDir:      h.Dir,
		Cwd:          cwd,
		MaxFileChars: maxChars,
		Model:        ep.Model,
		Provider:     ep.Provider,
		Platform:     "cli",
		Now:          started,
		ToolGuidance: guidance,
		Memory:       memBlocks,
		Logger:       logger,
	})
	maxTurns, _ := cfg.Int("agent", "max_turns")

	client := newClient(ep)
	a := agent.New(agent.Options{
		Client:     client,
		Compressor: newCompressor(cfg, client, ep.Model, env.Now, logger),
		Tools:      reg,
		Model:      ep.Model,
		System:     sys.Tiers(),
		CacheTTL:   cacheTTL(cfg),
		MaxTurns:   maxTurns,
		Logger:     logger,
		OnMessage:  func(m llm.Message) { rec.message(ctx, m) },
		OnEvent: func(e agent.Event) {
			switch {
			case e.Kind == agent.EventUsage:
				rec.usage(ctx, e.Usage)
			case e.Kind == agent.EventText:
				_, _ = io.WriteString(env.Stdout, e.Text)
			case e.Kind == agent.EventToolStart && f.verbose && e.ToolCall != nil:
				_, _ = fmt.Fprintf(env.Stderr, "[tool] %s %s\n", e.ToolCall.Name, e.ToolCall.Arguments)
			}
		},
	})
	res, err := a.Run(ctx, query)
	if res.Text != "" && !strings.HasSuffix(res.Text, "\n") {
		_, _ = io.WriteString(env.Stdout, "\n")
	}
	if err != nil {
		return err
	}
	logger.Debug("usage", "turns", res.Turns, "input", res.Usage.InputTokens, "output", res.Usage.OutputTokens,
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
