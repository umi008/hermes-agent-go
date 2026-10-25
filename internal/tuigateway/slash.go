package tuigateway

import (
	"fmt"
	"strings"
)

// The gateway's slash catalog. It lists only what this server can answer: the
// client renders what it is offered, so advertising a command that slash.exec
// then refuses would be a worse experience than not offering it.
const slashCategorySession = "Session"

// slashCommand is one catalog entry. run receives the argument text after the
// command name and answers the output the client renders.
type slashCommand struct {
	name     string
	aliases  []string
	help     string
	category string
	run      func(s *Server, ls *liveSession, arg string) string
}

// slashCommands is the catalog, in display order.
func slashCommands() []slashCommand {
	return []slashCommand{
		{name: "help", aliases: []string{"?"}, help: "Show available commands", category: slashCategorySession,
			run: func(s *Server, _ *liveSession, _ string) string { return slashHelpText() }},
		{name: "model", help: "Show the current model", category: slashCategorySession,
			run: func(s *Server, _ *liveSession, _ string) string {
				return fmt.Sprintf("model: %s (provider %s)", s.o.Info.Model, s.o.Info.Provider)
			}},
		{name: "status", help: "Show the session, model and transcript size", category: slashCategorySession,
			run: (*Server).slashStatus},
	}
}

// lookupSlash resolves a name or alias, case-insensitively.
func lookupSlash(name string) (slashCommand, bool) {
	for _, c := range slashCommands() {
		if c.name == name {
			return c, true
		}
		for _, a := range c.aliases {
			if a == name {
				return c, true
			}
		}
	}
	return slashCommand{}, false
}

// slashUnavailable are commands the CLI has and this gateway cannot run. They
// are not advertised, but naming one answers why instead of pretending it is
// unknown.
var slashUnavailable = map[string]bool{
	"clear": true, "compress": true, "exit": true, "history": true, "new": true, "quit": true,
	"reset": true, "retry": true, "skills": true, "title": true, "tools": true, "toolsets": true,
	"undo": true,
}

// slashHelpText lists the catalog as plain text.
func slashHelpText() string {
	var b strings.Builder
	b.WriteString("Commands:")
	for _, c := range slashCommands() {
		fmt.Fprintf(&b, "\n/%s  %s", c.name, c.help)
	}
	return b.String()
}

// slashStatus answers the session and model a canonical client is looking at.
func (s *Server) slashStatus(ls *liveSession, _ string) string {
	ls.mu.Lock()
	running := ls.running
	ls.mu.Unlock()
	stored := ls.sess.ID()
	count := 0
	if s.o.Store != nil && stored != "" {
		if rows, err := s.o.Store.ActiveMessages(s.base, stored); err == nil {
			count = len(rows)
		}
	}
	return fmt.Sprintf("session: %s\nstored session: %s\nmodel: %s (provider %s)\ncwd: %s\nmessages: %d\nrunning: %t",
		ls.id, stored, s.o.Info.Model, s.o.Info.Provider, s.o.Info.Cwd, count, running)
}

// commandsCatalog answers commands.catalog: the flat pairs, the per-category
// pairs the picker groups by, and the alias map.
func (s *Server) commandsCatalog() map[string]any {
	pairs := []any{}
	canon := map[string]any{}
	grouped := map[string][]any{}
	var order []string
	for _, c := range slashCommands() {
		pair := []any{c.name, c.help}
		pairs = append(pairs, pair)
		if _, seen := grouped[c.category]; !seen {
			order = append(order, c.category)
		}
		grouped[c.category] = append(grouped[c.category], pair)
		for _, alias := range c.aliases {
			canon[alias] = c.name
		}
	}
	categories := make([]any, 0, len(order))
	for _, name := range order {
		categories = append(categories, map[string]any{"name": name, "pairs": grouped[name]})
	}
	return map[string]any{
		"pairs": pairs, "categories": categories, "canon": canon, "sub": map[string]any{},
		"skill_count": 0, "warning": "",
	}
}

// completeSlash answers complete.slash: command names and aliases by prefix.
// Arguments are not completed here (upstream offers none for these commands
// either), and replace_from is 1 because the whole name after the slash is
// replaced.
func (s *Server) completeSlash(params map[string]any) (any, *rpcError) {
	text, _ := params["text"].(string)
	items := []any{}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "/") {
		prefix := strings.ToLower(trimmed[1:])
		if !strings.ContainsAny(prefix, " \t") {
			for _, c := range slashCommands() {
				matched := strings.HasPrefix(c.name, prefix)
				for _, alias := range c.aliases {
					matched = matched || strings.HasPrefix(alias, prefix)
				}
				if !matched {
					continue
				}
				items = append(items, map[string]any{
					"display": "/" + c.name + "  " + c.help, "text": "/" + c.name,
					"kind": "command", "meta": c.help,
				})
			}
		}
	}
	return map[string]any{"items": items, "replace_from": 1}, nil
}

// slashExec answers slash.exec over the catalog.
func (s *Server) slashExec(c *conn, params map[string]any) (any, *rpcError) {
	sid, _ := params["session_id"].(string)
	if strings.TrimSpace(sid) == "" {
		return nil, canonErr(reasonInvalidParams, "session_id is required")
	}
	command, _ := params["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, canonErr(reasonInvalidParams, "command is required")
	}
	ls, err := s.session(sid)
	if err != nil || ls.conn != c {
		return nil, canonErr(reasonNotFound, "session not found")
	}
	name, arg, _ := strings.Cut(strings.TrimPrefix(command, "/"), " ")
	name = strings.ToLower(strings.TrimSpace(name))
	cmd, ok := lookupSlash(name)
	if !ok {
		if slashUnavailable[name] {
			return map[string]any{"output": "/" + name + " is not available from this gateway; run it in the CLI."}, nil
		}
		return map[string]any{"output": "Unknown command: /" + name + " (use /help)"}, nil
	}
	return map[string]any{"output": cmd.run(s, ls, strings.TrimSpace(arg))}, nil
}
