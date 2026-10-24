package mcpclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

// Upstream naming limits (tools/mcp_tool_schema.py).
const (
	NamePrefix    = "mcp__"
	maxNameLength = 64
	hashLength    = 8
)

var nameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_]`)

// ToolName is upstream mcp_prefixed_tool_name: mcp__<server>__<tool> with
// unsafe characters replaced by "_", clamped to 64 chars with a stable
// hash suffix.
func ToolName(server, tool string) string {
	full := NamePrefix + nameUnsafe.ReplaceAllString(server, "_") + "__" + nameUnsafe.ReplaceAllString(tool, "_")
	if len(full) <= maxNameLength {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "_" + hex.EncodeToString(sum[:])[:hashLength]
	return full[:maxNameLength-len(suffix)] + suffix
}

// stripTags drops Unicode TAG characters (U+E0000..U+E007F), an invisible
// prompt-injection channel. Upstream keeps valid emoji tag sequences; the
// port drops them all.
func stripTags(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0xE0000 && r <= 0xE007F {
			return -1
		}
		return r
	}, s)
}

var schemaMapKeys = map[string]bool{"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependentSchemas": true}

// NormalizeSchema is upstream _normalize_mcp_input_schema without the
// const-union collapse: definitions become $defs, nullable unions keep
// their one non-null branch, and object nodes get properties and a pruned
// required list.
func NormalizeSchema(schema any) json.RawMessage {
	m, ok := toMap(schema)
	if !ok || len(m) == 0 {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	n := repair(stripNullable(rewriteRefs(m)), true)
	out, ok := n.(map[string]any)
	if !ok {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	if out["type"] == "object" {
		if _, has := out["properties"]; !has {
			out["properties"] = map[string]any{}
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return data
}

// toMap round-trips a schema through JSON into plain maps.
func toMap(v any) (map[string]any, bool) {
	if v == nil {
		return nil, false
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return nil, false
	}
	return m, true
}

func rewriteRefs(node any) any {
	switch x := node.(type) {
	case []any:
		for i, e := range x {
			x[i] = rewriteRefs(e)
		}
		return x
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			if (k == "properties" || k == "patternProperties") && isMap(v) {
				props := map[string]any{}
				for name, s := range v.(map[string]any) {
					props[name] = rewriteRefs(s)
				}
				out[k] = props
				continue
			}
			if k == "definitions" {
				k = "$defs"
			}
			out[k] = rewriteRefs(v)
		}
		if ref, ok := out["$ref"].(string); ok && strings.HasPrefix(ref, "#/definitions/") {
			out["$ref"] = "#/$defs/" + strings.TrimPrefix(ref, "#/definitions/")
		}
		return out
	}
	return node
}

func isMap(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func isNullBranch(v any) bool {
	m, ok := v.(map[string]any)
	return ok && m["type"] == "null"
}

// stripNullable collapses anyOf/oneOf [X, {type: null}] to X with
// nullable: true, carrying the union's title, description and default.
func stripNullable(node any) any {
	switch x := node.(type) {
	case []any:
		for i, e := range x {
			x[i] = stripNullable(e)
		}
		return x
	case map[string]any:
		for k, v := range x {
			x[k] = stripNullable(v)
		}
		for _, key := range []string{"anyOf", "oneOf"} {
			variants, ok := x[key].([]any)
			if !ok {
				continue
			}
			var nonNull []any
			for _, v := range variants {
				if !isNullBranch(v) {
					nonNull = append(nonNull, v)
				}
			}
			if len(nonNull) != 1 || len(nonNull) == len(variants) {
				continue
			}
			repl := map[string]any{}
			if m, ok := nonNull[0].(map[string]any); ok {
				for k, v := range m {
					repl[k] = v
				}
			}
			if _, ok := repl["nullable"]; !ok {
				repl["nullable"] = true
			}
			for k, v := range x {
				if k == key {
					continue
				}
				if _, taken := repl[k]; !taken && (k != "default" || repl["$ref"] == nil) {
					repl[k] = v
				}
			}
			return stripNullable(repl)
		}
		return x
	}
	return node
}

// repair is upstream _repair_object_shape.
func repair(node any, root bool) any {
	switch x := node.(type) {
	case []any:
		for i, e := range x {
			x[i] = repair(e, false)
		}
		return x
	case map[string]any:
		_, hadType := x["type"]
		out := map[string]any{}
		for k, v := range x {
			if schemaMapKeys[k] && isMap(v) {
				m := map[string]any{}
				for name, s := range v.(map[string]any) {
					m[name] = repair(s, false)
				}
				out[k] = m
				continue
			}
			out[k] = repair(v, false)
		}
		_, hasProps := out["properties"]
		if root || hasProps || hadType {
			_, hasReq := out["required"]
			if t, _ := out["type"].(string); t == "" && (hasProps || hasReq) {
				out["type"] = "object"
			}
			if out["type"] == "object" {
				props, ok := out["properties"].(map[string]any)
				if !ok {
					props = map[string]any{}
					out["properties"] = props
				}
				req := []any{}
				if list, ok := out["required"].([]any); ok {
					for _, r := range list {
						if s, ok := r.(string); ok {
							if _, in := props[s]; in {
								req = append(req, s)
							}
						}
					}
				}
				out["required"] = req
			}
		}
		return out
	}
	return node
}

// filter is upstream _make_tool_filter: include is a whitelist (an empty
// one registers nothing) and wins over exclude; entries are exact names or
// globs.
type filter struct {
	include    []string
	includeSet bool
	exclude    []string
}

func matches(name string, patterns []string) bool {
	for _, p := range patterns {
		if p == name {
			return true
		}
		if strings.ContainsAny(p, "*?[") {
			if ok, _ := path.Match(p, name); ok {
				return true
			}
		}
	}
	return false
}

func (f filter) allows(name string) bool {
	if f.includeSet {
		return matches(name, f.include)
	}
	return !matches(name, f.exclude)
}
