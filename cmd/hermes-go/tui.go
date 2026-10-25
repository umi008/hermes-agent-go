package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/provider"
	"github.com/umi008/hermes-agent-go/internal/skills"
	"github.com/umi008/hermes-agent-go/internal/tools/approval"
	"github.com/umi008/hermes-agent-go/internal/tools/clarify"
	"github.com/umi008/hermes-agent-go/internal/tuigateway"
)

// tuiFlags are the `hermes-go tui` options.
type tuiFlags struct {
	chatFlags
	addr string
	// tuiDir is the upstream ui-tui checkout to launch. Empty serves only and
	// prints the URL to attach with.
	tuiDir string
}

func parseTUI(args []string, env Env) (tuiFlags, error) {
	var f tuiFlags
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.StringVar(&f.addr, "addr", "127.0.0.1:0", "listen address (loopback; port 0 picks a free one)")
	fs.StringVar(&f.tuiDir, "tui-dir", env.Getenv("HERMES_TUI_DIR"),
		"upstream ui-tui directory to launch (default $HERMES_TUI_DIR; empty serves only)")
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

// tuiCmd serves the upstream Ink TUI's WebSocket protocol on a loopback port
// and launches the TUI against it, as upstream `hermes --tui` attaches to an
// explicit gateway URL.
func tuiCmd(ctx context.Context, args []string, env Env) int {
	f, err := parseTUI(args, env)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	entry := ""
	if f.tuiDir != "" {
		entry = filepath.Join(f.tuiDir, "dist", "entry.js")
		if _, err := os.Stat(entry); err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "hermes-go: no TUI build at %s; build it with: cd %s && npm install && npm run build:ink\n",
				entry, f.tuiDir)
			return 1
		}
	}

	// Ctrl-C reaches the TUI through the terminal; when serving only, it
	// stops the server.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	launched := make(chan error, 1)
	onListen := func(url string) {
		if entry == "" {
			_, _ = fmt.Fprintf(env.Stdout, "TUI gateway listening. Attach the upstream TUI with:\n  HERMES_TUI_GATEWAY_URL=%s\n", url)
			go func() {
				select {
				case <-env.Interrupt:
					cancel()
				case <-ctx.Done():
				}
			}()
			return
		}
		go func() {
			launched <- launchTUI(ctx, entry, url, env)
			cancel()
		}()
	}
	err = runTUI(ctx, f, env, onListen)
	if entry != "" && err == nil {
		err = <-launched
	}
	if err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "hermes-go: %v\n", err)
		return 1
	}
	return 0
}

// launchTUI runs the built Ink client attached to url, on this terminal.
func launchTUI(ctx context.Context, entry, url string, env Env) error {
	cmd := exec.CommandContext(ctx, "node", "--expose-gc", entry)
	cmd.Dir = filepath.Dir(filepath.Dir(entry))
	cmd.Env = append(os.Environ(), "HERMES_TUI_GATEWAY_URL="+url)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = env.Stdin, env.Stdout, env.Stderr
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

// runTUI serves the TUI gateway until ctx is cancelled. onListen receives the
// URL, token included, once the listener is bound.
func runTUI(ctx context.Context, f tuiFlags, env Env, onListen func(url string)) error {
	level := slog.LevelWarn
	if f.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: level}))

	host, _, err := net.SplitHostPort(f.addr)
	if err != nil {
		return fmt.Errorf("tui: bad address %q: %w", f.addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("tui: refusing non-loopback address %q", f.addr)
	}
	token, err := newToken()
	if err != nil {
		return err
	}

	var (
		srv    *tuigateway.Server
		srvErr error
	)
	rt, err := newChatRuntime(ctx, f.chatFlags, env, logger, func(rt *chatRuntime) (clarify.Asker, approval.Prompter) {
		srv, srvErr = tuigateway.New(tuigateway.Options{
			Token: token,
			Info: tuigateway.Info{
				Model: rt.ep.Model, Provider: rt.ep.Provider, Cwd: rt.cwd, Version: version, Profile: rt.f.profile,
			},
			// Resolved per request: the server is built before buildTools fills
			// rt.reg, so a value captured here would be nil forever.
			Inventory: func() tuigateway.Inventory {
				return tuigateway.Inventory{Tools: rt.reg.Inventory(), Skills: skillInventory(rt.skills)}
			},
			NewSession: func(sctx context.Context, source string, emit func(agent.Event)) tuigateway.Session {
				return tuiSession{rt.newObservedSession(sctx, source, io.Discard, emit)}
			},
			Store:  rt.store,
			Logger: logger,
		})
		return srv, srv
	})
	if err != nil {
		return err
	}
	defer rt.close()
	if srvErr != nil {
		return srvErr
	}

	ln, err := net.Listen("tcp", f.addr)
	if err != nil {
		srv.Close()
		return err
	}
	url := "ws://" + ln.Addr().String() + "/api/ws?token=" + token
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	logger.Info("tui gateway listening", "addr", ln.Addr().String(), "model", rt.ep.Model)
	onListen(url)

	select {
	case err = <-served:
	case <-ctx.Done():
	}
	srv.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = hs.Shutdown(shutdownCtx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// tuiSession adapts the CLI's chat session to the TUI gateway's Session.
type tuiSession struct{ s *chatSession }

func (t tuiSession) ID() string { return t.s.id }

func (t tuiSession) Run(ctx context.Context, input string) (string, error) {
	res, err := t.s.run(ctx, input)
	return res.Text, err
}

func (t tuiSession) End(reason string) { t.s.end(reason) }

// skillInventory groups the catalog by category for the client's banner. A
// skill with no category lands in "uncategorized", which is this port's bucket:
// the client only counts the entries, and leaving them out would undercount
// what the agent can actually invoke.
func skillInventory(c *skills.Catalog) map[string][]string {
	if c == nil {
		return nil
	}
	out := map[string][]string{}
	for _, s := range c.All() {
		category := "uncategorized"
		if s.Category != nil && *s.Category != "" {
			category = *s.Category
		}
		out[category] = append(out[category], s.Name)
	}
	for _, names := range out {
		sort.Strings(names)
	}
	return out
}
