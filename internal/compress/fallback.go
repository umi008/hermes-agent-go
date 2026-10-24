package compress

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

var (
	blockerRE = regexp.MustCompile(`(?i)\b(error|failed|exception|traceback|timeout|timed out|fatal)\b`)
	pathRE    = regexp.MustCompile(`(?:[\w.-]+/)*[\w.-]+\.[A-Za-z0-9]{1,8}`)
)

// fallback is upstream _build_static_fallback_summary: a summary built
// locally from the window when the summary call fails.
func (c *Compressor) fallback(window []llm.Message, reason string) string {
	ask := "None. This session contains no user-authored turns."
	var actions, blockers, paths, dropped []string
	for _, m := range window {
		text := strings.TrimSpace(m.Content)
		switch m.Role {
		case llm.RoleUser:
			if text != "" {
				ask = "User asked: " + pyRepr(truncRunes(text, 500))
			}
		case llm.RoleAssistant:
			if text != "" {
				actions = append(actions, truncRunes(firstLine(text), 160))
			}
			for _, tc := range m.ToolCalls {
				actions = append(actions, digest(tc, ""))
				var args map[string]any
				if json.Unmarshal(tc.Arguments, &args) == nil {
					for _, k := range []string{"path", "file_path", "workdir"} {
						if s, ok := args[k].(string); ok && s != "" {
							paths = append(paths, s)
						}
					}
				}
			}
		case llm.RoleTool:
			if head := truncRunes(text, 500); blockerRE.MatchString(head) {
				blockers = append(blockers, truncRunes(firstLine(head), 200))
			}
		}
		paths = append(paths, pathRE.FindAllString(truncRunes(text, 2000), -1)...)
		if text != "" {
			dropped = append(dropped, strings.ToUpper(string(m.Role))+": "+truncRunes(firstLine(text), 200))
		}
	}
	completed := "None recoverable from compacted turns."
	if len(actions) > 0 {
		lines := make([]string, 0, 12)
		for i, a := range actions[:min(12, len(actions))] {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, a))
		}
		completed = strings.Join(lines, "\n")
	}
	prevNote := ""
	if c.previous != "" {
		prevNote = "\n\n## Previous Summary Snapshot\n" + elideMiddle(c.previous, 2000, 1000) +
			"\n\nThe previous compaction summary above remains background continuity context because the latest LLM summary update failed."
	}
	reasonNote := ""
	if reason != "" {
		reasonNote = " Summary failure reason: " + reason + "."
	}
	body := "## Historical Task Snapshot\n" + ask + "\n\n" +
		"## Goal\nRecovered from a deterministic fallback because the LLM context summarizer was unavailable. " +
		"Continue from the protected recent messages after this summary and use current file/system state for exact details." + prevNote + "\n\n" +
		"## Constraints & Preferences\n- This fallback was generated locally without an LLM summary call.\n" +
		"- Secrets and credentials were redacted before preservation.\n" +
		"- The summary may be incomplete; prefer verifying current files, git state, processes, and test results instead of assuming omitted details.\n\n" +
		"## Completed Actions\n" + completed + "\n\n" +
		"## Active State\nUnknown from deterministic fallback. Inspect current repository/session state if needed.\n\n" +
		"## Blocked\n" + bullets(blockers, 5) + "\n\n" +
		"## Key Decisions\nNone recoverable from deterministic fallback.\n\n" +
		"## Resolved Questions\nNone recoverable from deterministic fallback.\n\n" +
		"## Relevant Files\n" + bullets(paths, 12) + "\n\n" +
		"## Last Dropped Turns\n" + bullets(dropped[max(0, len(dropped)-8):], 8) + "\n\n" +
		"## Critical Context\nSummary generation was unavailable, so this is a best-effort deterministic fallback for " +
		fmt.Sprintf("%d compacted message(s).", len(window)) + reasonNote
	return elideMiddle(body, fallbackMaxChars*2/3, fallbackMaxChars/3-200)
}

// bullets is upstream _bullets: the first limit distinct non-blank items.
func bullets(items []string, limit int) string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" || seen[it] {
			continue
		}
		seen[it] = true
		out = append(out, "- "+it)
		if len(out) == limit {
			break
		}
	}
	if len(out) == 0 {
		return "None."
	}
	return strings.Join(out, "\n")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// pyRepr approximates Python's repr of a str: single quotes unless the
// text holds one and no double quote.
func pyRepr(s string) string {
	q := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = `"`
	}
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`, q, `\`+q)
	return q + r.Replace(s) + q
}
