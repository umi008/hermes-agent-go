// Package hooks ports upstream's shell hooks (agent/shell_hooks.py): user
// commands from the hooks: config run around tool calls and lifecycle
// events. A command reads a JSON payload on stdin and may answer with JSON
// on stdout; exit code 2 blocks a pre_tool_call.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Upstream limits and wire constants.
const (
	DefaultTimeout      = 60 * time.Second
	MaxTimeout          = 300 * time.Second
	BlockExitCode       = 2
	defaultBlockMessage = "Blocked by shell hook."
	stderrLimit         = 400

	PreToolCall  = "pre_tool_call"
	PostToolCall = "post_tool_call"
)

// Spec is one validated hooks: entry.
type Spec struct {
	Event      string
	Command    string
	Matcher    string
	Timeout    time.Duration
	FailClosed bool

	matcher *regexp.Regexp
}

// matches is upstream matches_tool: no matcher fires on every tool; a
// matcher is a full-match regex, or a literal when it does not compile.
func (s Spec) matches(tool string) bool {
	if s.Matcher == "" {
		return true
	}
	if s.matcher == nil {
		return tool == s.Matcher
	}
	return s.matcher.MatchString(tool)
}

// Directive is a hook's parsed answer.
type Directive struct {
	Action  string         // "block", "modify" or "approve"
	Message string         // block/approve message
	Args    map[string]any // modify arguments
	RuleKey string         // approve allowlist grain
	Context string         // context for non pre_tool_call events
}

// result is upstream's _spawn diagnostic dict.
type result struct {
	exitCode int
	stdout   string
	stderr   string
	timedOut bool
	elapsed  time.Duration
	err      string
}

// Runner spawns hook commands.
type Runner struct {
	// GOOS picks the command-line splitting and the Windows interpreter
	// mapping for bare .sh/.py scripts.
	GOOS string
	// Home expands a leading "~" in commands.
	Home string
	// Env is the child environment; nil inherits the process environment.
	Env    []string
	Logger *slog.Logger
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Logger
}

// windowsInterpreters is upstream _WINDOWS_SCRIPT_INTERPRETERS.
var windowsInterpreters = map[string]string{".sh": "bash", ".bash": "bash", ".py": "python"}

func (r *Runner) spawn(ctx context.Context, s Spec, stdin []byte) result {
	argv, err := SplitCommand(expandHome(s.Command, r.Home), r.GOOS == "windows")
	if err != nil {
		return result{err: fmt.Sprintf("command %q cannot be parsed: %v", s.Command, err)}
	}
	if len(argv) == 0 {
		return result{err: "empty command"}
	}
	if r.GOOS == "windows" {
		if interp, ok := windowsInterpreters[strings.ToLower(filepath.Ext(argv[0]))]; ok && isFile(argv[0]) {
			argv = append([]string{interp}, argv...)
		}
	}
	cctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Env = r.Env
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	start := time.Now()
	runErr := cmd.Run()
	res := result{stdout: decode(stdout.Bytes()), stderr: decode(stderr.Bytes()), elapsed: time.Since(start)}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		res.timedOut = true
		return res
	}
	var exit *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exit):
		res.exitCode = exit.ExitCode()
	case errors.Is(runErr, exec.ErrNotFound):
		res.err = "command not found"
	default:
		res.err = runErr.Error()
	}
	return res
}

// decode reads output as UTF-8 with replacement, like errors='replace'.
func decode(b []byte) string { return strings.ToValidUTF8(string(b), "\uFFFD") }

func expandHome(cmd, home string) string {
	if home == "" || !strings.HasPrefix(cmd, "~") {
		return cmd
	}
	if len(cmd) == 1 || cmd[1] == '/' || cmd[1] == '\\' {
		return home + cmd[1:]
	}
	return cmd
}

// evaluate is upstream _evaluate_result: the failure semantics of one run.
func (r *Runner) evaluate(s Spec, res result) *Directive {
	blocking := s.Event == PreToolCall
	failClosed := s.FailClosed && blocking
	log := r.logger()
	if res.err != "" || res.timedOut {
		reason := res.err
		if res.timedOut {
			log.Warn("shell hook timed out", "event", s.Event, "command", s.Command, "elapsed", res.elapsed)
			reason = fmt.Sprintf("timed out after %ds", int(s.Timeout/time.Second))
		} else {
			log.Warn("shell hook failed", "event", s.Event, "command", s.Command, "err", res.err)
		}
		if failClosed {
			return failClosedBlock(s, reason)
		}
		return nil
	}
	stderr := strings.TrimSpace(res.stderr)
	if res.exitCode == BlockExitCode && blocking {
		if d := r.parse(s.Event, res.stdout); d != nil && d.Action == "block" {
			return d
		}
		msg := truncate(stderr, stderrLimit)
		if msg == "" {
			msg = defaultBlockMessage
		}
		return &Directive{Action: "block", Message: msg}
	}
	if res.exitCode != 0 {
		log.Warn("shell hook exited non-zero", "event", s.Event, "command", s.Command, "code", res.exitCode,
			"stderr", truncate(stderr, stderrLimit))
	}
	stdout := strings.TrimSpace(res.stdout)
	d := r.parse(s.Event, stdout)
	if d == nil && failClosed && res.exitCode != 0 {
		return failClosedBlock(s, fmt.Sprintf("hook exited %d with no directive", res.exitCode))
	}
	if d == nil && failClosed && stdout != "" && !isJSONObject(stdout) {
		return failClosedBlock(s, "unparseable stdout (expected a JSON object)")
	}
	return d
}

func failClosedBlock(s Spec, reason string) *Directive {
	return &Directive{Action: "block", Message: "hook " + s.Command + " failed closed: " + reason}
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func isJSONObject(s string) bool {
	var m map[string]any
	return json.Unmarshal([]byte(s), &m) == nil && m != nil
}

// parse is upstream _parse_response.
func (r *Runner) parse(event, stdout string) *Directive {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		r.logger().Warn("shell hook stdout was not valid JSON", "event", event, "stdout", truncate(stdout, 200))
		return nil
	}
	data, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	if event == PreToolCall {
		return parsePreToolCall(data)
	}
	if c, ok := data["context"].(string); ok && strings.TrimSpace(c) != "" {
		return &Directive{Context: c}
	}
	return nil
}

// preToolDialects are Hermes "action" then Claude Code "decision", as
// (verb key, message key, fallback message key, modify payload key).
var preToolDialects = [][4]string{{"action", "message", "reason", "args"}, {"decision", "reason", "message", "tool_input"}}

func parsePreToolCall(data map[string]any) *Directive {
	for _, d := range preToolDialects {
		if data[d[0]] == "block" {
			return &Directive{Action: "block", Message: blockMessage(data[d[1]], data[d[2]])}
		}
	}
	for _, d := range preToolDialects {
		if args, ok := data[d[3]].(map[string]any); ok && data[d[0]] == "modify" {
			return &Directive{Action: "modify", Args: args}
		}
	}
	if data["action"] == "approve" {
		out := &Directive{Action: "approve"}
		if m, ok := data["message"].(string); ok {
			out.Message = strings.TrimSpace(m)
		}
		if k, ok := data["rule_key"].(string); ok {
			out.RuleKey = strings.TrimSpace(k)
		}
		return out
	}
	return nil
}

// blockMessage is upstream _block_message: Python truthiness picks the
// primary, then the secondary; only a non-empty string is kept.
func blockMessage(primary, secondary any) string {
	raw := primary
	if !truthy(primary) {
		raw = secondary
	}
	if s, ok := raw.(string); ok && s != "" {
		return s
	}
	return defaultBlockMessage
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
