package main

import (
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/tools/mcpclient"
)

// mcpServers reads mcp_servers like upstream _load_mcp_config. ${VAR}
// references are already expanded by config.Load. HERMES_SAFE_MODE turns
// MCP off, and a server with enabled: false is skipped.
func mcpServers(cfg *config.Config, lookup config.LookupFunc, logger *slog.Logger) []mcpclient.Server {
	if v, _ := lookup("HERMES_SAFE_MODE"); boolish(v, false) {
		return nil
	}
	raw := cfg.Map("mcp_servers")
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []mcpclient.Server
	for _, name := range names {
		m, ok := raw[name].(map[string]any)
		if !ok {
			logger.Warn("mcp_servers entry is not a mapping, skipping", "server", name)
			continue
		}
		if v, ok := m["enabled"]; ok && !boolish(v, true) {
			continue
		}
		s := mcpclient.Server{
			Name:      name,
			Command:   str(m["command"]),
			Args:      strs(m["args"]),
			Env:       strMap(m["env"]),
			Cwd:       str(m["cwd"]),
			URL:       str(m["url"]),
			Headers:   strMap(m["headers"]),
			Transport: strings.ToLower(str(m["transport"])),

			CallTimeout:    seconds(m["timeout"]),
			ConnectTimeout: seconds(m["connect_timeout"]),
		}
		if f, ok := m["tools"].(map[string]any); ok {
			if inc, ok := f["include"]; ok && inc != nil {
				s.Include = append([]string{}, strs(inc)...)
			}
			s.Exclude = strs(f["exclude"])
		}
		out = append(out, s)
	}
	return out
}

// boolish is upstream _parse_boolish: bools and numbers by truth, the
// usual words, and the default for anything else.
func boolish(v any, def bool) bool {
	switch x := v.(type) {
	case nil:
		return def
	case bool:
		return x
	case int:
		return x != 0
	case float64:
		return x != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
	}
	return def
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

// strs reads a list of scalars or a single string.
func strs(v any) []string {
	switch x := v.(type) {
	case string:
		if x = strings.TrimSpace(x); x != "" {
			return []string{x}
		}
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if e != nil {
				out = append(out, str(e))
			}
		}
		return out
	}
	return nil
}

func strMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, e := range m {
		out[k] = str(e)
	}
	return out
}

// seconds reads a timeout in seconds; zero keeps the client default.
func seconds(v any) time.Duration {
	var f float64
	switch x := v.(type) {
	case int:
		f = float64(x)
	case float64:
		f = x
	case string:
		f, _ = strconv.ParseFloat(strings.TrimSpace(x), 64)
	}
	if f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}
