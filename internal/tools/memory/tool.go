package memory

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

//go:embed schema.json
var schemaJSON []byte

// Guidance is upstream build_memory_guidance(True, True,
// skill_manage_available=False), the system prompt text shown when the
// memory tool is loaded.
const Guidance = "You have persistent memory, carried across sessions and loaded " +
	"into each new session's context; the memory tool's schema defines what belongs there. " +
	"Task-specific knowledge — procedures, pitfalls, and the user's preferences " +
	"and corrections for that kind of work — belongs in skills, not in memory, " +
	"even when skill writing is unavailable. " +
	"Memory is the narrow exception for facts that apply to EVERY " +
	"session regardless of task (who the user is, environment facts, " +
	"standing conventions with no task home); it has a hard character " +
	"budget, so when it fills, replace or consolidate stale entries " +
	"rather than skipping the save. Write entries as declarative facts, " +
	"not instructions to yourself: 'User prefers concise responses' ✓ — " +
	"'Always respond concisely' ✗ (imperative phrasing gets re-read as " +
	"a directive in later sessions and can override the user's current " +
	"request). A fact stale within a week belongs in session history; " +
	"procedures and workflows belong in skills."

const replaceRemoved = "'replace' is not available; use 'patch'. old_text is the exact span to change and " +
	"content is its new text; the rest of the entry is kept. To rewrite a whole entry, " +
	"set old_text to the whole entry. Nothing was written."

// Tool returns the memory tool bound to s.
func (s *Store) Tool() tools.Tool {
	var schema struct {
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		panic(fmt.Sprintf("memory: embedded schema: %v", err))
	}
	return tools.Tool{
		Name:        "memory",
		Toolset:     "memory",
		Description: schema.Description,
		Parameters:  schema.Parameters,
		Handler:     s.handle,
	}
}

func invalid(msg string) error { return tools.ErrorWith(msg, map[string]any{"success": false}) }

func encode(r result) (string, error) {
	data, err := json.Marshal(r)
	return string(data), err
}

func (s *Store) handle(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Action     string          `json:"action"`
		Target     *string         `json:"target"`
		Content    string          `json:"content"`
		OldText    string          `json:"old_text"`
		NewText    string          `json:"new_text"`
		Operations json.RawMessage `json:"operations"`
	}
	if err := tools.Decode(raw, &a); err != nil {
		return "", err
	}
	if a.Content == "" && a.NewText != "" {
		a.Content = a.NewText
	}
	var ops []Op
	if o := strings.TrimSpace(string(a.Operations)); o != "" && o != "null" {
		if err := json.Unmarshal(a.Operations, &ops); err != nil {
			return "", invalid("operations must be a list of {action, content?, old_text?} objects.")
		}
	}
	if r := targetError(a.Target, ops); r != nil {
		return encode(r)
	}
	target := *a.Target
	if len(ops) > 0 {
		for _, op := range ops {
			if op.Action == "replace" {
				return "", invalid(replaceRemoved)
			}
		}
		return encode(s.batch(target, ops))
	}
	switch a.Action {
	case "":
		return "", invalid("Missing required parameter: provide 'action' (add/patch/remove) " +
			"or 'operations' (batch list). Got neither.")
	case "replace":
		return "", invalid(replaceRemoved)
	case "add":
		if a.Content == "" {
			return "", invalid("Content is required for 'add' action.")
		}
		return encode(s.add(target, a.Content))
	case "patch", "remove":
		if a.OldText == "" {
			s.mu.Lock()
			r := fail(fmt.Sprintf("'%s' needs old_text -- a short unique substring of the entry "+
				"to %s. None was provided. Reissue the %s with old_text "+
				"set to part of one of the current_entries below.", a.Action, a.Action, a.Action),
				kv{"current_entries", nonNil(s.entries[target])}, kv{"usage", s.usage(target)})
			s.mu.Unlock()
			return encode(r)
		}
		if a.Action == "remove" {
			return encode(s.remove(target, a.OldText))
		}
		if a.Content == "" {
			return "", invalid("content (the new text for the old_text span) is required for 'patch' action. " + patchDropHint)
		}
		return encode(s.patch(target, a.OldText, a.Content))
	}
	return "", invalid(fmt.Sprintf("Unknown action '%s'. Use: add, patch, remove", a.Action))
}

func targetError(target *string, ops []Op) result {
	if target == nil {
		return fail("Missing required target. Retry with target='memory' or target='user' at the top level. " +
			"For a batch, remove target from every operation; use separate calls for different stores. " +
			"Nothing was written or staged.")
	}
	if *target != "memory" && *target != "user" {
		return fail(fmt.Sprintf("Invalid memory target '%s'. Use 'memory' or 'user'.", *target))
	}
	for i, op := range ops {
		if op.Target != nil && op.Target != *target {
			return fail(fmt.Sprintf("Operation %d names a different target than the one at the top level. "+
				"One call writes one store: retry with separate calls for different stores. "+
				"Nothing was written or staged.", i+1))
		}
	}
	return nil
}
