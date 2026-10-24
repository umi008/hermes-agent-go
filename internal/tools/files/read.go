package files

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

const (
	defaultReadLimit = 2000
	maxLineChars     = 2000
	readCharBudget   = 100_000
	binarySniffBytes = 8000
)

func (f *Files) readTool() tools.Tool {
	return tools.Tool{
		Name:    "read_file",
		Toolset: Toolset,
		Description: "Read a text file with line numbers and pagination. Use this instead of cat/head/tail in terminal. " +
			"Output format: 'LINE_NUM|CONTENT'. Suggests similar filenames if not found. Use offset and limit for large files. " +
			"Reads exceeding ~100K characters are truncated on a line boundary and return a next_offset; continue with offset to read the rest. " +
			"Cannot read images/binary.",
		Parameters: schema(`{"type":"object","properties":{` +
			`"path":{"type":"string","description":"Path to the file to read (absolute, relative, or ~/path)"},` +
			`"offset":{"type":"integer","description":"Line number to start reading from (1-indexed, default: 1)","default":1,"minimum":1},` +
			`"limit":{"type":"integer","description":"Maximum number of lines to read (default: 2000, max: 2000). Reads are additionally capped at a ~100K-character budget with a next_offset continuation.","default":2000,"maximum":2000}` +
			`},"required":["path"]}`),
		Handler:        f.read,
		MaxResultChars: tools.Unlimited,
	}
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

type readResult struct {
	Content    string `json:"content"`
	TotalLines int    `json:"total_lines"`
	FileSize   int64  `json:"file_size"`
	Truncated  bool   `json:"truncated"`
	TruncBy    string `json:"truncated_by,omitempty"`
	NextOffset int    `json:"next_offset,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

func (f *Files) read(_ context.Context, raw json.RawMessage) (string, error) {
	var a readArgs
	if err := tools.Decode(raw, &a); err != nil {
		return "", err
	}
	if a.Path == "" {
		return "", tools.Error("path is required")
	}
	if a.Offset < 1 {
		a.Offset = 1
	}
	if a.Limit <= 0 || a.Limit > defaultReadLimit {
		a.Limit = defaultReadLimit
	}
	path := f.resolve(a.Path)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		extra := map[string]any{"not_found": true}
		if similar := similarFiles(path); len(similar) > 0 {
			extra["similar_files"] = similar
		}
		return "", tools.ErrorWith("File not found: "+a.Path, extra)
	}
	if err != nil {
		return "", tools.Error(err.Error())
	}
	if info.IsDir() {
		return "", tools.Error(a.Path + " is a directory; use search_files to list it")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", tools.Error(err.Error())
	}
	if isBinary(data) {
		return "", tools.ErrorWith("Cannot read binary file: "+a.Path, map[string]any{"is_binary": true})
	}

	lines := splitLines(string(data))
	total := len(lines)
	if total > 0 && a.Offset > total {
		return "", tools.Error(fmt.Sprintf("offset %d is beyond the end of the file (%d lines)", a.Offset, total))
	}
	res := readResult{TotalLines: total, FileSize: info.Size()}
	var b strings.Builder
	chars, end := 0, a.Offset-1
	for i := a.Offset - 1; i < total && i < a.Offset-1+a.Limit; i++ {
		line := lines[i]
		if utf8.RuneCountInString(line) > maxLineChars {
			line = string([]rune(line)[:maxLineChars]) + "... [truncated]"
		}
		entry := strconv.Itoa(i+1) + "|" + line
		n := utf8.RuneCountInString(entry)
		if chars > 0 && chars+1+n > readCharBudget {
			res.Truncated, res.TruncBy, res.NextOffset = true, "bytes", i+1
			res.Hint = fmt.Sprintf("Output truncated at the 100,000-char read budget after %d line(s) (showing lines %d-%d of %d). Use offset=%d to continue.",
				i-a.Offset+1, a.Offset, i, total, i+1)
			break
		}
		if chars > 0 {
			b.WriteByte('\n')
			chars++
		}
		b.WriteString(entry)
		chars += n
		end = i + 1
	}
	res.Content = b.String()
	if !res.Truncated && end < total {
		res.Truncated = true
		res.Hint = fmt.Sprintf("Use offset=%d to continue reading (showing %d-%d of %d lines)", end+1, a.Offset, end, total)
	}
	if a.Offset == 1 && !res.Truncated {
		f.remember(path)
	}
	return tools.Result(res)
}

// splitLines drops one trailing newline so a final "\n" adds no phantom line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), binarySniffBytes)], 0) >= 0
}

// similarFiles suggests up to five names in the same directory that share
// the missing file's stem, case-insensitively.
func similarFiles(path string) []string {
	dir, base := filepath.Split(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	stem := strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))
	var out []string
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if stem != "" && (strings.Contains(name, stem) || strings.Contains(stem, strings.TrimSuffix(name, filepath.Ext(name)))) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}
