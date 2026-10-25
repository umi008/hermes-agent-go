package files

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/tools"
)

func setup(t *testing.T) (*tools.Registry, string) {
	t.Helper()
	dir := t.TempDir()
	f := New(func() string { return dir }, filepath.Join(dir, "home"))
	r, err := tools.NewRegistry(f.Tools()...)
	if err != nil {
		t.Fatal(err)
	}
	return r, dir
}

func put(t *testing.T, p, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, r *tools.Registry, name string, args any) (map[string]any, string) {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, err := r.Execute(context.Background(), llm.ToolCall{Name: name, Arguments: raw})
	if err != nil {
		var obj map[string]any
		if json.Unmarshal([]byte(err.Error()), &obj) != nil {
			t.Fatalf("error is not JSON: %v", err)
		}
		return obj, obj["error"].(string)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("result is not JSON: %s", out)
	}
	return obj, ""
}

func TestReadFile(t *testing.T) {
	r, dir := setup(t)
	put(t, filepath.Join(dir, "a.txt"), "one\r\ntwo\nthree\n")
	tests := []struct {
		name    string
		args    map[string]any
		content string
		hint    string
		errSub  string
	}{
		{"whole file", map[string]any{"path": "a.txt"}, "1|one\n2|two\n3|three", "", ""},
		{"page", map[string]any{"path": "a.txt", "offset": 2, "limit": 1}, "2|two", "Use offset=3 to continue reading (showing 2-2 of 3 lines)", ""},
		{"beyond end", map[string]any{"path": "a.txt", "offset": 9}, "", "", "beyond the end"},
		{"missing", map[string]any{"path": "b.txt"}, "", "", "File not found: b.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj, errText := call(t, r, "read_file", tt.args)
			if tt.errSub != "" {
				if !strings.Contains(errText, tt.errSub) {
					t.Fatalf("error = %q", errText)
				}
				return
			}
			if errText != "" || obj["content"] != tt.content || (tt.hint != "" && obj["hint"] != tt.hint) {
				t.Fatalf("got %v (err %q)", obj, errText)
			}
		})
	}
	obj, _ := call(t, r, "read_file", map[string]any{"path": "b.txt"})
	if obj["not_found"] != true {
		t.Errorf("not_found missing: %v", obj)
	}
}

func TestReadFileBudgetAndLongLines(t *testing.T) {
	r, dir := setup(t)
	long := strings.Repeat("x", 2500)
	put(t, filepath.Join(dir, "long.txt"), long+"\n")
	obj, _ := call(t, r, "read_file", map[string]any{"path": "long.txt"})
	if obj["content"] != "1|"+strings.Repeat("x", 2000)+"... [truncated]" {
		t.Fatalf("long line not cut")
	}
	var b strings.Builder
	for range 100 {
		b.WriteString(strings.Repeat("y", 1990) + "\n")
	}
	put(t, filepath.Join(dir, "big.txt"), b.String())
	obj, _ = call(t, r, "read_file", map[string]any{"path": "big.txt"})
	if obj["truncated_by"] != "bytes" || obj["next_offset"] != float64(51) {
		t.Fatalf("budget not applied: next_offset=%v by=%v", obj["next_offset"], obj["truncated_by"])
	}
	put(t, filepath.Join(dir, "bin"), "a\x00b")
	if _, e := call(t, r, "read_file", map[string]any{"path": "bin"}); !strings.Contains(e, "binary") {
		t.Fatalf("binary: %q", e)
	}
}

func TestWriteFileGuard(t *testing.T) {
	r, dir := setup(t)
	p := filepath.Join(dir, "sub", "new.txt")
	obj, e := call(t, r, "write_file", map[string]any{"path": "sub/new.txt", "content": "hi"})
	if e != "" || obj["dirs_created"] != true || obj["verified"] != true {
		t.Fatalf("create: %v %q", obj, e)
	}
	// Our own write counts as current knowledge.
	if _, e := call(t, r, "write_file", map[string]any{"path": "sub/new.txt", "content": "hi2"}); e != "" {
		t.Fatalf("rewrite own file: %q", e)
	}
	put(t, filepath.Join(dir, "other.txt"), "theirs")
	if _, e := call(t, r, "write_file", map[string]any{"path": "other.txt", "content": "x"}); !strings.Contains(e, "Refusing to overwrite") {
		t.Fatalf("unread overwrite allowed: %q", e)
	}
	call(t, r, "read_file", map[string]any{"path": "other.txt"})
	if _, e := call(t, r, "write_file", map[string]any{"path": "other.txt", "content": "x"}); e != "" {
		t.Fatalf("read then write refused: %q", e)
	}
	// An outside change after the read makes the knowledge stale.
	put(t, p, "changed by someone else, longer")
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(p, future, future)
	if _, e := call(t, r, "write_file", map[string]any{"path": "sub/new.txt", "content": "x"}); !strings.Contains(e, "Refusing") {
		t.Fatalf("stale overwrite allowed: %q", e)
	}
	if _, e := call(t, r, "write_file", map[string]any{"path": "x.txt"}); !strings.Contains(e, "requires 'content'") {
		t.Fatalf("missing content: %q", e)
	}
}

