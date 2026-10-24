package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func lookupFrom(m map[string]string) LookupFunc {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDeepMerge(t *testing.T) {
	tests := []struct {
		name           string
		base, override map[string]any
		want           map[string]any
	}{
		{
			"nested recurse keeps base keys",
			map[string]any{"a": map[string]any{"x": 1, "y": 2}},
			map[string]any{"a": map[string]any{"y": 3}},
			map[string]any{"a": map[string]any{"x": 1, "y": 3}},
		},
		{
			"null over section is ignored",
			map[string]any{"a": map[string]any{"x": 1}},
			map[string]any{"a": nil},
			map[string]any{"a": map[string]any{"x": 1}},
		},
		{
			"null over scalar replaces",
			map[string]any{"a": 1},
			map[string]any{"a": nil},
			map[string]any{"a": nil},
		},
		{
			"scalar over section replaces",
			map[string]any{"model": map[string]any{"default": "x"}},
			map[string]any{"model": "y"},
			map[string]any{"model": "y"},
		},
		{
			"unknown keys preserved",
			map[string]any{},
			map[string]any{"future_key": []any{"z"}},
			map[string]any{"future_key": []any{"z"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeepMerge(tt.base, tt.override); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestDeepMergeDoesNotMutateBase(t *testing.T) {
	base := map[string]any{"a": map[string]any{"x": 1}}
	DeepMerge(base, map[string]any{"a": map[string]any{"x": 2}})
	if base["a"].(map[string]any)["x"] != 1 {
		t.Fatal("base mutated")
	}
}

func TestExpandEnv(t *testing.T) {
	lookup := lookupFrom(map[string]string{"KEY": "secret", "EMPTY": ""})
	tests := []struct {
		in, want string
	}{
		{"${KEY}", "secret"},
		{"${env:KEY}", "secret"},
		{"${ env:KEY }", "secret"},
		{"pre-${KEY}-post", "pre-secret-post"},
		{"${EMPTY}", ""},
		{"${MISSING}", "${MISSING}"},
		{"${vault:KEY}", "${vault:KEY}"},
		{"${env:}", "${env:}"},
		{"$KEY", "$KEY"},
	}
	for _, tt := range tests {
		if got := ExpandEnv(tt.in, lookup); got != tt.want {
			t.Errorf("ExpandEnv(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	tree := map[string]any{"${KEY}": []any{"${KEY}", 3}}
	got := ExpandEnv(tree, lookup)
	want := map[string]any{"${KEY}": []any{"secret", 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree: got %#v", got)
	}
}

func TestParse(t *testing.T) {
	m, err := Parse([]byte("\xEF\xBB\xBFmodel: x\n1: one\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m["model"] != "x" || m["1"] != "one" {
		t.Fatalf("got %#v", m)
	}
	if m, err := Parse(nil); err != nil || len(m) != 0 {
		t.Fatalf("empty: %v %v", m, err)
	}
	if _, err := Parse([]byte("- a\n- b\n")); err == nil {
		t.Fatal("list top level should fail")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yamlText := strings.Join([]string{
		"_config_version: 5",
		"model:",
		"  default: claude-opus-5-5",
		"  provider: anthropic",
		"providers:",
		"  local:",
		"    base_url: ${BASE}",
		"agent: null",
		"custom_section: {keep: true}",
	}, "\n")
	if err := os.WriteFile(path, []byte(yamlText), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	c, err := Load(path, Options{
		Lookup: lookupFrom(map[string]string{"BASE": "http://localhost:11434/v1"}),
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.String("model", "default"); got != "claude-opus-5-5" {
		t.Errorf("model.default = %q", got)
	}
	if got := c.String("providers", "local", "base_url"); got != "http://localhost:11434/v1" {
		t.Errorf("base_url = %q", got)
	}
	if c.Map("agent") == nil {
		t.Error("agent: null should keep the default section")
	}
	if v, ok := c.Get("custom_section", "keep"); !ok || v != true {
		t.Error("unknown key lost")
	}
	if !strings.Contains(logs.String(), "older than this port supports") {
		t.Errorf("want version warning, logs: %s", logs.String())
	}
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := c.Get("model"); !ok || v != "" {
		t.Fatalf("model = %#v", v)
	}
}

func TestAsInt(t *testing.T) {
	tests := []struct {
		in   any
		want int
		ok   bool
	}{
		{3, 3, true}, {3.0, 3, true}, {3.5, 0, false}, {" 7 ", 7, true}, {"x", 0, false}, {nil, 0, false},
	}
	for _, tt := range tests {
		if got, ok := AsInt(tt.in); got != tt.want || ok != tt.ok {
			t.Errorf("AsInt(%#v) = %d, %v", tt.in, got, ok)
		}
	}
}
