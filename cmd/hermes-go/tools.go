package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/skills"
	"github.com/umi008/hermes-agent-go/internal/state"
	"github.com/umi008/hermes-agent-go/internal/tools"
	"github.com/umi008/hermes-agent-go/internal/tools/approval"
	"github.com/umi008/hermes-agent-go/internal/tools/clarify"
	"github.com/umi008/hermes-agent-go/internal/tools/files"
	"github.com/umi008/hermes-agent-go/internal/tools/mcpclient"
	"github.com/umi008/hermes-agent-go/internal/tools/memory"
	"github.com/umi008/hermes-agent-go/internal/tools/sessionsearch"
	"github.com/umi008/hermes-agent-go/internal/tools/terminal"
	"github.com/umi008/hermes-agent-go/internal/tools/todo"
	"github.com/umi008/hermes-agent-go/internal/tools/web"
)

// toolDeps are the inputs the tool registry needs from the CLI.
type toolDeps struct {
	cfg      *config.Config
	lookup   config.LookupFunc
	homeDir  string // the user's home directory
	hermes   string // the Hermes home
	cwd      string
	toolsets string // comma-separated --toolsets filter; empty keeps all
	logger   *slog.Logger

	memory    *memory.Store // nil when both stores are disabled
	skills    *skills.Catalog
	store     *state.Store  // nil when the session store is unavailable
	sessionID func() string // the live session id; it changes on /new
	profile   string
	asker     clarify.Asker     // nil in single-query mode: nobody to ask
	prompter  approval.Prompter // nil in single-query mode
	// route is the runtime's primary model, which an auxiliary side call
	// inherits unless auxiliary.<task> pins its own.
	route agent.Route
}

// buildTools assembles the V2 tools. The returned close releases the
// terminal backend and MCP sessions and must run even on error paths after success.
func buildTools(ctx context.Context, d toolDeps) (*tools.Registry, func(), error) {
	det, err := approval.NewDetector(d.homeDir, d.hermes)
	if err != nil {
		return nil, nil, fmt.Errorf("approval patterns: %w", err)
	}
	sudo, _ := d.lookup("SUDO_PASSWORD")
	gate := approval.New(approval.Options{
		Detector:          det,
		Prompter:          d.prompter,
		Allowlist:         stringList(d.cfg, "command_allowlist"),
		Off:               approvalsMode(d.cfg, d.logger) == "off",
		UnattendedApprove: d.cfg.String("approvals", "single_query_mode") == "approve",
		SudoPassword:      sudo != "",
		Smart:             smartReviewer(d.cfg, d.lookup, d.route, d.logger),
		BreakerThreshold:  denialBreakerThreshold(d.cfg),
		Logger:            d.logger,
	})

	var all []tools.Tool
	closeFn := func() {}
	backend, err := newBackend(ctx, d)
	if err != nil {
		d.logger.Warn("terminal tools disabled", "err", err)
		cwd := d.cwd
		all = append(all, files.New(func() string { return cwd }, d.homeDir).Tools()...)
	} else {
		term := terminal.New(backend, d.cwd, gate.Check)
		closeFn = func() {
			if err := term.Close(); err != nil {
				d.logger.Warn("close terminal", "err", err)
			}
		}
		all = append(all, term.Tools()...)
		all = append(all, files.New(term.Cwd, d.homeDir).Tools()...)
	}
	key, _ := d.lookup("TAVILY_API_KEY")
	baseURL, _ := d.lookup("TAVILY_BASE_URL")
	all = append(all, todo.New().Tools()...)
	if d.memory != nil {
		all = append(all, d.memory.Tool())
	}
	if d.skills != nil {
		all = append(all, skills.Tools(d.skills, d.sessionID)...)
		// Upstream hides skill_manage in single-query runs: an agent that
		// nobody is watching should not rewrite its own instructions.
		if d.asker != nil {
			all = append(all, d.skills.Manage().Tool())
		}
	}
	if d.store != nil {
		profile := d.profile
		if profile == "" {
			profile = "default"
		}
		all = append(all, sessionsearch.Tool(sessionsearch.Options{
			Store: d.store, Current: d.sessionID, Profile: profile,
		}))
	}
	all = append(all, clarify.Tool(d.asker))
	all = append(all, web.New(web.Options{
		APIKey: key, BaseURL: baseURL,
		CacheDir: filepath.Join(d.hermes, "cache", "web"), Logger: d.logger,
	}).Tools()...)
	if servers := mcpServers(d.cfg, d.lookup, d.logger); len(servers) > 0 {
		mgr := mcpclient.Connect(ctx, mcpclient.Options{Servers: servers, LookupEnv: d.lookup, Logger: d.logger})
		all = append(all, mgr.Tools()...)
		closeTerm := closeFn
		closeFn = func() {
			if err := mgr.Close(); err != nil {
				d.logger.Warn("close MCP servers", "err", err)
			}
			closeTerm()
		}
	}

	reg, err := tools.NewRegistry(all...)
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	if d.toolsets != "" {
		var sets []string
		for _, s := range strings.Split(d.toolsets, ",") {
			if s = strings.TrimSpace(s); s != "" {
				sets = append(sets, s)
			}
		}
		reg = reg.Select(sets)
	}
	return reg, closeFn, nil
}

