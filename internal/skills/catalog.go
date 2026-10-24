// Package skills ports upstream's skill discovery (agent/skill_utils.py,
// tools/skills_tool.py): SKILL.md packages under ranked roots, the
// skills_list and skill_view tools, and the skills index in the prompt.
package skills

import (
	"bytes"
	"crypto/sha256"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"
)

// Root tiers in precedence order: the lowest tier wins a name.
const (
	TierProject = iota
	TierLocal
	TierCreateDir
	TierExternal
)

// Upstream progressive-disclosure limits.
const (
	MaxNameLength        = 64
	MaxDescriptionLength = 1024
	PromptDescLimit      = 60
)

var (
	excludedDirs = map[string]bool{
		".git": true, ".github": true, ".hub": true, ".archive": true, ".curator_backups": true, ".locks": true,
		".venv": true, "venv": true, "node_modules": true, "site-packages": true, "__pycache__": true,
		".tox": true, ".nox": true, ".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true,
	}
	supportDirs = map[string]bool{"references": true, "templates": true, "assets": true, "scripts": true}
	platformMap = map[string]string{"macos": "darwin", "linux": "linux", "windows": "win32"}
)

// Root is one skills directory and its tier.
type Root struct {
	Tier int
	Dir  string
}

// Options configure a Catalog.
type Options struct {
	// LocalDir is the profile skills dir, <hermes>/skills.
	LocalDir string
	// CreateDir is skills.create_dir, where skill_manage writes; it joins
	// the roots only when it exists.
	CreateDir string
	// ExternalDirs are skills.external_dirs, already expanded; missing ones
	// are skipped.
	ExternalDirs []string
	// Disabled is skills.disabled.
	Disabled []string
	// GOOS defaults to runtime.GOOS.
	GOOS string
	// LookupEnv finds a skill's required environment variables.
	LookupEnv func(string) (string, bool)
	// TerminalBackend names the terminal backend for the setup note.
	TerminalBackend string
	// TemplateVars is skills.template_vars; nil means true.
	TemplateVars *bool
	Logger       *slog.Logger
}

// Catalog reads skills from its roots. It holds no cache: every call
// rescans, so skills written mid-session show up at once.
type Catalog struct {
	o        Options
	roots    []Root
	disabled map[string]bool
}

// New resolves the roots in upstream order: local, create_dir, external.
func New(o Options) *Catalog {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.LookupEnv == nil {
		o.LookupEnv = func(string) (string, bool) { return "", false }
	}
	c := &Catalog{o: o, disabled: map[string]bool{}}
	for _, d := range o.Disabled {
		c.disabled[d] = true
	}
	seen := map[string]bool{}
	add := func(t int, d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		c.roots = append(c.roots, Root{t, d})
	}
	if isDir(o.LocalDir) {
		add(TierLocal, o.LocalDir)
	}
	if o.CreateDir != "" && o.CreateDir != o.LocalDir && isDir(o.CreateDir) {
		add(TierCreateDir, o.CreateDir)
	}
	for _, d := range o.ExternalDirs {
		if d != o.LocalDir && isDir(d) {
			add(TierExternal, d)
		}
	}
	return c
}

