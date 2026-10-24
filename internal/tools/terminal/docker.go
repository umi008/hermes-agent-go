package terminal

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultDockerImage mirrors upstream's default sandbox base image.
const DefaultDockerImage = "nikolaik/python-nodejs:python3.11-nodejs20"

// containerWorkspace is where the host cwd is mounted.
const containerWorkspace = "/workspace"

// Upstream tools/environments/docker.py: drop every capability, add back the
// minimum package managers need, and size-limit the tmpfs mounts.
var dockerSecurityArgs = []string{
	"--cap-drop", "ALL",
	"--cap-add", "DAC_OVERRIDE",
	"--cap-add", "CHOWN",
	"--cap-add", "FOWNER",
	"--security-opt", "no-new-privileges",
	"--tmpfs", "/tmp:rw,nosuid,size=512m",
	"--tmpfs", "/var/tmp:rw,noexec,nosuid,size=256m",
	"--pids-limit", "2048",
}

// DockerOptions configure a Docker backend.
type DockerOptions struct {
	Image string // default DefaultDockerImage
	// MountDir is bind-mounted at /workspace (terminal.docker_mount_cwd_to_workspace).
	MountDir string
	Network  bool // false adds --network=none
	Memory   string
	CPUs     string
	Env      []string // KEY=VALUE pairs set in the container
	// Docker is the CLI path; default "docker" on PATH.
	Docker string
}

// Docker runs commands in one long-lived container via docker exec.
type Docker struct {
	docker string
	id     string
	mount  string
}

var _ Backend = (*Docker)(nil)

// ErrNoDocker reports that the docker CLI is missing.
var ErrNoDocker = errors.New("terminal: docker CLI not found")

// dockerRunArgs builds the `docker run` argument list.
func dockerRunArgs(name string, o DockerOptions) []string {
	args := []string{"run", "-d", "--init", "--name", name, "--label", "hermes-go=1"}
	args = append(args, dockerSecurityArgs...)
	if !o.Network {
		args = append(args, "--network=none")
	}
	if o.Memory != "" {
		args = append(args, "--memory", o.Memory)
	}
	if o.CPUs != "" {
		args = append(args, "--cpus", o.CPUs)
	}
	for _, e := range o.Env {
		args = append(args, "-e", e)
	}
	workdir := "/root"
	if o.MountDir != "" {
		args = append(args, "-v", o.MountDir+":"+containerWorkspace)
		workdir = containerWorkspace
	}
	image := o.Image
	if image == "" {
		image = DefaultDockerImage
	}
	return append(args, "-w", workdir, image, "sleep", "infinity")
}

// NewDocker starts the container. Close removes it.
func NewDocker(ctx context.Context, o DockerOptions) (*Docker, error) {
	docker := o.Docker
	if docker == "" {
		p, err := exec.LookPath("docker")
		if err != nil {
			return nil, ErrNoDocker
		}
		docker = p
	}
	name := "hermes-go-" + nonce()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute) // first run may pull the image
	defer cancel()
	out, err := exec.CommandContext(ctx, docker, dockerRunArgs(name, o)...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	id := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(id, '\n'); i >= 0 { // pull progress precedes the id
		id = id[i+1:]
	}
	return &Docker{docker: docker, id: id, mount: o.MountDir}, nil
}

// Name implements Backend.
func (d *Docker) Name() string { return "docker" }

// Command implements Backend. Killing the docker client does not always
// stop the process inside the container; the container is removed on Close.
func (d *Docker) Command(ctx context.Context, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, d.docker, "exec", "-i", d.id, "bash", "-c", script)
	setProcessGroup(cmd)
	return cmd
}

// StatePath implements Backend.
func (d *Docker) StatePath() string { return "/tmp/.hermes-go-env.sh" }

// ShellPath implements Backend: host paths under the mount map into
// /workspace; anything else is taken as a container path.
func (d *Docker) ShellPath(dir string) string { return mapIntoContainer(d.mount, dir) }

func mapIntoContainer(mount, dir string) string {
	if mount != "" {
		if rel, err := filepath.Rel(mount, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if rel == "." {
				return containerWorkspace
			}
			return containerWorkspace + "/" + filepath.ToSlash(rel)
		}
	}
	if filepath.VolumeName(dir) != "" { // a Windows host path outside the mount
		return "/root"
	}
	return filepath.ToSlash(dir)
}

// HostPath maps a container path reported by the shell back to the host
// when it lies inside the mount.
func (d *Docker) HostPath(p string) string {
	if d.mount != "" && (p == containerWorkspace || strings.HasPrefix(p, containerWorkspace+"/")) {
		return filepath.Join(d.mount, filepath.FromSlash(strings.TrimPrefix(p, containerWorkspace)))
	}
	return p
}

// Close implements Backend.
func (d *Docker) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, d.docker, "rm", "-f", d.id).CombinedOutput(); err != nil {
		return fmt.Errorf("docker rm: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
