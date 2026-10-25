package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorEnv finds every binary named in found and nothing else.
func doctorEnv(f *fakeEnv, found ...string) Env {
	env := f.env()
	env.LookPath = func(name string) (string, error) {
		for _, n := range found {
			if n == name {
				return "/usr/bin/" + name, nil
			}
		}
		return "", errors.New("not found")
	}
	return env
}

func runDoctor(t *testing.T, f *fakeEnv, args []string, found ...string) (int, string) {
	t.Helper()
	code := Run(context.Background(), append([]string{"doctor"}, args...), doctorEnv(f, found...))
	return code, f.stdout.String()
}

// line returns the report line of one check, or fails the test.
func line(t *testing.T, out, check string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		fields := strings.Fields(l)
		if len(fields) >= 2 && fields[1] == check {
			return l
		}
	}
	t.Fatalf("no %q check in:\n%s", check, out)
	return ""
}

func wantStatus(t *testing.T, out, check, status, contains string) {
	t.Helper()
	l := line(t, out, check)
	if got := strings.Fields(l)[0]; got != status {
		t.Errorf("%s status = %q, want %q: %s", check, got, status, l)
	}
	if !strings.Contains(l, contains) {
		t.Errorf("%s line %q does not contain %q", check, l, contains)
	}
}

func TestDoctorHealthySetupPasses(t *testing.T) {
	f := newHome(t, "_config_version: 12\nmodel:\n  provider: openrouter\n  default: test/model\n")
	f.vars["OPENROUTER_API_KEY"] = "sk-or-secret-value"
	f.vars["TAVILY_API_KEY"] = "tvly-secret"

	code, out := runDoctor(t, f, nil, "git")

	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s%s", code, out, f.stderr.String())
	}
	wantStatus(t, out, "version", "ok", "dev")
	wantStatus(t, out, "home", "ok", f.vars["HERMES_HOME"])
	wantStatus(t, out, "config", "ok", "_config_version 12")
	wantStatus(t, out, "provider", "ok", "openrouter, model test/model")
	wantStatus(t, out, "provider", "ok", "API key set")
	wantStatus(t, out, "web", "ok", "TAVILY_API_KEY set")
	wantStatus(t, out, "git", "ok", "/usr/bin/git")
	if strings.Contains(out, "sk-or-secret-value") || strings.Contains(out, "tvly-secret") {
		t.Errorf("doctor printed a secret:\n%s", out)
	}
	if !strings.Contains(out, "All required checks passed.") {
		t.Errorf("no summary line:\n%s", out)
	}
	// docker is reported only when terminal.backend is docker.
	for _, l := range strings.Split(out, "\n") {
		if fields := strings.Fields(l); len(fields) >= 2 && fields[1] == "docker" {
			t.Errorf("docker checked without terminal.backend: docker: %s", l)
		}
	}
}

func TestDoctorFailsWithoutProvider(t *testing.T) {
	f := newHome(t, "_config_version: 12\n")

	code, out := runDoctor(t, f, nil, "git")

	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	wantStatus(t, out, "provider", "fail", "no LLM provider configured")
	wantStatus(t, out, "web", "warn", "TAVILY_API_KEY not set")
	if !strings.Contains(out, "1 required check failed.") {
		t.Errorf("no failure summary:\n%s", out)
	}
}

func TestDoctorMissingConfigWarns(t *testing.T) {
	f := newHome(t, "")
	if err := os.Remove(filepath.Join(f.vars["HERMES_HOME"], "config.yaml")); err != nil {
		t.Fatal(err)
	}
	f.vars["OPENROUTER_API_KEY"] = "sk-or-x"

	code, out := runDoctor(t, f, nil, "git")

	wantStatus(t, out, "config", "warn", "not found")
	// Without config.yaml nothing names a model, so the provider check fails
	// and says where to set one.
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	wantStatus(t, out, "provider", "fail", "model.default")
}

func TestDoctorUnparsableConfigFails(t *testing.T) {
	f := newHome(t, "model: [unclosed\n")
	f.vars["OPENROUTER_API_KEY"] = "sk-or-x"

	code, out := runDoctor(t, f, nil, "git")

	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	wantStatus(t, out, "config", "fail", "parse")
}

func TestDoctorOldConfigVersionWarns(t *testing.T) {
	f := newHome(t, "_config_version: 5\n")
	f.vars["OPENROUTER_API_KEY"] = "sk-or-x"

	_, out := runDoctor(t, f, nil, "git")

	wantStatus(t, out, "config", "warn", "_config_version 5")
}

func TestDoctorMissingHomeWarns(t *testing.T) {
	f := newHome(t, "")
	f.vars["HERMES_HOME"] = filepath.Join(f.vars["HOME"], "nowhere")
	f.vars["OPENROUTER_API_KEY"] = "sk-or-x"

	_, out := runDoctor(t, f, nil, "git")

	wantStatus(t, out, "home", "warn", "does not exist")
	if _, err := os.Stat(f.vars["HERMES_HOME"]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("doctor created the home: %v", err)
	}
}

func TestDoctorDockerBackendNeedsDocker(t *testing.T) {
	f := newHome(t, "model:\n  provider: openrouter\n  default: test/model\nterminal:\n  backend: docker\n")
	f.vars["OPENROUTER_API_KEY"] = "sk-or-x"

	code, out := runDoctor(t, f, nil, "git")
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	wantStatus(t, out, "docker", "fail", "terminal.backend is docker")

	f.stdout.Reset()
	code, out = runDoctor(t, f, nil, "git", "docker")
	if code != 0 {
		t.Fatalf("exit with docker = %d, want 0\n%s", code, out)
	}
	wantStatus(t, out, "docker", "ok", "/usr/bin/docker")
}

func TestDoctorMissingGitWarns(t *testing.T) {
	f := newHome(t, "model:\n  provider: openrouter\n  default: test/model\n")
	f.vars["OPENROUTER_API_KEY"] = "sk-or-x"

	code, out := runDoctor(t, f, nil)

	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	wantStatus(t, out, "git", "warn", "not found")
}

func TestDoctorProfileFlag(t *testing.T) {
	f := newHome(t, "")
	prof := filepath.Join(f.vars["HERMES_HOME"], "profiles", "work")
	if err := os.MkdirAll(prof, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prof, "config.yaml"), []byte("model:\n  provider: openrouter\n  default: work/model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.vars["OPENROUTER_API_KEY"] = "sk-or-x"

	_, out := runDoctor(t, f, []string{"-p", "work"}, "git")

	wantStatus(t, out, "home", "ok", "profile work")
	wantStatus(t, out, "provider", "ok", "model work/model")
}

func TestUsageListsDoctor(t *testing.T) {
	f := newHome(t, "")
	Run(context.Background(), []string{"help"}, f.env())
	if !strings.Contains(f.stderr.String(), "doctor") {
		t.Errorf("usage does not list doctor:\n%s", f.stderr.String())
	}
}
