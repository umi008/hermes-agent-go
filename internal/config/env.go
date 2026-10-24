package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// LookupFunc reports an environment variable's value and whether it is set.
type LookupFunc func(name string) (string, bool)

// Overlay returns a lookup where values from dotenv win over next. Upstream
// loads ~/.hermes/.env with override semantics, so stale shell exports never
// shadow the user's saved secrets (hermes_cli/env_loader.py).
func Overlay(dotenv map[string]string, next LookupFunc) LookupFunc {
	return func(name string) (string, bool) {
		if v, ok := dotenv[name]; ok {
			return v, true
		}
		if next == nil {
			return "", false
		}
		return next(name)
	}
}

// LoadDotenv reads a .env file. A missing file yields an empty map.
func LoadDotenv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return ParseDotenv(data), nil
}

// ParseDotenv tokenizes .env content with the same rules as upstream
// agent/secret_scope.py: blank and # lines are skipped, a leading "export "
// is dropped, the key is everything before the first "=", inline comments
// are stripped, and quoted values are unquoted. Later keys win.
func ParseDotenv(data []byte) map[string]string {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	text := decodeText(data)
	out := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			continue
		}
		out[key] = parseEnvValue(stripInlineComment(value))
	}
	return out
}

// decodeText returns UTF-8 text, falling back to latin-1 like upstream.
func decodeText(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	runes := make([]rune, len(data))
	for i, b := range data {
		runes[i] = rune(b)
	}
	return string(runes)
}

var inlineCommentRE = regexp.MustCompile(`\s+#`)

func stripInlineComment(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return v
	}
	if q := v[0]; q == '"' || q == '\'' {
		end := closingQuote(v, q)
		if end < 0 {
			return v
		}
		if rest := strings.TrimLeft(v[end+1:], " \t"); strings.HasPrefix(rest, "#") {
			return v[:end+1]
		}
		return v
	}
	if loc := inlineCommentRE.FindStringIndex(v); loc != nil {
		v = v[:loc[0]]
	}
	return strings.TrimSpace(v)
}

// closingQuote returns the index of the quote closing v[0], or -1. Double
// quotes honour backslash escapes; single quotes do not.
func closingQuote(v string, q byte) int {
	for i := 1; i < len(v); i++ {
		if q == '"' && v[i] == '\\' {
			i++
			continue
		}
		if v[i] == q {
			return i
		}
	}
	return -1
}

func parseEnvValue(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		inner := v[1 : len(v)-1]
		var b strings.Builder
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) && (inner[i+1] == '"' || inner[i+1] == '\\') {
				i++
			}
			b.WriteByte(inner[i])
		}
		return b.String()
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	return v
}
