package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestRewind(t *testing.T) {
	calls := `[{"id":"c1","type":"function","function":{"name":"todo","arguments":"{}"}}]`
	turns := []Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "two"},
		{Role: "assistant", ToolCalls: calls},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
		{Role: "assistant", Content: "a2"},
	}
	tests := []struct {
		name         string
		n            int
		wantArchived int
		wantActive   []string
		wantCalls    int
	}{
		{"last turn", 1, 4, []string{"one", "a1"}, 0},
		{"zero means one", 0, 4, []string{"one", "a1"}, 0},
		{"two turns", 2, 6, nil, 0},
		{"clamps to oldest", 9, 6, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s := open(t, filepath.Join(t.TempDir(), "state.db"), Options{})
			if err := s.CreateSession(ctx, Session{ID: "a", Source: "cli"}); err != nil {
				t.Fatal(err)
			}
			for _, m := range turns {
				if _, err := s.AppendMessage(ctx, "a", m); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.Rewind(ctx, "a", tt.n)
			if err != nil || got != tt.wantArchived {
				t.Fatalf("Rewind = %d, %v", got, err)
			}
			msgs, err := s.Messages(ctx, "a")
			if err != nil {
				t.Fatal(err)
			}
			var contents []string
			for _, m := range msgs {
				contents = append(contents, m.Content)
			}
			if len(contents) != len(tt.wantActive) || (len(contents) > 0 && contents[0] != tt.wantActive[0]) {
				t.Errorf("active = %q", contents)
			}
			sess, _ := s.GetSession(ctx, "a")
			var rewinds, total int
			_ = s.db.QueryRow("SELECT rewind_count FROM sessions WHERE id = 'a'").Scan(&rewinds)
			_ = s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id = 'a'").Scan(&total)
			if sess.MessageCount != len(tt.wantActive) || sess.ToolCallCount != tt.wantCalls || rewinds != 1 || total != len(turns) {
				t.Errorf("session %+v rewinds=%d rows=%d", sess, rewinds, total)
			}
			if _, err := s.Rewind(ctx, "a", 1); len(tt.wantActive) == 0 && !errors.Is(err, ErrNoUserTurn) {
				t.Errorf("rewind of an empty transcript: %v", err)
			}
		})
	}
}

func TestToolCallsLen(t *testing.T) {
	for raw, want := range map[string]int{"": 0, "[]": 0, "[1,2]": 2, "null": 0, `"x"`: 1, `""`: 0, "{}": 0, "nope": 1, "true": 1} {
		if got := toolCallsLen(raw); got != want {
			t.Errorf("toolCallsLen(%q) = %d, want %d", raw, got, want)
		}
	}
}

func TestDeleteSessionIfEmpty(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"), Options{})
	for _, sess := range []Session{{ID: "empty"}, {ID: "titled", Title: "t"}, {ID: "parent"}, {ID: "child", ParentID: "parent"}, {ID: "talk"}, {ID: "undone"}} {
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"talk", "undone"} {
		if _, err := s.AppendMessage(ctx, id, Message{Role: "user", Content: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Rewind(ctx, "undone", 1); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"empty": true, "titled": false, "parent": false, "talk": false, "undone": false, "missing": false} {
		if got, err := s.DeleteSessionIfEmpty(ctx, id); err != nil || got != want {
			t.Errorf("%s: deleted=%v err=%v, want %v", id, got, err, want)
		}
	}
	if _, err := s.GetSession(ctx, "empty"); err == nil {
		t.Error("empty session still readable")
	}
}
