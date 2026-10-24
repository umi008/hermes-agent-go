package skills

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const lookupHint = "Use a skill name or relative path within the skills directory."

var (
	namespaceRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	templateRE  = regexp.MustCompile(`\$\{(HERMES_SKILL_DIR|HERMES_SESSION_ID)\}`)
	envNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	remoteEnvs  = map[string]bool{"docker": true, "modal": true, "ssh": true, "daytona": true, "vercel_sandbox": true}
)

// Object is a JSON object that keeps upstream key order.
type Object []Field

// Field is one key of an Object.
type Field struct {
	K string
	V any
}

func fail(msg string, extra ...Field) Object {
	return append(Object{{"success", false}, {"error", msg}}, extra...)
}

// hasTraversal is upstream has_traversal_component: a literal ".." part,
// splitting on both separators as Windows does.
func hasTraversal(p string) bool {
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

// lookupError is upstream skill_lookup_path_error.
func lookupError(name string) string {
	c := strings.TrimSpace(name)
	drive := len(c) >= 2 && c[1] == ':' && (c[0]|0x20 >= 'a' && c[0]|0x20 <= 'z')
	unc := strings.HasPrefix(c, `\\`) || strings.HasPrefix(c, "//")
	if strings.HasPrefix(c, "/") || drive || unc {
		return "Skill name must be a relative path within the skills directory."
	}
	if hasTraversal(c) {
		return "Skill name cannot contain '..' path traversal components."
	}
	return ""
}

// pySuffix is Python's PurePath.suffix.
func pySuffix(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	return name[i:]
}

// withSuffix is Python's PurePath.with_suffix(".md").
func withMD(p string) string {
	dir, base := filepath.Split(p)
	if s := pySuffix(base); s != "" {
		base = strings.TrimSuffix(base, s)
	}
	return dir + base + ".md"
}

func within(child, root string) bool {
	rel, err := filepath.Rel(root, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// packageOwned is upstream _is_package_owned_markdown.
func packageOwned(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || !within(p, root) {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for depth := 1; depth < len(parts); depth++ {
		if isFile(filepath.Join(append([]string{root}, append(parts[:depth:depth], "SKILL.md")...)...)) {
			return true
		}
	}
	return false
}

type found struct{ dir, md string }

// collect is upstream _collect_skill_candidates.
func (c *Catalog) collect(name, localCategory string) []found {
	var out []found
	seen := map[string]bool{}
	record := func(dir, md string) {
		key := md
		if r, err := filepath.EvalSymlinks(md); err == nil {
			key = r
		}
		if abs, err := filepath.Abs(key); err == nil {
			key = abs
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, found{dir, md})
		}
	}
	for _, r := range c.roots {
		for _, direct := range []string{name, localCategory} {
			if direct == "" {
				continue
			}
			p := filepath.Join(r.Dir, filepath.FromSlash(direct))
			flat := withMD(p)
			switch {
			case !isSupportPath(p) && isDir(p) && isFile(filepath.Join(p, "SKILL.md")):
				record(p, filepath.Join(p, "SKILL.md"))
			case isFile(flat) && !isSupportPath(flat) && !packageOwned(flat, r.Dir):
				record("", flat)
			}
		}
		for _, md := range indexFiles(r.Dir, "SKILL.md") {
			if filepath.Base(filepath.Dir(md)) == name || str(get(safeFrontmatter(md), "name", nil)) == name {
				record(filepath.Dir(md), md)
			}
		}
		suffix := name + ".md"
		var flats []string
		_ = filepath.WalkDir(r.Dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(r.Dir, p)
			rel = filepath.ToSlash(rel)
			if rel == suffix || strings.HasSuffix(rel, "/"+suffix) {
				flats = append(flats, p)
			}
			return nil
		})
		sort.Strings(flats)
		for _, md := range flats {
			if filepath.Base(md) != "SKILL.md" && !isSupportPath(md) && !packageOwned(md, r.Dir) {
				record("", md)
			}
		}
	}
	return out
}

// owningRoot is upstream _owning_search_dir: the most specific root that
// lexically contains p.
func (c *Catalog) owningRoot(p string) (Root, bool) {
	var best Root
	ok := false
	for _, r := range c.roots {
		if within(p, r.Dir) && (!ok || len(splitPath(r.Dir)) > len(splitPath(best.Dir))) {
			best, ok = r, true
		}
	}
	return best, ok
}

// ownedRelative is upstream _owned_relative: the exact load path.
func (c *Catalog) ownedRelative(f found) string {
	target := f.dir
	if target == "" {
		target = strings.TrimSuffix(f.md, pySuffix(f.md))
	}
	r, ok := c.owningRoot(f.md)
	if !ok {
		return target
	}
	rel, _ := filepath.Rel(r.Dir, target)
	return filepath.ToSlash(rel)
}

// locate is upstream locate_skill.
func (c *Catalog) locate(name, localCategory string) (Object, found) {
	if len(c.roots) == 0 {
		return fail("Skills directory does not exist yet. It will be created on first install."), found{}
	}
	cands := c.collect(name, localCategory)
	if len(cands) > 1 {
		ranked := make([]candidate, len(cands))
		for i, f := range cands {
			r, _ := c.owningRoot(f.md)
			ranked[i] = newCandidate(r.Tier, r.Dir, f.md)
		}
		won, contenders := pickCandidate(ranked)
		if won >= 0 {
			cands = []found{cands[won]}
		} else {
			kept := make([]found, 0, len(contenders))
			for _, i := range contenders {
				kept = append(kept, cands[i])
			}
			cands = kept
		}
	}
	if len(cands) > 1 {
		var paths []string
		names := map[string]bool{}
		for _, f := range cands {
			paths = append(paths, f.md)
			if rel := c.ownedRelative(f); rel != name {
				names[rel] = true
			}
		}
		loadNames := make([]string, 0, len(names))
		for n := range names {
			loadNames = append(loadNames, n)
		}
		sort.Strings(loadNames)
		c.o.Logger.Warn("skill name collision", "name", name, "candidates", len(cands))
		msg := "Ambiguous skill name '" + name + "': "
		if len(loadNames) > 0 {
			msg += "use one of " + strings.Join(loadNames, ", ") + ". "
		}
		msg += fmt.Sprintf("%d different skills share this name in the same skills directory tier; refusing to guess.", len(cands))
		return fail(msg, Field{"matches", paths}, Field{"load_names", loadNames},
			Field{"hint", "Pass the exact relative path instead of the bare name, or rename one of the " +
				"colliding skills so each name is unique."}), found{}
	}
	if len(cands) == 0 || !isFile(cands[0].md) {
		all := c.All()
		names := []string{}
		for _, s := range all[:min(20, len(all))] {
			names = append(names, s.Name)
		}
		return fail("Skill '"+name+"' not found.", Field{"available_skills", names},
			Field{"hint", "Use skills_list to see all available skills"}), found{}
	}
	return nil, cands[0]
}

// View is upstream skill_view. sessionID fills ${HERMES_SESSION_ID}.
func (c *Catalog) View(name, filePath, sessionID string) Object {
	if msg := lookupError(name); msg != "" {
		return fail(msg, Field{"hint", lookupHint})
	}
	localCategory := ""
	if ns, bare, ok := strings.Cut(name, ":"); ok {
		// No plugin skills in the Go port: "ns:skill" falls through to the
		// on-disk "ns/skill" form.
		if !namespaceRE.MatchString(ns) {
			return fail(fmt.Sprintf("Invalid namespace '%s' in '%s'. Namespaces must match [a-zA-Z0-9_-]+.", ns, name))
		}
		if bare != "" {
			localCategory = ns + "/" + bare
		}
	}
	if localCategory != "" {
		if msg := lookupError(localCategory); msg != "" {
			return fail(msg, Field{"hint", lookupHint})
		}
	}
	errObj, f := c.locate(name, localCategory)
	if errObj != nil {
		return errObj
	}
	content, err := readText(f.md)
	if err != nil {
		return fail(fmt.Sprintf("Failed to read skill '%s': %v", name, err))
	}
	fm, _ := ParseFrontmatter(content)
	if !platformMatches(c.o.GOOS, fm["platforms"]) {
		return fail(fmt.Sprintf("Skill '%s' is not supported on this platform.", name), Field{"readiness_status", "unsupported"})
	}
	resolved := str(get(fm, "name", filepath.Base(filepath.Dir(f.md))))
	rel := c.ownedRelative(f)
	if (c.disabled[resolved] || c.disabled[rel]) && (c.disabled[resolved] || c.loadsAs(f.md, rel)) {
		return fail(fmt.Sprintf("Skill '%s' is disabled. Enable it with `hermes skills` or inspect the files directly on disk.", resolved))
	}
	if filePath != "" && f.dir != "" {
		return serveFile(f.dir, filePath, name)
	}
	hm := hermesMeta(fm)
	tags := parseTags(orElse(hm["tags"], get(fm, "tags", "")))
	related := parseTags(orElse(hm["related_skills"], get(fm, "related_skills", "")))
	linked := linkedFiles(f.dir)
	relPath := ""
	if r, err := filepath.Rel(c.o.LocalDir, f.md); err == nil && within(f.md, c.o.LocalDir) {
		relPath = r
	} else {
		relPath, _ = filepath.Rel(filepath.Dir(filepath.Dir(f.md)), f.md)
	}
	skillName := filepath.Base(f.dir)
	if f.dir == "" {
		skillName = strings.TrimSuffix(filepath.Base(f.md), pySuffix(f.md))
	}
	skillName = str(get(fm, "name", skillName))
	if c.o.TemplateVars == nil || *c.o.TemplateVars {
		content = substitute(content, f.dir, sessionID)
	}
	var skillDir, linkedOut, usage any
	if f.dir != "" {
		skillDir = f.dir
	}
	if len(linked) > 0 {
		linkedOut = linked
		usage = "To view linked files, call skill_view(name, file_path) where file_path is e.g. 'references/api.md' or 'assets/config.yaml'"
	}
	out := Object{
		{"success", true}, {"name", skillName}, {"description", get(fm, "description", "")},
		{"tags", tags}, {"related_skills", related}, {"content", content},
		{"path", relPath}, {"skill_dir", skillDir}, {"linked_files", linkedOut}, {"usage_hint", usage},
	}
	out = append(out, c.readiness(fm)...)
	if truthy(fm["compatibility"]) {
		out = append(out, Field{"compatibility", fm["compatibility"]})
	}
	if md, ok := fm["metadata"].(map[string]any); ok {
		out = append(out, Field{"metadata", md})
	}
	return out
}

func orElse(a, b any) any {
	if truthy(a) {
		return a
	}
	return b
}

// loadsAs reports whether the catalog row for md loads under rel, which
// is what makes a path-form disabled entry apply to this copy.
func (c *Catalog) loadsAs(md, rel string) bool {
	for _, e := range c.scan(true, true) {
		if e.path == md && e.loadName == rel {
			return true
		}
	}
	return false
}

func substitute(content, dir, sessionID string) string {
	return templateRE.ReplaceAllStringFunc(content, func(m string) string {
		v := dir
		if strings.Contains(m, "SESSION") {
			v = sessionID
		}
		if v == "" {
			return m
		}
		return v
	})
}

// linkedFiles is upstream _skill_linked_files, with key order kept.
func linkedFiles(dir string) Object {
	if dir == "" {
		return nil
	}
	specs := []struct {
		sub       string
		globs     []string
		recursive bool
		filesOnly bool
	}{
		{"references", []string{"*.md"}, false, false},
		{"templates", []string{"*.md", "*.py", "*.yaml", "*.yml", "*.json", "*.tex", "*.sh"}, true, false},
		{"assets", []string{"*"}, true, true},
		{"scripts", []string{"*.py", "*.sh", "*.bash", "*.js", "*.ts", "*.rb"}, false, false},
	}
	var out Object
	for _, s := range specs {
		base := filepath.Join(dir, s.sub)
		if !isDir(base) {
			continue
		}
		var files []string
		for _, g := range s.globs {
			var hits []string
			_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
				if err != nil || p == base {
					return nil
				}
				if d.IsDir() && !s.recursive {
					return filepath.SkipDir
				}
				if ok, _ := filepath.Match(g, d.Name()); ok && (!s.filesOnly || d.Type().IsRegular()) {
					rel, _ := filepath.Rel(dir, p)
					hits = append(hits, filepath.ToSlash(rel))
				}
				return nil
			})
			sort.Strings(hits)
			files = append(files, hits...)
		}
		if len(files) > 0 {
			out = append(out, Field{s.sub, files})
		}
	}
	return out
}

