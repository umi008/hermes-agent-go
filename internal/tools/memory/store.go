// Package memory ports upstream tools/memory_tool.py and memory_tool_store.py:
// two bounded, §-delimited stores (MEMORY.md for the agent's notes, USER.md
// for the user profile) under <hermes home>/memories, edited through the
// memory tool and injected into the system prompt as a snapshot frozen at
// session start, so mid-session writes never break the prefix cache.
package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Delimiter separates entries in a store file.
const Delimiter = "\n§\n"

// Default character limits, upstream memory_char_limit and user_char_limit.
const (
	DefaultMemoryLimit = 2200
	DefaultUserLimit   = 1375
)

const patchDropHint = "To drop text inside an entry, patch a longer span that contains it; 'remove' deletes the whole entry."

var headers = map[string]string{"memory": "MEMORY (your personal notes)", "user": "USER PROFILE (who the user is)"}

// Options configure a Store.
type Options struct {
	MemoryLimit, UserLimit int
	Now                    func() time.Time
	Logger                 *slog.Logger
}

// Store holds both targets. Writes re-read the file under a mutex, so a hand
// edit made between calls is kept; the prompt snapshot never changes.
type Store struct {
	dir      string
	limits   map[string]int
	now      func() time.Time
	mu       sync.Mutex
	entries  map[string][]string
	snapshot map[string]string
}

// Open loads both files from dir and freezes the prompt snapshot.
func Open(dir string, o Options) (*Store, error) {
	if o.MemoryLimit <= 0 {
		o.MemoryLimit = DefaultMemoryLimit
	}
	if o.UserLimit <= 0 {
		o.UserLimit = DefaultUserLimit
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("memory dir: %w", err)
	}
	s := &Store{dir: dir, now: o.Now, limits: map[string]int{"memory": o.MemoryLimit, "user": o.UserLimit},
		entries: map[string][]string{}, snapshot: map[string]string{}}
	for _, target := range []string{"memory", "user"} {
		raw, _ := readRaw(s.path(target))
		entries := dedupe(parseEntries(raw))
		s.entries[target] = entries
		if n, limit := chars(entries), s.limits[target]; n > limit {
			o.Logger.Warn("memory store exceeds its char limit on load; further additions are blocked",
				"file", filepath.Base(s.path(target)), "chars", n, "limit", limit)
		}
		s.snapshot[target] = s.renderBlock(target, entries)
	}
	return s, nil
}

// PromptBlocks returns the non-empty frozen blocks, memory first.
func (s *Store) PromptBlocks() []string {
	var out []string
	for _, t := range []string{"memory", "user"} {
		if b := s.snapshot[t]; b != "" {
			out = append(out, b)
		}
	}
	return out
}

// Entries returns the live entries of target.
func (s *Store) Entries(target string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.entries[target]...)
}

func (s *Store) path(target string) string {
	if target == "user" {
		return filepath.Join(s.dir, "USER.md")
	}
	return filepath.Join(s.dir, "MEMORY.md")
}

func (s *Store) renderBlock(target string, entries []string) string {
	if len(entries) == 0 {
		return ""
	}
	content, sep := strings.Join(entries, Delimiter), strings.Repeat("═", 46)
	return fmt.Sprintf("%s\n%s [%s]\n%s\n%s", sep, headers[target], s.usagePct(target, runes(content)), sep, content)
}

func (s *Store) usagePct(target string, current int) string {
	limit, pct := s.limits[target], 0
	if limit > 0 {
		pct = min(100, current*100/limit)
	}
	return fmt.Sprintf("%d%% — %s/%s chars", pct, commas(current), commas(limit))
}

func (s *Store) usage(target string) string {
	return commas(chars(s.entries[target])) + "/" + commas(s.limits[target])
}

// result is a tool payload with upstream key order.
type result []kv

type kv struct {
	k string
	v any
}