// Roots returns the search roots in precedence order.
func (c *Catalog) Roots() []Root { return append([]Root(nil), c.roots...) }

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// indexFiles is upstream iter_skill_index_files: every file named name
// under dir, sorted, pruning excluded dirs and the support dirs of a skill.
func indexFiles(dir, name string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && (excludedDirs[d.Name()] || supportDirs[d.Name()] && isFile(filepath.Join(filepath.Dir(p), "SKILL.md"))) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == name {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// isSupportPath is upstream is_skill_support_path for an absolute path: a
// component below the first that is a support dir sitting in a skill.
func isSupportPath(p string) bool {
	parts := splitPath(p)
	for i := 1; i < len(parts)-1; i++ {
		if supportDirs[parts[i]] && isFile(filepath.Join(joinParts(parts[:i]), "SKILL.md")) {
			return true
		}
	}
	return false
}

func splitPath(p string) []string {
	p = filepath.Clean(p)
	vol := filepath.VolumeName(p)
	rest := strings.TrimPrefix(p[len(vol):], string(filepath.Separator))
	parts := []string{vol + string(filepath.Separator)}
	if rest != "" {
		parts = append(parts, strings.Split(rest, string(filepath.Separator))...)
	}
	return parts
}

func joinParts(parts []string) string { return filepath.Join(parts...) }

func platformMatches(goos string, platforms any) bool {
	list := stringList(platforms)
	if !truthy(platforms) {
		return true
	}
	current := goos
	if goos == "windows" {
		current = "win32"
	}
	for _, p := range list {
		n := strings.ToLower(strings.TrimSpace(p))
		if m, ok := platformMap[n]; ok {
			n = m
		}
		if strings.HasPrefix(current, n) {
			return true
		}
	}
	return false
}

// offerable is upstream's platform, environment and apps gates. Unknown
// environments fail open; requires_apps fails closed, since the Go port
// has no app declarations.
func (c *Catalog) offerable(fm map[string]any) bool {
	return platformMatches(c.o.GOOS, fm["platforms"]) && !truthy(fm["requires_apps"])
}

type entry struct {
	name, description string
	category          *string
	tier              int
	root, path        string
	visible           bool
	// resolved by resolveCatalog
	relative, status, loadName string
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func truncateDescription(s string) string {
	if utf8.RuneCountInString(s) <= MaxDescriptionLength {
		return s
	}
	return truncRunes(s, MaxDescriptionLength-3) + "..."
}

// categoryFromPath is upstream _get_category_from_path: the first
// component under the local or an external dir, for paths three deep.
func (c *Catalog) categoryFromPath(skillMD string) *string {
	dirs := []string{c.o.LocalDir}
	for _, r := range c.roots {
		if r.Tier == TierExternal {
			dirs = append(dirs, r.Dir)
		}
	}
	for _, d := range dirs {
		rel, err := filepath.Rel(d, skillMD)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		if parts := strings.Split(filepath.ToSlash(rel), "/"); len(parts) >= 3 {
			return &parts[0]
		}
	}
	return nil
}

// scan is upstream _skill_catalog: every SKILL.md resolved, hidden rows
// kept only with includeHidden, disabled rows dropped unless skipDisabled.
func (c *Catalog) scan(skipDisabled, includeHidden bool) []entry {
	var scanned []entry
	for _, r := range c.roots {
		for _, md := range indexFiles(r.Dir, "SKILL.md") {
			if hasExcludedPart(md) {
				continue
			}
			text, err := readText(md)
			if err != nil {
				continue
			}
			fm, body := ParseFrontmatter(truncRunes(text, 4000))
			desc := get(fm, "description", "")
			if !truthy(desc) {
				for _, ln := range strings.Split(strings.TrimSpace(body), "\n") {
					if ln = strings.TrimSpace(ln); ln != "" && !strings.HasPrefix(ln, "#") {
						desc = ln
						break
					}
				}
			}
			scanned = append(scanned, entry{
				name:        truncRunes(str(get(fm, "name", filepath.Base(filepath.Dir(md)))), MaxNameLength),
				description: truncateDescription(str(desc)),
				category:    c.categoryFromPath(md),
				tier:        r.Tier, root: r.Dir, path: md,
				visible: c.offerable(fm),
			})
		}
	}
	var out []entry
	for _, e := range resolveCatalog(scanned, c.o.Logger) {
		if (e.visible || includeHidden) && (skipDisabled || !c.disabledEntry(e)) {
			out = append(out, e)
		}
	}
	return out
}

func hasExcludedPart(p string) bool {
	for _, part := range splitPath(p) {
		if excludedDirs[part] {
			return true
		}
	}
	return false
}

// disabledEntry is upstream is_disabled_entry: the declared name or the
// load name is disabled.
func (c *Catalog) disabledEntry(e entry) bool {
	return c.disabled[e.name] || e.loadName != "" && c.disabled[e.loadName]
}

type candidate struct {
	tier    int
	root    string
	flat    bool
	depth   int
	skillMD string
}

// rankLess is upstream skill_candidate_rank ordering: a real SKILL.md
// beats a flat .md, then the shallower path wins.
func rankLess(a, b candidate) bool {
	if a.flat != b.flat {
		return !a.flat
	}
	return a.depth < b.depth
}

func newCandidate(tier int, root, skillMD string) candidate {
	rel, _ := filepath.Rel(root, skillMD)
	return candidate{tier: tier, root: root, flat: filepath.Base(skillMD) != "SKILL.md",
		depth: len(strings.Split(filepath.ToSlash(rel), "/")), skillMD: skillMD}
}

// provablySame is upstream provably_same_skill: one resolved file or
// byte-identical copies.
func provablySame(paths []string) bool {
	resolved := map[string]bool{}
	for _, p := range paths {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			return false
		}
		resolved[r] = true
	}
	if len(resolved) == 1 {
		return true
	}
	var first []byte
	for i, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return false
		}
		sum := sha256.Sum256(data)
		if i == 0 {
			first = sum[:]
		} else if !bytes.Equal(first, sum[:]) {
			return false
		}
	}
	return true
}

