package skills

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is testdata/upstream.json, dumped from upstream skills_tool and
// prompt_builder over a copy of testdata/home (skills.disabled
// [disabled-one], skills.external_dirs [ext]) on Windows.
type fixture struct {
	List []struct {
		Category *string         `json:"category"`
		Out      json.RawMessage `json:"out"`
	} `json:"list"`
	View []struct {
		Name     string          `json:"name"`
		FilePath *string         `json:"file_path"`
		Out      json.RawMessage `json:"out"`
	} `json:"view"`
	Index map[string]string `json:"index"`
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (*Catalog, func(string) string) {
	t.Helper()
	home := t.TempDir()
	copyTree(t, filepath.Join("testdata", "home"), home)
	c := New(Options{
		LocalDir:     filepath.Join(home, "skills"),
		ExternalDirs: []string{filepath.Join(home, "ext")},
		Disabled:     []string{"disabled-one"},
		GOOS:         "windows",
	})
	norm := func(s string) string {
		s = strings.ReplaceAll(s, home, "@HOME@")
		return strings.ReplaceAll(s, `\`, "/")
	}
	return c, norm
}

func load(t *testing.T) fixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "upstream.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// tokens flattens JSON into its token stream, so key order counts.
func tokens(t *testing.T, data []byte, norm func(string) string) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("%v in %s", err, data)
		}
		switch v := tok.(type) {
		case string:
			out = append(out, "s:"+norm(v))
		case json.Delim:
			out = append(out, v.String())
		default:
			b, _ := json.Marshal(v)
			out = append(out, string(b))
		}
	}
}

func sameJSON(t *testing.T, label string, got Object, want json.RawMessage, norm func(string) string) {
	t.Helper()
	g, err := result(got)
	if err != nil {
		t.Fatal(err)
	}
	gt, wt := tokens(t, []byte(g), norm), tokens(t, want, func(s string) string { return s })
	for i := range max(len(gt), len(wt)) {
		if i >= len(gt) || i >= len(wt) || gt[i] != wt[i] {
			t.Errorf("%s: token %d differs\ngot  %s\nwant %s", label, i, norm(g), want)
			return
		}
	}
}

func TestListMatchesUpstream(t *testing.T) {
	c, norm := setup(t)
	for _, tc := range load(t).List {
		cat := ""
		if tc.Category != nil {
			cat = *tc.Category
		}
		sameJSON(t, "list "+cat, c.List(cat), tc.Out, norm)
	}
}

func TestViewMatchesUpstream(t *testing.T) {
	c, norm := setup(t)
	for _, tc := range load(t).View {
		fp := ""
		if tc.FilePath != nil {
			fp = *tc.FilePath
		}
		sameJSON(t, "view "+tc.Name+" "+fp, c.View(tc.Name, fp, "sess-1"), tc.Out, norm)
	}
}

func TestIndexMatchesUpstream(t *testing.T) {
	c, norm := setup(t)
	set := func(xs ...string) map[string]bool {
		m := map[string]bool{}
		for _, x := range xs {
			m[x] = true
		}
		return m
	}
	tools := []string{"skill_view", "skills_list", "terminal", "read_file"}
	cases := map[string]IndexOptions{
		"no_web":  {Tools: set(tools...), Toolsets: set("skills", "terminal", "file")},
		"web":     {Tools: set(append(tools, "web_search")...), Toolsets: set("skills", "terminal", "file", "web")},
		"oneshot": {Tools: set(tools...), Toolsets: set("skills", "terminal", "file"), Oneshot: true},
	}
	want := load(t).Index
	for name, o := range cases {
		if got := norm(c.Index(o)); got != want[name] {
			t.Errorf("index %s:\ngot  %q\nwant %q", name, got, want[name])
		}
	}
}

func TestEmptyCatalog(t *testing.T) {
	c := New(Options{LocalDir: filepath.Join(t.TempDir(), "missing")})
	if got := c.Index(IndexOptions{}); got != "" {
		t.Fatalf("index = %q", got)
	}
	out, _ := result(c.List(""))
	if out != `{"success":true,"skills":[],"categories":[],"message":"No skills found in skills/ directory."}` {
		t.Fatalf("list = %s", out)
	}
	out, _ = result(c.View("x", "", ""))
	if !strings.Contains(out, "Skills directory does not exist yet") {
		t.Fatalf("view = %s", out)
	}
}

func TestToolsDecodeArgs(t *testing.T) {
	c, _ := setup(t)
	ts := Tools(c, func() string { return "s" })
	if len(ts) != 2 || ts[0].Name != "skills_list" || ts[1].Name != "skill_view" {
		t.Fatalf("%+v", ts)
	}
	for _, tl := range ts {
		var schema map[string]any
		if err := json.Unmarshal(tl.Parameters, &schema); err != nil {
			t.Fatalf("%s schema: %v", tl.Name, err)
		}
	}
	out, err := ts[1].Handler(t.Context(), json.RawMessage(`{"name":"deploy"}`))
	if err != nil || !strings.Contains(out, "in session s.") {
		t.Fatalf("%s %v", out, err)
	}
	if _, err := ts[1].Handler(t.Context(), json.RawMessage(`{"name":1}`)); err == nil {
		t.Fatal("bad args accepted")
	}
}
