package terminal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

const (
	logKeepBytes      = 1 << 20
	defaultLogLines   = 200
	previewChars      = 1000
	defaultWait       = 30 * time.Second
	maxWait           = 600 * time.Second
	processSessionPfx = "proc_"
)

type process struct {
	id      string
	command string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	started time.Time

	mu       sync.Mutex
	log      []byte
	done     chan struct{}
	exitCode int
	killed   bool
}

func (p *process) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, b...)
	if over := len(p.log) - logKeepBytes; over > 0 {
		p.log = append([]byte(nil), p.log[over:]...)
	}
	return len(b), nil
}

func (p *process) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return stripANSI(strings.ToValidUTF8(string(p.log), "�"))
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

type processes struct {
	mu    sync.Mutex
	byID  map[string]*process
	order []string
}

func newProcesses() *processes { return &processes{byID: map[string]*process{}} }

func (ps *processes) add(p *process) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.byID[p.id] = p
	ps.order = append(ps.order, p.id)
}

// find resolves an id or any unique prefix of one.
func (ps *processes) find(id string) (*process, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if p, ok := ps.byID[id]; ok {
		return p, nil
	}
	var hits []*process
	for k, p := range ps.byID {
		if id != "" && (strings.HasPrefix(k, id) || strings.HasPrefix(k, processSessionPfx+id)) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, tools.Error(fmt.Sprintf("No process with session_id %q; use action=list", id))
	}
	return nil, tools.Error(fmt.Sprintf("session_id %q is ambiguous (%d matches)", id, len(hits)))
}

func (ps *processes) all() []*process {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make([]*process, 0, len(ps.order))
	for _, id := range ps.order {
		out = append(out, ps.byID[id])
	}
	return out
}

func (ps *processes) killAll() {
	for _, p := range ps.all() {
		if !p.exited() {
			p.mu.Lock()
			p.killed = true
			p.mu.Unlock()
			_ = killTree(p.cmd)
			<-p.done
		}
	}
}

func (t *Terminal) startBackground(command, dir string) (runResult, error) {
	// Background processes outlive the tool call, so they are not tied to
	// the call's context; Close and process_manage kill them.
	cmd := t.backend.Command(context.Background(), t.script(command, dir, false))
	p := &process{id: processSessionPfx + nonce(), command: command, cmd: cmd, started: time.Now(), done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p, p
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return runResult{}, err
	}
	p.stdin = stdin
	if err := cmd.Start(); err != nil {
		return runResult{}, fmt.Errorf("start background process: %w", err)
	}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.exitCode = exitCode(err)
		p.mu.Unlock()
		close(p.done)
	}()
	t.procs.add(p)
	return runResult{Output: "Background process started", SessionID: p.id, PID: cmd.Process.Pid}, nil
}

func (t *Terminal) processTool() tools.Tool {
	return tools.Tool{
		Name:    "process_manage",
		Toolset: Toolset,
		Description: "Manage background processes started with terminal(background=true). " +
			"Actions: list (all processes), poll (status and recent output), log (output lines; default last 200), " +
			"wait (block until exit or timeout seconds), kill, write (send raw data to stdin), submit (send data plus Enter). " +
			"session_id accepts any unique prefix.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["list","poll","log","wait","kill","write","submit"]},` +
			`"session_id":{"type":"string","description":"Process session_id (or a unique prefix); required except for list"},` +
			`"data":{"type":"string","description":"Text for write/submit"},` +
			`"timeout":{"type":"integer","description":"Seconds to wait (wait action; default 30, max 600)"},` +
			`"offset":{"type":"integer","description":"First log line, 1-indexed (log action; default: last 200 lines)"},` +
			`"limit":{"type":"integer","description":"Number of log lines (log action; default 200)"}` +
			`},"required":["action"]}`),
		Handler: t.manage,
	}
}

type manageArgs struct {
	Action    string `json:"action"`
	SessionID string `json:"session_id"`
	Data      string `json:"data"`
	Timeout   int    `json:"timeout"`
	Offset    int    `json:"offset"`
	Limit     int    `json:"limit"`
}

