// Package provider resolves which LLM endpoint to call from config.yaml, the
// environment, and CLI overrides. It ports the subset of upstream
// resolve_provider (hermes_cli/auth.py) and the provider profiles under
// plugins/model-providers that this Go port supports.
package provider

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/config"
)

// API modes.
const (
	ModeChatCompletions   = "chat_completions"
	ModeAnthropicMessages = "anthropic_messages"
)

// Profile is a built-in provider definition.
type Profile struct {
	Name    string
	Aliases []string
	// EnvVars are API key variables, in priority order.
	EnvVars []string
	BaseURL string
	// BaseURLEnv overrides BaseURL when set.
	BaseURLEnv string
	APIMode    string
	// KeyOptional marks local servers that accept requests without a key.
	KeyOptional bool
}

// builtin is the supported subset of upstream provider profiles, in the
// order the auto ladder probes them.
var builtin = []Profile{
	{Name: "openrouter", Aliases: []string{"or"}, EnvVars: []string{"OPENROUTER_API_KEY"}, BaseURL: "https://openrouter.ai/api/v1", APIMode: ModeChatCompletions},
	{Name: "anthropic", Aliases: []string{"claude", "claude-oauth", "claude-code"}, EnvVars: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}, BaseURL: "https://api.anthropic.com", APIMode: ModeAnthropicMessages},
	{Name: "deepseek", EnvVars: []string{"DEEPSEEK_API_KEY"}, BaseURL: "https://api.deepseek.com/v1", APIMode: ModeChatCompletions},
	{Name: "ollama-cloud", EnvVars: []string{"OLLAMA_API_KEY"}, BaseURL: "https://ollama.com/v1", APIMode: ModeChatCompletions},
	// Upstream resolves the LM Studio URL elsewhere (hermes_cli/models_local.py);
	// the port defaults to the LM Studio server's standard address.
	{Name: "lmstudio", EnvVars: []string{"LM_API_KEY"}, BaseURL: "http://localhost:1234/v1", BaseURLEnv: "LM_BASE_URL", APIMode: ModeChatCompletions, KeyOptional: true},
	{Name: "custom", Aliases: []string{"ollama", "local", "vllm", "llamacpp", "llama.cpp", "llama-cpp"}, APIMode: ModeChatCompletions, KeyOptional: true},
}

// Endpoint is a resolved call target.
type Endpoint struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
	APIMode  string
}

// Overrides are CLI flags; empty fields defer to config.
type Overrides struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
}

// ErrNoProvider means nothing configured a usable provider.
var ErrNoProvider = errors.New("no LLM provider configured")

// Resolve picks the endpoint. Order for provider "auto" (or unset): an
// explicit base URL means "custom"; then config model.provider; then an
// OpenRouter key; then the first built-in profile with a key in the env.
func Resolve(cfg *config.Config, lookup config.LookupFunc, o Overrides) (Endpoint, error) {
	model, cfgProvider, cfgBaseURL := modelConfig(cfg)
	if o.Model != "" {
		model = o.Model
	}
	name := normalize(o.Provider)
	baseURL := o.BaseURL
	if name == "" || name == "auto" {
		name = autoProvider(cfgProvider, o, lookup)
		if baseURL == "" && name == normalize(cfgProvider) {
			baseURL = cfgBaseURL
		}
	}
	if name == "" {
		return Endpoint{}, fmt.Errorf("%w: set model.provider in config.yaml, pass --provider, or export a provider API key (%s)", ErrNoProvider, strings.Join(keyVars(), ", "))
	}

	var ep Endpoint
	if entry := namedEntry(cfg, name); entry != nil {
		ep = fromEntry(name, entry, lookup)
	} else if p, ok := profileFor(name); ok {
		ep = fromProfile(p, lookup)
	} else {
		return Endpoint{}, fmt.Errorf("unknown provider %q (known: %s)", name, strings.Join(Names(), ", "))
	}
	if baseURL != "" {
		ep.BaseURL = baseURL
	}
	if o.APIKey != "" {
		ep.APIKey = o.APIKey
	}
	if model != "" {
		ep.Model = model
	}

	switch {
	case ep.BaseURL == "":
		return Endpoint{}, fmt.Errorf("provider %q has no base_url; set model.base_url or providers.%s.base_url", ep.Provider, ep.Provider)
	case ep.Model == "":
		return Endpoint{}, fmt.Errorf("no model configured for provider %q; set model.default in config.yaml or pass --model", ep.Provider)
	case ep.APIKey == "" && !keyOptional(ep.Provider, cfg):
		return Endpoint{}, fmt.Errorf("provider %q has no API key; export one of %s", ep.Provider, strings.Join(envVarsFor(ep.Provider), ", "))
	}
	return ep, nil
}

