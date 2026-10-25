package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/home"
	"github.com/umi008/hermes-agent-go/internal/provider"
)

// doctorReport prints one line per check and counts the failed ones. Only a
// fail changes the exit code; a warn names something optional.
type doctorReport struct {
	w      io.Writer
	failed int
}

func (r *doctorReport) add(status, check, format string, args ...any) {
	if status == "fail" {
		r.failed++
	}
	_, _ = fmt.Fprintf(r.w, "%-5s %-9s %s\n", status, check, fmt.Sprintf(format, args...))
}

// doctorCmd checks the install without writing anything: the home, the
// config, the provider and the optional tools.
func doctorCmd(args []string, env Env) int {
	var profile string
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	for _, name := range []string{"p", "profile"} {
		fs.StringVar(&profile, name, "", "profile name")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	lookPath := env.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}

	r := &doctorReport{w: env.Stdout}
	r.add("ok", "version", "hermes-go %s", version)
	doctorChecks(r, profile, env, lookPath)

	if r.failed > 0 {
		noun := "check"
		if r.failed > 1 {
			noun = "checks"
		}
		_, _ = fmt.Fprintf(env.Stdout, "\n%d required %s failed.\n", r.failed, noun)
		return 1
	}
	_, _ = fmt.Fprintln(env.Stdout, "\nAll required checks passed.")
	return 0
}

func doctorChecks(r *doctorReport, profile string, env Env, lookPath func(string) (string, error)) {
	h, err := home.Resolver{GOOS: env.GOOS, Getenv: env.Getenv, UserHomeDir: env.UserHomeDir}.Resolve(profile)
	if err != nil {
		r.add("fail", "home", "%v", err)
		return
	}
	if info, err := os.Stat(h.Dir); err != nil || !info.IsDir() {
		r.add("warn", "home", "%s does not exist yet (profile %s); chat creates what it needs", h.Dir, h.Profile)
	} else {
		r.add("ok", "home", "%s (profile %s)", h.Dir, h.Profile)
	}

	envPath := filepath.Join(h.Dir, ".env")
	dotenv, err := config.LoadDotenv(envPath)
	if err != nil {
		r.add("fail", "env", "%v", err)
		dotenv = map[string]string{}
	} else if len(dotenv) > 0 {
		r.add("ok", "env", "%s (%d keys)", envPath, len(dotenv))
	}
	lookup := config.Overlay(dotenv, env.LookupEnv)

	cfgPath := filepath.Join(h.Dir, "config.yaml")
	cfg, err := config.Load(cfgPath, config.Options{Lookup: lookup})
	switch {
	case err != nil:
		r.add("fail", "config", "%v", err)
		return
	case !fileExists(cfgPath):
		r.add("warn", "config", "%s not found; defaults and environment variables apply", cfgPath)
	default:
		if v, ok := cfg.Int("_config_version"); !ok {
			r.add("ok", "config", "%s (no _config_version)", cfgPath)
		} else if v < config.MinConfigVersion {
			r.add("warn", "config", "%s has _config_version %d, older than %d; run upstream `hermes` once to migrate it", cfgPath, v, config.MinConfigVersion)
		} else {
			r.add("ok", "config", "%s (_config_version %d)", cfgPath, v)
		}
	}

	if ep, err := provider.Resolve(cfg, lookup, provider.Overrides{}); err != nil {
		r.add("fail", "provider", "%v", err)
	} else {
		key := "API key set"
		if ep.APIKey == "" {
			key = "no API key (not required by this provider)"
		}
		r.add("ok", "provider", "%s, model %s, %s, %s", ep.Provider, ep.Model, ep.BaseURL, key)
	}

	if v, ok := lookup("TAVILY_API_KEY"); ok && v != "" {
		r.add("ok", "web", "TAVILY_API_KEY set")
	} else {
		r.add("warn", "web", "TAVILY_API_KEY not set; web_search and web_extract are unavailable")
	}
	if cfg.String("terminal", "backend") == "docker" {
		if p, err := lookPath("docker"); err != nil {
			r.add("fail", "docker", "terminal.backend is docker but docker is not on PATH")
		} else {
			r.add("ok", "docker", "%s", p)
		}
	}
	if p, err := lookPath("git"); err != nil {
		r.add("warn", "git", "not found; search_files also searches git-ignored untracked files")
	} else {
		r.add("ok", "git", "%s", p)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
