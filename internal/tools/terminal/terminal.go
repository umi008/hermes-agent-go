// Package terminal ports the upstream terminal and process_manage tools
// (tools/terminal_tool.py, tools/process_registry.py): bash commands whose
// working directory and exported variables persist between calls.
package terminal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

// Toolset is the upstream toolset name.
const Toolset = "terminal"

const (
	defaultTimeout    = 180 * time.Second
	maxForeground     = 600 * time.Second
	maxOutputChars    = 50_000
	truncateHeadShare = 0.4
	maxCaptureBytes   = 16 << 20
)

// Guard approves a command before it runs. A non-nil error blocks it and is
// shown to the model.
type Guard func(ctx context.Context, command string) error

// Terminal is one session's shell state.
type Terminal struct {
	backend Backend
	guard   Guard
	marker  string

	mu  sync.Mutex
	cwd string

	procs *processes
}

// New builds a terminal starting in cwd. guard may be nil.
func New(b Backend, cwd string, guard Guard) *Terminal {
	return &Terminal{backend: b, guard: guard, cwd: cwd, marker: "__HERMES_CWD_" + nonce() + "__", procs: newProcesses()}
}

// Cwd returns the current working directory.
func (t *Terminal) Cwd() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cwd
}

// Close kills background processes and releases the backend.
func (t *Terminal) Close() error {
	t.procs.killAll()
	return t.backend.Close()
}

// Tools returns terminal and process_manage.
func (t *Terminal) Tools() []tools.Tool {
	return []tools.Tool{t.terminalTool(), t.processTool()}
}

const description = `Run shell commands on the OS/shell/backend named in your environment section. The working directory, files and exported variables persist between calls: activate a virtualenv or export once, not before every command.

Use read_file, search_files, patch and write_file instead of cat/head/tail, grep/find/ls, sed/awk and echo/heredoc. Output is auto-truncated; never pipe through head/tail to shorten it.

Foreground (default) returns as soon as the command finishes. To wait a fixed time, run the wait in the foreground; never background a sleep or polling loop. Background is only for commands that must keep running after this call returns; health-check a server in a separate call and manage jobs with process_manage. When a command changes directory, trust the result's "cwd" instead of prefixing later commands with cd.`

func (t *Terminal) terminalTool() tools.Tool {
	return tools.Tool{
		Name:        "terminal",
		Toolset:     Toolset,
		Description: description,
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"command":{"type":"string","description":"The bash command to run"},` +
			`"background":{"type":"boolean","description":"Run in the background and return a session_id for process_manage (default: false)","default":false},` +
			`"timeout":{"type":"integer","description":"Seconds before a foreground command is killed (default: 180, max: 600; longer runs move to the background)","minimum":1},` +
			`"workdir":{"type":"string","description":"Absolute directory for this command only; does not change the persistent cwd"}` +
			`},"required":["command"]}`),
		Handler: t.run,
	}
}

type runArgs struct {
	Command    string `json:"command"`
	Background bool   `json:"background"`
	Timeout    int    `json:"timeout"`
	Workdir    string `json:"workdir"`
}

type runResult struct {
	Output    string  `json:"output"`
	ExitCode  int     `json:"exit_code"`
	Error     *string `json:"error"`
	Cwd       string  `json:"cwd,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	PID       int     `json:"pid,omitempty"`
	Note      string  `json:"note,omitempty"`
}

func (t *Terminal) run(ctx context.Context, raw json.RawMessage) (string, error) {
	var a runArgs
	if err := tools.Decode(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", tools.Error("terminal requires 'command'")
	}
	if t.guard != nil {
		if err := t.guard(ctx, a.Command); err != nil {
			return "", err
		}
	}
	timeout := defaultTimeout
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout) * time.Second
	}
	dir := t.Cwd()
	if a.Workdir != "" {
		dir = a.Workdir
	}
	if a.Background || timeout > maxForeground {
		res, err := t.startBackground(a.Command, dir)
		if err != nil {
			return "", tools.Error(err.Error())
		}
		if !a.Background {
			res.Note = fmt.Sprintf("timeout %ds exceeds the %ds foreground limit, so the command runs in the background; use process_manage wait", a.Timeout, int(maxForeground.Seconds()))
		}
		return tools.Result(res)
	}
	return t.foreground(ctx, a, dir, timeout)
}

