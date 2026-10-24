package main

import (
	"context"
	"errors"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/hooks"
	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/prompt"
	"github.com/umi008/hermes-agent-go/internal/provider"
	"github.com/umi008/hermes-agent-go/internal/state"
	"github.com/umi008/hermes-agent-go/internal/tools"
	"github.com/umi008/hermes-agent-go/internal/tools/approval"
	"github.com/umi008/hermes-agent-go/internal/tools/delegate"
)

// parentSessionKey carries the id of the session whose tool call is running,
// so a subagent records its parent as upstream parent_session_id.
type parentSessionKey struct{}

// sessionScoped tags each tool call's context with its session id.
type sessionScoped struct {
	agent.ToolExecutor
	s *chatSession
}

func (e sessionScoped) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	return e.ToolExecutor.Execute(context.WithValue(ctx, parentSessionKey{}, e.s.id), call)
}

// Upstream child prompt pieces (tools/delegate_tool_progress.py).
const (
	childIntro          = "You are a focused subagent working on a specific delegated task."
	childContextFiles   = "\nThe workspace's project context files are reproduced below. Their conventions and invariants are binding for your work in this workspace.\n\n"
	childCompletionRule = "\nComplete this task using the tools available to you. When finished, provide a clear, concise summary of:\n" +
		"- What you did\n- What you found or accomplished\n- Any files you created or modified\n- Any issues encountered\n\n" +
		"Important workspace rule: Never assume a repository lives at /workspace/... or any other container-style path unless " +
		"the task/context explicitly gives that path. If no exact local path is provided, discover it first before issuing " +
		"git/workdir-specific commands.\n\n" +
		"Keep your final summary tight: lead with outcomes, prefer bullet points over paragraphs, and don't replay your whole " +
		"process. Your response is returned to the parent agent as a summary, and overlong summaries crowd out the parent's " +
		"context window."
)

// childSystemPrompt is upstream _build_child_system_prompt for a leaf child.
func childSystemPrompt(t delegate.Task, cwd, contextFiles string) string {
	parts := []string{childIntro}
	if c := strings.TrimSpace(t.Context); c != "" {
		parts = append(parts, "\nCONTEXT:\n"+c)
	}
	parts = append(parts, "\nWORKSPACE PATH:\n"+cwd+
		"\nUse this exact path for local repository/workdir operations unless the task explicitly says otherwise.")
	if c := strings.TrimSpace(contextFiles); c != "" {
		parts = append(parts, childContextFiles+c)
	}
	parts = append(parts, childCompletionRule)
	return strings.Join(parts, "\n")
}

// childPrompter answers a child's approval requests, which no human sees:
// deny by default, once with delegation.subagent_auto_approve.
type childPrompter struct {
	approve bool
	rt      *chatRuntime
}

func (p childPrompter) Approve(_ context.Context, r approval.Request) (approval.Choice, error) {
	if p.approve {
		return approval.Once, nil
	}
	p.rt.logger.Warn("subagent approval denied: no human can answer", "command", r.Command, "reason", r.Description)
	return approval.Deny, nil
}

// withDelegation adds delegate_task to the runtime's registry when the
// delegation toolset is selected. Children get the registry without the
// blocked tools, so they cannot spawn further children.
func (rt *chatRuntime) withDelegation() error {
	if sets := rt.f.toolsets; sets != "" && !containsSet(sets, delegate.Toolset) {
		return nil
	}
	childRoute, err := rt.childRoute()
	if err != nil {
		return err
	}
	children := rt.reg.Without(delegate.Blocked...)
	maxChildren, _ := rt.cfg.Int("delegation", "max_concurrent_children")
	maxSummary, _ := rt.cfg.Int("delegation", "max_summary_chars")
	reg, err := rt.reg.With(delegate.Tool(delegate.Options{
		MaxChildren:     maxChildren,
		MaxSummaryChars: maxSummary,
		Now:             rt.env.Now,
		Run: func(ctx context.Context, t delegate.Task) delegate.Outcome {
			return rt.runChild(ctx, t, childRoute, children)
		},
	}))
	if err != nil {
		return err
	}
	rt.reg = reg
	return nil
}

func containsSet(list, name string) bool {
	for _, s := range strings.Split(list, ",") {
		if strings.TrimSpace(s) == name {
			return true
		}
	}
	return false
}

