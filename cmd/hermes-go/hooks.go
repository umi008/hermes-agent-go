package main

import (
	"log/slog"
	"path/filepath"

	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/hooks"
)

// hookSpecs reads hooks: like upstream register_from_config, minus the
// TTY prompt: a hook runs when --accept-hooks, HERMES_ACCEPT_HOOKS or
// hooks_auto_accept is on, or upstream's allowlist already approves it.
// The port never writes the allowlist. HERMES_SAFE_MODE runs no hooks.
func hookSpecs(cfg *config.Config, hermes string, accept bool, lookup config.LookupFunc, logger *slog.Logger) []hooks.Spec {
	if v, _ := lookup("HERMES_SAFE_MODE"); boolish(v, false) {
		return nil
	}
	raw, _ := cfg.Get("hooks")
	specs := hooks.Parse(raw, logger)
	if len(specs) == 0 {
		return nil
	}
	if v, _ := lookup("HERMES_ACCEPT_HOOKS"); boolish(v, false) {
		accept = true
	}
	switch v, _ := cfg.Get("hooks_auto_accept"); x := v.(type) {
	case bool:
		accept = accept || x
	case string:
		accept = accept || boolish(x, false)
	}
	path := filepath.Join(hermes, "shell-hooks-allowlist.json")
	consent := hooks.LoadConsent(path, accept)
	var out []hooks.Spec
	for _, s := range specs {
		if consent.Allows(s) {
			out = append(out, s)
			continue
		}
		logger.Warn("shell hook not allowlisted, skipped; use --accept-hooks, HERMES_ACCEPT_HOOKS=1 or hooks_auto_accept: true",
			"event", s.Event, "command", s.Command, "allowlist", path)
	}
	return out
}
