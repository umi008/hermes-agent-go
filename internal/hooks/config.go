package hooks

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// validEvents is upstream VALID_HOOKS minus the Python-plugin-only events.
// The events in supported fire; the others parse and are reported as
// unsupported.
var validEvents = map[string]bool{
	"pre_tool_call": true, "post_tool_call": true, "transform_terminal_output": true, "transform_tool_result": true,
	"pre_llm_call": true, "post_llm_call": true, "pre_api_request": true, "post_api_request": true,
	"on_session_start": true, "on_session_end": true, "on_session_finalize": true, "on_session_reset": true,
	"subagent_start": true, "subagent_stop": true, "pre_gateway_dispatch": true, "pre_approval_request": true,
	"post_approval_response": true, "pre_verify": true, "pre_compress": true, "post_compress": true,
}

// Supported events fire in the Go port.
var supported = map[string]bool{
	PreToolCall: true, PostToolCall: true, PreLLMCall: true, PostLLMCall: true,
	OnSessionStart: true, OnSessionEnd: true, SubagentStart: true, SubagentStop: true,
}

// toolEvents honor the matcher field.
var toolEvents = map[string]bool{PreToolCall: true, PostToolCall: true}

// Parse is upstream _parse_hooks over the hooks: mapping. Malformed
// entries are logged and skipped. Events come in sorted order because a
// YAML mapping loses its order; entries keep their list order.
func Parse(raw any, logger *slog.Logger) []Spec {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if raw == nil {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		logger.Warn("hooks: must be a mapping of event names to hook lists")
		return nil
	}
	events := make([]string, 0, len(m))
	for e := range m {
		events = append(events, e)
	}
	sort.Strings(events)
	var specs []Spec
	for _, event := range events {
		if event == "output_spill" || event == "outbound" {
			continue
		}
		if !validEvents[event] {
			logger.Warn("unknown hook event in hooks: config", "event", event)
			continue
		}
		entries := m[event]
		if entries == nil {
			continue
		}
		list, ok := entries.([]any)
		if !ok {
			logger.Warn(fmt.Sprintf("hooks.%s must be a list of hook definitions; got %T", event, entries))
			continue
		}
		if !supported[event] {
			logger.Warn("hook event is not supported by the Go port yet, skipping", "event", event)
			continue
		}
		for i, e := range list {
			if s, ok := parseEntry(event, i, e, logger); ok {
				specs = append(specs, s)
			}
		}
	}
	return specs
}

func parseEntry(event string, index int, raw any, logger *slog.Logger) (Spec, bool) {
	warn := func(msg string, args ...any) {
		logger.Warn(fmt.Sprintf("hooks.%s[%d]", event, index) + fmt.Sprintf(msg, args...))
	}
	m, ok := raw.(map[string]any)
	if !ok {
		warn(" must be a mapping with a 'command' key; got %T", raw)
		return Spec{}, false
	}
	command, _ := m["command"].(string)
	if strings.TrimSpace(command) == "" {
		warn(" is missing a non-empty 'command' field")
		return Spec{}, false
	}
	s := Spec{Event: event, Command: strings.TrimSpace(command), Timeout: DefaultTimeout}
	if v, ok := m["matcher"]; ok && v != nil {
		matcher, isStr := v.(string)
		switch {
		case !isStr:
			warn(".matcher must be a string regex; ignoring")
		case !toolEvents[event]:
			warn(".matcher=%q will be ignored at runtime", matcher)
		default:
			s.Matcher = strings.TrimSpace(matcher)
		}
	}
	if s.Matcher != "" {
		if re, err := regexp.Compile(`^(?:` + s.Matcher + `)$`); err == nil {
			s.matcher = re
		} else {
			logger.Warn("shell hook matcher is invalid, treating as literal equality", "matcher", s.Matcher, "err", err)
		}
	}
	if v, ok := m["timeout"]; ok {
		secs, ok := pyInt(v)
		switch {
		case !ok:
			warn(".timeout must be an int (got %v); using default 60s", v)
		case secs < 1:
			warn(".timeout must be >=1; using default 60s")
		case time.Duration(secs)*time.Second > MaxTimeout:
			warn(".timeout=%ds exceeds max 300s; clamping", secs)
			s.Timeout = MaxTimeout
		default:
			s.Timeout = time.Duration(secs) * time.Second
		}
	}
	fc, ok := m["fail_closed"]
	if !ok {
		fc, ok = m["failClosed"]
	}
	if ok && fc != nil {
		b, isBool := fc.(bool)
		switch {
		case !isBool:
			warn(".fail_closed must be a boolean (got %v); using default false (fail open)", fc)
		case b && event != PreToolCall:
			warn(".fail_closed=true will be ignored at runtime")
		default:
			s.FailClosed = b
		}
	}
	return s, true
}

// pyInt is Python int(): ints, truncated floats and integer strings.
func pyInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case uint64:
		return int(x), true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return int(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		return n, err == nil
	}
	return 0, false
}

// Consent is the first-use approval state. The Go port never prompts and
// never writes: a hook runs when an opt-in is on or upstream's allowlist
// file already approves its (event, command) pair.
type Consent struct {
	Accept   bool
	approved map[[2]string]bool
}

// LoadConsent reads <hermes>/shell-hooks-allowlist.json. A missing or
// broken file approves nothing.
func LoadConsent(path string, accept bool) Consent {
	c := Consent{Accept: accept, approved: map[[2]string]bool{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	data = []byte(strings.TrimPrefix(string(data), "\uFEFF"))
	var raw struct {
		Approvals []any `json:"approvals"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return c
	}
	for _, item := range raw.Approvals {
		e, _ := item.(map[string]any)
		ev, ok1 := e["event"].(string)
		cmd, ok2 := e["command"].(string)
		if ok1 && ok2 {
			c.approved[[2]string{ev, cmd}] = true
		}
	}
	return c
}

// Allows reports whether s may run.
func (c Consent) Allows(s Spec) bool {
	return c.Accept || c.approved[[2]string{s.Event, s.Command}]
}