func (t *Terminal) foreground(ctx context.Context, a runArgs, dir string, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := t.backend.Command(runCtx, t.script(a.Command, dir, true))
	cmd.WaitDelay = 2 * time.Second
	var out capped
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()

	output, newCwd := t.splitMarker(out.String())
	res := runResult{Output: truncateOutput(stripANSI(output)), ExitCode: exitCode(err)}
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		msg := fmt.Sprintf("Command timed out after %ds and was killed", int(timeout.Seconds()))
		res.Error, res.ExitCode = &msg, -1
	case err != nil && res.ExitCode == -1:
		msg := err.Error()
		res.Error = &msg
	}
	if newCwd != "" && a.Workdir == "" {
		t.mu.Lock()
		if newCwd != t.cwd {
			t.cwd, res.Cwd = newCwd, newCwd
		}
		t.mu.Unlock()
	}
	return tools.Result(res)
}

// script wraps a command so exported variables reload before it and, for
// foreground runs, are saved after it with the final directory reported.
func (t *Terminal) script(command, dir string, foreground bool) string {
	state := quote(t.backend.StatePath())
	var b strings.Builder
	if foreground {
		fmt.Fprintf(&b, "__hermes_exit() { __hermes_ec=$?; export -p > %s 2>/dev/null; printf '\\n%s%%s\\n' \"$(pwd -W 2>/dev/null || pwd)\"; exit $__hermes_ec; }\n", state, t.marker)
		b.WriteString("trap __hermes_exit EXIT\n")
	}
	fmt.Fprintf(&b, "[ -r %s ] && . %s >/dev/null 2>&1\n", state, state)
	fmt.Fprintf(&b, "cd %s || exit 1\n", quote(t.backend.ShellPath(dir)))
	b.WriteString(command)
	b.WriteString("\n")
	return b.String()
}

// splitMarker removes the trailing cwd report from output.
func (t *Terminal) splitMarker(out string) (string, string) {
	i := strings.LastIndex(out, "\n"+t.marker)
	if i < 0 {
		return strings.TrimRight(out, "\n"), ""
	}
	cwd := strings.TrimSpace(out[i+1+len(t.marker):])
	return strings.TrimRight(out[:i], "\n"), fromShellPath(cwd)
}

// fromShellPath turns Git Bash "C:/x" output into a native path on Windows.
func fromShellPath(p string) string {
	if len(p) >= 2 && p[1] == ':' {
		return strings.ReplaceAll(p, "/", `\`)
	}
	return p
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() >= 0 {
		return ee.ExitCode()
	}
	return -1
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// truncateOutput keeps 40% of the budget from the start and 60% from the
// end, like upstream tools/tool_output_truncate.py.
func truncateOutput(s string) string {
	n := utf8.RuneCountInString(s)
	if n <= maxOutputChars {
		return s
	}
	r := []rune(s)
	head := int(maxOutputChars * truncateHeadShare)
	tail := maxOutputChars - head
	return string(r[:head]) + fmt.Sprintf("\n\n... [OUTPUT TRUNCATED - %d chars omitted out of %d total] ...\n\n", n-head-tail, n) + string(r[n-tail:])
}

// capped buffers output up to maxCaptureBytes; it is safe for concurrent
// writes from stdout and stderr.
type capped struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	dropped int
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := maxCaptureBytes - c.buf.Len()
	if room <= 0 {
		c.dropped += len(p)
		return len(p), nil
	}
	if len(p) > room {
		c.dropped += len(p) - room
		c.buf.Write(p[:room])
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := strings.ToValidUTF8(c.buf.String(), "\uFFFD")
	if c.dropped > 0 {
		s += fmt.Sprintf("\n[... %d further bytes not captured]\n", c.dropped)
	}
	return s
}

func nonce() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
