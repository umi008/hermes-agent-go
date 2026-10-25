package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxTitleLength is upstream SessionDB.MAX_TITLE_LENGTH.
const MaxTitleLength = 100

// previewMax is upstream _PREVIEW_MAX_CHARS.
const previewMax = 60

// ErrSessionExists reports a create for an id that already has a row.
var ErrSessionExists = errors.New("state: session already exists")

// TitleError is a rejected title; its text is upstream's ValueError message.
type TitleError struct{ msg string }

func (e *TitleError) Error() string { return e.msg }

// Record is the client-facing view of a sessions row, as upstream
// get_session and list_sessions_rich return it.
type Record struct {
	Session
	UserID          string
	ReasoningTokens int64
	APICallCount    int
	EstimatedCost   *float64
	ActualCost      *float64
	Pinned          bool
	Archived        bool
	Hidden          bool
	SystemPrompt    string
	ModelConfig     string
	// HasPreview is set by ListSessions, which computes LastActive and
	// Preview the way upstream's listing does.
	HasPreview bool
}

const recordCols = `id, source, COALESCE(model,''), COALESCE(title,''), COALESCE(cwd,''), COALESCE(parent_session_id,''),
	started_at, COALESCE(ended_at,0), COALESCE(end_reason,''), COALESCE(message_count,0), COALESCE(tool_call_count,0),
	COALESCE(input_tokens,0), COALESCE(output_tokens,0), COALESCE(cache_read_tokens,0), COALESCE(cache_write_tokens,0),
	COALESCE(user_id,''), COALESCE(reasoning_tokens,0), COALESCE(api_call_count,0), estimated_cost_usd, actual_cost_usd,
	pinned, archived, hidden, COALESCE(system_prompt,''), COALESCE(model_config,'')`

// lastActiveSQL is upstream's last_active: the later of the activity stamp
// and the newest message, falling back to started_at.
const lastActiveSQL = `COALESCE(MAX(COALESCE(s.last_activity_at, 0),
	COALESCE((SELECT MAX(timestamp) FROM messages WHERE session_id = s.id), 0)), s.started_at)`

// previewSQL is upstream _sql_preview_raw: the first visible user message.
const previewSQL = `COALESCE((SELECT content FROM messages WHERE session_id = s.id AND role = 'user'
	AND content IS NOT NULL AND COALESCE(display_kind, '') <> 'hidden' ORDER BY timestamp, id LIMIT 1), '')`

func scanRecord(sc interface{ Scan(...any) error }, extra ...any) (Record, error) {
	var r Record
	var est, act sql.NullFloat64
	dest := []any{&r.ID, &r.Source, &r.Model, &r.Title, &r.Cwd, &r.ParentID, &r.StartedAt, &r.EndedAt, &r.EndReason,
		&r.MessageCount, &r.ToolCallCount, &r.InputTokens, &r.OutputTokens, &r.CacheRead, &r.CacheWrite,
		&r.UserID, &r.ReasoningTokens, &r.APICallCount, &est, &act, &r.Pinned, &r.Archived, &r.Hidden,
		&r.SystemPrompt, &r.ModelConfig}
	if err := sc.Scan(append(dest, extra...)...); err != nil {
		return Record{}, err
	}
	if est.Valid {
		r.EstimatedCost = &est.Float64
	}
	if act.Valid {
		r.ActualCost = &act.Float64
	}
	return r, nil
}

