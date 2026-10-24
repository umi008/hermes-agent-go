package provider

import (
	"errors"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/config"
)

func cfgFrom(t *testing.T, yaml string) *config.Config {
	t.Helper()
	user, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return config.FromMap(config.DeepMerge(config.Defaults(), user))
}

func env(m map[string]string) config.LookupFunc {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		env  map[string]string
		o    Overrides
		want Endpoint
	}{
		{
			name: "config provider anthropic",
			yaml: "model: {default: claude-opus-5-5, provider: claude}",
			env:  map[string]string{"ANTHROPIC_API_KEY": "ak"},
			want: Endpoint{Provider: "anthropic", Model: "claude-opus-5-5", BaseURL: "https://api.anthropic.com", APIKey: "ak", APIMode: ModeAnthropicMessages},
		},
		{
			name: "auto prefers openrouter key",
			yaml: "model: anthropic/claude-opus-5-5",
			env:  map[string]string{"OPENROUTER_API_KEY": "or", "ANTHROPIC_API_KEY": "ak"},
			want: Endpoint{Provider: "openrouter", Model: "anthropic/claude-opus-5-5", BaseURL: "https://openrouter.ai/api/v1", APIKey: "or", APIMode: ModeChatCompletions},
		},
		{
			name: "sk-or key in OPENAI_API_KEY means openrouter",
			yaml: "model: m",
			env:  map[string]string{"OPENAI_API_KEY": "sk-or-123"},
			want: Endpoint{Provider: "openrouter", Model: "m", BaseURL: "https://openrouter.ai/api/v1", APIKey: "sk-or-123", APIMode: ModeChatCompletions},
		},
		{
			name: "auto falls through to first key in ladder",
			yaml: "model: deepseek-chat",
			env:  map[string]string{"DEEPSEEK_API_KEY": "dk"},
			want: Endpoint{Provider: "deepseek", Model: "deepseek-chat", BaseURL: "https://api.deepseek.com/v1", APIKey: "dk", APIMode: ModeChatCompletions},
		},
		{
			name: "named providers entry with key_env",
			yaml: "model: {default: llama3, provider: home}\nproviders:\n  home:\n    base_url: http://localhost:11434/v1\n    key_env: HOME_KEY",
			env:  map[string]string{"HOME_KEY": "hk"},
			want: Endpoint{Provider: "home", Model: "llama3", BaseURL: "http://localhost:11434/v1", APIKey: "hk", APIMode: ModeChatCompletions},
		},
		{
			name: "named entry default model and transport",
			yaml: "model: {provider: proxy}\nproviders:\n  proxy:\n    base_url: https://p.example\n    api_key: k\n    transport: anthropic\n    default_model: c",
			want: Endpoint{Provider: "proxy", Model: "c", BaseURL: "https://p.example", APIKey: "k", APIMode: ModeAnthropicMessages},
		},
		{
			name: "custom alias with model.base_url and no key",
			yaml: "model: {default: qwen, provider: ollama, base_url: http://127.0.0.1:11434/v1}",
			want: Endpoint{Provider: "custom", Model: "qwen", BaseURL: "http://127.0.0.1:11434/v1", APIMode: ModeChatCompletions},
		},
		{
			name: "explicit base url overrides means custom",
			yaml: "model: m",
			o:    Overrides{BaseURL: "http://x/v1"},
			want: Endpoint{Provider: "custom", Model: "m", BaseURL: "http://x/v1", APIMode: ModeChatCompletions},
		},
		{
			name: "cli overrides win",
			yaml: "model: {default: a, provider: deepseek}",
			env:  map[string]string{"DEEPSEEK_API_KEY": "dk", "ANTHROPIC_API_KEY": "ak"},
			o:    Overrides{Provider: "anthropic", Model: "b", APIKey: "flag"},
			want: Endpoint{Provider: "anthropic", Model: "b", BaseURL: "https://api.anthropic.com", APIKey: "flag", APIMode: ModeAnthropicMessages},
		},
		{
			name: "lmstudio base url env",
			yaml: "model: {default: m, provider: lmstudio}",
			env:  map[string]string{"LM_BASE_URL": "http://box:1234/v1"},
			want: Endpoint{Provider: "lmstudio", Model: "m", BaseURL: "http://box:1234/v1", APIMode: ModeChatCompletions},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(cfgFrom(t, tt.yaml), env(tt.env), tt.o)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestResolveErrors(t *testing.T) {
	tests := []struct {
		name, yaml string
		env        map[string]string
		o          Overrides
	}{
		{"nothing configured", "model: m", nil, Overrides{}},
		{"unknown provider", "model: {default: m, provider: nope}", nil, Overrides{}},
		{"missing key", "model: {default: m, provider: anthropic}", nil, Overrides{}},
		{"missing model", "model: {provider: anthropic}", map[string]string{"ANTHROPIC_API_KEY": "k"}, Overrides{}},
		{"custom without base url", "model: {default: m, provider: custom}", nil, Overrides{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Resolve(cfgFrom(t, tt.yaml), env(tt.env), tt.o); err == nil {
				t.Fatal("want error")
			}
		})
	}
	_, err := Resolve(cfgFrom(t, "model: m"), env(nil), Overrides{})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("err = %v, want ErrNoProvider", err)
	}
}