func (r result) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range r {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(p.k)
		b.Write(k)
		b.WriteByte(':')
		var v bytes.Buffer
		enc := json.NewEncoder(&v)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(p.v); err != nil {
			return nil, err
		}
		b.Write(bytes.TrimRight(v.Bytes(), "\n"))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (r result) success() bool {
	for _, p := range r {
		if p.k == "success" {
			ok, _ := p.v.(bool)
			return ok
		}
	}
	return false
}

func fail(msg string, extra ...kv) result {
	return append(result{{"success", false}, {"error", msg}}, extra...)
}

func (s *Store) successResult(target, message string, extra ...kv) result {
	r := result{{"success", true}, {"done", true}, {"target", target},
		{"usage", s.usagePct(target, chars(s.entries[target]))}, {"entry_count", len(s.entries[target])}}
	if message != "" {
		r = append(r, kv{"message", message})
	}
	r = append(r, extra...)
	return append(r, kv{"note", "Write saved. This update is complete — do not repeat it."})
}

func (s *Store) overBudget(target, msg string) result {
	return fail(msg, kv{"current_entries", nonNil(s.entries[target])}, kv{"usage", s.usage(target)})
}

func (s *Store) batchFailure(target, msg string, closest []string) result {
	extra := []kv{{"usage", s.usage(target)}}
	if len(closest) > 0 {
		extra = append(extra, kv{"closest_entries", closest})
	}
	return fail(msg+" No operations were applied (batch is all-or-nothing).", extra...)
}

// mutation returns either a failure (nothing persisted) or the new entries,
// a success message and extra payload fields.
type mutation func(entries []string, limit int) (next []string, msg string, extra []kv, failure result)

// mutate re-reads the file, applies fn and persists the result.
func (s *Store) mutate(target string, checkDrift bool, fn mutation) result {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.path(target)
	raw, err := readRaw(p)
	if err != nil {
		return fail(fmt.Sprintf("Refusing to write %s: the file exists on disk but could not be read right now "+
			"(temporarily locked by another program, a permission change, invalid/corrupt text encoding, "+
			"or a filesystem error). Treating an unreadable file as empty and saving would wipe existing "+
			"memory, so the write is refused. Nothing was changed — retry in a moment.", filepath.Base(p)))
	}
	parsed := parseEntries(raw)
	s.entries[target] = dedupe(parsed)
	if checkDrift {
		if r := s.driftGuard(target, raw, parsed); r != nil {
			return r
		}
	}
	next, msg, extra, failure := fn(s.entries[target], s.limits[target])
	if failure != nil {
		return failure
	}
	if err := writeAtomic(p, strings.Join(next, Delimiter)); err != nil {
		return fail(fmt.Sprintf("Failed to write memory file %s: %v", p, err))
	}
	s.entries[target] = next
	return s.successResult(target, msg, extra...)
}

// driftGuard refuses to rewrite a file holding an entry larger than the
// whole store limit: free-form text appended without delimiters, which a
// rewrite would lose. It snapshots the raw file first.
func (s *Store) driftGuard(target, raw string, parsed []string) result {
	longest := 0
	for _, e := range parsed {
		longest = max(longest, runes(e))
	}
	if longest <= s.limits[target] {
		return nil
	}
	p := s.path(target)
	bak := fmt.Sprintf("%s.bak.%d", p, s.now().Unix())
	if err := os.WriteFile(bak, []byte(raw), 0o644); err != nil {
		bak += " (BACKUP FAILED — file unchanged on disk)"
	}
	name := filepath.Base(p)
	return fail(fmt.Sprintf("Refusing to write %s: it holds an entry larger than the whole store's char limit, "+
		"i.e. free-form text written without § delimiters (the patch tool, a shell append, a manual "+
		"edit). Rewriting it would lose that text. A snapshot was saved to %s. Resolve the drift first — "+
		"either rewrite the file as a clean §-delimited list of entries, or move the extra "+
		"content out — then retry. This guard exists to prevent silent data loss (issue #26045).", name, bak),
		kv{"drift_backup", bak},
		kv{"remediation", "Open the .bak file, integrate the missing entries into the memory tool one at a time via " +
			fmt.Sprintf("memory(action=add, target='%s', content=...), then remove or rewrite the ", target) +
			"original file to a clean state."})
}

// add appends an entry.
func (s *Store) add(target, content string) result {
	content = strings.TrimSpace(content)
	if content == "" {
		return fail("Content cannot be empty.")
	}
	return s.mutate(target, false, func(entries []string, limit int) ([]string, string, []kv, result) {
		for _, e := range entries {
			if e == content {
				return nil, "", nil, s.successResult(target, "Entry already exists (no duplicate added).")
			}
		}
		next := append(append([]string(nil), entries...), content)
		if total := chars(next); total > limit {
			over := commas(total - limit)
			return nil, "", nil, s.overBudget(target, fmt.Sprintf("Memory at %s/%s chars; adding this entry (%d chars) "+
				"would exceed the limit by %s chars. Retry as ONE 'operations' batch that "+
				"removes or shortens (patch) stale entries from current_entries below to free at least "+
				"%s chars AND adds this entry — the limit is checked only on the batch result.",
				commas(chars(entries)), commas(limit), runes(content), over, over))
		}
		return next, "Entry added.", nil, nil
	})
}

// patch replaces one span inside the matched entry.
func (s *Store) patch(target, oldText, newText string) result {
	if strings.TrimSpace(oldText) == "" {
		return fail("old_text cannot be empty.")
	}
	if strings.TrimSpace(newText) == "" {
		return fail("new_text cannot be empty. " + patchDropHint)
	}
	return s.mutate(target, true, func(entries []string, limit int) ([]string, string, []kv, result) {
		idx, failure := s.locate(entries, oldText, "patch")
		if failure != nil {
			return nil, "", nil, failure
		}
		before := entries[idx]
		after, msg := patchEntry(before, oldText, newText)
		if msg != "" {
			return nil, "", nil, fail(msg)
		}
		next := splice(entries, idx, after)
		if total := chars(next); total > limit {
			return nil, "", nil, s.overBudget(target, fmt.Sprintf("Patch would put memory at %s/%s chars. Shorten the new "+
				"text, or 'remove' other stale or less important entries to make room "+
				"(see current_entries below), then retry — all in this turn.", commas(total), commas(limit)))
		}
		return next, "Entry patched.", []kv{{"patched_entry_before", before}, {"patched_entry_after", after}}, nil
	})
}

// remove deletes the matched entry.
func (s *Store) remove(target, oldText string) result {
	oldText = strings.TrimSpace(oldText)
	if oldText == "" {
		return fail("old_text cannot be empty.")
	}
	return s.mutate(target, true, func(entries []string, _ int) ([]string, string, []kv, result) {
		idx, failure := s.locate(entries, oldText, "remove")
		if failure != nil {
			return nil, "", nil, failure
		}
		return splice(entries, idx), "Entry removed.", []kv{{"removed_entry", entries[idx]}}, nil
	})
}

func (s *Store) locate(entries []string, oldText, verb string) (int, result) {
	idx, ambiguous := findUnique(entries, oldText)
	if ambiguous {
		var matches []string
		for _, i := range substringMatches(entries, oldText) {
			matches = append(matches, truncate(entries[i], 80))
		}
		return 0, fail(fmt.Sprintf("Multiple entries matched '%s'. Be more specific.", oldText), kv{"matches", matches})
	}
	if idx < 0 {
		msg, closest := noMatch(entries, oldText, verb)
		extra := []kv{}
		if len(closest) > 0 {
			extra = append(extra, kv{"closest_entries", closest})
		}
		return 0, fail(msg, append(extra, kv{"current_entries", nonNil(entries)})...)
	}
	return idx, nil
}

// Op is one batch operation.
type Op struct {
	Action  string `json:"action"`
	Content string `json:"content"`
	NewText string `json:"new_text"`
	OldText string `json:"old_text"`
	Target  any    `json:"target"`
}

// batch applies ops atomically against the final budget.
func (s *Store) batch(target string, ops []Op) result {
	if len(ops) == 0 {
		return fail("operations list is empty.")
	}
	return s.mutate(target, true, func(entries []string, limit int) ([]string, string, []kv, result) {
		working := append([]string(nil), entries...)
		patched, produced, removed := map[int]string{}, map[int]string{}, map[int]string{}
		for i, op := range ops {
			act := op.Action
			content := op.Content
			if content == "" {
				content = op.NewText
			}
			oldText := op.OldText
			if act != "patch" {
				content, oldText = strings.TrimSpace(content), strings.TrimSpace(oldText)
			}
			label := act
			if label == "" {
				label = "unknown"
			}
			pos := fmt.Sprintf("Operation %d (%s)", i+1, label)
			n := i + 1
			if act == "add" {
				if content == "" {
					return nil, "", nil, s.batchFailure(target, pos+": content is required.", nil)
				}
				if !contains(working, content) {
					working = append(working, content)
				}
				continue
			}
			if act != "remove" && act != "patch" {
				return nil, "", nil, s.batchFailure(target, pos+": unknown action. Use add, patch, or remove.", nil)
			}
			if strings.TrimSpace(oldText) == "" {
				return nil, "", nil, s.batchFailure(target, pos+": old_text is required.", nil)
			}
			if act == "patch" && strings.TrimSpace(content) == "" {
				return nil, "", nil, s.batchFailure(target, pos+": content is required — set this operation object's own "+
					"'content' to the new text for the old_text span. "+patchDropHint, nil)
			}
			idx, ambiguous := findUnique(working, oldText)
			if ambiguous {
				return nil, "", nil, s.batchFailure(target, fmt.Sprintf("%s: '%s' matched multiple distinct entries -- be more specific.", pos, oldText), nil)
			}
			if idx < 0 {
				msg, closest := noMatch(working, oldText, act)
				return nil, "", nil, s.batchFailure(target, pos+": "+msg, closest)
			}
			prev := working[idx]
			if act == "remove" {
				working = splice(working, idx)
				removed[n] = prev
				continue
			}
			after, msg := patchEntry(prev, oldText, content)
			if msg != "" {
				return nil, "", nil, s.batchFailure(target, pos+": "+msg, nil)
			}
			working = splice(working, idx, after)
			patched[n], produced[n] = prev, after
		}
		if len(entries) > 0 && len(working) == 0 {
			return nil, "", nil, s.batchFailure(target, fmt.Sprintf("Refusing to empty %s: this batch would remove every entry from a "+
				"previously non-empty store. Keep at least one entry — merge overlapping "+
				"entries into a shorter one instead of removing the last one. To delete the "+
				"final entry deliberately, use single remove() calls.", filepath.Base(s.path(target))), nil)
		}
		if total := chars(working); total > limit {
			return nil, "", nil, s.batchFailure(target, fmt.Sprintf("Applying all %d operations %s more. Add "+
				"removes or shortening patches of other stale entries to the same batch, then retry.",
				len(ops), overLimit(total, limit)), nil)
		}
		var extra []kv
		if len(patched) > 0 {
			extra = append(extra, kv{"patched_entries", indexMap(patched)}, kv{"patched_entries_after", indexMap(produced)})
		}
		if len(removed) > 0 {
			extra = append(extra, kv{"removed_entries", indexMap(removed)})
		}
		return working, fmt.Sprintf("Applied %d operation(s).", len(ops)), extra, nil
	})
}

// indexMap keeps Python's insertion order (ascending op index).
func indexMap(m map[int]string) result {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	out := make(result, 0, len(keys))
	for _, k := range keys {
		out = append(out, kv{fmt.Sprint(k), m[k]})
	}
	return out
}

func overLimit(total, limit int) string {
	return fmt.Sprintf("would put memory at %s/%s chars: free at least %s chars", commas(total), commas(limit), commas(total-limit))
}

func patchEntry(entry, oldText, newText string) (string, string) {
	if n := spanOccurrences(entry, oldText); n > 1 {
		return "", fmt.Sprintf("'%s' appears %d times in the matched entry, and patch edits one span. "+
			"Include more surrounding text so old_text occurs once.", oldText, n)
	}
	patched := strings.Replace(entry, oldText, newText, 1)
	if patched == entry {
		return "", fmt.Sprintf("Patch is a no-op: '%s' -> '%s' leaves the entry unchanged.", oldText, newText)
	}
	return patched, ""
}

// spanOccurrences counts overlapping occurrences: "00" occurs twice in "1000".
func spanOccurrences(text, span string) int {
	if span == "" {
		return 0
	}
	n := 0
	for i := 0; i+len(span) <= len(text); i++ {
		if strings.HasPrefix(text[i:], span) {
			n++
		}
	}
	return n
}

var fold = strings.NewReplacer(
	"\"", "'", "`", "'", "‘", "'", "’", "'", "‚", "'", "‛", "'", "“", "'", "”", "'", "„", "'", "‟", "'",
	"‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-", "―", "-", "−", "-")

func normalize(text string) string {
	return strings.Join(strings.Fields(fold.Replace(strings.ReplaceAll(text, `\n`, " "))), " ")
}

func substringMatches(entries []string, oldText string) []int {
	var exact []int
	for i, e := range entries {
		if strings.Contains(e, oldText) {
			exact = append(exact, i)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	needle := normalize(oldText)
	if !strings.ContainsFunc(needle, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		return nil
	}
	var out []int
	for i, e := range entries {
		if strings.Contains(normalize(e), needle) {
			out = append(out, i)
		}
	}
	return out
}

// findUnique returns the matched index (-1 for none) and whether distinct
// entries matched. A whole-entry exact match wins over substring matches.
func findUnique(entries []string, oldText string) (int, bool) {
	var matches []int
	for i, e := range entries {
		if e == oldText {
			matches = append(matches, i)
		}
	}
	if len(matches) == 0 {
		matches = substringMatches(entries, oldText)
	}
	distinct := map[string]bool{}
	for _, i := range matches {
		distinct[entries[i]] = true
	}
	if len(distinct) > 1 {
		return -1, true
	}
	if len(matches) == 0 {
		return -1, false
	}
	return matches[0], false
}

var wordRE = regexp.MustCompile(`[\p{L}\p{N}_]+`)

func words(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordRE.FindAllString(strings.ToLower(text), -1) {
		out[w] = true
	}
	return out
}

// noMatch names the entries sharing the most words with oldText.
func noMatch(entries []string, oldText, verb string) (string, []string) {
	want := words(oldText)
	type scored struct {
		score float64
		i     int
	}
	var all []scored
	if len(want) > 0 {
		for i, e := range entries {
			n := 0
			for w := range words(e) {
				if want[w] {
					n++
				}
			}
			all = append(all, scored{float64(n) / float64(len(want)), i})
		}
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].score > all[b].score })
	var closest []string
	for _, c := range all[:min(3, len(all))] {
		if c.score >= 0.3 {
			closest = append(closest, entries[c.i])
		}
	}
	hint := ""
	if strings.Contains(oldText, "§") {
		hint = " old_text must come from ONE entry ('§' separates entries); use one op per entry."
	}
	tail := "."
	if len(closest) > 0 {
		tail = " (closest_entries below)."
	}
	return fmt.Sprintf("No entry matched '%s'.%s Retry the %s with old_text copied verbatim from the intended entry%s",
		oldText, hint, verb, tail), closest
}

// readRaw returns "" for a missing file and an error only when the file
// exists but cannot be read or is not valid UTF-8. A BOM is stripped.
func readRaw(p string) (string, error) {
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", errors.New("invalid utf-8")
	}
	return strings.TrimPrefix(string(data), "Feff"), nil
}

func parseEntries(raw string) []string {
	var out []string
	for _, e := range strings.Split(raw, Delimiter) {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

func writeAtomic(p, content string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".mem_*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range in {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

func splice(entries []string, idx int, with ...string) []string {
	out := append([]string(nil), entries[:idx]...)
	out = append(out, with...)
	return append(out, entries[idx+1:]...)
}

func contains(entries []string, s string) bool {
	for _, e := range entries {
		if e == s {
			return true
		}
	}
	return false
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func chars(entries []string) int { return runes(strings.Join(entries, Delimiter)) }

func runes(s string) int { return utf8.RuneCountInString(s) }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// commas formats like Python's "{:,}".
func commas(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
