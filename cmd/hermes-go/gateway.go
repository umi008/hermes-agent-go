package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/discord"
	"github.com/umi008/hermes-agent-go/internal/gateway"
	"github.com/umi008/hermes-agent-go/internal/provider"
	"github.com/umi008/hermes-agent-go/internal/telegram"
	"github.com/umi008/hermes-agent-go/internal/tools/approval"
	"github.com/umi008/hermes-agent-go/internal/tools/clarify"
)

// gatewayFlags are `hermes-go gateway`'s own flags. The rest of chatFlags is
// shared with `chat`, so both commands read a profile, a model and a provider
// the same way.
type gatewayFlags struct {
	platform string
	chatFlags
}

func parseGateway(args []string, stderr io.Writer) (gatewayFlags, error) {
	var f gatewayFlags
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.platform, "platform", "telegram", "chat platform to serve (telegram, discord)")
	for _, name := range []string{"p", "profile"} {
		fs.StringVar(&f.profile, name, "", "profile name")
	}
	for _, name := range []string{"m", "model"} {
		fs.StringVar(&f.model, name, "", "model override")
	}
	fs.StringVar(&f.provider, "provider", "", "provider override: "+strings.Join(provider.Names(), ", "))
	fs.BoolVar(&f.verbose, "v", false, "log debug output to stderr")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if fs.NArg() > 0 {
		return f, fmt.Errorf("gateway takes no arguments, got %q", fs.Arg(0))
	}
	return f, nil
}

// gatewayCmd serves a chat platform until ctx is cancelled (Ctrl-C through
// env.Interrupt) and returns the process exit code.
func gatewayCmd(ctx context.Context, args []string, env Env) int {
	f, err := parseGateway(args, env.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	if err := runGateway(ctx, f, env); err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "hermes-go: %v\n", err)
		return 1
	}
	return 0
}

func runGateway(ctx context.Context, f gatewayFlags, env Env) error {
	level := slog.LevelWarn
	if f.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: level}))
	if f.platform != "telegram" && f.platform != "discord" {
		return fmt.Errorf("unknown platform %q (telegram, discord)", f.platform)
	}

	h, cfg, lookup, err := loadRuntimeConfig(f.chatFlags, env, logger)
	if err != nil {
		return err
	}
	adapter, maxChunk, err := newPlatformAdapter(f.platform, cfg, lookup, logger)
	if err != nil {
		return err
	}

	// Ctrl-C cancels the gateway; every chat's running turn and its session
	// are then cancelled and closed by the core.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-env.Interrupt:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Sessions are opened through the shared runtime; one per chat, created by
	// the core. rt is assigned before the core serves anything.
	var rt *chatRuntime
	core, err := gateway.New(gateway.Options{
		NewSession: func(sctx context.Context, source string, out io.Writer) gateway.Session {
			return gatewaySession{rt.newSession(sctx, source, out)}
		},
		Logger:       logger,
		Now:          env.Now,
		MaxChunk:     maxChunk,
		AllowedChats: stringList(cfg, "gateway", "allowed_chats"),
	})
	if err != nil {
		return err
	}
	if rt, err = buildRuntime(ctx, f.chatFlags, env, logger, h, cfg, lookup,
		func(*chatRuntime) (clarify.Asker, approval.Prompter) { return nil, core }); err != nil {
		return err
	}
	defer rt.close()

	logger.Info("gateway started", "platform", adapter.Name(), "home", h.Dir,
		"model", rt.ep.Model, "provider", rt.ep.Provider)
	return core.Run(ctx, adapter)
}

// newPlatformAdapter builds the adapter for platform and returns the longest
// message it accepts, which the core uses to chunk replies.
func newPlatformAdapter(platform string, cfg *config.Config, lookup config.LookupFunc, logger *slog.Logger) (gateway.Adapter, int, error) {
	if platform == "discord" {
		opts, err := discord.FromConfig(cfg, lookup, logger)
		if err != nil {
			return nil, 0, err
		}
		adapter, err := discord.New(opts)
		return adapter, discord.MaxMessageChars, err
	}
	token := strings.TrimSpace(cfg.String("gateway", "telegram", "token"))
	if token == "" {
		if v, ok := lookup("TELEGRAM_BOT_TOKEN"); ok {
			token = strings.TrimSpace(v)
		}
	}
	if token == "" {
		return nil, 0, errors.New("telegram token is required: set gateway.telegram.token in config.yaml or TELEGRAM_BOT_TOKEN")
	}
	adapter, err := telegram.New(telegram.Options{
		Token:   token,
		APIBase: cfg.String("gateway", "telegram", "api_base"),
		Logger:  logger,
	})
	return adapter, telegram.MaxMessageLength, err
}

// gatewaySession adapts the CLI's chat session to the gateway's Session
// contract. The session records its own source and writes its text to the sink
// the core passes in, so the gateway never reads the REPL's session.
type gatewaySession struct{ s *chatSession }

func (g gatewaySession) ID() string { return g.s.id }

func (g gatewaySession) Run(ctx context.Context, input string) error {
	_, err := g.s.run(ctx, input)
	return err
}

func (g gatewaySession) End(reason string) { g.s.end(reason) }