type procStatus struct {
	SessionID string  `json:"session_id"`
	Command   string  `json:"command"`
	Status    string  `json:"status"`
	PID       int     `json:"pid"`
	Uptime    float64 `json:"uptime_seconds"`
	ExitCode  *int    `json:"exit_code,omitempty"`
	Preview   *string `json:"output_preview,omitempty"`
	TimedOut  bool    `json:"timed_out,omitempty"`
}

func status(p *process, withPreview bool) procStatus {
	s := procStatus{SessionID: p.id, Command: p.command, Status: "running", PID: p.cmd.Process.Pid, Uptime: time.Since(p.started).Round(time.Second).Seconds()}
	if p.exited() {
		p.mu.Lock()
		code, killed := p.exitCode, p.killed
		p.mu.Unlock()
		s.Status, s.ExitCode = "exited", &code
		if killed {
			s.Status = "killed"
		}
	}
	if withPreview {
		out := p.output()
		if n := utf8.RuneCountInString(out); n > previewChars {
			out = string([]rune(out)[n-previewChars:])
		}
		s.Preview = &out
	}
	return s
}

func (t *Terminal) manage(ctx context.Context, raw json.RawMessage) (string, error) {
	var a manageArgs
	if err := tools.Decode(raw, &a); err != nil {
		return "", err
	}
	if a.Action == "list" {
		list := []procStatus{}
		for _, p := range t.procs.all() {
			list = append(list, status(p, false))
		}
		return tools.Result(map[string]any{"processes": list})
	}
	if a.Action == "poll" {
		p, err := t.procs.find(a.SessionID)
		if err != nil {
			return "", err
		}
		return tools.Result(status(p, true))
	}
	handler, ok := map[string]func(context.Context, *process, manageArgs) (string, error){
		"log":    logAction,
		"wait":   waitAction,
		"kill":   killAction,
		"write":  writeAction,
		"submit": writeAction,
	}[a.Action]
	if !ok {
		return "", tools.Error(fmt.Sprintf("unknown action %q", a.Action))
	}
	p, err := t.procs.find(a.SessionID)
	if err != nil {
		return "", err
	}
	return handler(ctx, p, a)
}

func logAction(_ context.Context, p *process, a manageArgs) (string, error) {
	lines := strings.Split(strings.TrimRight(p.output(), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	limit := a.Limit
	if limit <= 0 {
		limit = defaultLogLines
	}
	start := len(lines) - limit
	if a.Offset > 0 {
		start = a.Offset - 1
	}
	start = min(max(start, 0), len(lines))
	end := min(start+limit, len(lines))
	return tools.Result(map[string]any{
		"session_id": p.id, "output": strings.Join(lines[start:end], "\n"),
		"total_lines": len(lines), "showing": fmt.Sprintf("%d-%d", start+1, end),
	})
}

func waitAction(ctx context.Context, p *process, a manageArgs) (string, error) {
	d := defaultWait
	if a.Timeout > 0 {
		d = min(time.Duration(a.Timeout)*time.Second, maxWait)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	timedOut := false
	select {
	case <-p.done:
	case <-timer.C:
		timedOut = true
	case <-ctx.Done():
		return "", ctx.Err()
	}
	s := status(p, true)
	s.TimedOut = timedOut
	return tools.Result(s)
}

func killAction(_ context.Context, p *process, _ manageArgs) (string, error) {
	if !p.exited() {
		p.mu.Lock()
		p.killed = true
		p.mu.Unlock()
		if err := killTree(p.cmd); err != nil {
			return "", tools.Error("kill failed: " + err.Error())
		}
		<-p.done
	}
	return tools.Result(status(p, false))
}

func writeAction(_ context.Context, p *process, a manageArgs) (string, error) {
	if p.exited() {
		return "", tools.Error("process " + p.id + " has exited")
	}
	data := a.Data
	if a.Action == "submit" {
		data += "\n"
	}
	if _, err := io.WriteString(p.stdin, data); err != nil {
		return "", tools.Error("write to stdin failed: " + err.Error())
	}
	return tools.Result(map[string]any{"session_id": p.id, "bytes_written": len(data)})
}
