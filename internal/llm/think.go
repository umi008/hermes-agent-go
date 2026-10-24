package llm

import (
	"regexp"
	"strings"
)

// thinkTagNames mirrors upstream THINK_TAG_NAMES (agent/think_scrubber.py).
var thinkTagNames = []string{
	"think", "thinking", "reasoning", "thought", "reasoning_scratchpad",
	"思考", "反思", "推理", "推敲",
}

var (
	thinkOpenTags, thinkCloseTags, thinkAllTags = buildThinkTags()
	thinkMaxTagLen                              = maxLen(thinkAllTags)
	orphanCloseRE                               = buildOrphanCloseRE()
)

func buildThinkTags() (open, closing, all []string) {
	for _, n := range thinkTagNames {
		open = append(open, "<"+n+">")
		closing = append(closing, "</"+n+">")
	}
	all = append(append(all, open...), closing...)
	return open, closing, all
}

func buildOrphanCloseRE() *regexp.Regexp {
	parts := make([]string, len(thinkCloseTags))
	for i, t := range thinkCloseTags {
		parts[i] = regexp.QuoteMeta(t)
	}
	return regexp.MustCompile(`(?i)(?:` + strings.Join(parts, "|") + `)[ \t\n\r]*`)
}

func maxLen(tags []string) int {
	n := 0
	for _, t := range tags {
		n = max(n, len(t))
	}
	return n
}

// asciiLower lowercases ASCII letters only, so byte offsets stay aligned
// with the original string. Every tag is ASCII or caseless CJK.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// ThinkScrubber removes reasoning blocks such as <think>...</think> from
// streamed text whose tags may be split across deltas. It is a port of
// upstream StreamingThinkScrubber: an unterminated open tag only starts a
// block at a line boundary, closed pairs are always removed, and orphan
// close tags are dropped.
type ThinkScrubber struct {
	inBlock         bool
	buf             string
	lastEndsNewline bool
}

// NewThinkScrubber returns a scrubber at the start of a stream.
func NewThinkScrubber() *ThinkScrubber { return &ThinkScrubber{lastEndsNewline: true} }

// Feed consumes one delta and returns the visible text and the hidden
// reasoning it removed.
func (s *ThinkScrubber) Feed(text string) (visible, hidden string) {
	if text == "" {
		return "", ""
	}
	buf := s.buf + text
	s.buf = ""
	var out, hid []string
	for buf != "" {
		if s.inBlock {
			idx, n := firstTag(buf, thinkCloseTags)
			if idx < 0 {
				hid = append(hid, s.holdPartial(buf, thinkCloseTags))
				break
			}
			hid = append(hid, buf[:idx])
			buf = buf[idx+n:]
			s.inBlock = false
			continue
		}
		pStart, pEnd, hasPair := earliestPair(buf)
		openIdx, openLen := s.openAtBoundary(buf, out)
		if hasPair && (openIdx < 0 || pStart <= openIdx) {
			out = s.emit(out, buf[:pStart])
			inner := buf[pStart:pEnd]
			hid = append(hid, inner[strings.IndexByte(inner, '>')+1:strings.LastIndexByte(inner, '<')])
			buf = buf[pEnd:]
			continue
		}
		if openIdx >= 0 {
			out = s.emit(out, buf[:openIdx])
			s.inBlock = true
			buf = buf[openIdx+openLen:]
			continue
		}
		out = s.emit(out, s.holdPartial(buf, thinkAllTags))
		break
	}
	return strings.Join(out, ""), strings.Join(hid, "")
}

// Flush ends the stream. Held text inside an unterminated block is dropped;
// otherwise the held tail is returned.
func (s *ThinkScrubber) Flush() string {
	tail := s.buf
	if s.inBlock {
		tail = ""
	}
	s.buf, s.inBlock, s.lastEndsNewline = "", false, true
	return stripOrphanClose(tail)
}

func (s *ThinkScrubber) emit(out []string, text string) []string {
	text = stripOrphanClose(text)
	if text == "" {
		return out
	}
	s.lastEndsNewline = strings.HasSuffix(text, "\n")
	return append(out, text)
}

func (s *ThinkScrubber) holdPartial(buf string, tags []string) string {
	held := maxPartialSuffix(buf, tags)
	if held == 0 {
		return buf
	}
	s.buf = buf[len(buf)-held:]
	return buf[:len(buf)-held]
}

func (s *ThinkScrubber) openAtBoundary(buf string, emitted []string) (int, int) {
	lower := asciiLower(buf)
	best, bestLen := -1, 0
	for _, tag := range thinkOpenTags {
		from := 0
		for {
			i := strings.Index(lower[from:], tag)
			if i < 0 {
				break
			}
			idx := from + i
			if s.isBoundary(buf, idx, emitted) {
				if best < 0 || idx < best || (idx == best && len(tag) < bestLen) {
					best, bestLen = idx, len(tag)
				}
				break
			}
			from = idx + 1
		}
	}
	return best, bestLen
}

func (s *ThinkScrubber) isBoundary(buf string, idx int, emitted []string) bool {
	prior := s.lastEndsNewline
	if len(emitted) > 0 {
		prior = strings.HasSuffix(emitted[len(emitted)-1], "\n")
	}
	if idx == 0 {
		return prior
	}
	preceding := buf[:idx]
	nl := strings.LastIndexByte(preceding, '\n')
	if nl < 0 && !prior {
		return false
	}
	return strings.TrimSpace(preceding[nl+1:]) == ""
}

func firstTag(buf string, tags []string) (int, int) {
	lower := asciiLower(buf)
	best, bestLen := -1, 0
	for _, t := range tags {
		if i := strings.Index(lower, t); i >= 0 && (best < 0 || i < best || (i == best && len(t) < bestLen)) {
			best, bestLen = i, len(t)
		}
	}
	return best, bestLen
}

func earliestPair(buf string) (start, end int, ok bool) {
	lower := asciiLower(buf)
	for i, open := range thinkOpenTags {
		o := strings.Index(lower, open)
		if o < 0 {
			continue
		}
		c := strings.Index(lower[o+len(open):], thinkCloseTags[i])
		if c < 0 {
			continue
		}
		e := o + len(open) + c + len(thinkCloseTags[i])
		if !ok || o < start || (o == start && e < end) {
			start, end, ok = o, e, true
		}
	}
	return start, end, ok
}

func maxPartialSuffix(buf string, tags []string) int {
	lower := asciiLower(buf)
	for i := min(len(lower), thinkMaxTagLen-1); i > 0; i-- {
		suffix := lower[len(lower)-i:]
		for _, t := range tags {
			if len(t) > i && strings.HasPrefix(t, suffix) {
				return i
			}
		}
	}
	return 0
}

func stripOrphanClose(text string) string {
	if !strings.Contains(text, "</") {
		return text
	}
	return orphanCloseRE.ReplaceAllString(text, "")
}
