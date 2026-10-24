package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/tools/approval"
	"github.com/umi008/hermes-agent-go/internal/tools/clarify"
)

// command is one slash command of the interactive chat, a subset of
// upstream hermes_cli/commands.py COMMAND_REGISTRY with its descriptions.
type command struct {
	name    string
	aliases []string
	args    string
	help    string
	run     func(rt *chatRuntime, ctx context.Context, arg string) (quit bool)
}

// commands is a function so /help can list it without an init cycle.
func commands() []command {
	return []command{
		{name: "new", aliases: []string{"reset"}, help: "Start a new session (fresh session ID + history)", run: (*chatRuntime).cmdNew},
		{name: "clear", help: "Clear screen and start a new session", run: (*chatRuntime).cmdClear},
		{name: "history", help: "Show conversation history", run: (*chatRuntime).cmdHistory},
		{name: "retry", help: "Retry the last message (resend to agent)", run: (*chatRuntime).cmdRetry},
		{name: "undo", args: "[N]", help: "Back up N user turns and re-prompt (default 1)", run: (*chatRuntime).cmdUndo},
		{name: "title", args: "[name]", help: "Set a title for the current session", run: (*chatRuntime).cmdTitle},
		{name: "status", help: "Show session, model, token, and context info", run: (*chatRuntime).cmdStatus},
		{name: "model", help: "Show the current model", run: (*chatRuntime).cmdModel},
		{name: "tools", help: "List the tools the agent can call", run: (*chatRuntime).cmdTools},
		{name: "toolsets", help: "List available toolsets", run: (*chatRuntime).cmdToolsets},
		{name: "skills", help: "List installed skills", run: (*chatRuntime).cmdSkills},
		{name: "help", help: "Show available commands", run: (*chatRuntime).cmdHelp},
		{name: "quit", aliases: []string{"exit"}, help: "Exit the CLI", run: func(*chatRuntime, context.Context, string) bool { return true }},
	}
}

func lookupCommand(name string) (command, bool) {
	for _, c := range commands() {
		if c.name == name {
			return c, true
		}
		for _, a := range c.aliases {
			if a == name {
				return c, true
			}
		}
	}
	return command{}, false
}

// readLines feeds stdin lines to a channel, closed at EOF. The REPL and
// the clarify asker both read from it, one at a time.
func readLines(r io.Reader) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			out <- strings.TrimRight(sc.Text(), "\r")
		}
	}()
	return out
}

// repl is the interactive chat: one message per line, slash commands,
// Ctrl-C cancels the running turn and exits when idle.
func (rt *chatRuntime) repl(ctx context.Context) error {
	rt.lines = readLines(rt.env.Stdin)
	rt.printf("Hermes Agent (Go). Type /help for commands, /quit to exit.\n")
	for {
		rt.printf("> ")
		var line string
		select {
		case <-ctx.Done():
			return nil
		case <-rt.env.Interrupt:
			rt.printf("\n")
			return nil
		case l, ok := <-rt.lines:
			if !ok {
				rt.printf("\n")
				return nil
			}
			line = l
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "/"):
			name, arg, _ := strings.Cut(line[1:], " ")
			c, ok := lookupCommand(strings.ToLower(name))
			if !ok {
				rt.printf("Unknown command: /%s (use /help)\n", name)
				continue
			}
			if c.run(rt, ctx, strings.TrimSpace(arg)) {
				return nil
			}
		default:
			rt.interactiveTurn(ctx, line)
		}
	}
}

// interactiveTurn runs one message; Ctrl-C cancels just this turn, and
// errors are reported without leaving the REPL.
func (rt *chatRuntime) interactiveTurn(ctx context.Context, input string) {
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-rt.env.Interrupt:
			cancel()
		case <-done:
		}
	}()
	// The watcher is gone before the prompt returns, so the next Ctrl-C
	// reaches the idle loop.
	defer func() { close(done); <-watched }()
	err := rt.turn(tctx, input)
	switch {
	case err == nil:
	case tctx.Err() != nil && ctx.Err() == nil:
		rt.printf("\nInterrupted.\n")
	default:
		_, _ = fmt.Fprintf(rt.env.Stderr, "hermes-go: %v\n", err)
	}
}

func (rt *chatRuntime) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(rt.env.Stdout, format, args...)
}

func (rt *chatRuntime) cmdNew(ctx context.Context, _ string) bool {
	rt.endSession("new_session")
	rt.startSession(ctx)
	rt.printf("New session started: %s\n", rt.sessionID())
	return false
}

