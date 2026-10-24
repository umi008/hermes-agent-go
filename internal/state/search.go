package state

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxQueryChars caps a search query before sanitizing (upstream
// MAX_FTS5_QUERY_CHARS).
const MaxQueryChars = 2048

// Query is a message search.
type Query struct {
	Text           string
	Roles          []string // nil: every role
	ExcludeSources []string
	Limit          int    // default 20
	Sort           string // "", "newest" or "oldest"
	After, Before  *int64 // bounds on sessions.started_at (inclusive, exclusive)
}

// Hit is one matching message.
type Hit struct {
	ID             int64
	SessionID      string
	Role           string
	Snippet        string
	Timestamp      float64
	ToolName       string
	Source         string
	Model          string
	SessionStarted float64
}

var (
	quotedPhraseRE = regexp.MustCompile(`"[^"]*"`)
	ftsSpecialRE   = regexp.MustCompile(`[+{}():"^@/#&|~\[\]<>,;!?$=\\']`)
	starsRE        = regexp.MustCompile(`\*+`)
	leadingStarRE  = regexp.MustCompile(`(^|\s)\*`)
	leadingOpRE    = regexp.MustCompile(`(?i)^(AND|OR|NOT)\b\s*`)
	trailingOpRE   = regexp.MustCompile(`(?i)\s+(AND|OR|NOT)\s*$`)
	compoundRE     = regexp.MustCompile(`\b(\w+(?:[._-]\w+)+)\b`)
	tokenRE        = regexp.MustCompile(`"[^"]+"|\S+`)
)

// SanitizeQuery ports upstream _sanitize_fts5_query: keep balanced quoted
// phrases, strip characters FTS5 rejects, and quote dotted or hyphenated
// terms so they match as phrases.
func SanitizeQuery(q string) string {
	if utf8.RuneCountInString(q) > MaxQueryChars {
		q = string([]rune(q)[:MaxQueryChars])
	}
	var held []string
	s := quotedPhraseRE.ReplaceAllStringFunc(q, func(m string) string {
		held = append(held, m)
		return fmt.Sprintf("\x00Q%d\x00", len(held)-1)
	})
	s = strings.ReplaceAll(s, `"`, " ")
	s = ftsSpecialRE.ReplaceAllString(s, " ")
	if strings.Contains(s, "%") && !containsCJK(s) {
		s = strings.ReplaceAll(s, "%", " ")
	}
	s = starsRE.ReplaceAllString(s, "*")
	s = leadingStarRE.ReplaceAllString(s, "$1")
	s = leadingOpRE.ReplaceAllString(strings.TrimSpace(s), "")
	s = trailingOpRE.ReplaceAllString(strings.TrimSpace(s), "")
	s = compoundRE.ReplaceAllString(s, `"$1"`)
	for i, h := range held {
		s = strings.ReplaceAll(s, fmt.Sprintf("\x00Q%d\x00", i), h)
	}
	return strings.TrimSpace(s)
}

// orRelaxed ports _or_relaxed_query: an implicit-AND query rewritten to
// match any term; "" when it does not apply.
func orRelaxed(q string) string {
	var units []string
	for _, tok := range tokenRE.FindAllString(q, -1) {
		switch strings.ToUpper(tok) {
		case "OR", "NOT":
			return ""
		case "AND":
			continue
		}
		units = append(units, tok)
	}
	if len(units) < 2 {
		return ""
	}
	return strings.Join(units, " OR ")
}

// quoteTokens ports _quote_fts_tokens.
func quoteTokens(q string) (string, bool) {
	var out []string
	eligible := true
	for _, tok := range strings.Fields(q) {
		if up := strings.ToUpper(tok); up == "AND" || up == "OR" || up == "NOT" {
			out = append(out, tok)
			continue
		}
		if utf8.RuneCountInString(tok) < 3 {
			eligible = false // trigram needs 3 chars per token
		}
		out = append(out, `"`+strings.ReplaceAll(tok, `"`, `""`)+`"`)
	}
	return strings.Join(out, " "), eligible && len(out) > 0
}

func containsCJK(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return true
		}
	}
	return false
}

var orderBy = map[string]string{
	"newest": "ORDER BY m.timestamp DESC, rank",
	"oldest": "ORDER BY m.timestamp ASC, rank",
}

