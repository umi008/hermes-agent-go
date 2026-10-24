package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/umi008/hermes-agent-go/internal/tools"
	"github.com/umi008/hermes-agent-go/internal/tools/files"
)

// Manage is the write half of the skills tools: upstream skill_manage, the
// tool an agent uses to write its own procedural memory. It only ever writes
// inside skills.create_dir when configured, otherwise the profile skills dir,
// and it modifies an existing skill wherever it already lives.
type Manage struct{ c *Catalog }

// Manage returns the write side of the catalog.
func (c *Catalog) Manage() *Manage { return &Manage{c: c} }

// skillNamePattern is upstream's name rule: lowercase words joined by hyphens
// or underscores, at most MaxNameLength characters.
var skillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// operation is one mutation, in the per-action shape upstream advertises.
type operation struct {
	Action      string `json:"action"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Content     string `json:"content"`
	FilePath    string `json:"file_path"`
	FileContent string `json:"file_content"`
	OldString   string `json:"old_string"`
	NewString   string `json:"new_string"`
	ReplaceAll  bool   `json:"replace_all"`
}

// manageArgs accepts both call shapes: the advertised operations array and the
// legacy flat single op, which old transcripts still send.
type manageArgs struct {
	Operations []operation `json:"operations"`
	operation
}

// Run applies one skill_manage call. The ops apply in order against an
// in-memory plan, so nothing touches disk until every op succeeded: a failing
// op rolls the whole batch back by construction.
func (m *Manage) Run(raw json.RawMessage) (Object, error) {
	var a manageArgs
	if err := tools.Decode(raw, &a); err != nil {
		return nil, err
	}
	ops := a.Operations
	if len(ops) == 0 {
		if strings.TrimSpace(a.Action) == "" {
			return nil, tools.Error("skill_manage requires 'operations' (an array of ops)")
		}
		ops = []operation{a.operation}
	}
	for i, op := range ops {
		if op.Action == "delete" && len(ops) > 1 {
			return nil, tools.Error("delete must be the only operation in a call")
		}
		if strings.TrimSpace(op.Action) == "" {
			return nil, tools.Error(fmt.Sprintf("operation %d has no action", i+1))
		}
	}

	p := newPlan()
	results := make([]Object, 0, len(ops))
	for _, op := range ops {
		obj, err := m.apply(p, op)
		if err != nil {
			return nil, err
		}
		results = append(results, obj)
	}
	changed, err := p.flush()
	if err != nil {
		return nil, err
	}
	return Object{{"success", true}, {"operations", len(ops)}, {"results", results}, {"files", changed}}, nil
}

// createRoot is where a new skill lands: skills.create_dir when configured,
// otherwise the profile skills dir.
func (m *Manage) createRoot() string {
	if m.c.o.CreateDir != "" {
		return m.c.o.CreateDir
	}
	return m.c.o.LocalDir
}

func (m *Manage) apply(p *plan, op operation) (Object, error) {
	category, name, err := splitName(op.Name)
	if err != nil {
		return nil, err
	}
	switch op.Action {
	case "create":
		return m.create(p, name, op)
	case "patch":
		return m.patch(p, category, name, op)
	case "write_file":
		return m.writeFile(p, category, name, op)
	case "remove_file":
		return m.removeFile(p, category, name, op)
	case "delete":
		return m.delete(p, category, name)
	default:
		return nil, tools.Error(fmt.Sprintf("Unknown action %q. Use: create, edit, patch, delete, write_file, remove_file", op.Action))
	}
}

// splitName validates a skill name. A leading "category/" is accepted and
// returned separately, as upstream allows for every action but create.
func splitName(raw string) (category, name string, err error) {
	s := strings.TrimSpace(filepath.ToSlash(raw))
	if s == "" {
		return "", "", tools.Error("skill_manage requires 'name'")
	}
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		category, s = s[:i], s[i+1:]
		for _, part := range strings.Split(category, "/") {
			if !skillNamePattern.MatchString(part) {
				return "", "", tools.Error(fmt.Sprintf("invalid skill category %q: use lowercase letters, digits, hyphens or underscores", category))
			}
		}
	}
	switch {
	case s == "":
		return "", "", tools.Error("skill_manage requires 'name'")
	case utf8.RuneCountInString(s) > MaxNameLength:
		return "", "", tools.Error(fmt.Sprintf("skill name is longer than %d characters", MaxNameLength))
	case !skillNamePattern.MatchString(s):
		return "", "", tools.Error(fmt.Sprintf("invalid skill name %q: use lowercase letters, digits, hyphens or underscores", s))
	}
	return category, s, nil
}

// create writes a whole new SKILL.md, optionally under a category directory.
func (m *Manage) create(p *plan, name string, op operation) (Object, error) {
	if strings.TrimSpace(op.Content) == "" {
		return nil, tools.Error("create requires 'content', the full SKILL.md")
	}
	if err := validateSkillMD(name, op.Content); err != nil {
		return nil, err
	}
	category := strings.TrimSpace(filepath.ToSlash(op.Category))
	for _, part := range strings.Split(category, "/") {
		if part != "" && !skillNamePattern.MatchString(part) {
			return nil, tools.Error(fmt.Sprintf("invalid skill category %q: use lowercase letters, digits, hyphens or underscores", category))
		}
	}
	dir := filepath.Join(m.createRoot(), filepath.FromSlash(category), name)
	md := filepath.Join(dir, "SKILL.md")
	_, exists, err := p.read(md)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, tools.Error(fmt.Sprintf("A skill named %q already exists at %s; use patch to change it", name, md))
	}
	p.mkdir(dir)
	p.write(md, op.Content)
	return Object{{"action", "create"}, {"name", name}, {"path", md}}, nil
}

// patch changes one file of an existing skill: a targeted old/new replacement,
// or a whole-file rewrite of SKILL.md when content is given.
func (m *Manage) patch(p *plan, category, name string, op operation) (Object, error) {
	md, dir, err := m.target(category, name)
	if err != nil {
		return nil, err
	}
	abs := md
	if strings.TrimSpace(op.FilePath) != "" {
		if abs, err = supportFile(dir, op.FilePath); err != nil {
			return nil, err
		}
	}
	if op.Content != "" {
		if abs != md {
			return nil, tools.Error("content rewrites SKILL.md; use old_string/new_string, or write_file for a supporting file")
		}
		if err := validateSkillMD(name, op.Content); err != nil {
			return nil, err
		}
		p.write(md, op.Content)
		return Object{{"action", "patch"}, {"name", name}, {"path", md}, {"strategy", "rewrite"}}, nil
	}
	if op.OldString == "" {
		return nil, tools.Error("patch requires 'old_string' and 'new_string', or 'content'")
	}
	if op.OldString == op.NewString {
		return nil, tools.Error("new_string must differ from old_string")
	}
	cur, exists, err := p.read(abs)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, tools.Error("File not found: " + op.FilePath)
	}
	crlf := strings.Contains(cur, "\r\n")
	text := strings.ReplaceAll(cur, "\r\n", "\n")
	oldS := strings.ReplaceAll(op.OldString, "\r\n", "\n")
	newS := strings.ReplaceAll(op.NewString, "\r\n", "\n")
	spans, strategy := files.FindMatches(text, oldS)
	switch {
	case len(spans) == 0:
		return nil, tools.Error("Could not find old_string in " + abs + ". Re-read the file with skill_view and copy the exact text.")
	case len(spans) > 1 && !op.ReplaceAll:
		return nil, tools.Error(fmt.Sprintf("Found %d matches for old_string in %s; include more context to make it unique, or set replace_all=true.", len(spans), abs))
	}
	var b strings.Builder
	last := 0
	for _, s := range spans {
		b.WriteString(text[last:s[0]])
		b.WriteString(newS)
		last = s[1]
	}
	b.WriteString(text[last:])
	out := b.String()
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	p.write(abs, out)
	return Object{{"action", "patch"}, {"name", name}, {"path", abs}, {"strategy", strategy}, {"replacements", len(spans)}}, nil
}

// writeFile writes one supporting file inside the skill's directory.
func (m *Manage) writeFile(p *plan, category, name string, op operation) (Object, error) {
	_, dir, err := m.target(category, name)
	if err != nil {
		return nil, err
	}
	abs, err := supportFile(dir, op.FilePath)
	if err != nil {
		return nil, err
	}
	if op.FileContent == "" {
		return nil, tools.Error("write_file requires 'file_content'")
	}
	p.mkdir(filepath.Dir(abs))
	p.write(abs, op.FileContent)
	return Object{{"action", "write_file"}, {"name", name}, {"path", abs}}, nil
}

// removeFile removes one supporting file from a skill.
func (m *Manage) removeFile(p *plan, category, name string, op operation) (Object, error) {
	_, dir, err := m.target(category, name)
	if err != nil {
		return nil, err
	}
	abs, err := supportFile(dir, op.FilePath)
	if err != nil {
		return nil, err
	}
	_, exists, err := p.read(abs)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, tools.Error("File not found: " + op.FilePath)
	}
	p.remove(abs)
	return Object{{"action", "remove_file"}, {"name", name}, {"path", abs}}, nil
}

// delete removes a whole skill: SKILL.md plus its supporting directories.
func (m *Manage) delete(p *plan, category, name string) (Object, error) {
	md, dir, err := m.target(category, name)
	if err != nil {
		return nil, err
	}
	p.remove(md)
	if dir == "" {
		return Object{{"action", "delete"}, {"name", name}, {"path", md}}, nil
	}
	// The package is the directory: SKILL.md and every supporting file go.
	p.removeTree(dir)
	return Object{{"action", "delete"}, {"name", name}, {"path", dir}}, nil
}

// target resolves an existing skill to its SKILL.md and its directory. dir is
// empty for a flat skill, which is a single markdown file.
func (m *Manage) target(category, name string) (md, dir string, err error) {
	lookups := []string{name}
	if category != "" {
		lookups = append(lookups, category+"/"+name)
	}
	for _, key := range lookups {
		for _, f := range m.c.collect(key, "") {
			if f.dir != "" {
				return f.md, f.dir, nil
			}
		}
	}
	for _, key := range lookups {
		for _, f := range m.c.collect(key, "") {
			if f.dir == "" {
				return f.md, "", nil
			}
		}
	}
	return "", "", tools.Error(fmt.Sprintf("No skill named %q. Use skills_list to see what exists.", name))
}

// supportFile validates a supporting-file path: relative to the skill's own
// directory and under references/, templates/, scripts/ or assets/.
func supportFile(dir, rel string) (string, error) {
	if dir == "" {
		return "", tools.Error("this skill is a single markdown file; it has no supporting files")
	}
	clean := filepath.ToSlash(strings.TrimSpace(rel))
	switch {
	case clean == "":
		return "", tools.Error("file_path is required")
	case strings.HasPrefix(clean, "/"), filepath.IsAbs(clean), strings.Contains(clean, ".."):
		return "", tools.Error("file_path must be relative to the skill directory and must not contain '..'")
	case !supportDirs[strings.Split(clean, "/")[0]]:
		return "", tools.Error("file_path must start with references/, templates/, scripts/ or assets/")
	}
	return filepath.Join(dir, filepath.FromSlash(clean)), nil
}

// validateSkillMD rejects content the catalog could not load: upstream needs
// YAML frontmatter with a description, and the frontmatter name must agree with
// the directory the tool writes it to.
func validateSkillMD(name, content string) error {
	fm, _ := ParseFrontmatter(content)
	if len(fm) == 0 {
		return tools.Error("SKILL.md needs YAML frontmatter with at least name and description")
	}
	desc, _ := get(fm, "description", "").(string)
	if strings.TrimSpace(desc) == "" {
		return tools.Error("SKILL.md frontmatter needs a description")
	}
	if utf8.RuneCountInString(desc) > MaxDescriptionLength {
		return tools.Error(fmt.Sprintf("description is longer than %d characters", MaxDescriptionLength))
	}
	if got, ok := fm["name"].(string); ok && got != "" && got != name {
		return tools.Error(fmt.Sprintf("frontmatter name %q does not match the skill name %q", got, name))
	}
	return nil
}

// plan holds a batch's writes. Nothing reaches disk until flush, so an op that
// fails half way through a batch leaves the skills untouched.
type plan struct {
	files map[string]*string // absolute path -> new content; nil means delete
	dirs  []string
	trees []string // directories removed wholesale, after the files
}

func newPlan() *plan { return &plan{files: map[string]*string{}} }

// read returns the file's content as the batch sees it: a planned write wins
// over what is on disk.
func (p *plan) read(abs string) (string, bool, error) {
	if v, ok := p.files[abs]; ok {
		if v == nil {
			return "", false, nil
		}
		return *v, true, nil
	}
	data, err := os.ReadFile(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, tools.Error(err.Error())
	}
	return string(data), true, nil
}

func (p *plan) write(abs, content string) {
	c := content
	p.files[abs] = &c
}

func (p *plan) remove(abs string) { p.files[abs] = nil }

func (p *plan) mkdir(dir string) { p.dirs = append(p.dirs, dir) }

func (p *plan) removeTree(dir string) { p.trees = append(p.trees, dir) }

// flush writes the batch and returns the paths it changed. When a write fails
// part way, every file already written is restored, so the batch stays atomic.
func (p *plan) flush() ([]string, error) {
	paths := make([]string, 0, len(p.files))
	for abs := range p.files {
		paths = append(paths, abs)
	}
	sort.Strings(paths)
	before, err := snapshot(paths)
	if err != nil {
		return nil, err
	}
	for _, dir := range p.dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, tools.Error(err.Error())
		}
	}
	for _, abs := range paths {
		var err error
		if v := p.files[abs]; v == nil {
			if err = os.Remove(abs); errors.Is(err, fs.ErrNotExist) {
				err = nil
			}
		} else {
			err = writeAtomic(abs, *v)
		}
		if err != nil {
			restore(before)
			return nil, tools.Error(err.Error())
		}
	}
	for _, dir := range p.trees {
		if err := os.RemoveAll(dir); err != nil {
			restore(before)
			return nil, tools.Error(err.Error())
		}
	}
	return paths, nil
}

// fileSnapshot is the state a file had before the batch, for rollback.
type fileSnapshot struct {
	content string
	exists  bool
}

func snapshot(paths []string) (map[string]fileSnapshot, error) {
	out := make(map[string]fileSnapshot, len(paths))
	for _, abs := range paths {
		data, err := os.ReadFile(abs)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			out[abs] = fileSnapshot{}
		case err != nil:
			return nil, tools.Error(err.Error())
		default:
			out[abs] = fileSnapshot{content: string(data), exists: true}
		}
	}
	return out, nil
}

func restore(before map[string]fileSnapshot) {
	for abs, snap := range before {
		if snap.exists {
			_ = writeAtomic(abs, snap.content)
			continue
		}
		_ = os.Remove(abs)
	}
}

// writeAtomic writes through a temporary file in the same directory, so a
// crash never leaves a half-written SKILL.md.
func writeAtomic(abs, content string) error {
	dir := filepath.Dir(abs)
	f, err := os.CreateTemp(dir, ".skill-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, abs)
}

// skillManageSchema is upstream's advertised call shape: one operations array,
// each op its own object, so a model cannot fill another action's text slot.
const skillManageSchema = `{"type":"object","properties":{` +
	`"operations":{"type":"array","description":"Ordered ops; each names its target skill (lowercase, hyphens/underscores, max 64 chars).","items":{"anyOf":[` +
	`{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"},"action":{"type":"string","enum":["create"]},"content":{"type":"string","description":"Full SKILL.md text (YAML frontmatter + markdown body)."},"category":{"type":"string","description":"Optional category subdirectory."}},"required":["name","action","content"]},` +
	`{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"},"action":{"type":"string","enum":["patch"]},"old_string":{"type":"string","description":"Text to find, with the patch tool's matching semantics."},"new_string":{"type":"string","description":"Replacement; an empty string deletes the match."},"replace_all":{"type":"boolean"},"file_path":{"type":"string","description":"Optional supporting file; default SKILL.md."}},"required":["name","action","old_string","new_string"]},` +
	`{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"},"action":{"type":"string","enum":["patch"]},"content":{"type":"string","description":"Full SKILL.md rewrite; replaces the whole file."}},"required":["name","action","content"]},` +
	`{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"},"action":{"type":"string","enum":["write_file"]},"file_path":{"type":"string","description":"Path relative to the skill directory, starting with references/, templates/, scripts/ or assets/."},"file_content":{"type":"string"}},"required":["name","action","file_path","file_content"]},` +
	`{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"},"action":{"type":"string","enum":["remove_file"]},"file_path":{"type":"string"}},"required":["name","action","file_path"]},` +
	`{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"},"action":{"type":"string","enum":["delete"]}},"required":["name","action"]}` +
	`]}},"required":["operations"]}}`

// Tool is upstream skill_manage, as a tools.Tool the CLI registers for
// interactive sessions only.
func (m *Manage) Tool() tools.Tool {
	return tools.Tool{
		Name:    "skill_manage",
		Toolset: Toolset,
		Description: "Create, update or delete skills: your procedural memory for recurring tasks. " +
			"The call is an operations array (a single edit is a list of one); it applies atomically, " +
			"so a failing op leaves every skill untouched. Ops: create (full SKILL.md), " +
			"patch (targeted old_string/new_string fix, preferred; content alone replaces the whole file), " +
			"write_file and remove_file (supporting files under references/, templates/, scripts/ or assets/), " +
			"delete (sole op only). Write lessons, not logs: one rule per lesson, and why it matters.",
		Parameters: json.RawMessage(skillManageSchema),
		Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
			obj, err := m.Run(raw)
			if err != nil {
				return "", err
			}
			return result(obj)
		},
	}
}
