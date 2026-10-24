package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/tools"
)

func newTerm(t *testing.T, guard Guard) (*Terminal, *tools.Registry, string) {
	t.Helper()
	b, err := NewLocal()
	if errors.Is(err, ErrNoBash) {
		t.Skip("bash not available")
	}
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	term := New(b, dir, guard)
	t.Cleanup(func() { _ = term.Close() })
	r, err := tools.NewRegistry(term.Tools()...)
	if err != nil {
		t.Fatal(err)
	}
	return term, r, dir
}

func invoke(t *testing.T, r *tools.Registry, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, err := r.Execute(context.Background(), llm.ToolCall{Name: name, Arguments: raw})
	if err != nil {
		return nil, err.Error()
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("bad JSON %s", out)
	}
	return obj, ""
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func TestForeground(t *testing.T) {
	term, r, dir := newTerm(t, nil)
	obj, _ := invoke(t, r, "terminal", map[string]any{"command": "echo hello; echo oops >&2; exit 3"})
	if obj["output"] != "hello\noops" || obj["exit_code"] != float64(3) || obj["error"] != nil {
		t.Fatalf("got %v", obj)
	}
	if _, ok := obj["cwd"]; ok {
		t.Error("cwd reported without a change")
	}

	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	obj, _ = invoke(t, r, "terminal", map[string]any{"command": "cd sub && export HERMES_T=kept"})
	if cwd, _ := obj["cwd"].(string); !samePath(cwd, filepath.Join(dir, "sub")) || !samePath(term.Cwd(), filepath.Join(dir, "sub")) {
		t.Fatalf("cwd = %v (term %s)", obj["cwd"], term.Cwd())
	}
	obj, _ = invoke(t, r, "terminal", map[string]any{"command": "echo $HERMES_T; basename \"$PWD\""})
	if obj["output"] != "kept\nsub" {
		t.Fatalf("state not persisted: %v", obj)
	}
	obj, _ = invoke(t, r, "terminal", map[string]any{"command": "cd /; true", "workdir": dir})
	if _, ok := obj["cwd"]; ok || !samePath(term.Cwd(), filepath.Join(dir, "sub")) {
		t.Fatalf("workdir changed persistent cwd: %v %s", obj, term.Cwd())
	}
}

func TestTimeoutAndGuard(t *testing.T) {
	_, r, _ := newTerm(t, func(_ context.Context, cmd string) error {
		if strings.Contains(cmd, "rm -rf") {
			return tools.ErrorWith("blocked", map[string]any{"status": "blocked"})
		}
		return nil
	})
	start := time.Now()
	obj, _ := invoke(t, r, "terminal", map[string]any{"command": "sleep 20", "timeout": 1})
	if obj["exit_code"] != float64(-1) || !strings.Contains(obj["error"].(string), "timed out after 1s") {
		t.Fatalf("got %v", obj)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took %v", time.Since(start))
	}
	if _, e := invoke(t, r, "terminal", map[string]any{"command": "rm -rf /x"}); !strings.Contains(e, `"status":"blocked"`) {
		t.Fatalf("guard: %q", e)
	}
}

func TestBackgroundProcess(t *testing.T) {
	_, r, _ := newTerm(t, nil)
	obj, _ := invoke(t, r, "terminal", map[string]any{"command": "read line; echo got $line", "background": true})
	id, _ := obj["session_id"].(string)
	if !strings.HasPrefix(id, "proc_") || obj["output"] != "Background process started" {
		t.Fatalf("start: %v", obj)
	}
	if obj, _ = invoke(t, r, "process_manage", map[string]any{"action": "poll", "session_id": id[:8]}); obj["status"] != "running" {
		t.Fatalf("poll: %v", obj)
	}
	if _, e := invoke(t, r, "process_manage", map[string]any{"action": "submit", "session_id": id, "data": "hi"}); e != "" {
		t.Fatal(e)
	}
	obj, _ = invoke(t, r, "process_manage", map[string]any{"action": "wait", "session_id": id, "timeout": 20})
	if obj["status"] != "exited" || obj["exit_code"] != float64(0) || !strings.Contains(obj["output_preview"].(string), "got hi") {
		t.Fatalf("wait: %v", obj)
	}

	obj, _ = invoke(t, r, "terminal", map[string]any{"command": "echo started; sleep 60", "background": true})
	id = obj["session_id"].(string)
	deadline := time.Now().Add(10 * time.Second)
	for {
		obj, _ = invoke(t, r, "process_manage", map[string]any{"action": "log", "session_id": id})
		if obj["output"] == "started" || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if obj["output"] != "started" {
		t.Fatalf("log: %v", obj)
	}
	if obj, _ = invoke(t, r, "process_manage", map[string]any{"action": "kill", "session_id": id}); obj["status"] != "killed" {
		t.Fatalf("kill: %v", obj)
	}
	obj, _ = invoke(t, r, "process_manage", map[string]any{"action": "list"})
	if n := len(obj["processes"].([]any)); n != 2 {
		t.Fatalf("list: %v", obj)
	}
	if _, e := invoke(t, r, "process_manage", map[string]any{"action": "poll", "session_id": "zzz"}); !strings.Contains(e, "No process") {
		t.Fatalf("missing: %q", e)
	}
}

func TestTruncateOutput(t *testing.T) {
	s := strings.Repeat("a", 30_000) + strings.Repeat("b", 40_000)
	got := truncateOutput(s)
	if !strings.HasPrefix(got, strings.Repeat("a", 20_000)+"\n\n... [OUTPUT TRUNCATED - 20000 chars omitted out of 70000 total] ...\n\n") ||
		!strings.HasSuffix(got, strings.Repeat("b", 30_000)) {
		t.Fatal("bad truncation")
	}
	if truncateOutput("short") != "short" {
		t.Fatal("short changed")
	}
	if stripANSI("\x1b[31mred\x1b[0m") != "red" {
		t.Fatal("ansi kept")
	}
}

func TestFindBash(t *testing.T) {
	look := func(paths map[string]string) func(string) (string, error) {
		return func(name string) (string, error) {
			if p, ok := paths[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		}
	}
	has := func(set ...string) func(string) bool {
		return func(p string) bool {
			for _, s := range set {
				if p == s {
					return true
				}
			}
			return false
		}
	}
	gitBash := filepath.Join(`C:\Git`, "bin", "bash.exe")
	tests := []struct {
		name, goos string
		paths      map[string]string
		exists     func(string) bool
		want       string
	}{
		{"unix path", "linux", map[string]string{"bash": "/bin/bash"}, has(), "/bin/bash"},
		{"git bash next to git", "windows", map[string]string{"git": filepath.Join(`C:\Git`, "cmd", "git.exe")}, has(gitBash), gitBash},
		{"skip wsl launcher", "windows", map[string]string{"bash": `C:\Windows\System32\bash.exe`}, has(`C:\Windows\System32\bash.exe`), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findBash(tt.goos, look(tt.paths), tt.exists)
			if tt.want == "" {
				if !errors.Is(err, ErrNoBash) {
					t.Fatalf("got %q %v", got, err)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("got %q %v", got, err)
			}
		})
	}
}
