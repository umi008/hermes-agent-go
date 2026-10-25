package provider

import (
	"strings"

	"github.com/umi008/hermes-agent-go/internal/config"
)

// Auxiliary resolves the `auxiliary.<task>` block for a side call, as
// upstream _resolve_task_provider_model (agent/auxiliary_client.py). When
// the block pins a route (a base_url, or a provider other than "auto"),
// pinned is true and ep is that endpoint; a bare base_url resolves as
// `custom`. Otherwise the call inherits the main route and model, when set,
// overrides only the model name ("auto" counts as unset).
func Auxiliary(cfg *config.Config, lookup config.LookupFunc, task string) (ep Endpoint, model string, pinned bool, err error) {
	if cfg == nil {
		return Endpoint{}, "", false, nil
	}
	name := strings.ToLower(strings.TrimSpace(cfg.String("auxiliary", task, "provider")))
	model = strings.TrimSpace(cfg.String("auxiliary", task, "model"))
	if strings.EqualFold(model, "auto") {
		model = ""
	}
	baseURL := strings.TrimSpace(cfg.String("auxiliary", task, "base_url"))
	if baseURL == "" && (name == "" || name == "auto") {
		return Endpoint{}, model, false, nil
	}
	if name == "auto" || (name == "" && baseURL != "") {
		name = "custom"
	}
	apiKey := strings.TrimSpace(cfg.String("auxiliary", task, "api_key"))
	if apiKey == "" {
		if env := strings.TrimSpace(cfg.String("auxiliary", task, "key_env")); env != "" {
			apiKey, _ = lookupNonEmpty(lookup, env)
		} else if env := strings.TrimSpace(cfg.String("auxiliary", task, "api_key_env")); env != "" {
			apiKey, _ = lookupNonEmpty(lookup, env)
		}
	}
	ep, err = Resolve(cfg, lookup, Overrides{Provider: name, Model: model, BaseURL: baseURL, APIKey: apiKey})
	if err != nil {
		return Endpoint{}, "", false, err
	}
	if mode := canonicalMode(cfg.String("auxiliary", task, "api_mode")); mode != "" {
		ep.APIMode = mode
	}
	return ep, ep.Model, true, nil
}
