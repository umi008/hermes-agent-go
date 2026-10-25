package main

import (
	"context"
	"fmt"
	"io"
	"strings"
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
	rt *chatRuntime
	id string
	// source is the upstream session source, the hooks' platform.
	source  string
	rec     *recorder
	agent   *agent.Agent
	started time.Time
	out     io.Writer
	usage   llm.Usage
	// observe, when set, receives every agent event; the TUI backend maps
	// them to its session events.
	observe func(agent.Event)
	// seed is the history the session was opened with; turn collects the
	// messages of the running turn and compressed notes a compaction, so the
	// API servers can hand back the transcript to replay.
	seed       []llm.Message
	turn       []llm.Message
	compressed bool
}

// sessionSpec opens a conversation. Only source is required.
type sessionSpec struct {
	source string
	out    io.Writer
	// observe sees every agent event before the session handles it.
	observe func(agent.Event)
	// id continues a stored session; empty mints a new id.
	id string
	// history seeds the agent with prior messages.
	history []llm.Message
	// instructions is an ephemeral system prompt tier for this session.
	instructions string
}

// newSession opens a conversation: a fresh id, a recorder and an agent with a
// newly built system prompt, as upstream new_session. source is the upstream
// session source ("cli", "api", "telegram", "discord"); it also fills the
// prompt's platform line.
func (rt *chatRuntime) newSession(ctx context.Context, source string, out io.Writer) *chatSession {
	return rt.openSession(ctx, sessionSpec{source: source, out: out})
}

// newObservedSession is newSession with an observer that sees every agent
// event before the session handles it.
func (rt *chatRuntime) newObservedSession(ctx context.Context, source string, out io.Writer, observe func(agent.Event)) *chatSession {
	return rt.openSession(ctx, sessionSpec{source: source, out: out, observe: observe})
}

// openSession opens a conversation from spec.
func (rt *chatRuntime) openSession(ctx context.Context, spec sessionSpec) *chatSession {
	source, out := spec.source, spec.out
	if out == nil {
		out = io.Discard
	}
	started := rt.env.Now()
	s := &chatSession{rt: rt, source: source, out: out, started: started, observe: spec.observe,
		seed: append([]llm.Message(nil), spec.history...)}
	id := spec.id
	if id == "" {
		id = state.NewSessionID(started)
	}
	s.rec = newRecorder(ctx, rt.store, state.Session{
		ID: id, Source: source, Model: rt.ep.Model, Cwd: rt.cwd,
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
		Compressor: newCompressor(rt.cfg, rt.lookup, rt.primaryRoute(), rt.env.Now, rt.logger),
		Tools:      rt.toolExecutor(s),
		Model:      rt.ep.Model,
		Provider:   rt.ep.Provider,
		BaseURL:    rt.ep.BaseURL,
		Fallbacks:  rt.fallbacks,
		System:     systemTiers(sys.Tiers(), spec.instructions),
		History:    spec.history,
		CacheTTL:   cacheTTL(rt.cfg),
		MaxTurns:   maxTurns,
		Hooks:      rt.lifecycle(func() string { return s.id }, source).Turns(),
		Logger:     rt.logger,
		OnMessage: func(m llm.Message) {
			s.turn = append(s.turn, m)
			s.rec.message(ctx, m)
		},
		OnEvent: func(e agent.Event) { s.onEvent(ctx, e) },
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
	case e.Kind == agent.EventCompress && e.Err == nil:
		s.compressed = true
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
	s.turn, s.compressed = nil, false
	return s.agent.Run(ctx, input)
}

// transcript is the history a follow-up turn replays: the seed plus every
// message recorded since, or the agent's own history after a compaction.
// turn is the last turn's messages after its user message.
func (s *chatSession) transcript() (history, turn []llm.Message) {
	if len(s.turn) > 0 && s.turn[0].Role == llm.RoleUser {
		turn = s.turn[1:]
	}
	if s.compressed {
		return s.agent.History(), turn
	}
	return append(append([]llm.Message(nil), s.seed...), s.turn...), turn
}

// systemTiers appends an ephemeral instructions tier to the system prompt.
func systemTiers(tiers []string, instructions string) []string {
	if strings.TrimSpace(instructions) == "" {
		return tiers
	}
	return append(tiers, instructions)
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
