package files

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

const (
	defaultSearchLimit = 50
	maxMatchLineChars  = 500
	maxSearchFileBytes = 10 << 20
)

// skipDirs are never descended into: VCS metadata and dependency caches that
// ripgrep's ignore rules usually hide upstream.
var skipDirs = map[string]bool{".git": true, ".hg": true, ".svn": true, "node_modules": true, "__pycache__": true, ".venv": true}

func (f *Files) searchTool() tools.Tool {
	return tools.Tool{
		Name:    "search_files",
		Toolset: Toolset,
		Description: "Search file contents or find files by name. Use this instead of grep/find/ls in terminal. " +
			"target='content' searches inside files with a regular expression (RE2 syntax) and returns path, line number and line; " +
			"target='files' finds files whose name matches a glob pattern (e.g. '*.go'). " +
			"Use file_glob to restrict content searches, and limit/offset to page through results.",
		Parameters: schema(`{"type":"object","properties":{` +
			`"pattern":{"type":"string","description":"Regex for target='content', or a filename glob for target='files'"},` +
			`"target":{"type":"string","enum":["content","files"],"description":"What to search (default: content)","default":"content"},` +
			`"path":{"type":"string","description":"Directory or file to search (default: current directory)","default":"."},` +
			`"file_glob":{"type":"string","description":"Only search files whose name matches this glob (content target)"},` +
			`"limit":{"type":"integer","description":"Maximum results to return (default: 50)","default":50},` +
			`"offset":{"type":"integer","description":"Skip this many results, for paging (default: 0)","default":0},` +
			`"output_mode":{"type":"string","enum":["content","files_only","count"],"description":"content: matching lines; files_only: matching file paths; count: matches per file (default: content)","default":"content"}` +
			`},"required":["pattern"]}`),
		Handler: f.search,
	}
}

type searchArgs struct {
	Pattern    string `json:"pattern"`
	Target     string `json:"target"`
	Path       string `json:"path"`
	FileGlob   string `json:"file_glob"`
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
	OutputMode string `json:"output_mode"`
}

type match struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Content string `json:"content"`
}

type searchResult struct {
	TotalCount int            `json:"total_count"`
	Matches    []match        `json:"matches,omitempty"`
	Files      []string       `json:"files,omitempty"`
	Counts     map[string]int `json:"counts,omitempty"`
	Truncated  bool           `json:"truncated,omitempty"`
	Hint       string         `json:"_hint,omitempty"`
}

