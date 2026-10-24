package sessionsearch

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/state"
	"github.com/umi008/hermes-agent-go/internal/tools"
)

func setup(t *testing.T) *tools.Registry {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	st, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"), state.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, s := range []struct {
		id, source string
		msgs       []string
	}{
		{"old", "cli", []string{"set up the postgres replica", "Replica configured on port 5433"}},
		{"cron1", "cron", []string{"nightly postgres backup", "backup done"}},
		{"cur", "cli", []string{"postgres question again", "answer"}},
		{"hidden", "subagent", []string{"postgres internals", "x"}},
	} {
		if err := st.CreateSession(ctx, state.Session{ID: s.id, Source: s.source, Model: "tiny"}); err != nil {
			t.Fatal(err)
		}
		for i, c := range s.msgs {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			if _, err := st.AppendMessage(ctx, s.id, state.Message{Role: role, Content: c}); err != nil {
				t.Fatal(err)
			}
		}
	}
	r, err := tools.NewRegistry(Tool(Options{Store: st, Current: func() string { return "cur" }, Profile: "default",
		Now: func() time.Time { return now }}))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func call(t *testing.T, r *tools.Registry, args string) map[string]any {
	t.Helper()
	out, err := r.Execute(context.Background(), llm.ToolCall{Name: "session_search", Arguments: json.RawMessage(args)})
	if err != nil {
		out = err.Error()
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return m
}

func TestDiscover(t *testing.T) {
	r := setup(t)
	m := call(t, r, `{"query":"postgres","limit":5}`)
	results := m["results"].([]any)
	if m["mode"] != "discover" || len(results) != 2 {
		t.Fatalf("%v", m)
	}
	first, second := results[0].(map[string]any), results[1].(map[string]any)
	// Current and hidden sessions are skipped; cron is demoted below cli.
	if first["session_id"] != "old" || second["session_id"] != "cron1" {
		t.Fatalf("order %v %v", first["session_id"], second["session_id"])
	}
	if first["detail"] != "full" || second["detail"] != "compact" || first["link"] != "@session:default/old" {
		t.Fatalf("%v", first)
	}
	if !strings.Contains(first["snippet"].(string), ">>>postgres<<<") {
		t.Fatalf("snippet %v", first["snippet"])
	}
	none := call(t, r, `{"query":"zebra"}`)
	if none["count"] != float64(0) || !strings.Contains(none["message"].(string), "No matching sessions") {
		t.Fatalf("%v", none)
	}
	if bad := call(t, r, `{"query":"x","after":"yesterday"}`); bad["success"] != false {
		t.Fatalf("%v", bad)
	}
}

func TestReadScrollBrowse(t *testing.T) {
	r := setup(t)
	read := call(t, r, `{"session_id":"old"}`)
	if read["mode"] != "read" || read["message_count"] != float64(2) || read["truncated"] != false {
		t.Fatalf("%v", read)
	}
	if miss := call(t, r, `{"session_id":"nope"}`); miss["success"] != false {
		t.Fatalf("%v", miss)
	}
	sc := call(t, r, `{"session_id":"old","around_message_id":1,"window":1}`)
	if sc["mode"] != "scroll" || len(sc["messages"].([]any)) != 2 || sc["messages_after"] != float64(1) {
		t.Fatalf("%v", sc)
	}
	if rej := call(t, r, `{"session_id":"cur","around_message_id":5}`); rej["success"] != false {
		t.Fatalf("%v", rej)
	}
	br := call(t, r, `{}`)
	if br["mode"] != "browse" || br["count"] != float64(2) {
		t.Fatalf("%v", br)
	}
}
