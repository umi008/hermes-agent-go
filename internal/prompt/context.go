package prompt

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// DefaultMaxFileChars is upstream CONTEXT_FILE_MAX_CHARS.
const DefaultMaxFileChars = 20_000

const (
	truncateHeadRatio = 0.7
	truncateTailRatio = 0.2
	maxOmittedHeads   = 15
)

const contextHeader = "# Project Context\n\nThe following project context files have been loaded and should be followed:\n\n"

// ContextFiles discovers project context starting at cwd. Only one kind
// loads, first found wins: .hermes.md / HERMES.md (nearest, up to the git
// root), the AGENTS.md chain (git root down to cwd), CLAUDE.md (cwd), then
// .cursorrules plus .cursor/rules/*.mdc (cwd).
func ContextFiles(cwd string, t truncator) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	for _, load := range []func(string, truncator) string{loadHermesMD, loadAgentsMD, loadClaudeMD, loadCursorRules} {
		if section := load(abs, t); section != "" {
			return contextHeader + section
		}
	}
	return ""
}

func loadSoul(home string, t truncator) (content, path string) {
	if home == "" {
		return "", ""
	}
	path = filepath.Join(home, "SOUL.md")
	content = readContextFile(path)
	if content == "" {
		return "", ""
	}
	return t.truncate(content, "SOUL.md", path), path
}

func readContextFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(string(data), "\uFEFF"))
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// findGitRoot returns the nearest ancestor (or start) containing .git.
func findGitRoot(start string) string {
	for dir := start; ; {
		if exists(filepath.Join(dir, ".git")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func section(content, label, warnName, path string, t truncator) string {
	return t.truncate("## "+label+"\n\n"+content, warnName, path)
}

func loadHermesMD(cwd string, t truncator) string {
	root := findGitRoot(cwd)
	for dir := cwd; ; {
		for _, name := range []string{".hermes.md", "HERMES.md"} {
			p := filepath.Join(dir, name)
			if !isFile(p) {
				continue
			}
			content := readContextFile(p)
			if content == "" {
				return ""
			}
			label := name
			if rel, err := filepath.Rel(cwd, p); err == nil && !strings.HasPrefix(rel, "..") {
				label = filepath.ToSlash(rel)
			}
			return section(stripFrontmatter(content), label, ".hermes.md", p, t)
		}
		parent := filepath.Dir(dir)
		if root == "" || dir == root || parent == dir {
			return ""
		}
		dir = parent
	}
}

func stripFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---") {
		return content
	}
	end := strings.Index(content[3:], "\n---")
	if end < 0 {
		return content
	}
	body := strings.TrimLeft(content[3+end+4:], "\n")
	if body == "" {
		return content
	}
	return body
}

// agentsChain lists directories from the git root down to cwd.
func agentsChain(cwd string) []string {
	root := findGitRoot(cwd)
	if root == "" || root == cwd {
		return []string{cwd}
	}
	rel, err := filepath.Rel(root, cwd)
	if err != nil || strings.HasPrefix(rel, "..") {
		return []string{cwd}
	}
	chain := []string{root}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		chain = append(chain, cur)
	}
	return chain
}

func loadAgentsMD(cwd string, t truncator) string {
	var sections []string
	seen := map[string]bool{}
	for _, dir := range agentsChain(cwd) {
		for _, name := range []string{"AGENTS.override.md", "AGENTS.md", "agents.md"} {
			p := filepath.Join(dir, name)
			if !exists(p) {
				continue
			}
			content := readContextFile(p)
			if content == "" {
				continue
			}
			if !seen[content] {
				seen[content] = true
				label := name
				if dir != cwd {
					if rel, err := filepath.Rel(cwd, p); err == nil {
						label = filepath.ToSlash(rel)
					}
				}
				sections = append(sections, section(content, label, label, p, t))
			}
			break
		}
	}
	switch len(sections) {
	case 0:
		return ""
	case 1:
		return sections[0]
	}
	return t.truncate(strings.Join(sections, "\n\n"), "AGENTS.md (directory chain)", filepath.Join(cwd, "AGENTS.md"))
}

func loadClaudeMD(cwd string, t truncator) string {
	for _, name := range []string{"CLAUDE.md", "claude.md"} {
		p := filepath.Join(cwd, name)
		if content := readContextFile(p); content != "" {
			return section(content, name, "CLAUDE.md", p, t)
		}
	}
	return ""
}

func loadCursorRules(cwd string, t truncator) string {
	type candidate struct{ label, path string }
	cands := []candidate{{".cursorrules", filepath.Join(cwd, ".cursorrules")}}
	matches, _ := filepath.Glob(filepath.Join(cwd, ".cursor", "rules", "*.mdc"))
	sort.Strings(matches)
	for _, m := range matches {
		cands = append(cands, candidate{".cursor/rules/" + filepath.Base(m), m})
	}
	var b strings.Builder
	for _, c := range cands {
		if content := readContextFile(c.path); content != "" {
			b.WriteString("## " + c.label + "\n\n" + content + "\n\n")
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return t.truncate(b.String(), ".cursorrules", filepath.Join(cwd, ".cursorrules"))
}

type truncator struct {
	max    int
	logger *slog.Logger
}

var headingRE = regexp.MustCompile(`^#{1,6} \S`)

// truncate keeps the head and tail of an over-long file with a marker that
// names the omitted headings, like upstream _truncate_content. Lengths are
// counted in characters, as Python does.
func (t truncator) truncate(content, name, readPath string) string {
	runes := []rune(content)
	if len(runes) <= t.max {
		return content
	}
	if t.logger != nil {
		t.logger.Warn("context file truncated", "file", name, "chars", len(runes), "limit", t.max)
	}
	head := int(float64(t.max) * truncateHeadRatio)
	tail := int(float64(t.max) * truncateTailRatio)
	omitted := omittedHeadings(runes, head, len(runes)-tail)
	sections := ""
	if len(omitted) > 0 {
		sections = " Omitted sections: " + strings.Join(omitted, "; ") + "."
	}
	marker := fmt.Sprintf("\n\n[...truncated %s: kept %d+%d of %d chars. The middle is omitted.%s If you need the full instructions, read the complete file with the read_file tool: %s]\n\n",
		name, head, tail, len(runes), sections, readPath)
	return string(runes[:head]) + marker + string(runes[len(runes)-tail:])
}

func omittedHeadings(runes []rune, start, end int) []string {
	var heads []string
	offset, fenced := 0, false
	for _, line := range strings.SplitAfter(string(runes), "\n") {
		stripped := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(stripped, "```") || strings.HasPrefix(stripped, "~~~"):
			fenced = !fenced
		case !fenced && start <= offset && offset < end && headingRE.MatchString(stripped):
			heads = append(heads, strings.TrimSpace(strings.TrimLeft(stripped, "#")))
		}
		offset += len([]rune(line))
	}
	if len(heads) > maxOmittedHeads {
		heads = append(heads[:maxOmittedHeads], "...")
	}
	return heads
}
