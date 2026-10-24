package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var shadowRE = regexp.MustCompile(`^messages_fts(_trigram)?_(data|idx|docsize|config)$`)

func copyFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/upstream_v32.db")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func open(t *testing.T, path string, o Options) *Store {
	t.Helper()
	s, err := Open(context.Background(), path, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func schemaObjects(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT type || ':' || name FROM sqlite_master WHERE sql IS NOT NULL ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		if name := n[strings.IndexByte(n, ':')+1:]; name != "sqlite_sequence" && !shadowRE.MatchString(name) {
			out = append(out, n)
		}
	}
	return out
}

func TestFreshStoreMatchesUpstreamSchema(t *testing.T) {
	fresh := open(t, filepath.Join(t.TempDir(), "sub", "state.db"), Options{})
	upstream := open(t, copyFixture(t), Options{ReadOnly: true})
	got, want := schemaObjects(t, fresh.db), schemaObjects(t, upstream.db)
	if !slices.Equal(got, want) {
		t.Fatalf("schema objects differ:\n got %v\nwant %v", got, want)
	}
	var v int
	if err := fresh.db.QueryRow("SELECT version FROM schema_version").Scan(&v); err != nil || v != SchemaVersion {
		t.Fatalf("version %d %v", v, err)
	}
}

func TestReadsUpstreamFixture(t *testing.T) {
	ctx := context.Background()
	s := open(t, copyFixture(t), Options{})
	sess, err := s.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Source != "cli" || sess.Model != "m1" || sess.MessageCount != 4 || sess.ToolCallCount != 1 || sess.EndReason != "cli_close" {
		t.Fatalf("session %+v", sess)
	}
	msgs, err := s.Messages(ctx, "s1")
	if err != nil || len(msgs) != 4 || msgs[2].ToolName != "terminal" || msgs[2].ToolCallID != "c1" {
		t.Fatalf("messages %+v %v", msgs, err)
	}
	hits, err := s.Search(ctx, Query{Text: "kubernetes"})
	if err != nil || len(hits) != 1 || !strings.Contains(hits[0].Snippet, ">>>kubernetes<<<") || hits[0].Source != "cli" {
		t.Fatalf("hits %+v %v", hits, err)
	}
	// Writes keep working on an upstream store at the same schema version.
	if _, err := s.AppendMessage(ctx, "s1", Message{Role: "user", Content: "more"}); err != nil {
		t.Fatal(err)
	}
}

