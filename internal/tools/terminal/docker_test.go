package terminal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

func TestDockerRunArgs(t *testing.T) {
	args := dockerRunArgs("c1", DockerOptions{MountDir: "/src", Memory: "1g", Env: []string{"A=1"}})
	joined := strings.Join(args, " ")
	for _, want := range []string{"run -d --init --name c1", "--cap-drop ALL", "--network=none", "--memory 1g", "-e A=1", "-v /src:/workspace", "-w /workspace " + DefaultDockerImage + " sleep infinity"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if args = dockerRunArgs("c2", DockerOptions{Network: true, Image: "alpine"}); slices.Contains(args, "--network=none") || !strings.HasSuffix(strings.Join(args, " "), "-w /root alpine sleep infinity") {
		t.Errorf("got %v", args)
	}
}

func TestContainerPaths(t *testing.T) {
	mount := filepath.Join(t.TempDir(), "proj")
	d := &Docker{mount: mount}
	tests := []struct{ in, want string }{
		{mount, "/workspace"},
		{filepath.Join(mount, "a", "b"), "/workspace/a/b"},
		{"/workspace/x", "/workspace/x"},
		{"/etc", "/etc"},
	}
	for _, tt := range tests {
		if got := d.ShellPath(tt.in); got != tt.want {
			t.Errorf("ShellPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := d.HostPath("/workspace/a"); got != filepath.Join(mount, "a") {
		t.Errorf("HostPath = %q", got)
	}
	if got := d.HostPath("/etc"); got != "/etc" {
		t.Errorf("HostPath outside = %q", got)
	}
}

// TestDockerIntegration needs a working docker daemon; it is skipped
// otherwise (and on Windows runners, which lack Linux containers).
func TestDockerIntegration(t *testing.T) {
	if os.Getenv("HERMES_GO_DOCKER_TESTS") == "" {
		t.Skip("set HERMES_GO_DOCKER_TESTS=1 to run")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewDocker(context.Background(), DockerOptions{Image: "bash:5", MountDir: dir})
	if errors.Is(err, ErrNoDocker) {
		t.Skip("docker not installed")
	}
	if err != nil {
		t.Fatal(err)
	}
	term := New(b, dir, nil)
	t.Cleanup(func() {
		// Files written by the container's root user would block TempDir cleanup.
		_ = b.Command(context.Background(), "rm -rf /workspace/* /workspace/.[!.]*").Run()
		_ = term.Close()
	})
	r, err := tools.NewRegistry(term.Tools()...)
	if err != nil {
		t.Fatal(err)
	}
	obj, _ := invoke(t, r, "terminal", map[string]any{"command": "mkdir -p sub && cd sub && export K=v && echo hi > f.txt && echo ok"})
	if obj["output"] != "ok" || !samePath(term.Cwd(), filepath.Join(dir, "sub")) {
		t.Fatalf("got %v cwd %s", obj, term.Cwd())
	}
	if b, err := os.ReadFile(filepath.Join(dir, "sub", "f.txt")); err != nil || string(b) != "hi\n" {
		t.Fatalf("mount: %q %v", b, err)
	}
	if obj, _ = invoke(t, r, "terminal", map[string]any{"command": "echo $K; pwd"}); obj["output"] != "v\n/workspace/sub" {
		t.Fatalf("state: %v", obj)
	}
}
