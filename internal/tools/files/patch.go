package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

func (f *Files) patchTool() tools.Tool {
	return tools.Tool{
		Name:    "patch",
		Toolset: Toolset,
		Description: "Targeted find-and-replace edits in files. Use this instead of sed/awk in terminal. " +
			"Uses fuzzy matching (exact, then line-trimmed, whitespace-normalized, indentation-flexible, escaped-whitespace, and unicode-normalized) so minor whitespace/indentation/escaping differences won't break it. " +
			"Returns a unified diff. Finds a unique string and replaces it.",
		Parameters: schema(`{"type":"object","properties":{` +
			`"path":{"type":"string","description":"File path to edit."},` +
			`"old_string":{"type":"string","description":"Exact text to find and replace. Must be unique in the file unless replace_all=true. Include surrounding context lines to ensure uniqueness."},` +
			`"new_string":{"type":"string","description":"Changed replacement text; it must differ from old_string. Pass empty string '' to delete the matched text."},` +
			`"replace_all":{"type":"boolean","description":"Replace all occurrences instead of requiring a unique match (default: false)","default":false}` +
			`},"required":["path","old_string","new_string"]}`),
		Handler: f.patch,
	}
}

type patchResult struct {
	Success       bool     `json:"success"`
	Diff          string   `json:"diff"`
	FilesModified []string `json:"files_modified"`
	Strategy      string   `json:"strategy,omitempty"`
	Replacements  int      `json:"replacements"`
}

func (f *Files) patch(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := tools.Decode(raw, &a); err != nil {
		return "", err
	}
	switch {
	case a.Path == "":
		return "", tools.Error("patch requires 'path'")
	case a.OldString == "":
		return "", tools.Error("old_string must not be empty; use write_file to create a file")
	case a.OldString == a.NewString:
		return "", tools.Error("new_string must differ from old_string")
	}
	path := f.resolve(a.Path)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", tools.Error("File not found: " + a.Path)
	}
	if err != nil {
		return "", tools.Error(err.Error())
	}
	before := string(data)
	crlf := strings.Contains(before, "\r\n")
	text := before
	if crlf {
		text = strings.ReplaceAll(before, "\r\n", "\n")
	}
	oldS := strings.ReplaceAll(a.OldString, "\r\n", "\n")
	newS := strings.ReplaceAll(a.NewString, "\r\n", "\n")

	spans, strategy := findMatches(text, oldS)
	switch {
	case len(spans) == 0:
		return "", tools.Error("Could not find old_string in " + a.Path + ". Re-read the file and copy the exact text, including surrounding lines.")
	case len(spans) > 1 && !a.ReplaceAll:
		return "", tools.Error(fmt.Sprintf("Found %d matches for old_string in %s; include more surrounding context to make it unique, or set replace_all=true.", len(spans), a.Path))
	}
	var b strings.Builder
	last := 0
	for _, s := range spans {
		b.WriteString(text[last:s[0]])
		b.WriteString(newS)
		last = s[1]
	}
	b.WriteString(text[last:])
	after := b.String()
	if crlf {
		after = strings.ReplaceAll(after, "\n", "\r\n")
	}
	if err := writeKeepingMode(path, []byte(after)); err != nil {
		return "", tools.Error(err.Error())
	}
	f.remember(path)
	return tools.Result(patchResult{
		Success:       true,
		Diff:          unifiedDiff(a.Path, strings.ReplaceAll(before, "\r\n", "\n"), strings.ReplaceAll(after, "\r\n", "\n")),
		FilesModified: []string{a.Path},
		Strategy:      strategy,
		Replacements:  len(spans),
	})
}

// lineNormalizers are the line-based fuzzy strategies, tried in order after
// an exact substring match fails, as in upstream tools/fuzzy_match.py.
var lineNormalizers = []struct {
	name string
	norm func(string) string
}{
	{"line_trimmed", strings.TrimSpace},
	{"whitespace_normalized", func(s string) string { return strings.Join(strings.Fields(s), " ") }},
	{"indentation_flexible", func(s string) string { return strings.TrimLeftFunc(s, unicode.IsSpace) }},
}