func TestPatch(t *testing.T) {
	src := "func a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 1\n}\n"
	tests := []struct {
		name, old, new string
		all            bool
		want, errSub   string
		strategy       string
	}{
		{"exact unique", "func a() {\n\treturn 1", "func a() {\n\treturn 2", false, "func a() {\n\treturn 2\n}\n\nfunc b() {\n\treturn 1\n}\n", "", "exact"},
		{"ambiguous", "return 1", "return 2", false, "", "Found 2 matches", ""},
		{"replace all", "return 1", "return 2", true, "func a() {\n\treturn 2\n}\n\nfunc b() {\n\treturn 2\n}\n", "", "exact"},
		{"indentation fuzzy", "func b() {\n    return 1\n}\n", "func b() {\n\treturn 3\n}\n", false, "func a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 3\n}\n", "", "line_trimmed"},
		{"escaped newline and tab", "func a() {\n\\treturn 1", "func a() {\n\treturn 9", false, "func a() {\n\treturn 9\n}\n\nfunc b() {\n\treturn 1\n}\n", "", "escape_normalized"},
		{"not found", "nope", "x", false, "", "Could not find", ""},
		{"same", "a", "a", false, "", "must differ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, dir := setup(t)
			p := filepath.Join(dir, "x.go")
			put(t, p, src)
			obj, e := call(t, r, "patch", map[string]any{"path": "x.go", "old_string": tt.old, "new_string": tt.new, "replace_all": tt.all})
			if tt.errSub != "" {
				if !strings.Contains(e, tt.errSub) {
					t.Fatalf("error = %q", e)
				}
				return
			}
			got, _ := os.ReadFile(p)
			if e != "" || string(got) != tt.want || obj["strategy"] != tt.strategy {
				t.Fatalf("got %q strategy=%v err=%q", got, obj["strategy"], e)
			}
		})
	}
}

