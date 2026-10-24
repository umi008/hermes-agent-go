// Package config loads the Hermes config.yaml with upstream semantics
// (hermes_cli/config.py): deep merge over defaults, ${VAR} expansion in
// string values, and preservation of keys this port does not know.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// MinConfigVersion is the oldest _config_version this port reads without a
// warning. Upstream owns migrations; see docs/PROGRESS.md.
const MinConfigVersion = 12

// Config is a loaded, merged, and expanded config.yaml.
type Config struct {
	raw map[string]any
}

// Defaults returns the subset of upstream DEFAULT_CONFIG that V1 reads.
func Defaults() map[string]any {
	return map[string]any{
		"model":              "",
		"providers":          map[string]any{},
		"fallback_providers": []any{},
		"agent": map[string]any{
			"max_turns": nil,
		},
	}
}

// Options controls Load.
type Options struct {
	// Lookup resolves ${VAR} references. Nil leaves every reference verbatim.
	Lookup LookupFunc
	// Logger receives warnings. Nil discards them.
	Logger *slog.Logger
}

// Load reads path and merges it over Defaults. A missing file yields the
// defaults.
func Load(path string, opts Options) (*Config, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	user := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	default:
		user, err = Parse(data)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	merged := DeepMerge(Defaults(), user)
	expanded, _ := ExpandEnv(merged, opts.Lookup).(map[string]any)
	c := &Config{raw: expanded}
	if v, ok := c.Int("_config_version"); ok && v < MinConfigVersion {
		logger.Warn("config.yaml is older than this port supports; run upstream `hermes` once to migrate it",
			"path", path, "version", v, "min", MinConfigVersion)
	}
	return c, nil
}

// Parse decodes YAML whose top level must be a mapping. An empty document is
// an empty mapping. A UTF-8 BOM is ignored.
func Parse(data []byte) (map[string]any, error) {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode yaml: %w", err)
	}
	if doc == nil {
		return map[string]any{}, nil
	}
	m, ok := normalize(doc).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("top level must be a mapping, got %T", doc)
	}
	return m, nil
}

// normalize turns non-string YAML keys into strings so the tree is uniform.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = normalize(val)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[fmt.Sprint(k)] = normalize(val)
		}
		return m
	case []any:
		for i := range t {
			t[i] = normalize(t[i])
		}
		return t
	default:
		return v
	}
}

// DeepMerge merges override into a copy of base: a mapping over a mapping
// recurses, null over a mapping keeps the base mapping, anything else
// replaces. Keys only present in override are kept.
func DeepMerge(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, ov := range override {
		bv, exists := out[k]
		bm, baseIsMap := bv.(map[string]any)
		switch om, overIsMap := ov.(map[string]any); {
		case exists && baseIsMap && overIsMap:
			out[k] = DeepMerge(bm, om)
		case exists && baseIsMap && ov == nil:
		default:
			out[k] = ov
		}
	}
	return out
}

var (
	envRefRE       = regexp.MustCompile(`\$\{([^}]+)\}`)
	nonEnvSourceRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*:`)
)

// ExpandEnv replaces ${VAR} and ${env:VAR} in string values, recursively.
// Keys, non-strings, non-env sources such as ${vault:x}, and unset variables
// stay verbatim.
func ExpandEnv(v any, lookup LookupFunc) any {
	switch t := v.(type) {
	case string:
		if lookup == nil {
			return t
		}
		return envRefRE.ReplaceAllStringFunc(t, func(raw string) string {
			name, ok := envRefName(raw[2 : len(raw)-1])
			if !ok {
				return raw
			}
			if val, set := lookup(name); set {
				return val
			}
			return raw
		})
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = ExpandEnv(val, lookup)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = ExpandEnv(val, lookup)
		}
		return out
	default:
		return v
	}
}

func envRefName(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if rest, ok := strings.CutPrefix(ref, "env:"); ok {
		rest = strings.TrimSpace(rest)
		return rest, rest != ""
	}
	if nonEnvSourceRE.MatchString(ref) {
		return "", false
	}
	return ref, true
}

// FromMap wraps an already merged and expanded tree.
func FromMap(raw map[string]any) *Config {
	if raw == nil {
		raw = map[string]any{}
	}
	return &Config{raw: raw}
}

// Raw returns the merged tree. Callers must not mutate it.
func (c *Config) Raw() map[string]any { return c.raw }

// Get walks a key path through nested mappings.
func (c *Config) Get(path ...string) (any, bool) {
	var cur any = c.raw
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[key]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// String returns the string at path, or "" when absent or not a string.
func (c *Config) String(path ...string) string {
	v, _ := c.Get(path...)
	s, _ := v.(string)
	return s
}

// Map returns the mapping at path, or nil.
func (c *Config) Map(path ...string) map[string]any {
	v, _ := c.Get(path...)
	m, _ := v.(map[string]any)
	return m
}

// Int returns the integer at path. Integral floats and numeric strings count.
func (c *Config) Int(path ...string) (int, bool) {
	v, _ := c.Get(path...)
	return AsInt(v)
}

// AsInt converts a decoded YAML scalar to an int.
func AsInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case uint64:
		return int(t), true
	case float64:
		if t == float64(int(t)) {
			return int(t), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n, true
		}
	}
	return 0, false
}
