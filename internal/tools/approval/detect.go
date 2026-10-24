package approval

import (
	"path"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Detector ports upstream tools/approval_detection.py: hardline commands
// that are never allowed and dangerous commands that need approval.
type Detector struct {
	rules ruleSet
	// homes are folded to "~" and hermesHomes to "~/.hermes" before matching
	// so absolute spellings of protected paths hit the same rules.
	homes, hermesHomes []string
}

// NewDetector compiles the embedded pattern set. home and hermesHome may be
// empty.
func NewDetector(home, hermesHome string) (*Detector, error) {
	rs, err := loadRules()
	if err != nil {
		return nil, err
	}
	d := &Detector{rules: rs}
	if home != "" {
		d.homes = []string{home}
	}
	if hermesHome != "" {
		d.hermesHomes = []string{hermesHome}
	}
	return d, nil
}

var (
	ansiRE         = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)
	continuationRE = regexp.MustCompile(`\\\r?\n`)
	escapeRE       = regexp.MustCompile(`\\([^\n])`)
	emptyQuotesRE  = regexp.MustCompile(`''|""`)
	ifsRE          = regexp.MustCompile(`\$\{IFS\b[^}]*\}|\$IFS\b`)
	winPathRE      = regexp.MustCompile(`(?:[A-Za-z]:|\\\\)\\|[A-Za-z]:\\`)
	sudoStdinRE    = regexp.MustCompile("(?i)(?:^|[;&|`\\n]|&&|\\|\\||\\$\\()\\s*sudo\\s+-S\\b")
	commandWordRE  = regexp.MustCompile("(?:^|[;&|\\n(`]|\\$\\()\\s*([^\\s;&|()`]+)")
)

var shellCarriers = map[string]bool{"eval": true, "sh": true, "bash": true, "zsh": true, "ksh": true, "dash": true, "source": true, ".": true}

// normalize strips obfuscation that would otherwise dodge the patterns:
// ANSI escapes, NULs, fullwidth forms, line continuations, backslash escapes,
// empty quotes and $IFS splicing. Home prefixes fold to ~ first.
func (d *Detector) normalize(cmd string) string {
	cmd = norm.NFKC.String(strings.ReplaceAll(ansiRE.ReplaceAllString(cmd, ""), "\x00", ""))
	cmd = continuationRE.ReplaceAllString(cmd, "")
	cmd = foldHomes(cmd, d.hermesHomes, "~/.hermes")
	cmd = foldHomes(cmd, d.homes, "~")
	cmd = escapeRE.ReplaceAllString(cmd, "$1")
	cmd = emptyQuotesRE.ReplaceAllString(cmd, "")
	return ifsRE.ReplaceAllString(cmd, " ")
}

func foldHomes(cmd string, homes []string, to string) string {
	for _, h := range homes {
		h = strings.TrimRight(h, `/\`)
		if len(h) < 2 {
			continue
		}
		for _, form := range []string{h, strings.ReplaceAll(h, `\`, "/")} {
			for _, sep := range []string{"/", `\`} {
				cmd = replaceFold(cmd, form+sep, to+"/")
			}
		}
	}
	return cmd
}

// replaceFold replaces old case-insensitively (Windows paths ignore case).
func replaceFold(s, old, repl string) string {
	lower, lowOld := strings.ToLower(s), strings.ToLower(old)
	if len(lower) != len(s) || !strings.Contains(lower, lowOld) {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(lower, lowOld)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(repl)
		s, lower = s[i+len(old):], lower[i+len(old):]
	}
}

// lowerPreservingFlags lowercases every token except dash-prefixed flags so
// case-sensitive flag groups keep their meaning.
func lowerPreservingFlags(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		tok := s[start:end]
		if !strings.HasPrefix(tok, "-") {
			tok = strings.ToLower(tok)
		}
		b.WriteString(tok)
		start = -1
	}
	for i := 0; i < len(s); i++ {
		if isSpace(s[i]) || s[i] == '\f' || s[i] == '\v' {
			flush(i)
			b.WriteByte(s[i])
		} else if start < 0 {
			start = i
		}
	}
	flush(len(s))
	return b.String()
}

// variants yields the detection spellings of a command, deduplicated:
// normalized, Windows backslash paths, command starts marked, and the
// program payloads of sh -c and similar flags (recursively).
func (d *Detector) variants(cmd string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) bool {
		if v == "" || seen[v] {
			return false
		}
		seen[v] = true
		out = append(out, v)
		return true
	}
	normalized := d.normalize(maskQuotedNewlines(cmd))
	add(normalized)
	if winPathRE.MatchString(cmd) {
		add(d.normalize(maskQuotedNewlines(strings.ReplaceAll(cmd, `\`, "/"))))
	}
	add(markCommandStarts(normalized, "\n"))
	add(d.normalize(markCommandStarts(maskQuotedNewlines(cmd), " \n")))
	pending := []string{normalized}
	for depth := 0; len(pending) > 0 && depth < 8; depth++ {
		var next []string
		for _, v := range pending {
			for _, f := range execFindings(v) {
				payload := f.payload
				if add(payload) {
					add(markCommandStarts(payload, "\n"))
					next = append(next, payload)
				}
			}
		}
		pending = next
	}
	return out
}

func hasShellCarrier(s string) bool {
	for _, m := range commandWordRE.FindAllStringSubmatch(s, -1) {
		if shellCarriers[strings.ToLower(path.Base(m[1]))] {
			return true
		}
	}
	return false
}

// Hardline reports a command that must never run, with its description.
func (d *Detector) Hardline(cmd string) (string, bool) {
	for _, v := range d.variants(cmd) {
		lower := strings.ToLower(v)
		masked := ""
		for _, r := range d.rules.hardline {
			target := lower
			if r.quoteMasked {
				if masked == "" {
					masked = lower
					if !hasShellCarrier(v) {
						masked = strings.ToLower(maskQuotedProse(v))
					}
				}
				target = masked
			}
			if r.match(target) {
				return r.description, true
			}
		}
	}
	return "", false
}

// Dangerous reports a command that needs approval; the description is also
// its approval key.
func (d *Detector) Dangerous(cmd string) (string, bool) {
	for _, v := range d.variants(cmd) {
		lower := lowerPreservingFlags(v)
		masked := ""
		for _, r := range d.rules.dangerous {
			target := lower
			if r.quoteMasked {
				if masked == "" {
					masked = lowerPreservingFlags(maskQuotedProse(v))
				}
				target = masked
			}
			if r.match(target) {
				return r.description, true
			}
		}
	}
	if f := execFindings(d.normalize(cmd)); len(f) > 0 {
		return f[0].description, true
	}
	return "", false
}

// SudoStdin reports an explicit `sudo -S`, the model piping a guessed
// password, which upstream blocks unless SUDO_PASSWORD is configured.
func (d *Detector) SudoStdin(cmd string) bool {
	return sudoStdinRE.MatchString(strings.ToLower(d.normalize(cmd)))
}
