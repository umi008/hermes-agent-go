package apiserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"
)

// maxStoredResponses is upstream MAX_STORED_RESPONSES: the LRU bound of the
// response store.
const maxStoredResponses = 100

// StoredResponse is one /v1/responses record, as upstream ResponseStore
// rows: the response object, the transcript a chained request replays, the
// instructions it inherits and the session it continues.
type StoredResponse struct {
	Response            json.RawMessage `json:"response"`
	ConversationHistory []historyEntry  `json:"conversation_history"`
	Instructions        *string         `json:"instructions"`
	SessionID           string          `json:"session_id"`
}

// ResponseStore is upstream's SQLite-backed LRU of stored responses plus the
// conversation-name pointers. It is safe for concurrent use.
type ResponseStore struct {
	db  *sql.DB
	max int
}

// OpenResponseStore opens (or creates) the store at path.
func OpenResponseStore(ctx context.Context, path string) (*ResponseStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return openResponseStore(ctx, path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
}

// MemoryResponseStore is a store that lives as long as the process.
func MemoryResponseStore() *ResponseStore {
	s, err := openResponseStore(context.Background(), ":memory:")
	if err != nil {
		// The in-memory driver only fails on a broken build.
		panic(err)
	}
	return s
}

func openResponseStore(ctx context.Context, dsn string) (*ResponseStore, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: an in-memory database is per connection, and the LRU
	// bookkeeping must not interleave.
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		"CREATE TABLE IF NOT EXISTS responses (response_id TEXT PRIMARY KEY, data TEXT NOT NULL, accessed_at INTEGER NOT NULL)",
		"CREATE TABLE IF NOT EXISTS conversations (name TEXT PRIMARY KEY, response_id TEXT NOT NULL)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return &ResponseStore{db: db, max: maxStoredResponses}, nil
}

// Close releases the database.
func (s *ResponseStore) Close() error { return s.db.Close() }

// Get returns the record and marks it recently used.
func (s *ResponseStore) Get(ctx context.Context, id string) (StoredResponse, bool, error) {
	var data string
	err := s.db.QueryRowContext(ctx, "SELECT data FROM responses WHERE response_id = ?", id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredResponse{}, false, nil
	}
	if err != nil {
		return StoredResponse{}, false, err
	}
	var rec StoredResponse
	if err := json.Unmarshal([]byte(data), &rec); err != nil {
		return StoredResponse{}, false, err
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE responses SET accessed_at = "+nextAccess+" WHERE response_id = ?", id); err != nil {
		return StoredResponse{}, false, err
	}
	return rec, true, nil
}

// nextAccess orders the LRU by an increasing counter rather than wall time,
// so records written in the same instant still evict oldest first.
const nextAccess = "(SELECT COALESCE(MAX(accessed_at), 0) + 1 FROM responses)"

// Put stores or replaces the record, then evicts the least recently used
// records past the bound together with the conversation names that point at
// them.
func (s *ResponseStore) Put(ctx context.Context, id string, rec StoredResponse) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO responses (response_id, data, accessed_at) VALUES (?, ?, "+nextAccess+")", id, string(data)); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT response_id FROM responses ORDER BY accessed_at ASC LIMIT MAX((SELECT COUNT(*) FROM responses) - ?, 0)", s.max)
	if err != nil {
		return err
	}
	var evict []any
	for rows.Next() {
		var rid string
		if err := rows.Scan(&rid); err != nil {
			_ = rows.Close()
			return err
		}
		evict = append(evict, rid)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(evict) > 0 {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(evict)), ",")
		for _, table := range []string{"conversations", "responses"} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE response_id IN ("+marks+")", evict...); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Delete removes the record and the conversation names that point at it;
// false when there was none.
func (s *ResponseStore) Delete(ctx context.Context, id string) (bool, error) {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM conversations WHERE response_id = ?", id); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM responses WHERE response_id = ?", id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Conversation returns the latest response id of a named conversation.
func (s *ResponseStore) Conversation(ctx context.Context, name string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, "SELECT response_id FROM conversations WHERE name = ?", name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// SetConversation points a conversation name at a response.
func (s *ResponseStore) SetConversation(ctx context.Context, name, id string) error {
	_, err := s.db.ExecContext(ctx, "INSERT OR REPLACE INTO conversations (name, response_id) VALUES (?, ?)", name, id)
	return err
}