// childRoute is the parent model unless delegation.model, provider,
// base_url or api_key pin another one.
func (rt *chatRuntime) childRoute() (agent.Route, error) {
	o := provider.Overrides{
		Provider: rt.cfg.String("delegation", "provider"),
		Model:    rt.cfg.String("delegation", "model"),
		BaseURL:  rt.cfg.String("delegation", "base_url"),
		APIKey:   rt.cfg.String("delegation", "api_key"),
	}
	if o == (provider.Overrides{}) {
		return agent.Route{Client: rt.client, Provider: rt.ep.Provider, Model: rt.ep.Model, BaseURL: rt.ep.BaseURL}, nil
	}
	if o.Provider == "" && o.BaseURL == "" {
		o.Provider = rt.ep.Provider
	}
	ep, err := provider.Resolve(rt.cfg, rt.lookup, o)
	if err != nil {
		return agent.Route{}, errors.New("delegation: " + err.Error())
	}
	return agent.Route{Client: newClient(ep), Provider: ep.Provider, Model: ep.Model, BaseURL: ep.BaseURL}, nil
}

// runChild runs one subagent: a fresh conversation recorded as a "subagent"
// session under the caller's session, with the goal as its first message.
func (rt *chatRuntime) runChild(ctx context.Context, t delegate.Task, route agent.Route, reg *tools.Registry) delegate.Outcome {
	parent, _ := ctx.Value(parentSessionKey{}).(string)
	started := rt.env.Now()
	rec := newRecorder(ctx, rt.store, state.Session{
		ID: state.NewSessionID(started), Source: "subagent", Model: route.Model, Cwd: rt.cwd, ParentID: parent,
	}, rt.logger)
	var id string
	if rec != nil {
		id = rec.id
	}
	var exec agent.ToolExecutor = reg
	if len(rt.hookSpecs) > 0 {
		exec = hooks.Wrap(reg, hooks.Options{
			Specs: rt.hookSpecs, Runner: rt.hookRunner, SessionID: func() string { return id },
			Cwd: rt.cwd, Profile: rt.f.profile, Approve: rt.approve,
		})
	}
	maxChars, _ := rt.cfg.Int("context_file_max_chars")
	maxTurns, _ := rt.cfg.Int("delegation", "max_iterations")
	if maxTurns <= 0 {
		maxTurns = delegate.DefaultMaxIterations
	}
	var fallbacks []agent.Route
	if route.Client == rt.client {
		fallbacks = rt.fallbacks
	}
	var usage llm.Usage
	a := agent.New(agent.Options{
		Client:     route.Client,
		Compressor: newCompressor(rt.cfg, route.Client, route.Model, rt.env.Now, rt.logger),
		Tools:      exec,
		Model:      route.Model,
		Provider:   route.Provider,
		BaseURL:    route.BaseURL,
		Fallbacks:  fallbacks,
		System:     []string{childSystemPrompt(t, rt.cwd, prompt.ProjectContext(rt.cwd, maxChars, rt.logger))},
		CacheTTL:   cacheTTL(rt.cfg),
		MaxTurns:   maxTurns,
		Logger:     rt.logger,
		OnMessage:  func(m llm.Message) { rec.message(ctx, m) },
		OnEvent: func(e agent.Event) {
			if e.Kind == agent.EventUsage {
				usage.Add(e.Usage)
				rec.usage(ctx, e.Usage)
			}
		},
	})
	auto, _ := rt.cfg.Get("delegation", "subagent_auto_approve")
	childCtx := approval.WithPrompter(ctx, childPrompter{approve: auto == true, rt: rt})
	res, err := a.Run(childCtx, t.Goal)
	history := a.History()
	text := res.Text
	if err != nil {
		text = lastAssistantText(history)
	}
	switch {
	case errors.Is(err, agent.ErrMaxTurns):
		err = delegate.ErrBudget
		rec.end("max_iterations")
	case err != nil && ctx.Err() != nil:
		err = ctx.Err()
		rec.end("interrupted")
	case err != nil:
		rec.end("error")
	default:
		rec.end("completed")
	}
	return delegate.Outcome{Text: text, Turns: res.Turns, Usage: usage, Model: route.Model, Messages: history, Err: err}
}

// lastAssistantText is the last non-empty assistant text, upstream's
// fallback summary for a child that stopped early.
func lastAssistantText(history []llm.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if m := history[i]; m.Role == llm.RoleAssistant && strings.TrimSpace(m.Content) != "" {
			return m.Content
		}
	}
	return ""
}
