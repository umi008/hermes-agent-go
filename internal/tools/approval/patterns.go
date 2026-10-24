package approval

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/dlclark/regexp2"
)

// patterns.json is generated from upstream tools/approval_detection.py
// (HARDLINE_PATTERNS, DANGEROUS_PATTERNS and the quote-masked description
// sets). Each entry is [pattern, description]; the description doubles as
// the approval key stored in command_allowlist.
//
//go:embed patterns.json
var patternsJSON []byte

// matchTimeout bounds one backtracking match. A timeout counts as a match so
// a pathological command fails closed.
const matchTimeout = 250 * time.Millisecond

type rule struct {
	re          *regexp2.Regexp
	description string
	quoteMasked bool
}

func (r rule) match(s string) bool {
	ok, err := r.re.MatchString(s)
	return ok || err != nil
}

type ruleSet struct {
	hardline  []rule
	dangerous []rule
}

var (
	// Python-only syntax: possessive quantifiers and \u/\U escapes.
	possessiveRE = regexp.MustCompile(`(\\[sS])\+\+`)
	unicodeEscRE = regexp.MustCompile(`\\u([0-9a-fA-F]{4})|\\U([0-9a-fA-F]{8})`)
)

// translate rewrites Python re syntax that regexp2 lacks.
func translate(p string) string {
	p = possessiveRE.ReplaceAllString(p, "(?>$1+)")
	return unicodeEscRE.ReplaceAllStringFunc(p, func(m string) string {
		n, err := strconv.ParseUint(m[2:], 16, 32)
		if err != nil {
			return m
		}
		return string(rune(n))
	})
}

func loadRules() (ruleSet, error) {
	var raw struct {
		Hardline             [][2]string `json:"hardline"`
		Dangerous            [][2]string `json:"dangerous"`
		QuoteMaskedHardline  []string    `json:"quote_masked_hardline"`
		QuoteMaskedDangerous []string    `json:"quote_masked_dangerous"`
	}
	if err := json.Unmarshal(patternsJSON, &raw); err != nil {
		return ruleSet{}, fmt.Errorf("decode patterns: %w", err)
	}
	compile := func(list [][2]string, masked []string) ([]rule, error) {
		set := map[string]bool{}
		for _, d := range masked {
			set[d] = true
		}
		out := make([]rule, 0, len(list))
		for _, e := range list {
			re, err := regexp2.Compile(translate(e[0]), regexp2.IgnoreCase|regexp2.Singleline)
			if err != nil {
				return nil, fmt.Errorf("compile %q: %w", e[1], err)
			}
			re.MatchTimeout = matchTimeout
			out = append(out, rule{re: re, description: e[1], quoteMasked: set[e[1]]})
		}
		return out, nil
	}
	var rs ruleSet
	var err error
	if rs.hardline, err = compile(raw.Hardline, raw.QuoteMaskedHardline); err != nil {
		return ruleSet{}, err
	}
	if rs.dangerous, err = compile(raw.Dangerous, raw.QuoteMaskedDangerous); err != nil {
		return ruleSet{}, err
	}
	return rs, nil
}
