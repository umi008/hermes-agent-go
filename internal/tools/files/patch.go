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

	"github.com/umi008/hermes-agent-go/internal/tools"
)

func (f *Files) patchTool() tools.Tool {
	return tools.Tool{
		Name:    "patch",
		Toolset: Toolset,
		Description: "Targeted find-and-replace edits in files. Use this instead of sed/awk in terminal. " +
			"Uses fuzzy matching (exact, then line-trimmed, whitespace-normalized, and indentation-flexible) so minor whitespace/indentation differences won't break it. " +
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

// findMatches returns non-overlapping byte spans of needle in text.
func findMatches(text, needle string) ([][2]int, string) {
	if n := strings.Count(text, needle); n > 0 {
		spans := make([][2]int, 0, n)
		for off := 0; ; {
			i := strings.Index(text[off:], needle)
			if i < 0 {
				return spans, "exact"
			}
			spans = append(spans, [2]int{off + i, off + i + len(needle)})
			off += i + len(needle)
		}
	}
	lines, starts := splitWithOffsets(text)
	want := strings.Split(strings.TrimSuffix(needle, "\n"), "\n")
	for _, s := range lineNormalizers {
		norm := make([]string, len(want))
		for i, w := range want {
			norm[i] = s.norm(w)
		}
		var spans [][2]int
		for i := 0; i+len(want) <= len(lines); {
			if windowMatches(lines[i:i+len(want)], norm, s.norm) {
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
		if len(spans) > 0 {
			return spans, s.name
		}
	}
	return nil, ""
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