// availableFiles is upstream _available_skill_files.
func availableFiles(dir string) Object {
	groups := map[string][]string{}
	exts := map[string]bool{".md": true, ".py": true, ".yaml": true, ".yml": true, ".json": true, ".tex": true, ".sh": true}
	var all []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && d.Name() != "SKILL.md" {
			all = append(all, p)
		}
		return nil
	})
	sort.Strings(all)
	for _, p := range all {
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		top, _, nested := strings.Cut(rel, "/")
		if !nested {
			top = ""
		}
		if supportDirs[top] {
			groups[top] = append(groups[top], rel)
		} else if exts[pySuffix(filepath.Base(p))] {
			groups["other"] = append(groups["other"], rel)
		}
	}
	var out Object
	for _, k := range []string{"references", "templates", "assets", "scripts", "other"} {
		if g, ok := groups[k]; ok {
			out = append(out, Field{k, g})
		}
	}
	return out
}

// serveFile is upstream _serve_skill_file for a local skill.
func serveFile(dir, filePath, label string) Object {
	hint := Field{"hint", "Use a relative path within the skill directory"}
	if hasTraversal(filePath) {
		return fail("Path traversal ('..') is not allowed.", hint)
	}
	target := filepath.Join(dir, filepath.FromSlash(filePath))
	realTarget, err1 := filepath.EvalSymlinks(target)
	realDir, err2 := filepath.EvalSymlinks(dir)
	if err1 == nil && err2 == nil && !within(realTarget, realDir) {
		return fail("Path escapes allowed directory: "+realTarget+" is not in the subpath of "+realDir, hint)
	}
	if !isFile(target) {
		return fail(fmt.Sprintf("File '%s' not found in skill '%s'.", filePath, label),
			Field{"available_files", availableFiles(dir)}, Field{"hint", "Use one of the available file paths listed above"})
	}
	content, err := readText(target)
	if err != nil {
		return fail(fmt.Sprintf("Failed to read '%s': %v", filePath, err))
	}
	return Object{{"success", true}, {"name", label}, {"file", filePath}, {"content", content}, {"file_type", pySuffix(filepath.Base(target))}}
}

