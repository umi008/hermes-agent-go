package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNoUserTurn means there is no active user message to rewind to.
var ErrNoUserTurn = errors.New("state: no user turn to rewind")

// Rewind soft-archives the last n user turns of a session, as upstream
// rewind_user_turn with ordinal -n: every active row from the Nth-from-last
// user message onward gets active = 0 (kept for audit and search),
// rewind_count goes up, and the counters are recomputed from the active
// rows. n clamps to the oldest turn. It returns the number of archived rows.
func (s *Store) Rewind(ctx context.Context, sessionID string, n int) (int, error) {
	if s.readOnly {
		return 0, ErrReadOnly
	}
	if n < 1 {
		n = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("state: rewind: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	users, err := activeUserIDs(ctx, tx, sessionID)
	if err != nil {
		return 0, err
	}
	if len(users) == 0 {
		return 0, ErrNoUserTurn
	}
	cut := users[max(len(users)-n, 0)]
	res, err := tx.ExecContext(ctx, "UPDATE messages SET active = 0 WHERE session_id = ? AND id >= ? AND active = 1", sessionID, cut)
	if err != nil {
		return 0, fmt.Errorf("state: rewind: %w", err)
	}
	archived, _ := res.RowsAffected()
	msgs, calls, err := activeCounts(ctx, tx, sessionID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET rewind_count = COALESCE(rewind_count, 0) + 1,
		message_count = ?, tool_call_count = ? WHERE id = ?`, msgs, calls, sessionID); err != nil {
		return 0, fmt.Errorf("state: rewind: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("state: rewind: %w", err)
	}
	return int(archived), nil
}

func activeUserIDs(ctx context.Context, tx *sql.Tx, sessionID string) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM messages WHERE session_id = ? AND active = 1 AND role = 'user' ORDER BY id", sessionID)
	if err != nil {
		return nil, fmt.Errorf("state: rewind: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("state: rewind: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// activeCounts is upstream _active_transcript_counts: active rows, and
// tool calls counted as list length (1 for any other non-empty value).
func activeCounts(ctx context.Context, tx *sql.Tx, sessionID string) (msgs, calls int, err error) {
	rows, err := tx.QueryContext(ctx, "SELECT COALESCE(tool_calls, '') FROM messages WHERE session_id = ? AND active = 1", sessionID)
	if err != nil {
		return 0, 0, fmt.Errorf("state: rewind: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return 0, 0, fmt.Errorf("state: rewind: %w", err)
		}
		msgs++
		calls += toolCallsLen(raw)
	}
	return msgs, calls, rows.Err()
}

func toolCallsLen(raw string) int {
	if raw == "" {
		return 0
	}
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return 1
	}
	switch x := v.(type) {
	case []any:
		return len(x)
	case nil:
		return 0
	case bool:
		if !x {
			return 0
		}
	case string:
		if x == "" {
			return 0
		}
	case float64:
		if x == 0 {
			return 0
		}
	case map[string]any:
		if len(x) == 0 {
			return 0
		}
	}
	return 1
}

// DeleteSessionIfEmpty drops a session that never gained content, as
// upstream delete_session_if_empty: no messages (active or not), no title
// and no child sessions. Rows other tables still reference are kept. It
// reports whether the row went.
func (s *Store) DeleteSessionIfEmpty(ctx context.Context, id string) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ? AND title IS NULL
		AND NOT EXISTS (SELECT 1 FROM messages WHERE messages.session_id = sessions.id)
		AND NOT EXISTS (SELECT 1 FROM sessions child WHERE child.parent_session_id = sessions.id)
		AND NOT EXISTS (SELECT 1 FROM session_admissions a WHERE a.target_session_id = sessions.id)
		AND NOT EXISTS (SELECT 1 FROM worker_executions w WHERE w.session_id = sessions.id)`, id)
	if err != nil {
		return false, fmt.Errorf("state: delete empty session: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
