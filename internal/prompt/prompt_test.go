package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, p, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixedNow(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2026, 10, 4, 9, 30, 0, 0, time.FixedZone("CST", -6*3600))
}

func TestBuildDefaultIdentityAndTimestamp(t *testing.T) {
	p := Build(Inputs{HomeDir: t.TempDir(), Model: "m", Provider: "anthropic", Platform: "cli", Now: fixedNow(t)})
	if p.Stable != DefaultAgentIdentity {
		t.Fatalf("stable = %q", p.Stable)
	}
	want := "Conversation started: Sunday, October 04, 2026 (CST, UTC-06:00)\nModel: m\nProvider: anthropic\nPlatform: cli"
	if p.Volatile != want {
		t.Fatalf("volatile = %q\nwant       %q", p.Volatile, want)
	}
	if p.Context != "" {
		t.Fatalf("context = %q", p.Context)
	}
	if got := p.String(); got != p.Stable+"\n\n"+p.Volatile {
		t.Fatalf("String() = %q", got)
	}
}

func TestBuildPlacesGuidanceAndMemory(t *testing.T) {
	p := Build(Inputs{HomeDir: t.TempDir(), Model: "m", Now: fixedNow(t), ToolGuidance: []string{"use memory"}, Memory: []string{"MEM", "USER"}})
	if p.Stable != DefaultAgentIdentity+"\n\nuse memory" {
		t.Fatalf("stable = %q", p.Stable)
	}
	if !strings.HasPrefix(p.Volatile, "MEM\n\nUSER\n\nConversation started:") {
		t.Fatalf("volatile = %q", p.Volatile)
	}
}

func TestBuildSoulReplacesIdentity(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "SOUL.md"), "\uFEFF  You are Pip.  \n")
	p := Build(Inputs{HomeDir: home})
	if p.Stable != "You are Pip." {
		t.Fatalf("stable = %q", p.Stable)
	}
}

func TestBuildIsByteStable(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	write(t, filepath.Join(cwd, "AGENTS.md"), "rules")
	in := Inputs{HomeDir: home, Cwd: cwd, Model: "m", Now: fixedNow(t)}
	first, second := Build(in), Build(in)
	if first.String() != second.String() {
		t.Fatal("same inputs gave different prompts")
	}
}

func TestContextFilesPriority(t *testing.T) {
	tr := truncator{max: DefaultMaxFileChars}
	t.Run("hermes.md wins and strips frontmatter", func(t *testing.T) {
		cwd := t.TempDir()
		write(t, filepath.Join(cwd, ".hermes.md"), "---\ntitle: x\n---\nbody")
		write(t, filepath.Join(cwd, "AGENTS.md"), "agents")
		got := ContextFiles(cwd, tr)
		if got != contextHeader+"## .hermes.md\n\nbody" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("hermes.md found in parent up to git root", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, ".git", "HEAD"), "ref")
		write(t, filepath.Join(root, "HERMES.md"), "top")
		sub := filepath.Join(root, "a", "b")
		write(t, filepath.Join(sub, "x.txt"), "")
		got := ContextFiles(sub, tr)
		if !strings.Contains(got, "## HERMES.md\n\ntop") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("agents chain from git root with override and dedupe", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, ".git", "HEAD"), "ref")
		write(t, filepath.Join(root, "AGENTS.md"), "root rules")
		sub := filepath.Join(root, "pkg")
		write(t, filepath.Join(sub, "AGENTS.override.md"), "mine")
		write(t, filepath.Join(sub, "AGENTS.md"), "ignored")
		deeper := filepath.Join(sub, "deep")
		write(t, filepath.Join(deeper, "AGENTS.md"), "root rules")
		got := ContextFiles(deeper, tr)
		want := contextHeader + "## ../../AGENTS.md\n\nroot rules\n\n## ../AGENTS.override.md\n\nmine"
		if got != want {
			t.Fatalf("got  %q\nwant %q", got, want)
		}
	})
	t.Run("claude.md in cwd only", func(t *testing.T) {
		cwd := t.TempDir()
		write(t, filepath.Join(cwd, "CLAUDE.md"), "c")
		if got := ContextFiles(cwd, tr); got != contextHeader+"## CLAUDE.md\n\nc" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("cursor rules concatenated", func(t *testing.T) {
		cwd := t.TempDir()
		write(t, filepath.Join(cwd, ".cursorrules"), "r")
		write(t, filepath.Join(cwd, ".cursor", "rules", "b.mdc"), "b")
		write(t, filepath.Join(cwd, ".cursor", "rules", "a.mdc"), "a")
		want := contextHeader + "## .cursorrules\n\nr\n\n## .cursor/rules/a.mdc\n\na\n\n## .cursor/rules/b.mdc\n\nb\n\n"
		if got := ContextFiles(cwd, tr); got != want {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("nothing found", func(t *testing.T) {
		if got := ContextFiles(t.TempDir(), tr); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestTruncate(t *testing.T) {
	tr := truncator{max: 100}
	short := strings.Repeat("é", 100)
	if tr.truncate(short, "f", "p") != short {
		t.Fatal("100 chars must not truncate (count runes, not bytes)")
	}
	body := strings.Repeat("a", 70) + "\n## Lost Part\n" + strings.Repeat("b", 100) + strings.Repeat("z", 20)
	got := tr.truncate(body, "AGENTS.md", "/x/AGENTS.md")
	if !strings.HasPrefix(got, strings.Repeat("a", 70)+"\n\n[...truncated AGENTS.md: kept 70+20 of") {
		t.Fatalf("head wrong: %q", got[:90])
	}
	if !strings.Contains(got, "Omitted sections: Lost Part.") || !strings.Contains(got, "read_file tool: /x/AGENTS.md]") {
		t.Fatalf("marker wrong: %q", got)
	}
	if !strings.HasSuffix(got, "]\n\n"+strings.Repeat("z", 20)) {
		t.Fatalf("tail wrong: %q", got)
	}
}