func TestWriteAndSearch(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	s := open(t, filepath.Join(t.TempDir(), "state.db"), Options{Now: func() time.Time { return clock }})
	if err := s.CreateSession(ctx, Session{ID: "a", Source: "cli", Model: "tiny", Cwd: "/w"}); err != nil {
		t.Fatal(err)
	}
	calls := `[{"id":"c1","type":"function","function":{"name":"terminal","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"read_file","arguments":"{}"}}]`
	for _, m := range []Message{
		{Role: "user", Content: "please configure the nginx reverse proxy"},
		{Role: "assistant", ToolCalls: calls},
		{Role: "tool", ToolCallID: "c1", ToolName: "terminal", Content: "nginx: configuration ok"},
		{Role: "assistant", Content: "The proxy is configured."},
	} {
		if _, err := s.AppendMessage(ctx, "a", m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddUsage(ctx, "a", Usage{Input: 10, Output: 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.EndSession(ctx, "a", "cli_close"); err != nil {
		t.Fatal(err)
	}
	if err := s.EndSession(ctx, "a", "later"); err != nil {
		t.Fatal(err)
	}
	sess, err := s.GetSession(ctx, "a")
	if err != nil || sess.MessageCount != 4 || sess.ToolCallCount != 2 || sess.InputTokens != 10 || sess.EndReason != "cli_close" {
		t.Fatalf("session %+v %v", sess, err)
	}
	var uid, uids string
	var order int64
	if err := s.db.QueryRow("SELECT message_uid, tool_call_uids, display_order FROM messages WHERE id = 2").Scan(&uid, &uids, &order); err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if len(uid) != 32 || json.Unmarshal([]byte(uids), &m) != nil || len(m) != 2 || order != 2 {
		t.Fatalf("uid %q uids %q order %d", uid, uids, order)
	}

	tests := []struct {
		name  string
		q     Query
		wants []int64
	}{
		{"fts", Query{Text: "nginx"}, []int64{1, 3}},
		{"roles exclude tool by filter", Query{Text: "nginx", Roles: []string{"user", "assistant"}}, []int64{1}},
		{"tool role uses like", Query{Text: "configuration", Roles: []string{"tool"}}, []int64{3}},
		{"trigram substring", Query{Text: "everse"}, []int64{1}},
		{"or relaxed", Query{Text: "proxy banana"}, []int64{1, 4}},
		{"source excluded", Query{Text: "nginx", ExcludeSources: []string{"cli"}}, nil},
		{"after bound", Query{Text: "nginx", After: ptr(clock.Unix() + 10)}, nil},
		{"syntax error is empty", Query{Text: `"unterminated`}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits, err := s.Search(ctx, tt.q)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, h := range hits {
				ids = append(ids, h.ID)
			}
			slices.Sort(ids)
			if !slices.Equal(ids, tt.wants) {
				t.Fatalf("ids %v want %v (%+v)", ids, tt.wants, hits)
			}
		})
	}

	w, err := s.MessagesAround(ctx, "a", 2, 1)
	if err != nil || len(w.Messages) != 3 || w.Before != 1 || w.After != 1 {
		t.Fatalf("window %+v %v", w, err)
	}
	if w, _ := s.MessagesAround(ctx, "b", 2, 1); len(w.Messages) != 0 {
		t.Fatalf("foreign anchor %+v", w)
	}
	recent, err := s.RecentSessions(ctx, 5, nil)
	if err != nil || len(recent) != 1 || recent[0].Preview != "please configure the nginx reverse proxy" {
		t.Fatalf("recent %+v %v", recent, err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestOtherSchemaVersionOpensReadOnly(t *testing.T) {
	p := copyFixture(t)
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE schema_version SET version = 33"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s := open(t, p, Options{})
	if !s.ReadOnly() {
		t.Fatal("want read-only")
	}
	if _, err := s.AppendMessage(context.Background(), "s1", Message{Role: "user", Content: "x"}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("err %v", err)
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open(context.Background(), filepath.Join(t.TempDir(), "none.db"), Options{ReadOnly: true}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing read-only: %v", err)
	}
	p := filepath.Join(t.TempDir(), "other.db")
	db, _ := sql.Open("sqlite", p)
	_, _ = db.Exec("CREATE TABLE x(y)")
	_ = db.Close()
	if _, err := Open(context.Background(), p, Options{}); err == nil || !strings.Contains(err.Error(), "not a Hermes state store") {
		t.Fatalf("foreign db: %v", err)
	}
}

func TestSanitizeMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/sanitize.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases [][3]*string
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		got := SanitizeQuery(*c[0])
		if got != *c[1] {
			t.Errorf("sanitize(%q) = %q, want %q", *c[0], got, *c[1])
		}
		want := ""
		if c[2] != nil {
			want = *c[2]
		}
		if r := orRelaxed(got); r != want {
			t.Errorf("orRelaxed(%q) = %q, want %q", got, r, want)
		}
	}
}

// TestConcurrentAppendMessage covers the seam a chat runtime with several live
// sessions needs: two sessions write the store from their own goroutines.
func TestConcurrentAppendMessage(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, id := range []string{"a", "b"} {
		if err := st.CreateSession(ctx, Session{ID: id, Source: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 50; i++ {
				if _, err := st.AppendMessage(ctx, id, Message{Role: "user", Content: fmt.Sprintf("%s-%d", id, i)}); err != nil {
					t.Errorf("append %s: %v", id, err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, id := range []string{"a", "b"} {
		msgs, err := st.Messages(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 50 {
			t.Fatalf("%s: %d messages, want 50", id, len(msgs))
		}
	}
}
