package provider

import "testing"

func TestAuxiliary(t *testing.T) {
	lookup := env(map[string]string{"ANTHROPIC_API_KEY": "ak", "AUX_KEY": "xk"})
	cases := []struct {
		name, yaml string
		want       Endpoint
		model      string
		pinned     bool
	}{
		{name: "unset inherits", yaml: "model: {default: main, provider: anthropic}\n"},
		{name: "auto with model overrides only the model",
			yaml:  "model: {default: main, provider: anthropic}\nauxiliary: {compression: {provider: auto, model: cheap}}\n",
			model: "cheap"},
		{name: "auto model is unset",
			yaml: "model: {default: main, provider: anthropic}\nauxiliary: {compression: {provider: auto, model: auto}}\n"},
		{name: "pinned provider",
			yaml:   "model: {default: main, provider: custom, base_url: http://x/v1}\nauxiliary: {compression: {provider: anthropic, model: haiku}}\n",
			want:   Endpoint{Provider: "anthropic", Model: "haiku", BaseURL: "https://api.anthropic.com", APIKey: "ak", APIMode: ModeAnthropicMessages},
			model:  "haiku",
			pinned: true},
		{name: "bare base_url is custom with key_env",
			yaml:   "model: {default: main, provider: anthropic}\nauxiliary: {compression: {base_url: \"http://127.0.0.1:9/v1\", model: tiny, key_env: AUX_KEY}}\n",
			want:   Endpoint{Provider: "custom", Model: "tiny", BaseURL: "http://127.0.0.1:9/v1", APIKey: "xk", APIMode: ModeChatCompletions},
			model:  "tiny",
			pinned: true},
	}
	for _, c := range cases {
		ep, model, pinned, err := Auxiliary(cfgFrom(t, c.yaml), lookup, "compression")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if ep != c.want || model != c.model || pinned != c.pinned {
			t.Errorf("%s: got %+v %q %v, want %+v %q %v", c.name, ep, model, pinned, c.want, c.model, c.pinned)
		}
	}
	if _, _, _, err := Auxiliary(cfgFrom(t, "auxiliary: {compression: {provider: nowhere, model: m}}\n"), lookup, "compression"); err == nil {
		t.Error("an unknown pinned provider must fail")
	}
}