func (rt *chatRuntime) cmdClear(ctx context.Context, arg string) bool {
	rt.printf("\x1b[H\x1b[2J")
	return rt.cmdNew(ctx, arg)
}

func (rt *chatRuntime) cmdHistory(context.Context, string) bool {
	hist := rt.agent.History()
	if len(hist) == 0 {
		rt.printf("(._.) No conversation history yet.\n")
		return false
	}
	for _, m := range hist {
		text := m.Content
		if len(m.ToolCalls) > 0 {
			names := make([]string, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				names = append(names, c.Name)
			}
			text = strings.TrimSpace(text + " [calls: " + strings.Join(names, ", ") + "]")
		}
		rt.printf("%s: %s\n", m.Role, preview(text, 200))
	}
	return false
}

func (rt *chatRuntime) cmdRetry(ctx context.Context, _ string) bool {
	text, ok := rt.rewind(ctx, 1, "retry")
	if !ok {
		return false
	}
	rt.printf("(^_^)b Retrying: %q\n", preview(text, 60))
	rt.interactiveTurn(ctx, text)
	return false
}

func (rt *chatRuntime) cmdUndo(ctx context.Context, arg string) bool {
	n := 1
	if arg != "" {
		v, err := strconv.Atoi(arg)
		if err != nil || v < 1 {
			rt.printf("Usage: /undo [N]\n")
			return false
		}
		n = v
	}
	users := 0
	for _, m := range rt.agent.History() {
		if m.Role == llm.RoleUser {
			users++
		}
	}
	before := len(rt.agent.History())
	text, ok := rt.rewind(ctx, n, "undo")
	if !ok {
		return false
	}
	undone, removed := min(n, users), before-len(rt.agent.History())
	turns := "turn"
	if undone != 1 {
		turns = "turns"
	}
	rt.printf("(^_^)b Undid %d %s (%d message(s)). Backed up to: %q\n", undone, turns, removed, preview(text, 60))
	rt.printf("  %d message(s) remaining in history.\n", len(rt.agent.History()))
	return false
}

// rewind drops the last n user turns on disk first, then in memory, as
// upstream publishes the shorter history only after the durable rewind.
func (rt *chatRuntime) rewind(ctx context.Context, n int, verb string) (string, bool) {
	if len(rt.agent.History()) == 0 {
		rt.printf("(._.) No messages to %s.\n", verb)
		return "", false
	}
	hasUser := false
	for _, m := range rt.agent.History() {
		hasUser = hasUser || m.Role == llm.RoleUser
	}
	if !hasUser {
		rt.printf("(._.) No user message found to %s.\n", verb)
		return "", false
	}
	if err := rt.rec.rewind(ctx, n); err != nil {
		rt.printf("(x_x) %s failed; history was not changed: %v\n", strings.ToUpper(verb[:1])+verb[1:], err)
		return "", false
	}
	text, _, _ := rt.agent.Rewind(n)
	return text, true
}

func (rt *chatRuntime) cmdTitle(ctx context.Context, arg string) bool {
	if arg == "" {
		rt.printf("Usage: /title <name>\n")
		return false
	}
	if err := rt.rec.title(ctx, arg); err != nil {
		rt.printf("(x_x) Could not set the title: %v\n", err)
		return false
	}
	rt.printf("Session title set: %s\n", arg)
	return false
}

