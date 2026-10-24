// Package state is the session store: an SQLite database with upstream
// Hermes' schema v32 (hermes_state*.py), written with upstream column
// semantics so either implementation can read the other's history.
package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"
)

// SchemaVersion is the upstream schema this package writes.
const SchemaVersion = 32

//go:embed schema.sql
var schemaSQL string

// ErrReadOnly reports a write to a store opened read-only, or to one whose
// schema version this package does not write.
var ErrReadOnly = errors.New("state: store is read-only")

// ErrNotFound reports a missing session.
var ErrNotFound = errors.New("state: not found")

// Options configure Open.
type Options struct {
	// ReadOnly opens an existing store for reading only.
	ReadOnly bool
	// Now is the clock; nil uses time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Store is an open session database.
type Store struct {
	db       *sql.DB
	readOnly bool
	now      func() time.Time
	// tsMu guards lastTS: several chat sessions write the store from their
	// own goroutines, and the timestamps must stay strictly increasing.
	tsMu   sync.Mutex
	lastTS float64
}

// Open opens path, creating it with the upstream schema when it does not
// exist (unless read-only).
func Open(ctx context.Context, path string, o Options) (*Store, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	_, statErr := os.Stat(path)
	fresh := errors.Is(statErr, os.ErrNotExist)
	if fresh && o.ReadOnly {
		return nil, fmt.Errorf("state: %s: %w", path, os.ErrNotExist)
	}
	if fresh {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)"
	if o.ReadOnly {
		dsn += "&_pragma=query_only(1)"
	} else {
		dsn += "&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("state: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // one writer; the CLI has no concurrent readers
	s := &Store{db: db, readOnly: o.ReadOnly, now: o.Now}
	if fresh {
		err = s.create(ctx)
	} else {
		err = s.checkVersion(ctx, o.Logger, path)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) create(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range strings.Split(schemaSQL, "-- ;;") {
		if stmt = strings.TrimSpace(stmt); stmt == "" || isComment(stmt) {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("state: create schema: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_version(version) VALUES (?)", SchemaVersion); err != nil {
		return fmt.Errorf("state: create: %w", err)
	}
	meta := [][2]string{
		{"store_instance_id", uuid4()},
		{"store_created_at_utc", s.now().UTC().Format("2006-01-02T15:04:05.000000+00:00")},
		{"message_uid_backfill", "1"},
		{"fts_storage_version", "3"},
		{"db_file_generation", newUID()},
	}
	for _, kv := range meta {
		if _, err := tx.ExecContext(ctx, "INSERT INTO state_meta(key, value) VALUES (?, ?)", kv[0], kv[1]); err != nil {
			return fmt.Errorf("state: create: %w", err)
		}
	}
	return tx.Commit()
}

func isComment(stmt string) bool {
	for _, line := range strings.Split(stmt, "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "--") {
			return false
		}
	}
	return true
}

func (s *Store) checkVersion(ctx context.Context, log *slog.Logger, path string) error {
	var v int
	if err := s.db.QueryRowContext(ctx, "SELECT version FROM schema_version LIMIT 1").Scan(&v); err != nil {
		return fmt.Errorf("state: %s is not a Hermes state store: %w", path, err)
	}
	if v != SchemaVersion && !s.readOnly {
		log.Warn("state store schema differs; opening read-only", "path", path, "version", v, "supported", SchemaVersion)
		s.readOnly = true
		if _, err := s.db.ExecContext(ctx, "PRAGMA query_only = 1"); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	return nil
}

// ReadOnly reports whether writes are refused.
func (s *Store) ReadOnly() bool { return s.readOnly }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// timestamp returns a strictly increasing unix time, as upstream keeps
// message timestamps ordered within a process. It is safe for concurrent use:
// every chat session writes through it.
func (s *Store) timestamp() float64 {
	s.tsMu.Lock()
	defer s.tsMu.Unlock()
	ts := float64(s.now().UnixNano()) / 1e9
	if ts <= s.lastTS {
		ts = s.lastTS + 1e-6
	}
	s.lastTS = ts
	return ts
}

// Session is a sessions row (the columns this port reads or writes).
type Session struct {
	ID            string
	Source        string
	Model         string
	Title         string
	Cwd           string
	ParentID      string
	StartedAt     float64
	EndedAt       float64 // 0 while open
	EndReason     string
	MessageCount  int
	ToolCallCount int
	InputTokens   int64
	OutputTokens  int64
	CacheRead     int64
	CacheWrite    int64
	LastActive    float64
	Preview       string // first user message, set by RecentSessions
}

// CreateSession inserts a session; an existing id is left unchanged.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if sess.StartedAt == 0 {
		sess.StartedAt = s.timestamp()
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO sessions
		(id, source, created_source, model, cwd, parent_session_id, started_at, title)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.Source, sess.Source, null(sess.Model), null(sess.Cwd), null(sess.ParentID), sess.StartedAt, null(sess.Title))
	if err != nil {
		return fmt.Errorf("state: create session: %w", err)
	}
	return nil
}

// EndSession stamps ended_at; the first end reason wins, as upstream.
func (s *Store) EndSession(ctx context.Context, id, reason string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL",
		s.timestamp(), reason, id)
	if err != nil {
		return fmt.Errorf("state: end session: %w", err)
	}
	return nil
}

// Usage is one model call's token counts.
type Usage struct {
	Input, Output, CacheRead, CacheWrite int64
}

// AddUsage adds token counts to a session and counts one API call.
func (s *Store) AddUsage(ctx context.Context, id string, u Usage) error {
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET
		input_tokens = input_tokens + ?, output_tokens = output_tokens + ?,
		cache_read_tokens = cache_read_tokens + ?, cache_write_tokens = cache_write_tokens + ?,
		api_call_count = COALESCE(api_call_count, 0) + 1, last_activity_at = ?
		WHERE id = ?`, u.Input, u.Output, u.CacheRead, u.CacheWrite, s.timestamp(), id)
	if err != nil {
		return fmt.Errorf("state: add usage: %w", err)
	}
	return nil
}

// SetTitle sets a session title.
func (s *Store) SetTitle(ctx context.Context, id, title string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE sessions SET title = ? WHERE id = ?", null(title), id); err != nil {
		return fmt.Errorf("state: set title: %w", err)
	}
	return nil
}

const sessionCols = `id, source, COALESCE(model,''), COALESCE(title,''), COALESCE(cwd,''), COALESCE(parent_session_id,''),
	started_at, COALESCE(ended_at,0), COALESCE(end_reason,''), COALESCE(message_count,0), COALESCE(tool_call_count,0),
	COALESCE(input_tokens,0), COALESCE(output_tokens,0), COALESCE(cache_read_tokens,0), COALESCE(cache_write_tokens,0),
	COALESCE(last_activity_at, ended_at, started_at)`

func scanSession(sc interface{ Scan(...any) error }) (Session, error) {
	var x Session
	err := sc.Scan(&x.ID, &x.Source, &x.Model, &x.Title, &x.Cwd, &x.ParentID, &x.StartedAt, &x.EndedAt, &x.EndReason,
		&x.MessageCount, &x.ToolCallCount, &x.InputTokens, &x.OutputTokens, &x.CacheRead, &x.CacheWrite, &x.LastActive)
	return x, err
}

// GetSession returns one session or ErrNotFound.
func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	x, err := scanSession(s.db.QueryRowContext(ctx, "SELECT "+sessionCols+" FROM sessions WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("state: get session: %w", err)
	}
	return x, nil
}

// RecentSessions returns sessions newest first, skipping excludeSources.
func (s *Store) RecentSessions(ctx context.Context, limit int, excludeSources []string) ([]Session, error) {
	q := "SELECT " + sessionCols + `, COALESCE((SELECT substr(content, 1, 60) FROM messages
		WHERE session_id = sessions.id AND role = 'user' AND active = 1 ORDER BY id LIMIT 1), '') FROM sessions`
	var args []any
	if len(excludeSources) > 0 {
		q += " WHERE source NOT IN (" + placeholders(len(excludeSources)) + ")"
		for _, src := range excludeSources {
			args = append(args, src)
		}
	}
	q += " ORDER BY started_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: recent sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Session
	for rows.Next() {
		var x Session
		if err := rows.Scan(&x.ID, &x.Source, &x.Model, &x.Title, &x.Cwd, &x.ParentID, &x.StartedAt, &x.EndedAt, &x.EndReason,
			&x.MessageCount, &x.ToolCallCount, &x.InputTokens, &x.OutputTokens, &x.CacheRead, &x.CacheWrite, &x.LastActive,
			&x.Preview); err != nil {
			return nil, fmt.Errorf("state: recent sessions: %w", err)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Message is a messages row.
type Message struct {
	ID           int64
	SessionID    string
	Role         string
	Content      string
	ToolCallID   string
	ToolName     string
	ToolCalls    string // JSON array as sent by the model, or ""
	FinishReason string
	Reasoning    string
	TokenCount   int
	Timestamp    float64
}

// AppendMessage inserts a message and bumps the session counters, as
// upstream append_message. It returns the row id.
func (s *Store) AppendMessage(ctx context.Context, sessionID string, m Message) (int64, error) {
	if s.readOnly {
		return 0, ErrReadOnly
	}
	if m.Timestamp == 0 {
		m.Timestamp = s.timestamp()
	}
	callUIDs, calls := toolCallUIDs(m.ToolCalls)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("state: append message: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `INSERT INTO messages
		(session_id, role, content, tool_call_id, tool_calls, tool_name, timestamp, token_count, finish_reason, reasoning,
		 message_uid, tool_call_uids)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, m.Role, null(m.Content), null(m.ToolCallID), null(m.ToolCalls), null(m.ToolName), m.Timestamp,
		nullInt(m.TokenCount), null(m.FinishReason), null(m.Reasoning), newUID(), null(callUIDs))
	if err != nil {
		return 0, fmt.Errorf("state: append message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("state: append message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET message_count = message_count + 1,
		tool_call_count = tool_call_count + ? WHERE id = ?`, calls, sessionID); err != nil {
		return 0, fmt.Errorf("state: append message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("state: append message: %w", err)
	}
	return id, nil
}

// toolCallUIDs mints upstream's per-occurrence ids: {"<call id>": "<uid>"}.
func toolCallUIDs(raw string) (string, int) {
	if raw == "" {
		return "", 0
	}
	var calls []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(raw), &calls) != nil {
		return "", 0
	}
	uids := map[string]string{}
	for _, c := range calls {
		if c.ID != "" {
			uids[c.ID] = newUID()
		}
	}
	if len(uids) == 0 {
		return "", len(calls)
	}
	b, _ := json.Marshal(uids)
	return string(b), len(calls)
}

const messageCols = `id, session_id, role, COALESCE(content,''), COALESCE(tool_call_id,''), COALESCE(tool_name,''),
	COALESCE(tool_calls,''), COALESCE(finish_reason,''), COALESCE(reasoning,''), COALESCE(token_count,0), timestamp`

func (s *Store) queryMessages(ctx context.Context, q string, args ...any) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.ToolCallID, &m.ToolName, &m.ToolCalls,
			&m.FinishReason, &m.Reasoning, &m.TokenCount, &m.Timestamp); err != nil {
			return nil, fmt.Errorf("state: messages: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Messages returns a session's active messages in order.
func (s *Store) Messages(ctx context.Context, sessionID string) ([]Message, error) {
	return s.queryMessages(ctx, "SELECT "+messageCols+" FROM messages WHERE session_id = ? AND active = 1 ORDER BY id", sessionID)
}

// Window is a slice of a session around an anchor message.
type Window struct {
	Messages []Message
	Before   int // messages strictly before the anchor in the window
	After    int
}

// MessagesAround returns up to window messages either side of anchor, as
// upstream get_messages_around. A foreign anchor yields an empty window.
func (s *Store) MessagesAround(ctx context.Context, sessionID string, anchor int64, window int) (Window, error) {
	window = max(window, 0)
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM messages WHERE id = ? AND session_id = ? LIMIT 1", anchor, sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return Window{}, nil
	}
	if err != nil {
		return Window{}, fmt.Errorf("state: messages around: %w", err)
	}
	before, err := s.queryMessages(ctx, "SELECT "+messageCols+" FROM messages WHERE session_id = ? AND id <= ? ORDER BY id DESC LIMIT ?",
		sessionID, anchor, window+1)
	if err != nil {
		return Window{}, err
	}
	after, err := s.queryMessages(ctx, "SELECT "+messageCols+" FROM messages WHERE session_id = ? AND id > ? ORDER BY id ASC LIMIT ?",
		sessionID, anchor, window)
	if err != nil {
		return Window{}, err
	}
	w := Window{Before: max(0, len(before)-1), After: len(after)}
	for i := len(before) - 1; i >= 0; i-- {
		w.Messages = append(w.Messages, before[i])
	}
	w.Messages = append(w.Messages, after...)
	return w, nil
}

func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func newUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewSessionID returns an upstream-style session id: a timestamp plus a
// random suffix (YYYYMMDD_HHMMSS_xxxxxx).
func NewSessionID(now time.Time) string {
	return now.Format("20060102_150405") + "_" + newUID()[:6]
}

func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// AnchoredView is MessagesAround filtered to user and assistant rows (the
// anchor always kept) plus up to bookend non-empty user/assistant messages
// strictly before and after the window, as upstream get_anchored_view.
type AnchoredView struct {
	Window
	Start, End []Message
}

// Anchored returns the anchored view, or an empty one for a foreign anchor.
func (s *Store) Anchored(ctx context.Context, sessionID string, anchor int64, window, bookend int) (AnchoredView, error) {
	w, err := s.MessagesAround(ctx, sessionID, anchor, window)
	if err != nil || len(w.Messages) == 0 {
		return AnchoredView{}, err
	}
	v := AnchoredView{Window: Window{Before: w.Before, After: w.After}}
	for _, m := range w.Messages {
		if m.ID == anchor || m.Role == "user" || m.Role == "assistant" {
			v.Messages = append(v.Messages, m)
		}
	}
	if bookend <= 0 {
		return v, nil
	}
	const roles = " AND role IN ('user','assistant') AND length(content) > 0"
	if v.Start, err = s.queryMessages(ctx, "SELECT "+messageCols+" FROM messages WHERE session_id = ? AND id < ?"+roles+" ORDER BY id ASC LIMIT ?",
		sessionID, w.Messages[0].ID, bookend); err != nil {
		return AnchoredView{}, err
	}
	end, err := s.queryMessages(ctx, "SELECT "+messageCols+" FROM messages WHERE session_id = ? AND id > ?"+roles+" ORDER BY id DESC LIMIT ?",
		sessionID, w.Messages[len(w.Messages)-1].ID, bookend)
	if err != nil {
		return AnchoredView{}, err
	}
	for i := len(end) - 1; i >= 0; i-- {
		v.End = append(v.End, end[i])
	}
	return v, nil
}
