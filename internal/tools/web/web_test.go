package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/tools"
)

type fakeTavily struct {
	t      *testing.T
	status int
	body   string
	got    map[string]any
	path   string
}

func (f *fakeTavily) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer k" || r.Header.Get("X-Client-Name") != "hermes-agent" {
		f.t.Errorf("headers %v", r.Header)
	}
	f.path = r.URL.Path
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &f.got)
	if f.status != 0 {
		w.WriteHeader(f.status)
	}
	_, _ = io.WriteString(w, f.body)
}

func setup(t *testing.T, f *fakeTavily, cache string) *tools.Registry {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	w := New(Options{APIKey: "k", BaseURL: srv.URL, CacheDir: cache, LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "intranet.example" {
			return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}})
	r, err := tools.NewRegistry(w.Tools()...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func call(t *testing.T, r *tools.Registry, name, args string) string {
	t.Helper()
	out, err := r.Execute(context.Background(), llm.ToolCall{Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		return err.Error()
	}
	return out
}

func TestSearch(t *testing.T) {
	f := &fakeTavily{t: t, body: `{"results":[{"title":"Go","url":"https://go.dev","content":"The Go language"},{"title":"B","url":"https://b","content":"b"}]}`}
	r := setup(t, f, "")
	got := call(t, r, "web_search", `{"query":"golang","limit":50}`)
	want := `{"success":true,"data":{"web":[{"title":"Go","url":"https://go.dev","description":"The Go language","position":1},{"title":"B","url":"https://b","description":"b","position":2}]}}`
	if got != want {
		t.Fatalf("got %s", got)
	}
	if f.path != "/search" || f.got["max_results"] != float64(20) || f.got["include_raw_content"] != false {
		t.Fatalf("request %s %v", f.path, f.got)
	}
	f.status, f.body = 432, "plan limit exceeded"
	if got := call(t, r, "web_search", `{"query":"x"}`); got != `{"success":false,"error":"plan limit exceeded"}` {
		t.Fatalf("error: %s", got)
	}
}

func TestExtract(t *testing.T) {
	long := strings.Repeat("line of text\n", 300)
	resp, _ := json.Marshal(map[string]any{
		"results":        []map[string]string{{"url": "https://b.example/", "title": "B", "raw_content": long}, {"url": "https://a.example/", "title": "A", "content": "short"}},
		"failed_results": []map[string]string{{"url": "https://c.example/", "error": "timeout"}},
	})
	f := &fakeTavily{t: t, body: string(resp)}
	cache := t.TempDir()
	r := setup(t, f, cache)
	got := call(t, r, "web_extract", `{"urls":["https://a.example/",{"href":"https://b.example/"},"http://127.0.0.1/x","https://intranet.example/","https://c.example/",42,"https://d.example/"],"char_limit":10}`)
	var out struct {
		Results []page `json:"results"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("%v: %s", err, got)
	}
	if len(out.Results) != 5 {
		t.Fatalf("results: %s", got)
	}
	if out.Results[0].Content != "short" || out.Results[0].Title != "A" {
		t.Errorf("a: %+v", out.Results[0])
	}
	b := out.Results[1].Content
	if !strings.Contains(b, "[... middle omitted — see footer ...]") || !strings.Contains(b, "of 3,900 total clean characters.") || !strings.Contains(b, "Full text saved to: "+cache) {
		t.Errorf("truncation: %s", b)
	}
	for i, want := range map[int]string{2: "private or internal network", 3: "private or internal network", 4: "timeout"} {
		if e := out.Results[i].Error; e == nil || !strings.Contains(*e, want) {
			t.Errorf("result %d: %+v", i, out.Results[i])
		}
	}
	if urls := f.got["urls"].([]any); len(urls) != 3 {
		t.Errorf("fetched %v", urls)
	}
	entries, _ := os.ReadDir(cache)
	if len(entries) != 1 {
		t.Errorf("cache entries %d", len(entries))
	}
}

func TestUnavailableWithoutKey(t *testing.T) {
	w := New(Options{})
	r, err := tools.NewRegistry(w.Tools()...)
	if err != nil {
		t.Fatal(err)
	}
	if specs := r.Specs(); len(specs) != 0 {
		t.Fatalf("specs %v", specs)
	}
}
