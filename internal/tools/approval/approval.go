// Package approval ports the upstream command approval gate
// (tools/approval.py, tools/approval_floors.py, tools/approval_detection.py):
// hardline commands are always blocked, dangerous commands need a human.
package approval

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

// Choice is a human's answer to an approval prompt.
type Choice int

// Upstream choices: once, session, always, deny.
const (
	Deny Choice = iota
	Once
	Session
	Always
)

// Request describes a command awaiting approval.
type Request struct {
	Command     string
	Description string
}

// Prompter asks a human. An error (cancellation, timeout) counts as no answer.
type Prompter interface {
	Approve(ctx context.Context, r Request) (Choice, error)
}

// Options configure a Gate.
type Options struct {
	Detector *Detector
	// Prompter is nil when nobody can answer (single-query mode, -q).
	Prompter Prompter
	// Allowlist is config command_allowlist: approval keys (pattern
	// descriptions), exact commands or shell-style globs.
	Allowlist []string
	// Persist saves an "always" key to command_allowlist. May be nil.
	Persist func(key string) error
	// Off is approvals.mode: off (yolo); hardline still blocks.
	Off bool
	// UnattendedApprove is approvals.single_query_mode: approve.
	UnattendedApprove bool
	// SudoPassword reports whether SUDO_PASSWORD is configured.
	SudoPassword bool
	Logger       *slog.Logger
}

// Gate decides whether a terminal command may run.
type Gate struct {
	o Options

	mu        sync.Mutex
	allowlist map[string]bool
	globs     []string
	session   map[string]bool
}

// New builds a gate.
func New(o Options) *Gate {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	g := &Gate{o: o, allowlist: map[string]bool{}, session: map[string]bool{}}
	for _, e := range o.Allowlist {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		g.allowlist[e] = true
		if strings.ContainsAny(e, "*?") {
			g.globs = append(g.globs, e)
		}
	}
	return g
}

const (
	stopCommand = " The user has NOT consented to this action. Do NOT retry this command, do " +
		"NOT rephrase it, and do NOT attempt the same outcome via a different " +
		"command. Stop the current workflow and wait for the user to respond before " +
		"taking any further destructive or irreversible action."
	singleQueryClause = "single-query mode (-q) runs without a user present to approve it"
)

func hardlineMessage(desc string) string {
	return "BLOCKED (hardline): " + desc + ". " +
		"This command is on the unconditional blocklist and cannot " +
		"be executed via the agent — not even with --yolo, /yolo, " +
		"approvals.mode=off, or cron approve mode. If you genuinely " +
		"need to run it, run it yourself in a terminal outside the agent."
}

// blocked builds the upstream terminal error envelope.
func blocked(msg string) error {
	return tools.ErrorWith(msg, map[string]any{"output": "", "exit_code": -1, "status": "blocked"})
}

type prompterKey struct{}

// WithPrompter makes p answer the approvals of commands run under ctx, in
// place of the gate's own prompter. Subagents use it to auto-deny or
// auto-approve without asking the human, as upstream; their commands also
// skip the parent's session approvals.
func WithPrompter(ctx context.Context, p Prompter) context.Context {
	return context.WithValue(ctx, prompterKey{}, p)
}

// Check is a terminal.Guard: nil lets the command run.
func (g *Gate) Check(ctx context.Context, command string) error {
	prompter, scoped := ctx.Value(prompterKey{}).(Prompter)
	if !scoped {
		prompter = g.o.Prompter
	}
	d := g.o.Detector
	if desc, ok := d.Hardline(command); ok {
		g.o.Logger.Warn("hardline command blocked", "description", desc)
		return blocked(hardlineMessage(desc))
	}
	if !g.o.SudoPassword && d.SudoStdin(command) {
		return blocked(hardlineMessage("sudo password guessing via stdin (sudo -S)"))
	}
	if g.o.Off || g.allowedCommand(command) {
		return nil
	}
	desc, ok := d.Dangerous(command)
	if !ok || g.approved(desc, !scoped) {
		return nil
	}
	subject := fmt.Sprintf("Command flagged as dangerous (%s)", desc)
	if prompter == nil {
		if g.o.UnattendedApprove {
			g.o.Logger.Info("auto-approved dangerous command in single-query mode", "description", desc)
			return nil
		}
		return blocked("BLOCKED: " + subject + " but " + singleQueryClause +
			". Find an alternative approach that avoids this command. To allow dangerous commands in single-query mode, set " +
			"approvals.single_query_mode: approve in config.yaml.")
	}
	choice, err := prompter.Approve(ctx, Request{Command: command, Description: desc})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return blocked("BLOCKED: Command timed out without user response." + stopCommand)
	}
	switch choice {
	case Once:
		return nil
	case Session, Always:
		if scoped {
			return nil
		}
		g.mu.Lock()
		g.session[desc] = true
		g.mu.Unlock()
		if choice == Always && g.o.Persist != nil {
			if err := g.o.Persist(desc); err != nil {
				g.o.Logger.Warn("persist command_allowlist", "err", err)
			}
		}
		return nil
	}
	return blocked("BLOCKED: User denied this command." + stopCommand)
}

func (g *Gate) approved(key string, withSession bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return (withSession && g.session[key]) || g.allowlist[key]
}

// allowedCommand matches command_allowlist command text or globs. Commands
// with shell operators never match, so `ls; rm -rf ~` cannot ride `ls*`.
func (g *Gate) allowedCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" || strings.ContainsAny(command, ";&|`$<>()\n") {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.allowlist[command] {
		return true
	}
	for _, p := range g.globs {
		if globMatch(p, command) {
			return true
		}
	}
	return false
}

// globMatch is fnmatch with * and ? (which, unlike path.Match, cross "/").
func globMatch(pattern, s string) bool {
	q := regexp.QuoteMeta(pattern)
	q = strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(q)
	re, err := regexp.Compile("(?s)^" + q + "$")
	return err == nil && re.MatchString(s)
}
