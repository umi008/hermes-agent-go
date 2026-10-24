package hooks

import (
	"errors"
	"strings"
)

// SplitCommand is upstream split_command_line: shlex.split on POSIX, and
// on Windows shlex in non-POSIX mode (backslashes stay literal) with one
// layer of matching quotes stripped per token.
func SplitCommand(line string, windows bool) ([]string, error) {
	if windows {
		return splitNonPOSIX(line)
	}
	return splitPOSIX(line)
}

func isSpace(c rune) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func splitPOSIX(line string) ([]string, error) {
	var (
		out     []string
		tok     strings.Builder
		inTok   bool
		quote   rune
		escaped bool
	)
	for _, c := range line {
		switch {
		case escaped:
			if quote == '"' && c != '"' && c != '\\' {
				tok.WriteRune('\\')
			}
			tok.WriteRune(c)
			escaped = false
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				tok.WriteRune(c)
			}
		case quote == '"':
			switch c {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				tok.WriteRune(c)
			}
		case c == '\\':
			escaped, inTok = true, true
		case c == '\'' || c == '"':
			quote, inTok = c, true
		case isSpace(c):
			if inTok {
				out = append(out, tok.String())
				tok.Reset()
				inTok = false
			}
		default:
			tok.WriteRune(c)
			inTok = true
		}
	}
	if escaped {
		return nil, errors.New("no escaped character")
	}
	if quote != 0 {
		return nil, errors.New("no closing quotation")
	}
	if inTok {
		out = append(out, tok.String())
	}
	return out, nil
}

func splitNonPOSIX(line string) ([]string, error) {
	var out []string
	rs := []rune(line)
	for i := 0; i < len(rs); {
		if isSpace(rs[i]) {
			i++
			continue
		}
		start := i
		if q := rs[i]; q == '\'' || q == '"' {
			// A quoted token ends at its closing quote.
			end := i + 1
			for end < len(rs) && rs[end] != q {
				end++
			}
			if end == len(rs) {
				return nil, errors.New("no closing quotation")
			}
			out = append(out, string(rs[start+1:end]))
			i = end + 1
			continue
		}
		for i < len(rs) && !isSpace(rs[i]) {
			i++
		}
		out = append(out, string(rs[start:i]))
	}
	return out, nil
}
