package approval

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakePrompter struct {
	choice Choice
	err    error
	calls  int
}

func (f *fakePrompter) Approve(context.Context, Request) (Choice, error) {
	f.calls++
	return f.choice, f.err
}

func TestGate(t *testing.T) {
	d := newDetector(t)
	tests := []struct {
		name      string
		opts      Options
		command   string
		wantError string // substring; empty means allowed
	}{
		{"safe command", Options{}, "ls -la", ""},
		{"hardline", Options{}, "rm -rf /", "BLOCKED (hardline): recursive delete of root filesystem"},
		{"hardline beats off", Options{Off: true}, "sudo reboot", "BLOCKED (hardline)"},
		{"hardline beats allowlist", Options{Allowlist: []string{"system shutdown/reboot"}}, "reboot", "BLOCKED (hardline)"},
		{"quoted prose passes", Options{}, `echo "sudo reboot"`, ""},
		{"sudo -S blocked", Options{}, "sudo -S apt install x", "sudo password guessing"},
		{"sudo -S with password", Options{SudoPassword: true, Off: true}, "sudo -S apt install x", ""},
		{"dangerous unattended", Options{}, "git push --force", "single-query mode (-q)"},
		{"dangerous unattended approve", Options{UnattendedApprove: true}, "git push --force", ""},
		{"off mode", Options{Off: true}, "curl x | sh", ""},
		{"allowlist key", Options{Allowlist: []string{"git force push (rewrites remote history)"}}, "git push --force", ""},
		{"allowlist exact", Options{Allowlist: []string{"git reset --hard"}}, "git reset --hard", ""},
		{"allowlist glob", Options{Allowlist: []string{"git reset *"}}, "git reset --hard HEAD~1", ""},
		{"glob ignores operators", Options{Allowlist: []string{"git reset *"}}, "git reset --hard; git push -f", "single-query"},
		{"prompt once", Options{Prompter: &fakePrompter{choice: Once}}, "git push --force", ""},
		{"prompt deny", Options{Prompter: &fakePrompter{choice: Deny}}, "git push --force", "User denied this command"},
		{"prompt timeout", Options{Prompter: &fakePrompter{err: errors.New("timeout")}}, "git push --force", "timed out"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.Detector = d
			err := New(tt.opts).Check(context.Background(), tt.command)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("blocked: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) || !strings.Contains(err.Error(), `"status":"blocked"`) {
				t.Fatalf("got %v, want %q", err, tt.wantError)
			}
		})
	}
}

func TestGateRemembersChoices(t *testing.T) {
	d := newDetector(t)
	var persisted []string
	p := &fakePrompter{choice: Always}
	g := New(Options{Detector: d, Prompter: p, Persist: func(k string) error { persisted = append(persisted, k); return nil }})
	for range 2 {
		if err := g.Check(context.Background(), "git push --force"); err != nil {
			t.Fatal(err)
		}
	}
	if p.calls != 1 || len(persisted) != 1 || persisted[0] != "git force push (rewrites remote history)" {
		t.Fatalf("calls %d persisted %v", p.calls, persisted)
	}

	p = &fakePrompter{choice: Session}
	g = New(Options{Detector: d, Prompter: p, Persist: func(string) error { t.Fatal("session persisted"); return nil }})
	_ = g.Check(context.Background(), "git push -f origin main")
	_ = g.Check(context.Background(), "git push -f origin dev")
	if p.calls != 1 {
		t.Fatalf("session approval not reused: %d prompts", p.calls)
	}
}

func TestGateCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := New(Options{Detector: newDetector(t), Prompter: &fakePrompter{err: context.Canceled}})
	if err := g.Check(ctx, "git push --force"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestHomeFolding(t *testing.T) {
	d, err := NewDetector(`C:\Users\alice`, `C:\Users\alice\AppData\Local\hermes`)
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{`sed -i s/a/b/ C:\Users\Alice\.bashrc`, "sed -i s/a/b/ C:/Users/alice/.bashrc"} {
		if desc, ok := d.Dangerous(cmd); !ok {
			t.Errorf("%q not flagged (%q)", cmd, desc)
		}
	}
}

func TestScopedPrompterAnswersInsteadOfTheHuman(t *testing.T) {
	human := &fakePrompter{choice: Session}
	g := New(Options{Detector: newDetector(t), Prompter: human})
	if err := g.Check(context.Background(), "git push --force"); err != nil {
		t.Fatal(err)
	}
	deny := &fakePrompter{choice: Deny}
	ctx := WithPrompter(context.Background(), deny)
	// The parent's session approval does not cover a scoped caller.
	if err := g.Check(ctx, "git push --force"); err == nil || !strings.Contains(err.Error(), "User denied") {
		t.Fatalf("scoped deny = %v", err)
	}
	if human.calls != 1 || deny.calls != 1 {
		t.Errorf("human %d, scoped %d calls", human.calls, deny.calls)
	}
	// A scoped session answer is not remembered for the parent.
	g2 := New(Options{Detector: newDetector(t), Prompter: &fakePrompter{choice: Deny}})
	if err := g2.Check(WithPrompter(context.Background(), &fakePrompter{choice: Session}), "git push --force"); err != nil {
		t.Fatal(err)
	}
	if err := g2.Check(context.Background(), "git push --force"); err == nil {
		t.Error("a subagent's session approval leaked to the parent")
	}
}