// pickCandidate is upstream pick_skill_candidate: the winner index (-1
// when ambiguous) and the contenders of the winning tier.
func pickCandidate(cands []candidate) (int, []int) {
	top := cands[0].tier
	for _, c := range cands {
		top = min(top, c.tier)
	}
	var contenders []int
	for i, c := range cands {
		if c.tier == top {
			contenders = append(contenders, i)
		}
	}
	if len(contenders) > 1 {
		sameRoot := true
		paths := make([]string, 0, len(contenders))
		for _, i := range contenders {
			sameRoot = sameRoot && cands[i].root == cands[contenders[0]].root
			paths = append(paths, cands[i].skillMD)
		}
		if sameRoot && provablySame(paths) {
			ranked := append([]int(nil), contenders...)
			sort.SliceStable(ranked, func(a, b int) bool { return rankLess(cands[ranked[a]], cands[ranked[b]]) })
			if rankLess(cands[ranked[0]], cands[ranked[1]]) {
				return ranked[0], contenders
			}
		}
	}
	if len(contenders) == 1 {
		return contenders[0], contenders
	}
	return -1, contenders
}

// resolveCatalog is upstream resolve_skill_catalog: each entry gets its
// relative path, status (unique, ambiguous, shadowed) and load name.
func resolveCatalog(entries []entry, logger *slog.Logger) []entry {
	out := append([]entry(nil), entries...)
	owners := map[string][]int{}
	var order []string
	for i := range out {
		dir := filepath.Dir(out[i].path)
		rel, _ := filepath.Rel(out[i].root, dir)
		out[i].relative = filepath.ToSlash(rel)
		aliases := map[string]bool{out[i].name: true, filepath.Base(dir): true, out[i].relative: true}
		for a := range aliases {
			if _, ok := owners[a]; !ok {
				order = append(order, a)
			}
			owners[a] = append(owners[a], i)
		}
	}
	winner := map[string]int{}
	for _, alias := range order {
		idxs := owners[alias]
		cands := make([]candidate, len(idxs))
		for k, j := range idxs {
			cands[k] = newCandidate(out[j].tier, out[j].root, out[j].path)
		}
		won, _ := pickCandidate(cands)
		winner[alias] = -1
		if won >= 0 {
			winner[alias] = idxs[won]
		}
	}
	for i := range out {
		e := &out[i]
		var higher []int
		for _, j := range owners[e.name] {
			if out[j].tier < e.tier {
				higher = append(higher, j)
			}
		}
		w := winner[e.name]
		switch {
		case len(higher) > 0 || w != -1 && w != i:
			e.status, e.loadName = "shadowed", ""
			if len(higher) > 0 {
				same := false
				for _, j := range higher {
					same = same || provablySame([]string{e.path, out[j].path})
				}
				if !same {
					logger.Warn("skill is shadowed by a higher-precedence copy", "name", e.name, "path", e.path)
				}
			}
		case w == i:
			e.status, e.loadName = "unique", e.name
		default:
			e.status, e.loadName = "ambiguous", ""
			if winner[e.relative] == i {
				e.loadName = e.relative
			}
		}
	}
	return out
}

// Summary is one skills_list row.
type Summary struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Category    *string `json:"category"`
}

// All is upstream find_all_skills sorted by sort_skills: every loadable,
// visible, enabled skill.
func (c *Catalog) All() []Summary {
	var out []Summary
	for _, e := range c.scan(false, false) {
		if e.loadName != "" {
			out = append(out, Summary{e.loadName, e.description, e.category})
		}
	}
	sortSummaries(out)
	return out
}

func sortSummaries(s []Summary) {
	cat := func(x Summary) string {
		if x.Category == nil {
			return ""
		}
		return *x.Category
	}
	sort.SliceStable(s, func(i, j int) bool {
		if ci, cj := cat(s[i]), cat(s[j]); ci != cj {
			return ci < cj
		}
		return s[i].Name < s[j].Name
	})
}