func TestPatchKeepsCRLFAndDiff(t *testing.T) {
	r, dir := setup(t)
	p := filepath.Join(dir, "w.txt")
	put(t, p, "a\r\nb\r\nc\r\n")
	obj, e := call(t, r, "patch", map[string]any{"path": "w.txt", "old_string": "b", "new_string": "B"})
	if e != "" {
		t.Fatal(e)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "a\r\nB\r\nc\r\n" {
		t.Fatalf("got %q", got)
	}
	want := "--- a/w.txt\n+++ b/w.txt\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"
	if obj["diff"] != want {
		t.Fatalf("diff = %q", obj["diff"])
	}
}

func TestSearchFiles(t *testing.T) {
	r, dir := setup(t)
	put(t, filepath.Join(dir, "a.go"), "package a\nfunc Foo() {}\n")
	put(t, filepath.Join(dir, "sub", "b.go"), "package b\nfunc FooBar() {}\nfunc Baz() {}\n")
	put(t, filepath.Join(dir, "sub", "c.txt"), "Foo in text\n")
	put(t, filepath.Join(dir, ".git", "x.go"), "func Foo() {}\n")

	obj, e := call(t, r, "search_files", map[string]any{"pattern": `func Foo`, "file_glob": "*.go"})
	if e != "" || obj["total_count"] != float64(2) {
		t.Fatalf("content: %v %q", obj, e)
	}
	first := obj["matches"].([]any)[0].(map[string]any)
	if first["path"] != "a.go" || first["line"] != float64(2) {
		t.Fatalf("first match = %v", first)
	}
	obj, _ = call(t, r, "search_files", map[string]any{"pattern": "*.go", "target": "files"})
	if fmt.Sprint(obj["files"]) != "[a.go sub/b.go]" {
		t.Fatalf("files = %v", obj["files"])
	}
	obj, _ = call(t, r, "search_files", map[string]any{"pattern": "Foo", "limit": 1})
	if obj["truncated"] != true || !strings.Contains(obj["_hint"].(string), "offset=1") || len(obj["matches"].([]any)) != 1 {
		t.Fatalf("paging: %v", obj)
	}
	obj, _ = call(t, r, "search_files", map[string]any{"pattern": "func", "output_mode": "count"})
	if c := obj["counts"].(map[string]any); c["sub/b.go"] != float64(2) || obj["total_count"] != float64(3) {
		t.Fatalf("count: %v", obj)
	}
	obj, _ = call(t, r, "search_files", map[string]any{"pattern": "Foo", "output_mode": "files_only"})
	if fmt.Sprint(obj["files"]) != "[a.go sub/b.go sub/c.txt]" {
		t.Fatalf("files_only: %v", obj["files"])
	}
	if _, e := call(t, r, "search_files", map[string]any{"pattern": "("}); !strings.Contains(e, "Invalid regex") {
		t.Fatalf("bad regex: %q", e)
	}
}

// TestPatchUnicodeNormalized pins strategy 7: typographic Unicode in the file
// matches the ASCII the model typed, and the span lands on the original text
// even when a character expands (an em dash normalizes to two hyphens).
func TestPatchUnicodeNormalized(t *testing.T) {
	r, dir := setup(t)
	p := filepath.Join(dir, "u.txt")
	put(t, p, "x := \u201ca\u2014b\u201d\ny := 2\n")
	obj, e := call(t, r, "patch", map[string]any{"path": "u.txt", "old_string": `x := "a--b"`, "new_string": `x := "a-b"`})
	if e != "" {
		t.Fatalf("error = %q", e)
	}
	if obj["strategy"] != "unicode_normalized" {
		t.Fatalf("strategy = %v", obj["strategy"])
	}
	got, _ := os.ReadFile(p)
	// The replacement is the caller's text verbatim, and the untouched line
	// keeps its typography.
	if want := "x := \"a-b\"\ny := 2\n"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestFindMatchesEmptyNeedle pins the guard around the exported matcher: an
// empty needle answers no match instead of looping, which the tool layer also
// refuses before it gets here.
func TestFindMatchesEmptyNeedle(t *testing.T) {
	if spans, strategy := FindMatches("abc", ""); len(spans) != 0 || strategy != "" {
		t.Fatalf("spans = %v, strategy = %q", spans, strategy)
	}
}

// TestSimilarity pins Python's difflib.SequenceMatcher.ratio(), which the two
// similarity strategies threshold against. The first cases are pairs where its
// greedy matching gives a lower ratio than an optimal LCS, so a naive port
// disagrees here and drifts across the 0.5/0.7/0.8 thresholds.
func TestSimilarity(t *testing.T) {
	tests := []struct {
		a, b string
		want float64
	}{
		{"bcbcbcba", "abbccaac", 0.5},
		{"abacab", "bbaabb", 0.5},
		{"ccbcbac", "caaacca", 0.4286},
		{"return 1 + 1", "return 1 + 2", 0.9167},
		{"x := compute(alpha, gamma)", "x := compute(alpha, beta)", 0.8627},
		{"startup() {", "start() {", 0.9},
		{"abc", "abc", 1},
		{"", "", 1},
		{"abc", "", 0},
	}
	for _, tt := range tests {
		if got := similarity(tt.a, tt.b); math.Abs(got-tt.want) > 5e-4 {
			t.Errorf("similarity(%q, %q) = %.4f, want %.4f", tt.a, tt.b, got, tt.want)
		}
	}
}

// TestPatchBlockAnchor pins strategy 8: the first and last lines are identical
// anchors, and a single candidate makes the middle similarity threshold 0.50.
func TestPatchBlockAnchor(t *testing.T) {
	r, dir := setup(t)
	p := filepath.Join(dir, "b.go")
	put(t, p, "func a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 1 + 1\n}\n")
	obj, e := call(t, r, "patch", map[string]any{
		"path":       "b.go",
		"old_string": "func b() {\n\treturn 1 + 2\n}",
		"new_string": "func b() {\n\treturn 1 + 3\n}",
	})
	if e != "" {
		t.Fatalf("error = %q", e)
	}
	if obj["strategy"] != "block_anchor" {
		t.Fatalf("strategy = %v", obj["strategy"])
	}
	got, _ := os.ReadFile(p)
	if want := "func a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 1 + 3\n}\n"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestPatchContextAware pins strategy 9: no exact anchors, every non-blank line
// at least 0.80 similar, which is what lets a drifted block still land.
func TestPatchContextAware(t *testing.T) {
	r, dir := setup(t)
	p := filepath.Join(dir, "c.go")
	put(t, p, "start() {\n\tx := compute(alpha, beta)\n\tdone()\n}\n")
	obj, e := call(t, r, "patch", map[string]any{
		"path":       "c.go",
		"old_string": "startup() {\n\tx := compute(alpha, gamma)\n\tdone();\n}",
		"new_string": "startup() {\n\tx := compute(alpha, delta)\n\tdone();\n}",
	})
	if e != "" {
		t.Fatalf("error = %q", e)
	}
	if obj["strategy"] != "context_aware" {
		t.Fatalf("strategy = %v", obj["strategy"])
	}
	got, _ := os.ReadFile(p)
	if want := "startup() {\n\tx := compute(alpha, delta)\n\tdone();\n}\n"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestPatchGuards pins upstream's two refusals that keep an approximate match
// from rewriting more than the caller meant: a whitespace-only anchor matches
// trivially, and replace_all over similarity windows rewrites text nobody
// quoted.
func TestPatchGuards(t *testing.T) {
	r, dir := setup(t)
	p := filepath.Join(dir, "g.go")
	src := "func a() {\n\treturn 1 + 1\n}\nfunc a() {\n\treturn 1 + 2\n}\n"
	put(t, p, src)

	if _, e := call(t, r, "patch", map[string]any{"path": "g.go", "old_string": "   \n\t", "new_string": "x"}); !strings.Contains(e, "only whitespace") {
		t.Errorf("blank anchor error = %q", e)
	}
	// Two windows share the block's anchors, so both are approximate matches.
	_, e := call(t, r, "patch", map[string]any{
		"path": "g.go", "old_string": "func a() {\n\treturn 1 + 3\n}", "new_string": "func a() {\n\treturn 9\n}",
		"replace_all": true,
	})
	if !strings.Contains(e, "approximate") {
		t.Fatalf("replace_all over fuzzy matches = %q", e)
	}
	if got, _ := os.ReadFile(p); string(got) != src {
		t.Fatalf("file changed: %q", got)
	}
	// Without replace_all the ambiguity is reported as before.
	if _, e := call(t, r, "patch", map[string]any{
		"path": "g.go", "old_string": "func a() {\n\treturn 1 + 3\n}", "new_string": "func a() {\n\treturn 9\n}",
	}); !strings.Contains(e, "Found 2 matches") {
		t.Errorf("ambiguous error = %q", e)
	}
}

// TestSearchFilesRespectsGitignore pins the ignore contract: inside a worktree
// the search skips what git ignores, for both targets, and paging counts only
// what survived. Upstream asks git rather than reimplementing the rules.
func TestSearchFilesRespectsGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r, dir := setup(t)
	put(t, filepath.Join(dir, ".gitignore"), "ignored.txt\nbuild/\n")
	put(t, filepath.Join(dir, "keep.go"), "package keep\n// needle\n")
	put(t, filepath.Join(dir, "ignored.txt"), "needle\n")
	put(t, filepath.Join(dir, "build", "x.go"), "package x\n// needle\n")
	// Untracked files are enough: the ignore verdict does not need a commit.
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init failed: %v (%s)", err, out)
	}

	obj, e := call(t, r, "search_files", map[string]any{"pattern": "needle"})
	if e != "" {
		t.Fatalf("error = %q", e)
	}
	if obj["total_count"] != float64(1) {
		t.Fatalf("matches = %v", obj)
	}
	if first := obj["matches"].([]any)[0].(map[string]any); first["path"] != "keep.go" {
		t.Fatalf("match = %v", first)
	}
	obj, _ = call(t, r, "search_files", map[string]any{"pattern": "*.go", "target": "files"})
	if fmt.Sprint(obj["files"]) != "[keep.go]" {
		t.Fatalf("files = %v", obj["files"])
	}
}
