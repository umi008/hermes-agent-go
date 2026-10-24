// Package prompt builds the Hermes system prompt in three cache tiers, as
// upstream agent/system_prompt.py does: stable (identity), context (project
// context files), and volatile (date and session facts). The prompt is built
// once per session so the provider's prefix cache stays warm.
package prompt

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// DefaultAgentIdentity is upstream DEFAULT_AGENT_IDENTITY, used when the home
// has no SOUL.md.
const DefaultAgentIdentity = "You are Hermes Agent, built by Nous Research. Be direct: match the length of your reply to the weight of the ask " +
	"— a one-line question gets a one-line answer, and finished work gets a short report of what changed, what's " +
	"verified, and what's left, never a replay of the process. No filler (\"Great question,\" \"I'd be happy to\"), no " +
	"restating the request back, no re-summarizing what you already said, no narrating tool calls the user can see. " +
	"Plain claims over adjectives; when unsure, say so plainly. Agree because it's right, not because the user said it. " +
	"Depth is earned — give it when the user asks for detail, teaches, or the stakes demand it, not by default."

// Inputs are the facts a prompt depends on. Everything that varies is
// passed in, so the same inputs always give the same bytes.
type Inputs struct {
	// HomeDir is the profile home holding SOUL.md.
	HomeDir string
	// Cwd is where project context discovery starts.
	Cwd string
	// SkipContextFiles disables project context discovery.
	SkipContextFiles bool
	// MaxFileChars caps each context file; zero means DefaultMaxFileChars.
	MaxFileChars int

	Model    string
	Provider string
	Platform string
	// Now is the session start time, in the user's location.
	Now time.Time

	// ToolGuidance is behavioral text for loaded tools, kept in the stable
	// tier after the identity.
	ToolGuidance []string
	// Memory holds the frozen memory blocks; they lead the volatile tier.
	Memory []string

	Logger *slog.Logger
}

// Prompt is a built system prompt.
type Prompt struct {
	Stable   string
	Context  string
	Volatile string
}

// Tiers returns the non-empty tiers in order, most stable first.
func (p Prompt) Tiers() []string {
	var out []string
	for _, t := range []string{p.Stable, p.Context, p.Volatile} {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// String joins the tiers with a blank line.
func (p Prompt) String() string { return strings.Join(p.Tiers(), "\n\n") }

// Build assembles the prompt.
func Build(in Inputs) Prompt {
	logger := in.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	maxChars := in.MaxFileChars
	if maxChars <= 0 {
		maxChars = DefaultMaxFileChars
	}
	t := truncator{max: maxChars, logger: logger}

	identity, soulPath := loadSoul(in.HomeDir, t)
	if identity == "" {
		identity = DefaultAgentIdentity
	}
	if soulPath != "" {
		logger.Debug("loaded SOUL.md", "path", soulPath)
	}

	var context string
	if !in.SkipContextFiles && in.Cwd != "" {
		context = ContextFiles(in.Cwd, t)
	}

	return Prompt{
		Stable:   joinTier(append([]string{identity}, in.ToolGuidance...)...),
		Context:  joinTier(context),
		Volatile: joinTier(append(append([]string(nil), in.Memory...), timestampLine(in))...),
	}
}

func joinTier(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

// timestampLine matches upstream _timestamp_line for a same-day session:
// a date-only line (byte-stable for the day), the zone, then session facts.
func timestampLine(in Inputs) string {
	if in.Now.IsZero() {
		return ""
	}
	line := "Conversation started: " + in.Now.Format("Monday, January 02, 2006")
	if bits := zoneBits(in.Now); len(bits) > 0 {
		line += " (" + strings.Join(bits, ", ") + ")"
	}
	for _, kv := range [][2]string{{"Model", in.Model}, {"Provider", in.Provider}, {"Platform", in.Platform}} {
		if kv[1] != "" {
			line += fmt.Sprintf("\n%s: %s", kv[0], kv[1])
		}
	}
	return line
}

func zoneBits(now time.Time) []string {
	var bits []string
	iana := now.Location().String()
	if iana != "Local" && iana != "" {
		bits = append(bits, iana)
	}
	if abbrev := now.Format("MST"); abbrev != "" && abbrev != iana && !strings.HasPrefix(abbrev, "+") && !strings.HasPrefix(abbrev, "-") {
		bits = append(bits, abbrev)
	}
	off := now.Format("-0700")
	bits = append(bits, "UTC"+off[:3]+":"+off[3:])
	return bits
}