func (rt *chatRuntime) cmdStatus(context.Context, string) bool {
	id := rt.sessionID()
	if id == "" {
		id = "(not saved)"
	}
	u := rt.usage
	rt.printf("Session:  %s\nModel:    %s (%s)\nMessages: %d\nTokens:   %d in, %d out, %d cache read, %d cache write\n",
		id, rt.ep.Model, rt.ep.Provider, len(rt.agent.History()),
		u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	return false
}

func (rt *chatRuntime) cmdModel(_ context.Context, arg string) bool {
	rt.printf("Current model: %s (%s)\n", rt.ep.Model, rt.ep.Provider)
	if arg != "" {
		rt.printf("Switching models mid-session is not supported yet; restart with -m %s.\n", arg)
	}
	return false
}

func (rt *chatRuntime) cmdTools(context.Context, string) bool {
	for _, s := range rt.reg.Specs() {
		desc, _, _ := strings.Cut(s.Description, "\n")
		rt.printf("  %-22s %s\n", s.Name, preview(desc, 80))
	}
	return false
}

func (rt *chatRuntime) cmdToolsets(context.Context, string) bool {
	for _, ts := range rt.reg.Toolsets() {
		rt.printf("  %s\n", ts)
	}
	return false
}

func (rt *chatRuntime) cmdSkills(context.Context, string) bool {
	var all []string
	if rt.skills != nil {
		for _, s := range rt.skills.All() {
			all = append(all, fmt.Sprintf("  %-22s %s", s.Name, preview(s.Description, 80)))
		}
	}
	if len(all) == 0 {
		rt.printf("No skills installed.\n")
		return false
	}
	rt.printf("%s\n", strings.Join(all, "\n"))
	return false
}

func (rt *chatRuntime) cmdHelp(context.Context, string) bool {
	rt.printf("Commands:\n")
	for _, c := range commands() {
		name := "/" + c.name
		if c.args != "" {
			name += " " + c.args
		}
		help := c.help
		if len(c.aliases) > 0 {
			help += " (alias: /" + strings.Join(c.aliases, ", /") + ")"
		}
		rt.printf("  %-14s %s\n", name, help)
	}
	return false
}

// preview cuts s to n runes with an ellipsis.
func preview(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// lineAsker answers clarify from the interactive chat's input lines.
// A number picks a choice (comma-separated for multi-select), other text
// is a free-form answer, and an empty line skips the question.
type lineAsker struct{ rt *chatRuntime }

func (a *lineAsker) Ask(ctx context.Context, qs []clarify.Question) (clarify.Reply, error) {
	rt := a.rt
	answers := map[string]any{}
	for _, q := range qs {
		rt.printf("\n? %s\n", q.Text)
		for i, c := range q.Choices {
			rt.printf("  %d. %s\n", i+1, c)
		}
		if len(q.Choices) > 0 {
			rt.printf("  (number, or type your own answer; empty skips)\n")
		}
		rt.printf("> ")
		var line string
		select {
		case <-ctx.Done():
			return clarify.Reply{}, ctx.Err()
		case l, ok := <-rt.lines:
			if !ok {
				return clarify.Reply{Answers: answers, Outcome: "cancelled", Notice: "input closed"}, nil
			}
			line = strings.TrimSpace(l)
		}
		answers[q.ID] = parseAnswer(line, q)
	}
	return clarify.Reply{Answers: answers, Outcome: "submitted"}, nil
}

func parseAnswer(line string, q clarify.Question) any {
	if line == "" {
		return nil
	}
	pick := func(s string) (string, bool) {
		i, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || i < 1 || i > len(q.Offered) {
			return "", false
		}
		return q.Offered[i-1], true
	}
	if q.MultiSelect {
		var out []string
		for _, part := range strings.Split(line, ",") {
			c, ok := pick(part)
			if !ok {
				return line
			}
			out = append(out, c)
		}
		return out
	}
	if c, ok := pick(line); ok {
		return c
	}
	return line
}

// linePrompter asks for dangerous-command approval in the interactive
// chat with upstream's CLI menu. The Go port never writes config.yaml, so
// the menu is the short one (no [a]lways), and "always" counts as session
// as upstream does when permanent approval is unavailable.
type linePrompter struct{ rt *chatRuntime }

func (p *linePrompter) Approve(ctx context.Context, r approval.Request) (approval.Choice, error) {
	rt := p.rt
	rt.printf("\n  ⚠️  DANGEROUS COMMAND: %s\n      %s\n\n      [o]nce  |  [s]ession  |  [d]eny\n\n      Choice [o/s/D]: ", r.Description, r.Command)
	var line string
	select {
	case <-ctx.Done():
		rt.printf("\n      ✗ Cancelled\n\n")
		return approval.Deny, ctx.Err()
	case l, ok := <-rt.lines:
		if !ok {
			rt.printf("\n      ✗ Cancelled\n\n")
			return approval.Deny, errors.New("input closed")
		}
		line = strings.ToLower(strings.TrimSpace(l))
	}
	choice, msg := approval.Deny, "      ✗ Denied"
	switch line {
	case "o", "once":
		choice, msg = approval.Once, "      ✓ Allowed once"
	case "s", "session", "a", "always":
		choice, msg = approval.Session, "      ✓ Allowed for this session"
	}
	rt.printf("%s\n\n", msg)
	return choice, nil
}
