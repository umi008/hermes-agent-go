package provider

import (
	"fmt"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/config"
)

// FallbackChain resolves the ordered backup endpoints, as upstream
// get_fallback_chain (hermes_cli/fallback_config.py): `fallback_providers`
// first, then the legacy `fallback_model` (one mapping or a list). Entries
// without a provider and a model are dropped, duplicates of an earlier entry
// (same provider, model and base URL) are dropped, and an entry that does not
// resolve (unknown provider, missing key) is skipped with a warning, as
// upstream marks it unavailable.
func FallbackChain(cfg *config.Config, lookup config.LookupFunc) ([]Endpoint, []string) {
	if cfg == nil {
		return nil, nil
	}
	var entries []map[string]any
	for _, key := range []string{"fallback_providers", "fallback_model"} {
		v, _ := cfg.Get(key)
		if m, ok := v.(map[string]any); ok {
			v = []any{m}
		}
		list, _ := v.([]any)
		for _, item := range list {
			if m, ok := item.(map[string]any); ok && firstString(m, "provider") != "" && firstString(m, "model") != "" {
				entries = append(entries, m)
			}
		}
	}

	var (
		out   []Endpoint
		warns []string
		seen  = map[string]bool{}
	)
	for _, e := range entries {
		name, model := firstString(e, "provider"), firstString(e, "model")
		baseURL := strings.TrimRight(firstString(e, "base_url"), "/")
		key := normalize(name) + "\x00" + strings.ToLower(model) + "\x00" + strings.ToLower(baseURL)
		if seen[key] {
			continue
		}
		seen[key] = true

		apiKey := firstString(e, "api_key")
		if apiKey == "" {
			if env := firstString(e, "key_env", "api_key_env"); env != "" {
				apiKey, _ = lookupNonEmpty(lookup, env)
			}
		}
		ep, err := Resolve(cfg, lookup, Overrides{Provider: name, Model: model, BaseURL: baseURL, APIKey: apiKey})
		if err != nil {
			warns = append(warns, fmt.Sprintf("fallback %s (%s) skipped: %v", model, name, err))
			continue
		}
		if mode := canonicalMode(firstString(e, "api_mode", "transport")); mode != "" {
			ep.APIMode = mode
		}
		out = append(out, ep)
	}
	return out, warns
}
