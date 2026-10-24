package skills

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

var fenceEnd = regexp.MustCompile(`\n---\s*\n`)

// ParseFrontmatter is upstream parse_frontmatter: YAML between leading
// "---" fences. Malformed YAML falls back to key: value line splitting; a
// leading BOM is stripped first.
func ParseFrontmatter(content string) (map[string]any, string) {
	content = strings.TrimPrefix(content, "\uFEFF")
	if !strings.HasPrefix(content, "---") {
		return map[string]any{}, content
	}
	loc := fenceEnd.FindStringIndex(content[3:])
	if loc == nil {
		return map[string]any{}, content
	}
	yamlText := content[3 : loc[0]+3]
	body := content[loc[1]+3:]
	fm := map[string]any{}
	var parsed any
	if err := yaml.Unmarshal([]byte(yamlText), &parsed); err != nil {
		for _, line := range strings.Split(strings.TrimSpace(yamlText), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok {
				fm[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		return fm, body
	}
	if m, ok := normalize(parsed).(map[string]any); ok {
		fm = m
	}
	return fm, body
}

// normalize turns yaml.v3 values into JSON-friendly ones: map keys become
// strings.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = normalize(e)
		}
		return x
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[fmt.Sprint(k)] = normalize(e)
		}
		return m
	case []any:
		for i, e := range x {
			x[i] = normalize(e)
		}
		return x
	}
	return v
}

// readText is upstream _read_skill_text: UTF-8 with the BOM dropped and
// invalid bytes replaced.
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.ToValidUTF8(strings.TrimPrefix(string(data), "\uFEFF"), "\uFFFD"), nil
}

func safeFrontmatter(path string) map[string]any {
	text, err := readText(path)
	if err != nil {
		return map[string]any{}
	}
	fm, _ := ParseFrontmatter(text)
	return fm
}

// truthy is Python truthiness for decoded YAML values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// str is Python str() for the scalar frontmatter values skills use.
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}

// get is dict.get(key, def): a present key wins even when its value is null.
func get(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// hermesMeta is upstream _hermes_metadata: metadata.hermes or empty.
func hermesMeta(fm map[string]any) map[string]any {
	if md, ok := fm["metadata"].(map[string]any); ok {
		if h, ok := md["hermes"].(map[string]any); ok {
			return h
		}
	}
	return map[string]any{}
}

// stringList reads a list-or-scalar frontmatter value as strings.
func stringList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, str(e))
		}
		return out
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	}
	return []string{str(v)}
}

// parseTags is upstream _parse_tags: a list, "[a, b]" or "a, b".
func parseTags(v any) []string {
	out := []string{}
	if !truthy(v) {
		return out
	}
	if list, ok := v.([]any); ok {
		for _, t := range list {
			if truthy(t) {
				out = append(out, strings.TrimSpace(str(t)))
			}
		}
		return out
	}
	s := strings.TrimSpace(str(v))
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, strings.Trim(t, `"'`))
		}
	}
	return out
}
