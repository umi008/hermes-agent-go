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
			"Uses fuzzy matching (exact, then line-trimmed, whitespace-normalized, indentation-flexible, escaped-whitespace, unicode-normalized, and finally similarity-based block-anchor and context-aware) so minor whitespace/indentation/escaping differences won't break it. " +
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

// approximate names the strategies that match by similarity: their windows were
// never quoted by the caller, so replace_all over several of them would rewrite
// text the model never saw. Upstream refuses exactly this.
var approximate = map[string]bool{"block_anchor": true, "context_aware": true}

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
	case strings.TrimSpace(a.OldString) == "":
		// A whitespace-only anchor matches trivially and would mass-replace or
		// report an ambiguity that no context can resolve.
		return "", tools.Error("old_string is only whitespace — provide non-blank text to match. Set it to the exact existing text the replacement should replace (read the file first if unsure). Do not re-send this call unchanged.")
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
	case len(spans) > 1 && a.ReplaceAll && approximate[strategy]:
		// The windows came from similarity, so the caller never quoted them.
		return "", tools.Error(fmt.Sprintf("Found %d approximate matches for old_string in %s via the '%s' strategy; replace_all only applies to exact matches. Provide the precise text (whitespace included) so an exact or line-trimmed match can be made.", len(spans), a.Path, strategy))
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
	if spans := blockAnchorMatches(text, needle); len(spans) > 0 {
		return spans, "block_anchor"
	}
	if spans := contextAwareMatches(text, needle); len(spans) > 0 {
		return spans, "context_aware"
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

// ── Similarity strategies ────────────────────────────────────────────────
//
// Upstream strategies 8 and 9 threshold Python's difflib.SequenceMatcher
// ratio, so the matcher itself is ported rather than approximated with a
// cheaper distance: the two disagree exactly around the floors in use.

// similarity reproduces Python's difflib.SequenceMatcher(None, a, b).ratio():
// 2*M/T, with T = len(a)+len(b) and M the characters covered by difflib's
// greedy longest-matching-block recursion. It is not an LCS ratio —
// "bcbcbcba" against "abbccaac" scores 0.5 here where an LCS scores 0.625 —
// and the 0.50/0.70/0.80 floors sit in that gap. Characters are runes, as
// Python compares code points.
func similarity(a, b string) float64 {
	ar, br := []rune(a), []rune(b)
	total := len(ar) + len(br)
	if total == 0 {
		return 1 // difflib's _calculate_ratio: the empty pair is fully similar
	}
	m := matchingRunes(ar, br)
	if m == 0 {
		return 0
	}
	return 2 * float64(m) / float64(total)
}

// sequenceMatcher is difflib's SequenceMatcher over rune sequences, built only
// as far as ratio needs.
type sequenceMatcher struct {
	a, b      []rune
	b2j       map[rune][]int // rune -> ascending positions in b, junk removed
	junk      map[rune]bool  // runes difflib calls junk, from the autojunk heuristic
	prev, cur []int          // difflib's j2len / newj2len, indexed by b position
}

func newSequenceMatcher(a, b []rune) *sequenceMatcher {
	m := &sequenceMatcher{
		a:    a,
		b:    b,
		b2j:  make(map[rune][]int, len(b)),
		junk: make(map[rune]bool),
		prev: make([]int, len(b)),
		cur:  make([]int, len(b)),
	}
	for j, r := range b {
		m.b2j[r] = append(m.b2j[r], j)
	}
	// difflib's autojunk heuristic: on a sequence of 200 or more elements, one
	// occupying more than 1% of it is "popular" and cannot start a match (it can
	// still be absorbed at a block's edges). Python's default is on, so leaving
	// it out would score long repetitive lines higher than upstream and let them
	// clear these floors more easily.
	if len(b) >= 200 {
		popular := len(b)/100 + 1
		for r, positions := range m.b2j {
			if len(positions) > popular {
				m.junk[r] = true
				delete(m.b2j, r)
			}
		}
	}
	return m
}

// matchingRunes is difflib's M: the sum of get_matching_blocks sizes. The
// sorted, adjacency-collapsed block list is not built because ratio only needs
// the sum, and the blocks are disjoint by construction.
func matchingRunes(a, b []rune) int {
	m := newSequenceMatcher(a, b)
	type region struct{ alo, ahi, blo, bhi int }
	// difflib keeps its recursion on a LIFO queue. The order decides when a block
	// is found, not which blocks exist: every region holds disjoint subranges.
	queue := []region{{0, len(a), 0, len(b)}}
	total := 0
	for len(queue) > 0 {
		r := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		i, j, size := m.findLongestMatch(r.alo, r.ahi, r.blo, r.bhi)
		if size == 0 {
			// difflib stops here: a region with no matching block is finished,
			// and requeueing it would spin on the same empty answer forever.
			continue
		}
		total += size
		if r.alo < i && r.blo < j {
			queue = append(queue, region{r.alo, i, r.blo, j})
		}
		if i+size < r.ahi && j+size < r.bhi {
			queue = append(queue, region{i + size, r.ahi, j + size, r.bhi})
		}
	}
	return total
}

// findLongestMatch is difflib's find_longest_match: the longest block free of
// junk and popular runes, extended over both ends — first over non-junk, then
// over junk, so a popular rune survives only at an edge.
func (m *sequenceMatcher) findLongestMatch(alo, ahi, blo, bhi int) (int, int, int) {
	besti, bestj, bestsize := alo, blo, 0
	prev, cur := m.prev[:bhi], m.cur[:bhi]
	clear(prev)
	for i := alo; i < ahi; i++ {
		clear(cur)
		for _, j := range m.b2j[m.a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := 1
			if j > 0 {
				k += prev[j-1]
			}
			cur[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		prev, cur = cur, prev
	}
	a, b := m.a, m.b
	for besti > alo && bestj > blo && !m.junk[b[bestj-1]] && a[besti-1] == b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && !m.junk[b[bestj+bestsize]] && a[besti+bestsize] == b[bestj+bestsize] {
		bestsize++
	}
	for besti > alo && bestj > blo && m.junk[b[bestj-1]] && a[besti-1] == b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && m.junk[b[bestj+bestsize]] && a[besti+bestsize] == b[bestj+bestsize] {
		bestsize++
	}
	return besti, bestj, bestsize
}

// blockAnchorMatches is upstream strategy 8: the pattern's first and last lines
// are literal anchors on the typographically folded content, and the lines
// between them only have to be similar enough. A lone candidate gets the 0.50
// floor; several need 0.70 so a repeated anchor pair cannot drag the edit into
// the wrong block.
func blockAnchorMatches(text, needle string) [][2]int {
	foldedPattern := foldTypographic(needle)
	patternLines := strings.Split(foldedPattern, "\n")
	n := len(patternLines)
	if n < 2 {
		return nil
	}
	firstLine := strings.TrimSpace(patternLines[0])
	lastLine := strings.TrimSpace(patternLines[n-1])

	// Lines are compared folded but measured on the original text: a fold can
	// expand a character (an em dash becomes two hyphens), so the two splits only
	// share line numbers, never offsets.
	lines, starts := splitPythonLines(text)
	foldedLines, _ := splitPythonLines(foldTypographic(text))
	potential := make([]bool, len(lines))
	candidates := 0
	for i := 0; i+n <= len(lines) && i+n <= len(foldedLines); i++ {
		if strings.TrimSpace(foldedLines[i]) == firstLine && strings.TrimSpace(foldedLines[i+n-1]) == lastLine {
			potential[i] = true
			candidates++
		}
	}
	if candidates == 0 {
		return nil
	}
	threshold := 0.70
	if candidates == 1 {
		threshold = 0.50
	}
	middle := strings.Join(patternLines[1:n-1], "\n")
	accept := func(i int) bool {
		if !potential[i] {
			return false
		}
		if n <= 2 {
			return true
		}
		return similarity(strings.Join(foldedLines[i+1:i+n-1], "\n"), middle) >= threshold
	}
	return windowSpans(text, lines, starts, n, accept)
}

// contextAwareFloor is upstream's per-line similarity floor for strategy 9.
const contextAwareFloor = 0.80

// contextAwareMatches is upstream strategy 9, the last resort: no line has to
// match exactly, but every line of the block must be at least 0.80 similar to
// the corresponding pattern line. The boundary lines are checked twice because
// they anchor the block; a blank pattern line needs no counterpart, as in
// upstream.
func contextAwareMatches(text, needle string) [][2]int {
	patternLines := strings.Split(needle, "\n")
	n := len(patternLines)
	lines, starts := splitPythonLines(text)
	if n > len(lines) {
		return nil
	}
	firstLine := strings.TrimSpace(patternLines[0])
	lastLine := strings.TrimSpace(patternLines[n-1])
	accept := func(i int) bool {
		block := lines[i : i+n]
		if similarity(firstLine, strings.TrimSpace(block[0])) < contextAwareFloor {
			return false
		}
		if similarity(lastLine, strings.TrimSpace(block[n-1])) < contextAwareFloor {
			return false
		}
		for k, p := range patternLines {
			if strings.TrimSpace(p) == "" {
				continue
			}
			if similarity(strings.TrimSpace(p), strings.TrimSpace(block[k])) < contextAwareFloor {
				return false
			}
		}
		return true
	}
	return windowSpans(text, lines, starts, n, accept)
}

// windowSpans returns the byte span of every window of n consecutive lines that
// accept approves, as upstream _window_spans: the span runs from the first byte
// of line i to the last byte of line i+n-1, so the next line's break survives
// the splice.
func windowSpans(text string, lines []string, starts []int, n int, accept func(int) bool) [][2]int {
	if n <= 0 {
		return nil
	}
	var spans [][2]int
	for i := 0; i+n <= len(lines); i++ {
		if !accept(i) {
			continue
		}
		end := starts[i+n-1] + len(lines[i+n-1])
		spans = append(spans, [2]int{starts[i], min(end, len(text))})
	}
	return spans
}

// splitPythonLines splits s on "\n" the way Python's str.split does, keeping the
// empty line a trailing newline produces, and records each line's byte offset.
// The two similarity strategies count windows in Python's lines, so a pattern
// ending in a newline keeps the empty last line upstream sees; splitWithOffsets
// drops it, which is right for the normalizers that extend a match over the
// needle's own line break instead.
func splitPythonLines(s string) ([]string, []int) {
	lines := strings.Split(s, "\n")
	starts := make([]int, len(lines))
	for i, off := 0, 0; i < len(lines); i++ {
		starts[i] = off
		off += len(lines[i]) + 1
	}
	return lines, starts
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
// whitespace, typographic folding, and finally the two similarity strategies
// (block_anchor, context_aware). The spans are non-overlapping byte
// offsets into text, and the strategy names which matcher won, so a caller can
// report how a patch landed.
func FindMatches(text, needle string) ([][2]int, string) {
	return findMatches(text, needle)
}
