package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

// MarshalJSON writes the fields in order without HTML escaping, like
// upstream json.dumps(ensure_ascii=False).
func (o Object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := encode(&b, f.K); err != nil {
			return nil, err
		}
		b.WriteByte(':')
		if err := encode(&b, f.V); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func encode(b *bytes.Buffer, v any) error {
	enc := json.NewEncoder(b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	b.Truncate(b.Len() - 1) // Encode appends a newline
	return nil
}

func result(o Object) (string, error) {
	var b bytes.Buffer
	if err := encode(&b, o); err != nil {
		return "", err
	}
	return b.String(), nil
}

// List is upstream skills_list.
func (c *Catalog) List(category string) Object {
	all := c.All()
	if len(all) == 0 {
		return Object{{"success", true}, {"skills", []Summary{}}, {"categories", []string{}},
			{"message", "No skills found in skills/ directory."}}
	}
	if category != "" {
		kept := []Summary{}
		for _, s := range all {
			if s.Category != nil && *s.Category == category {
				kept = append(kept, s)
			}
		}
		all = kept
	}
	seen := map[string]bool{}
	cats := []string{}
	for _, s := range all {
		if s.Category != nil && *s.Category != "" && !seen[*s.Category] {
			seen[*s.Category] = true
			cats = append(cats, *s.Category)
		}
	}
	sort.Strings(cats)
	return Object{{"success", true}, {"skills", all}, {"categories", cats}, {"count", len(all)},
		{"hint", "Use skill_view(name) to see full content, tags, and linked files"}}
}

// Toolset is the upstream toolset of the skills tools.
const Toolset = "skills"

// Tools returns skills_list and skill_view. sessionID fills
// ${HERMES_SESSION_ID} in loaded skills.
func Tools(c *Catalog, sessionID func() string) []tools.Tool {
	if sessionID == nil {
		sessionID = func() string { return "" }
	}
	return []tools.Tool{
		{
			Name:        "skills_list",
			Toolset:     Toolset,
			Description: "List available skills (name + description). Use skill_view(name) to load full content.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"category":{"type":"string",` +
				`"description":"Optional category filter to narrow results"}},"required":[]}`),
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Category string `json:"category"`
				}
				if err := tools.Decode(raw, &a); err != nil {
					return "", err
				}
				return result(c.List(a.Category))
			},
		},
		{
			Name:    "skill_view",
			Toolset: Toolset,
			Description: "Load a skill's SKILL.md, or one of its linked references, templates or scripts. " +
				"The first call returns SKILL.md plus a 'linked_files' dict; call again with file_path to read one.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string",` +
				`"description":"The skill name (use skills_list to see available skills). For plugin-provided skills, ` +
				`use the qualified form 'plugin:skill' (e.g. 'superpowers:writing-plans')."},` +
				`"file_path":{"type":"string","description":"OPTIONAL: Path to a linked file within the skill ` +
				`(e.g., 'references/api.md', 'templates/config.yaml', 'scripts/validate.py'). Omit to get the main SKILL.md content."}},` +
				`"required":["name"]}`),
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Name     string `json:"name"`
					FilePath string `json:"file_path"`
				}
				if err := tools.Decode(raw, &a); err != nil {
					return "", err
				}
				return result(c.View(a.Name, a.FilePath, sessionID()))
			},
		},
	}
}