// SessionRecord returns one session's client view or ErrNotFound.
func (s *Store) SessionRecord(ctx context.Context, id string) (Record, error) {
	r, err := scanRecord(s.db.QueryRowContext(ctx, "SELECT "+recordCols+" FROM sessions s WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("state: session record: %w", err)
	}
	return r, nil
}

// ListOptions filters ListSessions.
type ListOptions struct {
	Limit, Offset int
	// Source keeps one source only; empty keeps all.
	Source string
	// IncludeChildren keeps continuation children too.
	IncludeChildren bool
	// IncludeHidden keeps hidden rows.
	IncludeHidden bool
	// Search keeps rows whose title or id contains it.
	Search string
}

// modelConfigKey reads one key of the model_config JSON, NULL when absent
// or malformed.
func modelConfigKey(key string) string {
	return "(CASE WHEN json_valid(s.model_config) THEN json_extract(s.model_config, '$." + key + "') END)"
}

// ListSessions is upstream list_sessions_rich as /api/sessions calls it:
// newest activity first, archived rows out, hidden rows out unless asked,
// subagent runs and continuation children out unless asked (branches and
// resets stay), then every pinned row past the page appended.
func (s *Store) ListSessions(ctx context.Context, o ListOptions) ([]Record, error) {
	where := []string{modelConfigKey("_delegate_from") + " IS NULL"}
	var args []any
	if o.Source != "" {
		where = append(where, "s.source = ?")
		args = append(args, o.Source)
	}
	if !o.IncludeChildren {
		where = append(where, `(s.parent_session_id IS NULL OR `+modelConfigKey("_branched_from")+` IS NOT NULL
			OR `+modelConfigKey("_reset_from")+` IS NOT NULL
			OR EXISTS (SELECT 1 FROM sessions p WHERE p.id = s.parent_session_id AND p.end_reason = 'branched'))`)
	}
	if !o.IncludeHidden {
		where = append(where, "s.hidden = 0")
	}
	if o.Search != "" {
		where = append(where, "(s.title LIKE ? ESCAPE '\\' OR s.id LIKE ? ESCAPE '\\')")
		like := "%" + likeEscape(o.Search) + "%"
		args = append(args, like, like)
	}
	sel := "SELECT " + recordCols + ", " + lastActiveSQL + " AS last_active, " + previewSQL + " FROM sessions s WHERE "
	page := sel + strings.Join(append(where, "s.archived = 0"), " AND ") +
		" ORDER BY last_active DESC, s.started_at DESC, s.id DESC LIMIT ? OFFSET ?"
	out, err := s.queryRecords(ctx, page, append(append([]any{}, args...), o.Limit, o.Offset)...)
	if err != nil {
		return nil, err
	}
	// Pins survive archiving and the recency window.
	pins, err := s.queryRecords(ctx, sel+strings.Join(append(where, "s.pinned = 1"), " AND ")+" ORDER BY s.started_at DESC", args...)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, r := range out {
		seen[r.ID] = true
	}
	for _, r := range pins {
		if !seen[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Store) queryRecords(ctx context.Context, q string, args ...any) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Record
	for rows.Next() {
		var preview string
		var last float64
		r, err := scanRecord(rows, &last, &preview)
		if err != nil {
			return nil, fmt.Errorf("state: list sessions: %w", err)
		}
		r.LastActive, r.Preview, r.HasPreview = last, shapePreview(preview), true
		out = append(out, r)
	}
	return out, rows.Err()
}

// shapePreview is upstream _shape_preview without the skill rewrite.
func shapePreview(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if utf8.RuneCountInString(text) <= previewMax {
		return text
	}
	return string([]rune(text)[:previewMax]) + "..."
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

var (
	controlRE   = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
	invisibleRE = regexp.MustCompile(`[\x{200b}-\x{200f}\x{2028}-\x{202e}\x{2060}-\x{2069}\x{feff}\x{fffc}\x{fff9}-\x{fffb}]`)
	spaceRE     = regexp.MustCompile(`\s+`)
)

// SanitizeTitle is upstream sanitize_title: control and invisible
// characters go, whitespace collapses, and an empty result is "".
func SanitizeTitle(title string) (string, error) {
	title = strings.ToValidUTF8(title, "")
	title = invisibleRE.ReplaceAllString(controlRE.ReplaceAllString(title, ""), "")
	title = strings.TrimSpace(spaceRE.ReplaceAllString(title, " "))
	if n := utf8.RuneCountInString(title); n > MaxTitleLength {
		return "", &TitleError{fmt.Sprintf("Title too long (%d chars, max %d)", n, MaxTitleLength)}
	}
	return title, nil
}

type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// claimTitle is upstream _resolve_title_conflict: another row holding the
// title keeps it, unless it is an empty ended "ghost", which gives it up.
func claimTitle(ctx context.Context, x execer, id, title string) error {
	var other string
	var ghost bool
	err := x.QueryRowContext(ctx, `SELECT id, (COALESCE(message_count, 0) = 0 AND ended_at IS NOT NULL AND archived = 0
		AND hidden = 0 AND NOT EXISTS (SELECT 1 FROM messages WHERE session_id = sessions.id))
		FROM sessions WHERE title = ? AND id <> ? LIMIT 1`, title, id).Scan(&other, &ghost)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("state: title: %w", err)
	}
	if !ghost {
		return &TitleError{fmt.Sprintf("Title '%s' is already in use by session %s", title, other)}
	}
	if _, err := x.ExecContext(ctx, "UPDATE sessions SET title = NULL, title_source = NULL WHERE id = ?", other); err != nil {
		return fmt.Errorf("state: title: %w", err)
	}
	return nil
}

// RenameSession is upstream set_session_title from a user: the title is
// sanitized and must be free; "" clears it. A missing row is ErrNotFound.
func (s *Store) RenameSession(ctx context.Context, id, title string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	clean, err := SanitizeTitle(title)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: rename: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if clean != "" {
		if err := claimTitle(ctx, tx, id, clean); err != nil {
			return err
		}
	}
	src := any("user")
	if clean == "" {
		src = nil
	}
	res, err := tx.ExecContext(ctx, "UPDATE sessions SET title = ?, title_source = ? WHERE id = ?", null(clean), src, id)
	if err != nil {
		return fmt.Errorf("state: rename: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// NewAPISession is the row POST /api/sessions inserts. An empty Title
// leaves it unset.
type NewAPISession struct {
	ID, Source, Model, SystemPrompt, Title string
}

// ModelLock is the Browser model lock a session chat persists under
// model_config.browser_model_lock, as upstream update_session_runtime_lock.
type ModelLock struct {
	Provider    string          `json:"provider"`
	Model       string          `json:"model"`
	ModelOpts   json.RawMessage `json:"model_options"`
	RouteSource string          `json:"route_source"`
	Confirmed   bool            `json:"confirmed"`
	UpdatedAt   float64         `json:"updated_at"`
}

// SetModelLock persists a confirmed runtime lock: the lineage markers in
// model_config survive, the lock object is replaced wholesale, and a locked
// model becomes the row's model (COALESCE, as upstream). An unknown session
// is ErrNotFound.
func (s *Store) SetModelLock(ctx context.Context, sessionID string, lock ModelLock) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if len(lock.ModelOpts) == 0 {
		lock.ModelOpts = json.RawMessage(`{}`)
	}
	lock.UpdatedAt = s.timestamp()
	patch, err := json.Marshal(lock)
	if err != nil {
		return fmt.Errorf("state: model lock: %w", err)
	}
	var model any
	if lock.Model != "" {
		model = lock.Model
	}
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET
		model_config = json_set(COALESCE(NULLIF(model_config, ''), '{}'), '$.browser_model_lock', json(?)),
		model = COALESCE(?, model) WHERE id = ?`, string(patch), model, sessionID)
	if err != nil {
		return fmt.Errorf("state: model lock: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateAPISession inserts the row atomically with its title: an existing
// id is ErrSessionExists and a taken title a *TitleError, neither writing.
func (s *Store) CreateAPISession(ctx context.Context, n NewAPISession) error {
	if s.readOnly {
		return ErrReadOnly
	}
	clean, err := SanitizeTitle(n.Title)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: create session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ?", n.ID).Scan(&one); {
	case err == nil:
		return ErrSessionExists
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("state: create session: %w", err)
	}
	if clean != "" {
		if err := claimTitle(ctx, tx, n.ID, clean); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, source, created_source, model, system_prompt, started_at, title)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, n.ID, n.Source, n.Source, null(n.Model), null(n.SystemPrompt), s.timestamp(), null(clean)); err != nil {
		return fmt.Errorf("state: create session: %w", err)
	}
	return tx.Commit()
}

// Flag is a durable sidebar flag of a session.
type Flag int

// The flags PATCH /api/sessions sets.
const (
	FlagArchived Flag = iota
	FlagHidden
	FlagPinned
	FlagRead
)

// SetFlag is upstream set_session_archived / _hidden / _pinned / _read for
// one row: archiving clears auto_archived, pinning clears hidden, and read
// moves the last_read_at watermark (0 marks the session unread).
func (s *Store) SetFlag(ctx context.Context, id string, f Flag, on bool) error {
	if s.readOnly {
		return ErrReadOnly
	}
	var q string
	args := []any{on}
	switch f {
	case FlagArchived:
		q = "UPDATE sessions SET archived = ?, auto_archived = 0 WHERE id = ?"
	case FlagHidden:
		q = "UPDATE sessions SET hidden = ? WHERE id = ?"
	case FlagPinned:
		q = "UPDATE sessions SET pinned = ?, hidden = CASE WHEN ? THEN 0 ELSE hidden END WHERE id = ?"
		args = append(args, on)
	case FlagRead:
		q = "UPDATE sessions SET last_read_at = ? WHERE id = ?"
		args = []any{0.0}
		if on {
			args = []any{s.timestamp()}
		}
	default:
		return fmt.Errorf("state: unknown flag %d", f)
	}
	if _, err := s.db.ExecContext(ctx, q, append(args, id)...); err != nil {
		return fmt.Errorf("state: set flag: %w", err)
	}
	return nil
}

// DeleteSession removes a session and its messages; its children are kept
// as roots, as upstream delete_session does for branches. It reports
// whether the row existed.
func (s *Store) DeleteSession(ctx context.Context, id string) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("state: delete session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		"UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id = ?",
		"DELETE FROM messages WHERE session_id = ?",
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return false, fmt.Errorf("state: delete session: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id)
	if err != nil {
		return false, fmt.Errorf("state: delete session: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, tx.Commit()
}

// ClientMessage is a messages row as the client API projects it.
type ClientMessage struct {
	Message
	ReasoningContent string
	DisplayKind      string
}

// MessagePage is upstream get_messages(limit, offset, latest): active rows,
// oldest first; latest pages from the newest end.
func (s *Store) MessagePage(ctx context.Context, sessionID string, limit, offset int, latest bool) ([]ClientMessage, error) {
	dir := "ASC"
	if latest {
		dir = "DESC"
	}
	return s.clientMessages(ctx, sessionID, " ORDER BY id "+dir+" LIMIT ? OFFSET ?", limit, offset)
}

// ActiveMessages is upstream get_messages_as_conversation's row read: every
// active row of the session, oldest first and unbounded. A session chat seeds
// its turn with it.
func (s *Store) ActiveMessages(ctx context.Context, sessionID string) ([]ClientMessage, error) {
	return s.clientMessages(ctx, sessionID, " ORDER BY id")
}

// clientMessages reads the session's active rows with the given ordering and
// paging, reversing a descending page so callers always see oldest first.
func (s *Store) clientMessages(ctx context.Context, sessionID, tail string, args ...any) ([]ClientMessage, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+messageCols+`, COALESCE(reasoning_content,''), COALESCE(display_kind,'')
		FROM messages WHERE session_id = ? AND active = 1`+tail, append([]any{sessionID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("state: messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ClientMessage
	for rows.Next() {
		var m ClientMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.ToolCallID, &m.ToolName, &m.ToolCalls,
			&m.FinishReason, &m.Reasoning, &m.TokenCount, &m.Timestamp, &m.ReasoningContent, &m.DisplayKind); err != nil {
			return nil, fmt.Errorf("state: messages: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: messages: %w", err)
	}
	if len(tail) > 0 && strings.Contains(tail, "DESC") {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

var lineageRE = regexp.MustCompile(`^(.*?) #(\d+)$`)

// NextTitleInLineage is upstream get_next_title_in_lineage: "base" while
// free, else "base #N" one past the highest number in use.
func (s *Store) NextTitleInLineage(ctx context.Context, base string) (string, error) {
	if m := lineageRE.FindStringSubmatch(base); m != nil {
		base = m[1]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT title FROM sessions WHERE title = ? OR title LIKE ? ESCAPE '\'`,
		base, likeEscape(base)+" #%")
	if err != nil {
		return "", fmt.Errorf("state: lineage title: %w", err)
	}
	defer func() { _ = rows.Close() }()
	found, top := false, 1
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return "", fmt.Errorf("state: lineage title: %w", err)
		}
		found = true
		if m := lineageRE.FindStringSubmatch(t); m != nil && m[1] == base {
			if n, err := strconv.Atoi(m[2]); err == nil {
				top = max(top, n)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("state: lineage title: %w", err)
	}
	if !found {
		return base, nil
	}
	return fmt.Sprintf("%s #%d", base, top+1), nil
}

// ForkSession is upstream's API fork: a child of src marked _branched_from,
// sourced api_server, holding a copy of src's active messages; then src
// ends as "branched". An existing forkID is ErrSessionExists.
func (s *Store) ForkSession(ctx context.Context, src, forkID string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: fork: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ?", forkID).Scan(&one); {
	case err == nil:
		return ErrSessionExists
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("state: fork: %w", err)
	}
	now := s.timestamp()
	res, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, source, created_source, model, system_prompt, parent_session_id,
		model_config, started_at)
		SELECT ?, 'api_server', 'api_server', model, system_prompt, id, json_object('_branched_from', id), ? FROM sessions WHERE id = ?`,
		forkID, now, src)
	if err != nil {
		return fmt.Errorf("state: fork: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	steps := []struct {
		q    string
		args []any
	}{
		{"UPDATE sessions SET ended_at = ?, end_reason = 'branched' WHERE id = ? AND ended_at IS NULL", []any{s.timestamp(), src}},
		{`INSERT INTO messages (session_id, role, content, tool_call_id, tool_calls, tool_name, timestamp, token_count,
			finish_reason, reasoning, reasoning_content, display_kind, tool_call_uids)
			SELECT ?, role, content, tool_call_id, tool_calls, tool_name, timestamp, token_count, finish_reason, reasoning,
			reasoning_content, display_kind, tool_call_uids FROM messages WHERE session_id = ? AND active = 1 ORDER BY id`,
			[]any{forkID, src}},
		{`UPDATE sessions SET message_count = (SELECT COUNT(*) FROM messages WHERE session_id = ?),
			tool_call_count = (SELECT tool_call_count FROM sessions WHERE id = ?) WHERE id = ?`, []any{forkID, src, forkID}},
	}
	for _, st := range steps {
		if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
			return fmt.Errorf("state: fork: %w", err)
		}
	}
	return tx.Commit()
}
