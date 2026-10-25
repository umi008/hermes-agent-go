package provider

import (
	"reflect"
	"strings"
	"testing"
)

func TestFallbackChain(t *testing.T) {
	cfg := cfgFrom(t, `
model: {default: main, provider: openrouter}
providers:
  home:
    base_url: http://localhost:11434/v1
fallback_providers:
  - {provider: anthropic, model: claude-x}
  - {provider: home, model: llama3, api_mode: anthropic}
  - {provider: Anthropic, model: CLAUDE-X}
  - {provider: deepseek}
  - {provider: custom, model: local, base_url: "http://127.0.0.1:8080/v1/", key_env: LOCAL_KEY}
  - {provider: deepseek, model: deepseek-chat}
  - {provider: nowhere, model: m}
fallback_model: {provider: lmstudio, model: qwen}
`)
	got, warns := FallbackChain(cfg, env(map[string]string{"ANTHROPIC_API_KEY": "ak", "LOCAL_KEY": "lk"}))
	want := []Endpoint{
		{Provider: "anthropic", Model: "claude-x", BaseURL: "https://api.anthropic.com", APIKey: "ak", APIMode: ModeAnthropicMessages},
		{Provider: "home", Model: "llama3", BaseURL: "http://localhost:11434/v1", APIMode: ModeAnthropicMessages},
		{Provider: "custom", Model: "local", BaseURL: "http://127.0.0.1:8080/v1", APIKey: "lk", APIMode: ModeChatCompletions},
		{Provider: "lmstudio", Model: "qwen", BaseURL: "http://localhost:1234/v1", APIMode: ModeChatCompletions},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chain =\n%+v\nwant\n%+v", got, want)
	}
	// deepseek has no key and "nowhere" is unknown: both are skipped with a
	// warning, as upstream marks an unconfigured entry unavailable.
	if len(warns) != 2 || !strings.Contains(warns[0], "deepseek") || !strings.Contains(warns[1], "nowhere") {
		t.Fatalf("warnings = %q", warns)
	}
}

func TestFallbackModelListAndEmpty(t *testing.T) {
	cfg := cfgFrom(t, "fallback_model:\n  - {provider: anthropic, model: a}\n  - {provider: anthropic, model: b}\n")
	got, _ := FallbackChain(cfg, env(map[string]string{"ANTHROPIC_API_KEY": "ak"}))
	if len(got) != 2 || got[0].Model != "a" || got[1].Model != "b" {
		t.Fatalf("chain = %+v", got)
	}
	if got, warns := FallbackChain(cfgFrom(t, "model: m"), env(nil)); len(got) != 0 || len(warns) != 0 {
		t.Fatalf("empty config: %+v %q", got, warns)
	}
}