func autoProvider(cfgProvider string, o Overrides, lookup config.LookupFunc) string {
	if o.BaseURL != "" {
		return "custom"
	}
	if p := normalize(cfgProvider); p != "" && p != "auto" {
		return p
	}
	if v, ok := lookupNonEmpty(lookup, "OPENROUTER_API_KEY"); ok && v != "" {
		return "openrouter"
	}
	if v, _ := lookupNonEmpty(lookup, "OPENAI_API_KEY"); strings.HasPrefix(v, "sk-or-") {
		return "openrouter"
	}
	for _, p := range builtin {
		for _, env := range p.EnvVars {
			if _, ok := lookupNonEmpty(lookup, env); ok {
				return p.Name
			}
		}
	}
	return ""
}

// modelConfig reads `model:` as a string or a {default|model, provider,
// base_url} mapping.
func modelConfig(cfg *config.Config) (model, provider, baseURL string) {
	if cfg == nil {
		return "", "", ""
	}
	if s := cfg.String("model"); s != "" {
		return strings.TrimSpace(s), "", ""
	}
	m := cfg.Map("model")
	model = firstString(m, "default", "model")
	return model, firstString(m, "provider"), firstString(m, "base_url")
}

func namedEntry(cfg *config.Config, name string) map[string]any {
	if cfg == nil {
		return nil
	}
	providers := cfg.Map("providers")
	for key, v := range providers {
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if normalize(key) == name || normalize(firstString(entry, "name")) == name {
			return entry
		}
	}
	return nil
}

func fromEntry(name string, entry map[string]any, lookup config.LookupFunc) Endpoint {
	ep := Endpoint{
		Provider: name,
		BaseURL:  firstString(entry, "base_url", "url"),
		APIKey:   firstString(entry, "api_key"),
		APIMode:  canonicalMode(firstString(entry, "api_mode", "transport")),
		Model:    firstString(entry, "model", "default_model"),
	}
	if ep.APIKey == "" {
		if env := firstString(entry, "key_env", "api_key_env"); env != "" {
			ep.APIKey, _ = lookupNonEmpty(lookup, env)
		}
	}
	if ep.APIMode == "" {
		ep.APIMode = ModeChatCompletions
		if p, ok := profileFor(name); ok {
			ep.APIMode = p.APIMode
		}
	}
	return ep
}

func fromProfile(p Profile, lookup config.LookupFunc) Endpoint {
	ep := Endpoint{Provider: p.Name, BaseURL: p.BaseURL, APIMode: p.APIMode}
	if p.BaseURLEnv != "" {
		if v, ok := lookupNonEmpty(lookup, p.BaseURLEnv); ok {
			ep.BaseURL = v
		}
	}
	for _, env := range p.EnvVars {
		if v, ok := lookupNonEmpty(lookup, env); ok {
			ep.APIKey = v
			break
		}
	}
	if ep.APIKey == "" && p.Name == "openrouter" {
		if v, _ := lookupNonEmpty(lookup, "OPENAI_API_KEY"); strings.HasPrefix(v, "sk-or-") {
			ep.APIKey = v
		}
	}
	return ep
}

func canonicalMode(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "":
		return ""
	case "anthropic", "anthropic_messages", "messages":
		return ModeAnthropicMessages
	default:
		return ModeChatCompletions
	}
}

func keyOptional(name string, cfg *config.Config) bool {
	if namedEntry(cfg, name) != nil {
		return true
	}
	p, ok := profileFor(name)
	return ok && p.KeyOptional
}

func profileFor(name string) (Profile, bool) {
	for _, p := range builtin {
		if p.Name == name {
			return p, true
		}
		for _, a := range p.Aliases {
			if a == name {
				return p, true
			}
		}
	}
	return Profile{}, false
}

func normalize(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if p, ok := profileFor(n); ok {
		return p.Name
	}
	return n
}

// Names lists the built-in provider names.
func Names() []string {
	out := make([]string, 0, len(builtin))
	for _, p := range builtin {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

func keyVars() []string {
	var out []string
	for _, p := range builtin {
		out = append(out, p.EnvVars...)
	}
	return out
}

func envVarsFor(name string) []string {
	if p, ok := profileFor(name); ok && len(p.EnvVars) > 0 {
		return p.EnvVars
	}
	return []string{"providers." + name + ".api_key"}
}

func lookupNonEmpty(lookup config.LookupFunc, name string) (string, bool) {
	if lookup == nil {
		return "", false
	}
	v, ok := lookup(name)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