// filters appends the predicates every route shares (upstream
// _search_filter_clauses): live or compaction-archived rows, no hidden
// scaffolding, source, role and start-time bounds.
func filters(q Query) ([]string, []any) {
	where := []string{"(m.active = 1 OR m.compacted = 1)", "COALESCE(m.display_kind, '') <> 'hidden'"}
	var args []any
	if len(q.ExcludeSources) > 0 {
		where = append(where, "s.source NOT IN ("+placeholders(len(q.ExcludeSources))+")")
		for _, v := range q.ExcludeSources {
			args = append(args, v)
		}
	}
	if len(q.Roles) > 0 {
		where = append(where, "m.role IN ("+placeholders(len(q.Roles))+")")
		for _, v := range q.Roles {
			args = append(args, v)
		}
	}
	if q.After != nil {
		where = append(where, "s.started_at >= ?")
		args = append(args, *q.After)
	}
	if q.Before != nil {
		where = append(where, "s.started_at < ?")
		args = append(args, *q.Before)
	}
	return where, args
}

const selectTail = "m.timestamp, COALESCE(m.tool_name,''), s.source, COALESCE(s.model,''), s.started_at"

// Search runs an FTS5 search, as upstream search_messages: the unicode61
// index, then the trigram index on a miss, then an any-term retry. A
// tool-role search scans canonical rows with LIKE.
func (s *Store) Search(ctx context.Context, q Query) ([]Hit, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	match := SanitizeQuery(q.Text)
	if match == "" {
		return nil, nil
	}
	for _, r := range q.Roles {
		if r == "tool" {
			return s.likeSearch(ctx, match, q)
		}
	}
	hits, err := s.match(ctx, "messages_fts", match, q)
	if err != nil || len(hits) > 0 {
		return hits, err
	}
	if fb, ok := quoteTokens(strings.TrimSpace(strings.Trim(match, `"`))); ok {
		if hits, err = s.match(ctx, "messages_fts_trigram", fb, q); err != nil || len(hits) > 0 {
			return hits, err
		}
	}
	if relaxed := orRelaxed(match); relaxed != "" {
		return s.match(ctx, "messages_fts", relaxed, q)
	}
	return nil, nil
}

func (s *Store) match(ctx context.Context, table, match string, q Query) ([]Hit, error) {
	where, args := filters(q)
	where = append([]string{table + " MATCH ?"}, where...)
	args = append([]any{match}, args...)
	order, ok := orderBy[q.Sort]
	if !ok {
		order = "ORDER BY rank"
	}
	sqlText := fmt.Sprintf(`SELECT m.id, m.session_id, m.role,
		snippet(%s, -1, '>>>', '<<<', '...', 40), %s
		FROM %s JOIN messages m ON m.id = %s.rowid JOIN sessions s ON s.id = m.session_id
		WHERE %s %s LIMIT ?`, table, selectTail, table, table, strings.Join(where, " AND "), order)
	hits, err := s.scanHits(ctx, sqlText, append(args, q.Limit)...)
	if err != nil && strings.Contains(err.Error(), "fts5") {
		return nil, nil // FTS5 syntax error despite sanitizing: no hits, as upstream
	}
	return hits, err
}

func (s *Store) likeSearch(ctx context.Context, match string, q Query) ([]Hit, error) {
	where, args := filters(q)
	var first string
	for _, tok := range tokenRE.FindAllString(match, -1) {
		term := strings.Trim(strings.Trim(tok, `"`), "*")
		if up := strings.ToUpper(term); term == "" || up == "AND" || up == "OR" || up == "NOT" {
			continue
		}
		if first == "" {
			first = term
		}
		pat := "%" + escapeLike(term) + "%"
		where = append(where, `(COALESCE(m.content,'') LIKE ? ESCAPE '\' OR COALESCE(m.tool_name,'') LIKE ? ESCAPE '\' OR COALESCE(m.tool_calls,'') LIKE ? ESCAPE '\')`)
		args = append(args, pat, pat, pat)
	}
	if first == "" {
		return nil, nil
	}
	order := "ORDER BY m.timestamp DESC"
	if q.Sort == "oldest" {
		order = "ORDER BY m.timestamp ASC"
	}
	sqlText := fmt.Sprintf(`SELECT m.id, m.session_id, m.role,
		COALESCE(substr(m.content, max(1, instr(m.content, ?) - 40), 120), ''), %s
		FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE %s %s LIMIT ?`, selectTail, strings.Join(where, " AND "), order)
	return s.scanHits(ctx, sqlText, append(append([]any{first}, args...), q.Limit)...)
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func (s *Store) scanHits(ctx context.Context, q string, args ...any) ([]Hit, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: search: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Hit
	for rows.Next() {
		var h Hit
		var snippet *string
		if err := rows.Scan(&h.ID, &h.SessionID, &h.Role, &snippet, &h.Timestamp, &h.ToolName, &h.Source, &h.Model, &h.SessionStarted); err != nil {
			return nil, fmt.Errorf("state: search: %w", err)
		}
		if snippet != nil {
			h.Snippet = *snippet
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: search: %w", err)
	}
	return out, nil
}