// findMatches returns non-overlapping byte spans of needle in text, and the
// name of the strategy that found them, in upstream's strategy order.
func findMatches(text, needle string) ([][2]int, string) {
	// An empty needle is not a patch: Go's strings.Index would answer 0 forever,
	// so this refuses instead of spinning. Upstream's re.finditer would answer
	// every position, which no caller can use.
	if needle == "" {
		return nil, ""
	}
	if spans := exactMatches(text, needle); len(spans) > 0 {
		return spans, "exact"
	}
	want := strings.Split(strings.TrimSuffix(needle, "\n"), "\n")
	for _, s := range lineNormalizers {
		if spans := lineMatches(text, needle, want, s.norm); len(spans) > 0 {
			return spans, s.name
		}
	}
	if spans := escapeNormalizedMatches(text, needle); len(spans) > 0 {
		return spans, "escape_normalized"
	}
	if spans := unicodeNormalizedMatches(text, needle); len(spans) > 0 {
		return spans, "unicode_normalized"
	}
	return nil, ""
}

// exactMatches returns the byte spans of every non-overlapping occurrence of
// needle in text, as upstream's re.finditer gives for replace_all: overlapping
// spans would corrupt the file when spliced back to back.
func exactMatches(text, needle string) [][2]int {
	n := strings.Count(text, needle)
	if n == 0 {
		return nil
	}
	spans := make([][2]int, 0, n)
	for off := 0; ; {
		i := strings.Index(text[off:], needle)
		if i < 0 {
			return spans
		}
		spans = append(spans, [2]int{off + i, off + i + len(needle)})
		off += i + len(needle)
	}
}

// lineMatches returns the byte spans of needle in text, comparing each line
// under norm. want is the caller's split of the needle, so a strategy can hand
// over an already transformed needle instead of re-deriving it.
func lineMatches(text, needle string, want []string, norm func(string) string) [][2]int {
	lines, starts := splitWithOffsets(text)
	normalized := make([]string, len(want))
	for i, w := range want {
		normalized[i] = norm(w)
	}
	var spans [][2]int
	for i := 0; i+len(want) <= len(lines); {
		if windowMatches(lines[i:i+len(want)], normalized, norm) {
			end := starts[i+len(want)-1] + len(lines[i+len(want)-1])
			if strings.HasSuffix(needle, "\n") && end < len(text) {
				end++ // the needle's own line break
			}
			spans = append(spans, [2]int{starts[i], end})
			i += len(want)
			continue
		}
		i++
	}
	return spans
}

// escapeNormalizedMatches reads a literal \n, \t or \r in the needle as the
// control character it escapes: models sometimes emit the escape for text they
// saw as raw whitespace. A needle with no escape sequence is left to the other
// strategies, which match its literal form instead.
func escapeNormalizedMatches(text, needle string) [][2]int {
	unescaped := strings.ReplaceAll(needle, `\n`, "\n")
	unescaped = strings.ReplaceAll(unescaped, `\t`, "\t")
	unescaped = strings.ReplaceAll(unescaped, `\r`, "\r")
	if unescaped == needle {
		return nil
	}
	return exactMatches(text, unescaped)
}

// typographicFold maps the typographic variants a model retypes as plain ASCII
// onto those ASCII equivalents, as upstream tools/fuzzy_match.py UNICODE_MAP.
// The replacement is a string because a fold can expand: an em dash becomes two
// hyphens, so a folded offset no longer equals the original one.
var typographicFold = map[rune]string{
	'\u201c': `"`, '\u201d': `"`, // smart double quotes
	'\u2018': `'`, '\u2019': `'`, // smart single quotes
	'\u2014': `--`, '\u2013': `-`, // em and en dashes
	'\u2026': `...`, '\u00a0': ` `, // ellipsis and non-breaking space
	'\u2212': `-`, // typographic minus
	// The space separator family (Zs); files using these otherwise match nothing.
	'\u2000': ` `, '\u2001': ` `, '\u2002': ` `, '\u2003': ` `,
	'\u2004': ` `, '\u2005': ` `, '\u2006': ` `, '\u2007': ` `,
	'\u2008': ` `, '\u2009': ` `, '\u200a': ` `, '\u202f': ` `,
	'\u205f': ` `, '\u3000': ` `,
}

