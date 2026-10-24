package main

import (
	"log/slog"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/skills"
	"github.com/umi008/hermes-agent-go/internal/tools"
)

// essentialSkills can never be disabled, like upstream ESSENTIAL_SKILLS.
var essentialSkills = map[string]bool{"hermes-agent": true}

var envRefRE = regexp.MustCompile(`\$(\w+)|\$\{([^}]*)\}`)

// openSkills reads the user's skills read-only: <hermes>/skills plus
// skills.external_dirs (~ and $VAR expanded, relative to the Hermes home),
// minus skills.disabled.
func openSkills(hermes, userHome string, cfg *config.Config, lookup config.LookupFunc, logger *slog.Logger) *skills.Catalog {
	var disabled []string
	for _, n := range configStrings(cfg, "skills", "disabled") {
		if !essentialSkills[n] {
			disabled = append(disabled, n)
		}
	}
	var external []string
	for _, e := range configStrings(cfg, "skills", "external_dirs") {
		external = append(external, expandPath(e, hermes, userHome, lookup))
	}
	var templateVars *bool
	if isFalse(cfg, "skills", "template_vars") {
		f := false
		templateVars = &f
	}
	return skills.New(skills.Options{
		LocalDir:        filepath.Join(hermes, "skills"),
		ExternalDirs:    external,
		Disabled:        disabled,
		GOOS:            runtime.GOOS,
		LookupEnv:       func(k string) (string, bool) { return lookup(k) },
		TerminalBackend: cfg.String("terminal", "backend"),
		TemplateVars:    templateVars,
		Logger:          logger,
	})
}

// configStrings reads a list or a single string, the shapes upstream
// _config_str_list accepts.
func configStrings(cfg *config.Config, path ...string) []string {
	v, _ := cfg.Get(path...)
	if s, ok := v.(string); ok {
		if s = strings.TrimSpace(s); s != "" {
			return []string{s}
		}
		return nil
	}
	var out []string
	for _, s := range stringList(cfg, path...) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// expandPath is upstream _home_relative(_expand_path(entry)): $VAR and
// ${VAR} (unknown names stay as written), a leading ~, then relative
// paths anchor at the Hermes home.
func expandPath(entry, hermes, userHome string, lookup config.LookupFunc) string {
	p := envRefRE.ReplaceAllStringFunc(entry, func(m string) string {
		name := strings.Trim(m, "${}")
		if v, ok := lookup(name); ok {
			return v
		}
		return m
	})
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		p = userHome + p[1:]
	}
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(hermes, p)
	}
	return p
}

// skillsIndex is upstream _skills_prompt: the index when a skills tool is
// on. Every Go chat is a single -q query, so the one-shot header applies.
func skillsIndex(cat *skills.Catalog, reg *tools.Registry, lookup config.LookupFunc) string {
	names, sets := map[string]bool{}, map[string]bool{}
	for _, s := range reg.Specs() {
		names[s.Name] = true
	}
	if !names["skills_list"] && !names["skill_view"] {
		return ""
	}
	for _, ts := range reg.Toolsets() {
		sets[ts] = true
	}
	platform, _ := lookup("HERMES_PLATFORM")
	return cat.Index(skills.IndexOptions{Tools: names, Toolsets: sets, SessionPlatform: platform, Oneshot: true})
}
