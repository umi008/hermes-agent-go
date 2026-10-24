package skills

import (
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// OneshotGuidance is upstream ONESHOT_SKILLS_LOAD_GUIDANCE, the index
// header for single-query runs.
const OneshotGuidance = "## Skills\n" +
	"Scan the skills below and load one with skill_view(name) only when it carries domain knowledge you lack " +
	"for THIS task (an API, a tool's commands, a project's conventions). Do not load general process skills " +
	"(testing, debugging, review methodology) for work you already know how to do, and do not create or edit " +
	"skills: this is a one-shot run with no later session to reuse them.\n"

var conditionKeys = []string{"fallback_for_toolsets", "requires_toolsets", "fallback_for_tools", "requires_tools", "session_platforms"}

// IndexOptions say what the session can use; nil sets mean no filtering.
type IndexOptions struct {
	Tools, Toolsets map[string]bool
	// SessionPlatform gates metadata.hermes.session_platforms.
	SessionPlatform string
	// Oneshot selects the single-query header.
	Oneshot bool
}

// shouldShow is upstream _skill_should_show.
func shouldShow(cond map[string][]string, o IndexOptions) bool {
	if wanted := cond["session_platforms"]; len(wanted) > 0 && strings.TrimSpace(o.SessionPlatform) != "" {
		ok := false
		for _, p := range wanted {
			ok = ok || strings.EqualFold(strings.TrimSpace(p), strings.TrimSpace(o.SessionPlatform))
		}
		if !ok {
			return false
		}
	}
	if o.Tools == nil && o.Toolsets == nil {
		return true
	}
	anyIn := func(keys []string, set map[string]bool, want bool) bool {
		for _, k := range keys {
			if set[k] == want {
				return true
			}
		}
		return false
	}
	hidden := anyIn(cond["fallback_for_toolsets"], o.Toolsets, true) ||
		anyIn(cond["fallback_for_tools"], o.Tools, true) ||
		anyIn(cond["requires_toolsets"], o.Toolsets, false) ||
		anyIn(cond["requires_tools"], o.Tools, false)
	return !hidden
}

// promptDescription is upstream extract_skill_description.
func promptDescription(fm map[string]any) string {
	d := ""
	if truthy(fm["description"]) {
		d = strings.Trim(strings.TrimSpace(str(fm["description"])), `'"`)
	}
	if utf8.RuneCountInString(d) > PromptDescLimit {
		return truncRunes(d, PromptDescLimit-3) + "..."
	}
	return d
}

func indexCategory(root, md string) string {
	rel, _ := filepath.Rel(root, md)
	parts := strings.Split(filepath.ToSlash(rel), "/")
	switch {
	case len(parts) < 2:
		return "general"
	case len(parts) > 2:
		return strings.Join(parts[:len(parts)-2], "/")
	}
	return parts[0]
}

func categoryDescriptions(root string) map[string]string {
	out := map[string]string{}
	for _, f := range indexFiles(root, "DESCRIPTION.md") {
		d := get(safeFrontmatter(f), "description", nil)
		if !truthy(d) {
			continue
		}
		rel, _ := filepath.Rel(root, f)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		key := "general"
		if len(parts) > 1 {
			key = strings.Join(parts[:len(parts)-1], "/")
		}
		out[key] = strings.Trim(strings.TrimSpace(str(d)), `'"`)
	}
	return out
}

// Index is upstream build_skills_system_prompt: the "## Skills" block, or
// "" when nothing is listable.
func (c *Catalog) Index(o IndexOptions) string {
	var rows []entry
	descs := map[string]string{}
	for _, r := range c.roots {
		for _, md := range indexFiles(r.Dir, "SKILL.md") {
			fm := map[string]any{}
			if text, err := readText(md); err == nil {
				fm, _ = ParseFrontmatter(text)
			}
			dirName := filepath.Base(filepath.Dir(md))
			cond := map[string][]string{}
			hm := hermesMeta(fm)
			for _, k := range conditionKeys {
				cond[k] = stringList(hm[k])
			}
			cat := indexCategory(r.Dir, md)
			rows = append(rows, entry{
				name: str(get(fm, "name", dirName)), description: promptDescription(fm),
				category: &cat, tier: r.Tier, root: r.Dir, path: md,
				visible: c.offerable(fm) && !c.disabled[dirName] && shouldShow(cond, o),
			})
		}
		if r.Tier != TierLocal {
			for k, v := range categoryDescriptions(r.Dir) {
				if _, ok := descs[k]; !ok {
					descs[k] = v
				}
			}
		}
	}
	for _, r := range c.roots {
		if r.Tier == TierLocal {
			for k, v := range categoryDescriptions(r.Dir) {
				descs[k] = v
			}
		}
	}
	type row struct{ name, desc string }
	byCat := map[string][]row{}
	unloadable := map[string]bool{}
	for _, e := range resolveCatalog(rows, c.o.Logger) {
		if !e.visible || e.status == "shadowed" || c.disabledEntry(e) {
			continue
		}
		if e.loadName == "" {
			unloadable[e.name] = true
			continue
		}
		byCat[*e.category] = append(byCat[*e.category], row{e.loadName, e.description})
	}
	if len(byCat) == 0 {
		return ""
	}
	note := ""
	if len(unloadable) > 0 {
		names := make([]string, 0, len(unloadable))
		for n := range unloadable {
			names = append(names, n)
		}
		sort.Strings(names)
		note = "\n(A copy of " + strings.Join(names, ", ") + " is not listed: it shares both its name and its path " +
			"with a different skill in the same skills directory tier (e.g. another external_dirs entry), " +
			"so skill_view cannot load it — rename one.)"
	}
	cats := make([]string, 0, len(byCat))
	for k := range byCat {
		cats = append(cats, k)
	}
	sort.Strings(cats)
	var lines []string
	for _, cat := range cats {
		if d := descs[cat]; d != "" {
			lines = append(lines, "  "+cat+": "+d)
		} else {
			lines = append(lines, "  "+cat+":")
		}
		entries := byCat[cat]
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
		seen := map[string]bool{}
		for _, r := range entries {
			if seen[r.name] {
				continue
			}
			seen[r.name] = true
			if r.desc != "" {
				lines = append(lines, "    - "+r.name+": "+r.desc)
			} else {
				lines = append(lines, "    - "+r.name)
			}
		}
	}
	list := "<available_skills>\n" + strings.Join(lines, "\n") + "\n</available_skills>"
	if o.Oneshot {
		return OneshotGuidance + "\n" + list + note
	}
	basic := "web_search or terminal"
	if o.Tools != nil && !o.Tools["web_search"] {
		basic = "terminal"
	}
	return "## Skills\n" +
		"Before replying, scan the skills below. If a skill matches or is even partially relevant to your " +
		"task, you MUST load it with skill_view(name) and follow its instructions. Err on the side of " +
		"loading — it is always better to have context you don't need than to miss critical steps, pitfalls, " +
		"or established workflows. Skills contain specialized knowledge — API endpoints, tool-specific " +
		"commands, and proven workflows that outperform general-purpose approaches. Load the skill " +
		"even if you think you could handle the task with basic tools like " + basic + ". " +
		"Skills also encode the user's preferred approach, conventions, and quality standards for tasks like " +
		"code review, planning, and testing — load them even for tasks you already know how to do, because " +
		"the skill defines how it should be done here.\n" +
		"If a skill has issues, fix it with skill_manage(action='patch').\n" +
		"After difficult/iterative tasks, offer to save as a skill. If a skill you loaded was missing steps, " +
		"had wrong commands, or needed pitfalls you discovered, update it before finishing.\n" +
		"\n" + list + "\n\n" +
		"Only proceed without loading a skill if genuinely none are relevant to the task." + note
}