// foldTypographic applies typographicFold to every rune of s. Unmapped runes
// are copied byte for byte so invalid UTF-8 keeps its length: the offset map
// built in originalOffsets depends on that length matching exactly.
func foldTypographic(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if repl, ok := typographicFold[r]; ok {
			b.WriteString(repl)
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// unicodeNormalizedMatches retries the exact and line-trimmed matches on both
// sides folded to ASCII. Neither side changing means no typography drift is in
// play, and the earlier strategies already covered the identical text.
func unicodeNormalizedMatches(text, needle string) [][2]int {
	foldedText, foldedNeedle := foldTypographic(text), foldTypographic(needle)
	if foldedText == text && foldedNeedle == needle {
		return nil
	}
	spans := exactMatches(foldedText, foldedNeedle)
	if len(spans) == 0 {
		want := strings.Split(strings.TrimSuffix(foldedNeedle, "\n"), "\n")
		spans = lineMatches(foldedText, foldedNeedle, want, strings.TrimSpace)
	}
	if len(spans) == 0 {
		return nil
	}
	return originalOffsets(text, spans)
}

// originalOffsets maps spans of foldTypographic(text) back to byte offsets in
// text. Indexing folded offsets directly would splice a shifted region whenever
// a fold expanded: the em dash in "a—b" folds to two hyphens, one byte more
// than the original occupies.
func originalOffsets(text string, foldedSpans [][2]int) [][2]int {
	// origToFolded is indexed by original byte offset and records one entry per
	// byte of the rune, so every byte of a multi-byte rune resolves to the same
	// folded position. The trailing sentinel is one past the folded end, as
	// upstream's _build_orig_to_norm_map.
	origToFolded := make([]int, 0, len(text)+1)
	foldedPos := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		for j := 0; j < size; j++ {
			origToFolded = append(origToFolded, foldedPos)
		}
		if repl, ok := typographicFold[r]; ok {
			foldedPos += len(repl)
		} else {
			foldedPos += size
		}
		i += size
	}
	origToFolded = append(origToFolded, foldedPos)

	// foldedToOrig keeps the first original byte folding to each position, so a
	// span starts on the rune it matched rather than inside its expansion.
	foldedToOrig := make(map[int]int, len(origToFolded))
	for orig, folded := range origToFolded[:len(origToFolded)-1] {
		if _, ok := foldedToOrig[folded]; !ok {
			foldedToOrig[folded] = orig
		}
	}
	origLen := len(origToFolded) - 1
	spans := make([][2]int, 0, len(foldedSpans))
	for _, s := range foldedSpans {
		start, ok := foldedToOrig[s[0]]
		if !ok {
			continue // a folded position no original byte maps to: skip, as upstream
		}
		end := start
		for end < origLen && origToFolded[end] < s[1] {
			end++
		}
		spans = append(spans, [2]int{start, end})
	}
	return spans
}

func windowMatches(window, want []string, norm func(string) string) bool {
	for i := range window {
		if norm(window[i]) != want[i] {
			return false
		}
	}
	return true
}

// splitWithOffsets splits on "\n" and records each line's byte offset.
func splitWithOffsets(text string) ([]string, []int) {
	var lines []string
	var starts []int
	for off := 0; off <= len(text); {
		i := strings.IndexByte(text[off:], '\n')
		if i < 0 {
			if off < len(text) {
				lines, starts = append(lines, text[off:]), append(starts, off)
			}
			break
		}
		lines, starts = append(lines, text[off:off+i]), append(starts, off)
		off += i + 1
	}
	return lines, starts
}

const diffContext = 3

// unifiedDiff renders one hunk spanning the changed region (common prefix
// and suffix lines trimmed) with three lines of context.
func unifiedDiff(name, before, after string) string {
	a, b := diffLines(before), diffLines(after)
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	start := max(pre-diffContext, 0)
	endA := min(len(a)-suf+diffContext, len(a))
	endB := min(len(b)-suf+diffContext, len(b))
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n@@ -%s +%s @@\n", name, name, hunkRange(start, endA-start), hunkRange(start, endB-start))
	for _, l := range a[start:pre] {
		out.WriteString(" " + l + "\n")
	}
	for _, l := range a[pre : len(a)-suf] {
		out.WriteString("-" + l + "\n")
	}
	for _, l := range b[pre : len(b)-suf] {
		out.WriteString("+" + l + "\n")
	}
	for _, l := range a[len(a)-suf : endA] {
		out.WriteString(" " + l + "\n")
	}
	return out.String()
}

func diffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if n == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

// FindMatches exposes the patch tool's matching strategies to other tools: an
// exact substring match first, then the line-based normalizers, escaped
// whitespace, and typographic folding. The spans are non-overlapping byte
// offsets into text, and the strategy names which matcher won, so a caller can
// report how a patch landed.
func FindMatches(text, needle string) ([][2]int, string) {
	return findMatches(text, needle)
}