type envVar struct {
	name, prompt, help, requiredFor string
	optional                        bool
}

func (e envVar) object() Object {
	o := Object{{"name", e.name}, {"prompt", e.prompt}}
	if e.help != "" {
		o = append(o, Field{"help", e.help})
	}
	if e.requiredFor != "" {
		o = append(o, Field{"required_for", e.requiredFor})
	}
	if e.optional {
		o = append(o, Field{"optional", true})
	}
	return o
}

func dictList(v any) []any {
	switch x := v.(type) {
	case map[string]any:
		return []any{x}
	case []any:
		return x
	}
	return nil
}

func cleanStr(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func firstStr(vals ...any) any {
	for _, v := range vals {
		if truthy(v) {
			return v
		}
	}
	return nil
}

// requiredEnv is upstream _get_required_environment_variables.
func requiredEnv(fm map[string]any) []envVar {
	setup, _ := fm["setup"].(map[string]any)
	setupHelp := cleanStr(setup["help"])
	var legacy []any
	if pre, ok := fm["prerequisites"].(map[string]any); ok && truthy(pre["env_vars"]) {
		if s, ok := pre["env_vars"].(string); ok {
			legacy = []any{s}
		} else {
			legacy, _ = pre["env_vars"].([]any)
		}
	}
	var entries []map[string]any
	for _, it := range dictList(fm["required_environment_variables"]) {
		switch x := it.(type) {
		case string:
			entries = append(entries, map[string]any{"name": x})
		case map[string]any:
			entries = append(entries, x)
		}
	}
	for _, it := range dictList(setup["collect_secrets"]) {
		if x, ok := it.(map[string]any); ok {
			url := strings.TrimSpace(str(orElse(firstStr(x["provider_url"], x["url"]), "")))
			var u any
			if url != "" {
				u = url
			}
			entries = append(entries, map[string]any{"name": x["env_var"], "prompt": x["prompt"], "url": u})
		}
	}
	for _, v := range legacy {
		if strings.TrimSpace(str(v)) != "" {
			entries = append(entries, map[string]any{"name": str(v)})
		}
	}
	var out []envVar
	seen := map[string]bool{}
	for _, e := range entries {
		name := strings.TrimSpace(str(orElse(firstStr(e["name"], e["env_var"]), "")))
		if name == "" || seen[name] || !envNameRE.MatchString(name) {
			continue
		}
		seen[name] = true
		prompt := "Enter value for " + name
		if truthy(e["prompt"]) {
			prompt = str(e["prompt"])
		}
		help := cleanStr(firstStr(e["help"], e["provider_url"], e["url"]))
		if help == "" {
			help = setupHelp
		}
		out = append(out, envVar{name: name, prompt: strings.TrimSpace(prompt), help: help,
			requiredFor: cleanStr(e["required_for"]), optional: truthy(e["optional"])})
	}
	return out
}

// readiness is upstream _skill_readiness with no secret-capture callback:
// missing variables stay missing.
func (c *Catalog) readiness(fm map[string]any) Object {
	req := requiredEnv(fm)
	objs := make([]Object, 0, len(req))
	remaining := []string{}
	setupHelp := ""
	for _, e := range req {
		objs = append(objs, e.object())
		if setupHelp == "" && e.help != "" {
			setupHelp = e.help
		}
		if v, ok := c.o.LookupEnv(e.name); !e.optional && (!ok || v == "") {
			remaining = append(remaining, e.name)
		}
	}
	needed := len(remaining) > 0
	status := "available"
	if needed {
		status = "setup_needed"
	}
	out := Object{
		{"required_environment_variables", objs}, {"required_commands", []string{}},
		{"missing_required_environment_variables", remaining}, {"missing_credential_files", []string{}},
		{"missing_required_commands", []string{}}, {"setup_needed", needed}, {"setup_skipped", false},
		{"readiness_status", status},
	}
	if setupHelp != "" {
		out = append(out, Field{"setup_help", setupHelp})
	}
	if needed {
		items := make([]string, len(remaining))
		for i, n := range remaining {
			items[i] = "env $" + n
		}
		note := "Setup needed before using this skill: missing " + strings.Join(items, ", ") + "."
		if setupHelp != "" {
			note += " " + setupHelp
		}
		if b := strings.ToLower(strings.TrimSpace(c.o.TerminalBackend)); remoteEnvs[b] {
			note += " " + strings.ToUpper(b) + "-backed skills need these requirements available inside the remote environment as well."
		}
		out = append(out, Field{"setup_note", note})
	}
	return out
}
