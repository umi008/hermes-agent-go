package state

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func newCatalog(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s := open(t, filepath.Join(t.TempDir(), "state.db"), Options{})
	tick := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { tick = tick.Add(time.Second); return tick }
	return s, context.Background()
}

func ids(rs []Record) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

func TestCreateAPISessionConflictsAndTitles(t *testing.T) {
	s, ctx := newCatalog(t)
	if err := s.CreateAPISession(ctx, NewAPISession{ID: "a", Source: "api_server", SystemPrompt: "sp", Title: " My\u200b  title\n"}); err != nil {
		t.Fatal(err)
	}
	r, err := s.SessionRecord(ctx, "a")
	if err != nil || r.Title != "My title" || r.SystemPrompt != "sp" || r.Source != "api_server" {
		t.Fatalf("record = %+v, %v", r, err)
	}
	if err := s.CreateAPISession(ctx, NewAPISession{ID: "a", Source: "api_server"}); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("duplicate id: %v", err)
	}
	err = s.CreateAPISession(ctx, NewAPISession{ID: "b", Source: "api_server", Title: "My title"})
	if err == nil || err.Error() != "Title 'My title' is already in use by session a" {
		t.Fatalf("taken title: %v", err)
	}
	if _, err := s.SessionRecord(ctx, "b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused create wrote a row: %v", err)
	}
	if err := s.CreateAPISession(ctx, NewAPISession{ID: "c", Source: "api_server", Title: strings.Repeat("x", 101)}); err == nil ||
		err.Error() != "Title too long (101 chars, max 100)" {
		t.Fatalf("long title: %v", err)
	}
	// An empty ended ghost gives its title up.
	if err := s.EndSession(ctx, "a", "api_close"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAPISession(ctx, NewAPISession{ID: "d", Source: "api_server", Title: "My title"}); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.SessionRecord(ctx, "a"); r.Title != "" {
		t.Fatalf("ghost kept its title: %q", r.Title)
	}
}

