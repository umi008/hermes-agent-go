package memory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/tools"
)

// tokens decodes JSON into its token stream, so two payloads compare equal
// only with the same keys in the same order.
func tokens(t *testing.T, s string) []any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	var out []any
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("%v: %s", err, s)
		}
		out = append(out, tok)
	}
}

func run(t *testing.T, r *tools.Registry, args string) string {
	t.Helper()
	out, err := r.Execute(context.Background(), llm.ToolCall{Name: "memory", Arguments: json.RawMessage(args)})
	if err != nil {
		return err.Error()
	}
	return out
}

func TestMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []map[string]any
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	var want struct {
		Results []string          `json:"results"`
		Files   map[string]string `json:"files"`
		Prompt  []*string         `json:"prompt"`
	}
	data, err = os.ReadFile("testdata/upstream.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := tools.NewRegistry(s.Tool())
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		// The fixture pads only a top-level content; a batch op keeps the literal.
		if c["content"] == "PAD" {
			c["content"] = strings.Repeat("p", 1400)
		}
		args, _ := json.Marshal(c)
		got := run(t, r, string(args))
		if !reflect.DeepEqual(tokens(t, got), tokens(t, want.Results[i])) {
			t.Errorf("case %d %s\n got %s\nwant %s", i, args, got, want.Results[i])
		}
	}
	for name, content := range want.Files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != content {
			t.Errorf("%s = %q %v, want %q", name, got, err, content)
		}
	}
	reopened, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var wantBlocks []string
	for _, p := range want.Prompt {
		if p != nil {
			wantBlocks = append(wantBlocks, *p)
		}
	}
	if got := reopened.PromptBlocks(); !reflect.DeepEqual(got, wantBlocks) {
		t.Errorf("prompt blocks\n got %q\nwant %q", got, wantBlocks)
	}
}

func TestSnapshotIsFrozen(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("Feffone\n§\none\n§\ntwo"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	before := s.PromptBlocks()
	if len(before) != 1 || !strings.HasSuffix(before[0], "\none\n§\ntwo") || !strings.Contains(before[0], "MEMORY (your personal notes) [0% — 9/2,200 chars]") {
		t.Fatalf("%q", before)
	}
	if r := s.add("memory", "three"); !r.success() {
		t.Fatalf("%v", r)
	}
	if !reflect.DeepEqual(s.PromptBlocks(), before) {
		t.Fatal("snapshot changed after a write")
	}
	if got := s.Entries("memory"); !reflect.DeepEqual(got, []string{"one", "two", "three"}) {
		t.Fatalf("%q", got)
	}
}

func TestHandEditsSurviveAndDriftIsRefused(t *testing.T) {
	dir := t.TempDir()
	clock := time.Unix(1700000000, 0)
	s, err := Open(dir, Options{MemoryLimit: 20, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if r := s.add("memory", "a"); !r.success() {
		t.Fatalf("%v", r)
	}
	p := filepath.Join(dir, "MEMORY.md")
	// A delimited hand edit between calls is kept.
	if err := os.WriteFile(p, []byte("a\n§\nb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := s.add("memory", "c"); !r.success() {
		t.Fatalf("%v", r)
	}
	if got, _ := os.ReadFile(p); string(got) != "a\n§\nb\n§\nc" {
		t.Fatalf("%q", got)
	}
	// Free-form text longer than the whole limit blocks rewrites.
	raw := "a\n§\n" + strings.Repeat("x", 30)
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	r := s.remove("memory", "a")
	data, _ := json.Marshal(r)
	if r.success() || !strings.Contains(string(data), "Refusing to write MEMORY.md") {
		t.Fatalf("%s", data)
	}
	if got, _ := os.ReadFile(p); string(got) != raw {
		t.Fatal("drifted file was rewritten")
	}
	if bak, err := os.ReadFile(p + ".bak.1700000000"); err != nil || string(bak) != raw {
		t.Fatalf("backup %q %v", bak, err)
	}
	// An unreadable (invalid UTF-8) file is never overwritten.
	if err := os.WriteFile(p, []byte{0xff, 0xfe, 'a'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := s.add("memory", "d"); r.success() {
		t.Fatalf("%v", r)
	}
}