// newBackend picks terminal.backend: local (default) or docker.
func newBackend(ctx context.Context, d toolDeps) (terminal.Backend, error) {
	switch b := d.cfg.String("terminal", "backend"); b {
	case "", "local":
		return terminal.NewLocal()
	case "docker":
		o := terminal.DockerOptions{
			Image:  d.cfg.String("terminal", "docker_image"),
			Memory: d.cfg.String("terminal", "docker_memory"),
			CPUs:   d.cfg.String("terminal", "docker_cpus"),
		}
		if v, ok := d.cfg.Get("terminal", "docker_network"); ok && v == true {
			o.Network = true
		}
		if v, ok := d.cfg.Get("terminal", "docker_mount_cwd_to_workspace"); ok && v == true {
			o.MountDir = d.cwd
		}
		return terminal.NewDocker(ctx, o)
	default:
		return nil, fmt.Errorf("terminal.backend %q is not supported (local, docker)", b)
	}
}

// stringList reads a YAML list of strings, skipping non-strings.
func stringList(cfg *config.Config, path ...string) []string {
	v, _ := cfg.Get(path...)
	items, _ := v.([]any)
	var out []string
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// sessionSearchGuidance is upstream SESSION_SEARCH_GUIDANCE.
const sessionSearchGuidance = "When the user references something from a past conversation or you suspect relevant cross-session " +
	"context exists, use session_search to recall it before asking them to repeat themselves."

func hasTool(reg *tools.Registry, name string) bool {
	for _, s := range reg.Specs() {
		if s.Name == name {
			return true
		}
	}
	return false
}

// openMemory opens the Go port's memory dir. Like the session store it is
// separate from upstream's <home>/memories, so the port never rewrites the
// user's real notes. memory.memory_enabled and memory.user_profile_enabled
// both false disable the tool; limits come from memory.*_char_limit.
func openMemory(hermes string, cfg *config.Config, logger *slog.Logger) *memory.Store {
	if isFalse(cfg, "memory", "memory_enabled") && isFalse(cfg, "memory", "user_profile_enabled") {
		return nil
	}
	memLimit, _ := cfg.Int("memory", "memory_char_limit")
	userLimit, _ := cfg.Int("memory", "user_char_limit")
	s, err := memory.Open(filepath.Join(hermes, "hermes-go", "memories"),
		memory.Options{MemoryLimit: memLimit, UserLimit: userLimit, Logger: logger})
	if err != nil {
		logger.Warn("memory unavailable", "err", err)
		return nil
	}
	return s
}

func isFalse(cfg *config.Config, path ...string) bool {
	v, ok := cfg.Get(path...)
	if !ok {
		return false
	}
	switch x := v.(type) {
	case bool:
		return !x
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		return s == "false" || s == "0" || s == "no" || s == "off"
	}
	return false
}