func TestRenameSession(t *testing.T) {
	s, ctx := newCatalog(t)
	for _, id := range []string{"a", "b"} {
		if err := s.CreateAPISession(ctx, NewAPISession{ID: id, Source: "api_server"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AppendMessage(ctx, "a", Message{Role: "user", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameSession(ctx, "a", "Plan"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameSession(ctx, "b", "Plan"); err == nil || err.Error() != "Title 'Plan' is already in use by session a" {
		t.Fatalf("taken: %v", err)
	}
	if err := s.RenameSession(ctx, "a", ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.SessionRecord(ctx, "a"); r.Title != "" {
		t.Fatalf("clear: %q", r.Title)
	}
	if err := s.RenameSession(ctx, "zz", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestSetFlag(t *testing.T) {
	s, ctx := newCatalog(t)
	if err := s.CreateAPISession(ctx, NewAPISession{ID: "a", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []Flag{FlagHidden, FlagArchived} {
		if err := s.SetFlag(ctx, "a", f, true); err != nil {
			t.Fatal(err)
		}
	}
	if r, _ := s.SessionRecord(ctx, "a"); !r.Hidden || !r.Archived || r.Pinned {
		t.Fatalf("flags = %+v", r)
	}
	if err := s.SetFlag(ctx, "a", FlagPinned, true); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.SessionRecord(ctx, "a"); r.Hidden || !r.Pinned {
		t.Fatalf("a pin must unhide: %+v", r)
	}
	if err := s.SetFlag(ctx, "a", FlagRead, false); err != nil {
		t.Fatal(err)
	}
	var lastRead float64
	if err := s.db.QueryRow("SELECT last_read_at FROM sessions WHERE id = 'a'").Scan(&lastRead); err != nil || lastRead != 0 {
		t.Fatalf("unread watermark = %v, %v", lastRead, err)
	}
}

func TestListSessionsFilters(t *testing.T) {
	s, ctx := newCatalog(t)
	mk := func(id, source, parent, modelConfig string) {
		t.Helper()
		if err := s.CreateSession(ctx, Session{ID: id, Source: source, ParentID: parent}); err != nil {
			t.Fatal(err)
		}
		if modelConfig != "" {
			if _, err := s.db.Exec("UPDATE sessions SET model_config = ? WHERE id = ?", modelConfig, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("root", "api_server", "", "")
	mk("cli", "cli", "", "")
	mk("cont", "api_server", "root", "")
	mk("branch", "api_server", "root", `{"_branched_from":"root"}`)
	mk("sub", "api_server", "", `{"_delegate_from":"root"}`)
	mk("hid", "api_server", "", "")
	mk("arch", "api_server", "", "")
	mk("pin", "api_server", "", "")
	_ = s.SetFlag(ctx, "hid", FlagHidden, true)
	_ = s.SetFlag(ctx, "arch", FlagArchived, true)
	_ = s.SetFlag(ctx, "pin", FlagPinned, true)
	_ = s.SetFlag(ctx, "pin", FlagArchived, true)
	// Activity moves root to the front.
	if _, err := s.AppendMessage(ctx, "root", Message{Role: "user", Content: "line one\nline two " + strings.Repeat("y", 60)}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListSessions(ctx, ListOptions{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"root", "branch", "cli", "pin"}; !slices.Equal(ids(got), want) {
		t.Fatalf("default list = %v, want %v", ids(got), want)
	}
	if p := got[0].Preview; !strings.HasPrefix(p, "line one line two") || !strings.HasSuffix(p, "...") || len([]rune(p)) != 63 {
		t.Fatalf("preview = %q", p)
	}
	if got[0].LastActive <= got[1].LastActive {
		t.Fatalf("last_active order: %v", got)
	}

	got, _ = s.ListSessions(ctx, ListOptions{Limit: 50, IncludeChildren: true, IncludeHidden: true, Source: "api_server"})
	if want := []string{"root", "hid", "branch", "cont", "pin"}; !slices.Equal(ids(got), want) {
		t.Fatalf("wide list = %v, want %v", ids(got), want)
	}
	// The pinned back-fill lands past the page.
	got, _ = s.ListSessions(ctx, ListOptions{Limit: 1, Offset: 1})
	if want := []string{"branch", "pin"}; !slices.Equal(ids(got), want) {
		t.Fatalf("page = %v, want %v", ids(got), want)
	}
	_ = s.SetTitle(ctx, "cli", "50%_off")
	got, _ = s.ListSessions(ctx, ListOptions{Limit: 50, Search: "%_o"})
	if want := []string{"cli"}; !slices.Equal(ids(got), want) {
		t.Fatalf("search = %v, want %v", ids(got), want)
	}
}

func TestDeleteSessionKeepsChildren(t *testing.T) {
	s, ctx := newCatalog(t)
	_ = s.CreateSession(ctx, Session{ID: "p", Source: "api_server"})
	_ = s.CreateSession(ctx, Session{ID: "c", Source: "api_server", ParentID: "p"})
	if _, err := s.AppendMessage(ctx, "p", Message{Role: "user", Content: "searchable needle"}); err != nil {
		t.Fatal(err)
	}
	ok, err := s.DeleteSession(ctx, "p")
	if err != nil || !ok {
		t.Fatalf("delete = %v, %v", ok, err)
	}
	if _, err := s.SessionRecord(ctx, "p"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("row kept: %v", err)
	}
	if r, _ := s.SessionRecord(ctx, "c"); r.ParentID != "" {
		t.Fatalf("child parent = %q", r.ParentID)
	}
	if ms, _ := s.Messages(ctx, "p"); len(ms) != 0 {
		t.Fatalf("messages kept: %v", ms)
	}
	if ok, err := s.DeleteSession(ctx, "p"); ok || err != nil {
		t.Fatalf("second delete = %v, %v", ok, err)
	}
}

func TestMessagePage(t *testing.T) {
	s, ctx := newCatalog(t)
	_ = s.CreateSession(ctx, Session{ID: "a", Source: "api_server"})
	for _, c := range []string{"m1", "m2", "m3", "m4"} {
		if _, err := s.AppendMessage(ctx, "a", Message{Role: "user", Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	contents := func(ms []ClientMessage) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Content)
		}
		return out
	}
	latest, _ := s.MessagePage(ctx, "a", 2, 0, true)
	oldest, _ := s.MessagePage(ctx, "a", 2, 1, false)
	if !slices.Equal(contents(latest), []string{"m3", "m4"}) || !slices.Equal(contents(oldest), []string{"m2", "m3"}) {
		t.Fatalf("latest = %v, oldest = %v", contents(latest), contents(oldest))
	}
}

func TestForkSessionAndLineageTitle(t *testing.T) {
	s, ctx := newCatalog(t)
	if err := s.CreateAPISession(ctx, NewAPISession{ID: "src", Source: "api_server", Model: "m", SystemPrompt: "sp", Title: "Plan"}); err != nil {
		t.Fatal(err)
	}
	_, _ = s.AppendMessage(ctx, "src", Message{Role: "user", Content: "q"})
	_, _ = s.AppendMessage(ctx, "src", Message{Role: "assistant", ToolCalls: `[{"id":"c1"}]`})
	if title, _ := s.NextTitleInLineage(ctx, "Fresh"); title != "Fresh" {
		t.Fatalf("free base = %q", title)
	}
	if err := s.ForkSession(ctx, "src", "f1"); err != nil {
		t.Fatal(err)
	}
	if title, _ := s.NextTitleInLineage(ctx, "Plan"); title != "Plan #2" {
		t.Fatalf("first fork title = %q", title)
	}
	_ = s.RenameSession(ctx, "f1", "Plan #2")
	if title, _ := s.NextTitleInLineage(ctx, "Plan #2"); title != "Plan #3" {
		t.Fatalf("second fork title = %q", title)
	}
	f, _ := s.SessionRecord(ctx, "f1")
	if f.ParentID != "src" || f.Source != "api_server" || f.Model != "m" || f.SystemPrompt != "sp" ||
		f.ModelConfig != `{"_branched_from":"src"}` || f.MessageCount != 2 || f.ToolCallCount != 1 {
		t.Fatalf("fork = %+v", f)
	}
	if ms, _ := s.Messages(ctx, "f1"); len(ms) != 2 || ms[0].Content != "q" || ms[1].ToolCalls != `[{"id":"c1"}]` {
		t.Fatalf("fork messages = %+v", ms)
	}
	if src, _ := s.SessionRecord(ctx, "src"); src.EndReason != "branched" {
		t.Fatalf("source end = %q", src.EndReason)
	}
	if err := s.ForkSession(ctx, "src", "f1"); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("existing fork id: %v", err)
	}
	if err := s.ForkSession(ctx, "nope", "f2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing source: %v", err)
	}
	got, _ := s.ListSessions(ctx, ListOptions{Limit: 50})
	if !slices.Contains(ids(got), "f1") {
		t.Fatalf("a fork must stay listed: %v", ids(got))
	}
}

// TestStoreActiveMessages pins the conversation read a session chat seeds
// with: every active row, oldest first, and nothing else.
func TestStoreActiveMessages(t *testing.T) {
	s, ctx := newCatalog(t)
	_ = s.CreateSession(ctx, Session{ID: "a", Source: "api_server"})
	if _, err := s.AppendMessage(ctx, "a", Message{Role: "user", Content: "q1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, "a", Message{Role: "assistant", Content: "a1",
		ToolCalls: `[{"id":"c1"}]`, Reasoning: "why"}); err != nil {
		t.Fatal(err)
	}
	// An archived row is part of the compacted past, not of the live transcript.
	if _, err := s.db.Exec("UPDATE messages SET active = 0 WHERE content = 'a1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, "a", Message{Role: "tool", Content: "out", ToolCallID: "c1", ToolName: "shell"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ActiveMessages(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Content != "q1" || rows[1].Content != "out" || rows[1].ToolName != "shell" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows, err := s.ActiveMessages(ctx, "nope"); err != nil || len(rows) != 0 {
		t.Fatalf("unknown session = %+v, %v", rows, err)
	}
}

// TestStoreSetModelLock pins the persisted Browser model lock: the row keeps
// its lineage marker, gains the lock object, and adopts the locked model.
func TestStoreSetModelLock(t *testing.T) {
	s, ctx := newCatalog(t)
	if err := s.CreateAPISession(ctx, NewAPISession{ID: "a", Source: "api_server", Title: "Plan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE sessions SET model_config = ? WHERE id = ?",
		`{"_branched_from":"src"}`, "a"); err != nil {
		t.Fatal(err)
	}
	lock := ModelLock{Model: "sonnet-4", Provider: "anthropic", RouteSource: "raw_request", Confirmed: true}
	if err := s.SetModelLock(ctx, "a", lock); err != nil {
		t.Fatal(err)
	}
	rec, err := s.SessionRecord(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Model != "sonnet-4" {
		t.Fatalf("locked model = %q", rec.Model)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rec.ModelConfig), &cfg); err != nil {
		t.Fatalf("model_config = %s: %v", rec.ModelConfig, err)
	}
	if string(cfg["_branched_from"]) != `"src"` {
		t.Fatalf("lineage marker lost: %s", rec.ModelConfig)
	}
	var stored ModelLock
	if err := json.Unmarshal(cfg["browser_model_lock"], &stored); err != nil {
		t.Fatalf("lock = %v", err)
	}
	if stored.Model != "sonnet-4" || stored.Provider != "anthropic" || stored.RouteSource != "raw_request" || !stored.Confirmed {
		t.Fatalf("lock = %+v", stored)
	}
	// A prior lock is replaced wholesale, not merged field by field.
	if err := s.SetModelLock(ctx, "a", ModelLock{Model: "haiku", Confirmed: true}); err != nil {
		t.Fatal(err)
	}
	rec, _ = s.SessionRecord(ctx, "a")
	if err := json.Unmarshal([]byte(rec.ModelConfig), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(cfg["browser_model_lock"], &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Model != "haiku" || stored.Provider != "" || rec.Model != "haiku" {
		t.Fatalf("replaced lock = %+v, row model %q", stored, rec.Model)
	}
	if err := s.SetModelLock(ctx, "nope", lock); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
}