func (f *Files) search(ctx context.Context, raw json.RawMessage) (string, error) {
	var a searchArgs
	if err := tools.Decode(raw, &a); err != nil {
		return "", err
	}
	if a.Pattern == "" {
		return "", tools.Error("search_files requires 'pattern'")
	}
	switch a.Target {
	case "", "grep":
		a.Target = "content"
	case "find":
		a.Target = "files"
	}
	if a.Path == "" {
		a.Path = "."
	}
	if a.Limit <= 0 {
		a.Limit = defaultSearchLimit
	}
	a.Offset = max(a.Offset, 0)
	root := f.resolve(a.Path)
	if _, err := os.Stat(root); err != nil {
		return "", tools.Error("Path not found: " + a.Path)
	}
	display := func(p string) string {
		if rel, err := filepath.Rel(f.Cwd(), p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
		return filepath.ToSlash(p)
	}

	// git's ignore verdict is built once, then applied inside the walk, so
	// paging counts only the results that survived it.
	ignore := gitIgnores(ctx, root)
	var res searchResult
	var err error
	switch a.Target {
	case "files":
		err = findFiles(ctx, root, a, ignore, display, &res)
	case "content":
		err = grep(ctx, root, a, ignore, display, &res)
	default:
		return "", tools.Error(fmt.Sprintf("unknown target %q (use content or files)", a.Target))
	}
	if err != nil {
		return "", err
	}
	if res.Truncated {
		res.Hint = fmt.Sprintf("Results truncated. Use offset=%d to see more, or narrow the pattern or path.", a.Offset+a.Limit)
	}
	return tools.Result(res)
}

// walk visits every regular file under root. ignore may be nil, which walks
// unfiltered. An ignored directory is pruned instead of descended into, so
// nothing under it is even read.
func walk(ctx context.Context, root string, ignore *ignoreSet, visit func(path string) error) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil // unreadable entries are skipped, like ripgrep
		}
		ignored := ignore.ignores(p, d.IsDir())
		if d.IsDir() {
			if p != root && (ignored || skipDirs[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if ignored {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return visit(p)
	})
}

// ignoreSet is the set of paths under root that git reported as ignored. It
// holds slash-separated paths relative to root: files as-is and ignored
// directories with a trailing slash, as git's --directory form reports them.
type ignoreSet struct {
	root  string
	paths map[string]bool
}

// gitIgnores asks git once, before the walk, which paths it would ignore under
// root. Only untracked ignored paths are reported, which is git's own scope:
// ignore rules never apply to tracked files. Anything that keeps git from
// answering, including a plain directory, means no filtering at all, because a
// search must never fail or warn over an optional optimisation.
func gitIgnores(ctx context.Context, root string) *ignoreSet {
	out, err := gitOutput(ctx, root, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return nil
	}
	// --directory is what keeps this cheap: an ignored node_modules is one
	// entry instead of thousands of paths.
	out, err = gitOutput(ctx, root, "ls-files", "--others", "--ignored", "--exclude-standard", "-z", "--directory")
	if err != nil {
		return nil
	}
	set := &ignoreSet{root: root, paths: map[string]bool{}}
	for _, entry := range strings.Split(string(out), "\x00") {
		if entry != "" {
			set.paths[entry] = true
		}
	}
	if len(set.paths) == 0 {
		return nil
	}
	return set
}

// gitOutput runs git against root under the search's context, so a cancelled
// search does not leak a process.
func gitOutput(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	return cmd.Output()
}

// ignores reports whether p is ignored, checked through the path itself and
// every ancestor directory. Ancestors are directories, so they are looked up
// in git's trailing-slash form.
func (s *ignoreSet) ignores(p string, isDir bool) bool {
	if s == nil {
		return false
	}
	rel, err := filepath.Rel(s.root, p)
	if err != nil {
		return false
	}
	rel = path.Clean(filepath.ToSlash(rel))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	for {
		if s.paths[rel] || (isDir && s.paths[rel+"/"]) {
			return true
		}
		parent := path.Dir(rel)
		if parent == rel {
			return false
		}
		rel, isDir = parent, true
	}
}

// inPage reports whether the nth result falls in the [offset, offset+limit) window.
func inPage(n int, a searchArgs) bool { return n > a.Offset && n <= a.Offset+a.Limit }

func findFiles(ctx context.Context, root string, a searchArgs, ignore *ignoreSet, display func(string) string, res *searchResult) error {
	if _, err := filepath.Match(a.Pattern, ""); err != nil {
		return tools.Error("Invalid glob pattern: " + err.Error())
	}
	err := walk(ctx, root, ignore, func(p string) error {
		if ok, _ := filepath.Match(a.Pattern, filepath.Base(p)); ok {
			res.TotalCount++
			if inPage(res.TotalCount, a) {
				res.Files = append(res.Files, display(p))
			}
		}
		return nil
	})
	res.Truncated = res.TotalCount > a.Offset+a.Limit
	return err
}

func grep(ctx context.Context, root string, a searchArgs, ignore *ignoreSet, display func(string) string, res *searchResult) error {
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return tools.Error("Invalid regex: " + err.Error())
	}
	if a.FileGlob != "" {
		if _, err := filepath.Match(a.FileGlob, ""); err != nil {
			return tools.Error("Invalid file_glob: " + err.Error())
		}
	}
	mode := a.OutputMode
	if mode == "count" {
		res.Counts = map[string]int{}
	}
	files := 0
	err = walk(ctx, root, ignore, func(p string) error {
		if a.FileGlob != "" {
			if ok, _ := filepath.Match(a.FileGlob, filepath.Base(p)); !ok {
				return nil
			}
		}
		n, err := grepFile(p, re, func(line int, text string) {
			if mode != "" && mode != "content" {
				return
			}
			res.TotalCount++
			if inPage(res.TotalCount, a) {
				if utf8.RuneCountInString(text) > maxMatchLineChars {
					text = string([]rune(text)[:maxMatchLineChars]) + "..."
				}
				res.Matches = append(res.Matches, match{Path: display(p), Line: line, Content: text})
			}
		})
		if err != nil || n == 0 {
			return nil //nolint:nilerr // unreadable files are skipped, like ripgrep
		}
		switch mode {
		case "files_only":
			files++
			res.TotalCount = files
			if inPage(files, a) {
				res.Files = append(res.Files, display(p))
			}
		case "count":
			res.TotalCount += n
			res.Counts[display(p)] = n
		}
		return nil
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	res.Truncated = mode != "count" && res.TotalCount > a.Offset+a.Limit
	return nil
}

// grepFile reports each matching line and returns the match count. Binary
// and very large files are skipped.
func grepFile(path string, re *regexp.Regexp, onMatch func(line int, text string)) (int, error) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxSearchFileBytes {
		return 0, err
	}
	data, err := os.ReadFile(path)
	if err != nil || isBinary(data) {
		return 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), maxSearchFileBytes)
	n := 0
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSuffix(sc.Text(), "\r")
		if re.MatchString(text) {
			n++
			onMatch(line, text)
		}
	}
	return n, sc.Err()
}
