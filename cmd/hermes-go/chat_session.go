package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/hooks"
	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/prompt"
	"github.com/umi008/hermes-agent-go/internal/state"
	"github.com/umi008/hermes-agent-go/internal/tools/memory"
)

// chatSession is one conversation over a shared chatRuntime. It owns its
// session id, the recorder that persists it, its agent and the sink its text
// goes to. The REPL keeps one current session; the servers open one per
// request or per chat and never touch rt.cur.
type chatSession struct {
	rt      *chatRuntime
	id      string
	rec     *recorder
	agent   *agent.Agent
	started time.Time
	out     io.Writer
	usage   llm.Usage
	// observe, when set, receives every agent event; the TUI backend maps
	// them to its session events.
	observe func(agent.Event)
}

// newSession opens a conversation: a fresh id, a recorder and an agent with a
// newly built system prompt, as upstream new_session. source is the upstream
// session source ("cli", "api", "telegram", "discord"); it also fills the
// prompt's platform line.
func (rt *chatRuntime) newSession(ctx context.Context, source string, out io.Writer) *chatSession {
	return rt.newObservedSession(ctx, source, out, nil)
}

// newObservedSession is newSession with an observer that sees every agent
// event before the session handles it.
func (rt *chatRuntime) newObservedSession(ctx context.Context, source string, out io.Writer, observe func(agent.Event)) *chatSession {
	if out == nil {
		out = io.Discard
	}
	started := rt.env.Now()
	s := &chatSession{rt: rt, out: out, started: started, observe: observe}
	s.rec = newRecorder(ctx, rt.store, state.Session{
		ID: state.NewSessionID(started), Source: source, Model: rt.ep.Model, Cwd: rt.cwd,
	}, rt.logger)
	if s.rec != nil {
		s.id = s.rec.id
	}

	var guidance, memBlocks []string
	if hasTool(rt.reg, "memory") {
		guidance = append(guidance, memory.Guidance)
	}
	if hasTool(rt.reg, "session_search") {
		guidance = append(guidance, sessionSearchGuidance)
	}
	if rt.memory != nil {
		memBlocks = rt.memory.PromptBlocks()
	}
	maxChars, _ := rt.cfg.Int("context_file_max_chars")
	sys := prompt.Build(prompt.Inputs{
		HomeDir:      rt.hermes,
		Cwd:          rt.cwd,
		MaxFileChars: maxChars,
		Model:        rt.ep.Model,
		Provider:     rt.ep.Provider,
		Platform:     source,
		Now:          started,
		ToolGuidance: guidance,
		Skills:       skillsIndex(rt.skills, rt.reg, rt.lookup),
		Memory:       memBlocks,
		Logger:       rt.logger,
	})
	maxTurns, _ := rt.cfg.Int("agent", "max_turns")
	s.agent = agent.New(agent.Options{
		Client:     rt.client,
		Compressor: newCompressor(rt.cfg, rt.client, rt.ep.Model, rt.env.Now, rt.logger),
		Tools:      rt.toolExecutor(s),
		Model:      rt.ep.Model,
		Provider:   rt.ep.Provider,
		BaseURL:    rt.ep.BaseURL,
		Fallbacks:  rt.fallbacks,
		System:     sys.Tiers(),
		CacheTTL:   cacheTTL(rt.cfg),
		MaxTurns:   maxTurns,
		Logger:     rt.logger,
		OnMessage:  func(m llm.Message) { s.rec.message(ctx, m) },
		OnEvent:    func(e agent.Event) { s.onEvent(ctx, e) },
	})
	return s
}

// toolExecutor wraps the shared registry with this session's hooks, so a hook
// payload carries the id of the session that made the call. Without hooks the
// registry is used as is. Every call's context carries the session id, so
// delegate_task can record its children under it.
func (rt *chatRuntime) toolExecutor(s *chatSession) agent.ToolExecutor {
	if len(rt.hookSpecs) == 0 {
		return sessionScoped{rt.reg, s}
	}
	return sessionScoped{hooks.Wrap(rt.reg, hooks.Options{
		Specs:     rt.hookSpecs,
		Runner:    rt.hookRunner,
		SessionID: func() string { return s.id },
		Cwd:       rt.cwd,
		Profile:   rt.f.profile,
		Approve:   rt.approve,
	}), s}
}

// onEvent passes the event to the observer, streams text to the session
// sink and records usage.
func (s *chatSession) onEvent(ctx context.Context, e agent.Event) {
	if s.observe != nil {
		s.observe(e)
	}
	switch {
	case e.Kind == agent.EventUsage:
		s.usage.Add(e.Usage)
		s.rec.usage(ctx, e.Usage)
	case e.Kind == agent.EventText:
		_, _ = io.WriteString(s.out, e.Text)
	case e.Kind == agent.EventFallback:
		_, _ = fmt.Fprintln(s.rt.env.Stderr, e.Text)
	case e.Kind == agent.EventToolStart && s.rt.f.verbose && e.ToolCall != nil:
		_, _ = fmt.Fprintf(s.rt.env.Stderr, "[tool] %s %s\n", e.ToolCall.Name, e.ToolCall.Arguments)
	}
}

// run sends one user message through the agent. Callers own the framing of the
// reply, so a streamed response never gains a stray newline.
func (s *chatSession) run(ctx context.Context, input string) (agent.Result, error) {
	return s.agent.Run(ctx, input)
}

// history is the live session's conversation, empty when there is none.
func (rt *chatRuntime) history() []llm.Message {
	if s := rt.currentSession(); s != nil {
		return s.agent.History()
	}
	return nil
}

// recorder is the live session's recorder, nil when there is none.
func (rt *chatRuntime) recorder() *recorder {
	if s := rt.currentSession(); s != nil {
		return s.rec
	}
	return nil
}

// usageTotals is the live session's token totals.
func (rt *chatRuntime) usageTotals() llm.Usage {
	if s := rt.currentSession(); s != nil {
		return s.usage
	}
	return llm.Usage{}
}

// end closes the session with reason, as upstream "cli_close" or
// "new_session". It is safe on a nil session.
func (s *chatSession) end(reason string) {
	if s == nil {
		return
	}
	s.rec.end(reason)
}
