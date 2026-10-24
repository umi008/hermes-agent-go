package approval

import (
	"slices"
	"strings"
)

// A small quote-aware shell scanner, a subset of upstream
// _scan_shell/_iter_shell_command_starts. It classifies every byte as code or
// quoted data and records the offsets where a new command begins.

type frameKind int

const (
	frameDouble frameKind = iota // inside "..."
	frameSubst                   // inside $(...)
	frameTick                    // inside `...`
)

type frame struct {
	kind  frameKind
	depth int
}

type scan struct {
	quoted []bool // byte is literal data inside single or double quotes
	starts []int  // offsets where a command word may begin
}

var shellKeywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "do": true,
	"while": true, "until": true, "!": true, "time": true,
}

func scanShell(s string) scan {
	sc := scan{quoted: make([]bool, len(s))}
	var stack []frame
	top := func() *frame {
		if len(stack) == 0 {
			return nil
		}
		return &stack[len(stack)-1]
	}
	mark := func(at int) { sc.starts = append(sc.starts, at) }
	for i := 0; i < len(s); i++ {
		c := s[i]
		if f := top(); f != nil && f.kind == frameDouble {
			switch {
			case c == '"':
				stack = stack[:len(stack)-1]
			case c == '\\' && i+1 < len(s):
				sc.quoted[i], sc.quoted[i+1] = true, true
				i++
			case c == '$' && i+1 < len(s) && s[i+1] == '(':
				stack = append(stack, frame{kind: frameSubst, depth: 1})
				i++
				mark(i + 1)
			case c == '`':
				stack = append(stack, frame{kind: frameTick})
				mark(i + 1)
			default:
				sc.quoted[i] = true
			}
			continue
		}
		switch c {
		case '\\':
			i++
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			end := len(s)
			if j >= 0 {
				end = i + 1 + j
			}
			for k := i + 1; k < end; k++ {
				sc.quoted[k] = true
			}
			i = end
		case '"':
			stack = append(stack, frame{kind: frameDouble})
		case '$':
			if i+1 < len(s) && s[i+1] == '(' {
				stack = append(stack, frame{kind: frameSubst, depth: 1})
				i++
				mark(i + 1)
			}
		case '(':
			if f := top(); f != nil && f.kind == frameSubst {
				f.depth++
			}
			mark(i + 1)
		case ')':
			if f := top(); f != nil && f.kind == frameSubst {
				if f.depth--; f.depth == 0 {
					stack = stack[:len(stack)-1]
				}
			}
		case '`':
			if f := top(); f != nil && f.kind == frameTick {
				stack = stack[:len(stack)-1]
			} else {
				stack = append(stack, frame{kind: frameTick})
				mark(i + 1)
			}
		case ';', '\n', '|', '&':
			mark(i + 1)
		case '{':
			if (i == 0 || isSpace(s[i-1])) && i+1 < len(s) && isSpace(s[i+1]) {
				mark(i + 1)
			}
		}
	}
	sc.starts = expandKeywords(s, sc.starts)
	return sc
}

// expandKeywords adds the position after a leading shell keyword
// (`if reboot`, `then rm ...`) as another command start.
func expandKeywords(s string, starts []int) []int {
	out := make([]int, 0, len(starts))
	for _, at := range starts {
		out = append(out, at)
		for {
			i := at
			for i < len(s) && isSpace(s[i]) && s[i] != '\n' {
				i++
			}
			j := i
			for j < len(s) && !isSpace(s[j]) && !strings.ContainsRune(";|&()`", rune(s[j])) {
				j++
			}
			if !shellKeywords[s[i:j]] || j == i {
				break
			}
			at = j
			out = append(out, at)
		}
	}
	return out
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// markCommandStarts inserts marker before every non-initial command start so
// the anchored (^|\n) patterns see subshells, pipelines and lists.
func markCommandStarts(s, marker string) string {
	sc := scanShell(s)
	if len(sc.starts) == 0 {
		return s
	}
	var b strings.Builder
	prev := 0
	for _, at := range sortedUnique(sc.starts) {
		if at <= 0 || at > len(s) {
			continue
		}
		b.WriteString(s[prev:at])
		b.WriteString(marker)
		prev = at
	}
	b.WriteString(s[prev:])
	return b.String()
}

func sortedUnique(xs []int) []int {
	out := slices.Clone(xs)
	slices.Sort(out)
	return slices.Compact(out)
}

// maskQuotedNewlines turns newlines inside quotes into spaces: quoted
// newlines are data, not command separators.
func maskQuotedNewlines(s string) string {
	if !strings.Contains(s, "\n") {
		return s
	}
	sc := scanShell(s)
	b := []byte(s)
	for i, q := range sc.quoted {
		if q && b[i] == '\n' {
			b[i] = ' '
		}
	}
	return string(b)
}

// maskQuotedProse blanks quoted data (keeping the quote characters and any
// $(...) or backtick code inside double quotes).
func maskQuotedProse(s string) string {
	sc := scanShell(s)
	b := []byte(s)
	for i, q := range sc.quoted {
		if q && b[i] != '\'' && b[i] != '"' {
			b[i] = ' '
		}
	}
	return string(b)
}
