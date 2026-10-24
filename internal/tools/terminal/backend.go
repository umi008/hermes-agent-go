package terminal

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Backend runs bash scripts somewhere: the host, or a container.
type Backend interface {
	// Name labels the backend in results and the prompt, e.g. "local".
	Name() string
	// Command prepares bash to run script. The caller wires stdio.
	Command(ctx context.Context, script string) *exec.Cmd
	// StatePath is where the shell keeps exported variables, as the shell
	// sees it.
	StatePath() string
	// ShellPath converts a directory as Go sees it into the shell's form.
	ShellPath(dir string) string
	// Close releases backend resources.
	Close() error
}

// Local runs bash on the host. On Windows that is Git Bash, as upstream.
type Local struct {
	Bash  string
	state string
}

var _ Backend = (*Local)(nil)

// ErrNoBash reports that no usable bash was found.
var ErrNoBash = errors.New("terminal: bash not found (on Windows install Git for Windows)")

// NewLocal finds bash and creates the session state directory.
func NewLocal() (*Local, error) {
	bash, err := findBash(runtime.GOOS, exec.LookPath, fileExists)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "hermes-go-term-")
	if err != nil {
		return nil, err
	}
	return &Local{Bash: bash, state: filepath.Join(dir, "env.sh")}, nil
}

// Name implements Backend.
func (l *Local) Name() string { return "local" }

// Command implements Backend.
func (l *Local) Command(ctx context.Context, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, l.Bash, "-c", script)
	setProcessGroup(cmd)
	return cmd
}

// StatePath implements Backend.
func (l *Local) StatePath() string { return l.ShellPath(l.state) }

// ShellPath implements Backend. Git Bash accepts C:/x paths.
func (l *Local) ShellPath(dir string) string { return filepath.ToSlash(dir) }

// Close implements Backend.
func (l *Local) Close() error { return os.RemoveAll(filepath.Dir(l.state)) }

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// findBash prefers Git Bash next to git on Windows, never the WSL launcher
// in System32, and plain bash on PATH elsewhere.
func findBash(goos string, lookPath func(string) (string, error), exists func(string) bool) (string, error) {
	if goos != "windows" {
		if p, err := lookPath("bash"); err == nil {
			return p, nil
		}
		return "", ErrNoBash
	}
	var candidates []string
	if git, err := lookPath("git"); err == nil {
		root := filepath.Dir(filepath.Dir(git)) // <Git>\cmd\git.exe or <Git>\bin\git.exe
		candidates = append(candidates, filepath.Join(root, "bin", "bash.exe"), filepath.Join(root, "usr", "bin", "bash.exe"))
	}
	for _, env := range []string{"ProgramFiles", "ProgramW6432", "LOCALAPPDATA"} {
		if base := os.Getenv(env); base != "" {
			sub := "Git"
			if env == "LOCALAPPDATA" {
				sub = filepath.Join("Programs", "Git")
			}
			candidates = append(candidates, filepath.Join(base, sub, "bin", "bash.exe"))
		}
	}
	if p, err := lookPath("bash"); err == nil && !strings.Contains(strings.ToLower(p), `\system32\`) {
		candidates = append(candidates, p)
	}
	for _, c := range candidates {
		if exists(c) {
			return c, nil
		}
	}
	return "", ErrNoBash
}
