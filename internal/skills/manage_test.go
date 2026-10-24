package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// manageHome builds a catalog over a temp home whose skills dir is empty.
func manageHome(t *testing.T, o Options) (*Catalog, string) {
	t.Helper()
	root := t.TempDir()
	local := filepath.Join(root, "hermes", "skills")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	o.LocalDir = local
	if o.GOOS == "" {
		o.GOOS = "linux"
	}
	return New(o), local
}

// skillMD is the smallest SKILL.md the catalog accepts.
func skillMD(name, body string) string {
	return "---\nname: " + name + "\ndescription: Use when testing " + name + ".\n---\n\n" + body + "\n"
}

func writeSkill(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(path, "SKILL.md")
	if err := os.WriteFile(full, []byte(skillMD(name, body)), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func runOps(t *testing.T, m *Manage, ops string) (Object, error) {
	t.Helper()
	return m.Run(json.RawMessage(`{"operations":[` + ops + `]}`))
}

func TestSkillManageCreate(t *testing.T) {
	c, local := manageHome(t, Options{})
	m := c.Manage()

	if _, err := runOps(t, m, `{"action":"create","name":"deploy-checklist","content":`+quote(skillMD("deploy-checklist", "Check the deploy."))+`}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	path := filepath.Join(local, "deploy-checklist", "SKILL.md")
	if got := mustRead(t, path); !strings.Contains(got, "Check the deploy.") {
		t.Errorf("created SKILL.md = %q", got)
	}
	// The catalog rescans, so the new skill is visible at once.
	var found bool
	for _, s := range c.All() {
		found = found || s.Name == "deploy-checklist"
	}
	if !found {
		t.Errorf("skills_list does not show the new skill: %+v", c.All())
	}

	// A category puts the skill one level down.
	if _, err := runOps(t, m, `{"action":"create","name":"release","category":"devops","content":`+quote(skillMD("release", "Release it."))+`}`); err != nil {
		t.Fatalf("create with category: %v", err)
	}
	if _, err := os.Stat(filepath.Join(local, "devops", "release", "SKILL.md")); err != nil {
		t.Errorf("categorised skill not written: %v", err)
	}

	// skills.create_dir wins over the profile dir when configured.
	createDir := filepath.Join(t.TempDir(), "extra-skills")
	if err := os.MkdirAll(createDir, 0o755); err != nil {
		t.Fatal(err)
	}
	c2, _ := manageHome(t, Options{CreateDir: createDir})
	if _, err := runOps(t, c2.Manage(), `{"action":"create","name":"scratch","content":`+quote(skillMD("scratch", "Scratch."))+`}`); err != nil {
		t.Fatalf("create in create_dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(createDir, "scratch", "SKILL.md")); err != nil {
		t.Errorf("create_dir was ignored: %v", err)
	}
}

func TestSkillManageRejects(t *testing.T) {
	cases := []struct {
		name string
		op   string
		want string
	}{
		{"no frontmatter", `{"action":"create","name":"bare","content":"just text"}`, "frontmatter"},
		{"no description", `{"action":"create","name":"nodesc","content":"---\nname: nodesc\n---\nbody"}`, "description"},
		{"name mismatch", `{"action":"create","name":"one","content":` + quote(skillMD("two", "b")) + `}`, "does not match"},
		{"bad name", `{"action":"create","name":"Bad Name","content":` + quote(skillMD("ok", "b")) + `}`, "invalid skill name"},
		{"no content", `{"action":"create","name":"empty"}`, "requires 'content'"},
		{"unknown action", `{"action":"rename","name":"x"}`, "Unknown action"},
		{"missing name", `{"action":"patch","old_string":"a","new_string":"b"}`, "requires 'name'"},
		{"unknown skill", `{"action":"patch","name":"nope","old_string":"a","new_string":"b"}`, "No skill named"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, local := manageHome(t, Options{})
			_, err := runOps(t, c.Manage(), tc.op)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if entries, _ := os.ReadDir(local); len(entries) != 0 {
				t.Errorf("a rejected op wrote to disk: %v", entries)
			}
		})
	}
}

func TestSkillManagePatch(t *testing.T) {
	c, local := manageHome(t, Options{})
	md := writeSkill(t, local, "deploy", "one\ntwo\nthree")

	if _, err := runOps(t, c.Manage(), `{"action":"patch","name":"deploy","old_string":"two","new_string":"TWO"}`); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if got := mustRead(t, md); !strings.Contains(got, "one\nTWO\nthree") {
		t.Errorf("patched SKILL.md = %q", got)
	}

	// A non-matching old_string fails and leaves the file untouched.
	before := mustRead(t, md)
	_, err := runOps(t, c.Manage(), `{"action":"patch","name":"deploy","old_string":"absent","new_string":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "Could not find old_string") {
		t.Fatalf("err = %v, want a no-match error", err)
	}
	if got := mustRead(t, md); got != before {
		t.Errorf("a failed patch changed the file: %q", got)
	}

	// Two matches need replace_all.
	if err := os.WriteFile(md, []byte(skillMD("deploy", "twin\ntwin")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runOps(t, c.Manage(), `{"action":"patch","name":"deploy","old_string":"twin","new_string":"once"}`); err == nil {
		t.Fatal("ambiguous patch succeeded without replace_all")
	}
	if _, err := runOps(t, c.Manage(), `{"action":"patch","name":"deploy","old_string":"twin","new_string":"once","replace_all":true}`); err != nil {
		t.Fatalf("replace_all: %v", err)
	}
	if got := mustRead(t, md); strings.Count(got, "once") != 2 {
		t.Errorf("replace_all left %q", got)
	}

	// Content rewrites the whole SKILL.md.
	if _, err := runOps(t, c.Manage(), `{"action":"patch","name":"deploy","content":`+quote(skillMD("deploy", "rewritten"))+`}`); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got := mustRead(t, md); !strings.Contains(got, "rewritten") {
		t.Errorf("rewrite left %q", got)
	}
}

func TestSkillManageSupportFiles(t *testing.T) {
	c, local := manageHome(t, Options{})
	writeSkill(t, local, "deploy", "body")
	m := c.Manage()

	if _, err := runOps(t, m, `{"action":"write_file","name":"deploy","file_path":"references/api.md","file_content":"# API"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	ref := filepath.Join(local, "deploy", "references", "api.md")
	if got := mustRead(t, ref); got != "# API" {
		t.Errorf("supporting file = %q", got)
	}

	// Paths outside the skill dir or outside the support dirs are refused.
	for _, bad := range []string{"../escape.md", "/abs.md", "other/file.md", "SKILL.md"} {
		_, err := runOps(t, m, `{"action":"write_file","name":"deploy","file_path":"`+bad+`","file_content":"x"}`)
		if err == nil {
			t.Errorf("write_file accepted %q", bad)
		}
	}

	if _, err := runOps(t, m, `{"action":"remove_file","name":"deploy","file_path":"references/api.md"}`); err != nil {
		t.Fatalf("remove_file: %v", err)
	}
	if _, err := os.Stat(ref); !os.IsNotExist(err) {
		t.Errorf("supporting file still exists: %v", err)
	}
	if _, err := runOps(t, m, `{"action":"remove_file","name":"deploy","file_path":"references/api.md"}`); err == nil {
		t.Error("removing a missing file succeeded")
	}
}

func TestSkillManageDelete(t *testing.T) {
	c, local := manageHome(t, Options{})
	md := writeSkill(t, local, "gone", "body")
	if _, err := runOps(t, c.Manage(), `{"action":"write_file","name":"gone","file_path":"scripts/run.sh","file_content":"echo hi"}`); err != nil {
		t.Fatal(err)
	}

	// delete is a sole op: a batch is refused and nothing is removed.
	_, err := runOps(t, c.Manage(), `{"action":"delete","name":"gone"},{"action":"patch","name":"gone","old_string":"body","new_string":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "delete must be the only operation") {
		t.Fatalf("err = %v, want the sole-op error", err)
	}
	if _, statErr := os.Stat(md); statErr != nil {
		t.Fatalf("SKILL.md removed by a rejected batch: %v", statErr)
	}

	if _, err := runOps(t, c.Manage(), `{"action":"delete","name":"gone"}`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(local, "gone")); !os.IsNotExist(err) {
		t.Errorf("skill directory still exists: %v", err)
	}
}

// TestSkillManageAtomicBatch proves a batch is all or nothing: the second op
// fails, so the first must not reach disk.
func TestSkillManageAtomicBatch(t *testing.T) {
	c, local := manageHome(t, Options{})
	if _, err := runOps(t, c.Manage(),
		`{"action":"create","name":"kept","content":`+quote(skillMD("kept", "body"))+`},`+
			`{"action":"patch","name":"missing","old_string":"a","new_string":"b"}`); err == nil {
		t.Fatal("a batch with a failing op succeeded")
	}
	if _, err := os.Stat(filepath.Join(local, "kept", "SKILL.md")); !os.IsNotExist(err) {
		t.Errorf("the first op landed despite the failure: %v", err)
	}

	// A patch followed by a failure keeps the original text.
	md := writeSkill(t, local, "deploy", "original")
	if _, err := runOps(t, c.Manage(),
		`{"action":"patch","name":"deploy","old_string":"original","new_string":"changed"},`+
			`{"action":"remove_file","name":"deploy","file_path":"references/nope.md"}`); err == nil {
		t.Fatal("a batch with a failing op succeeded")
	}
	if got := mustRead(t, md); !strings.Contains(got, "original") {
		t.Errorf("the failed batch rewrote the file: %q", got)
	}
}

// TestSkillManageLegacyFlatShape keeps the old call shape working: transcripts
// and prompts written before the operations array still exist.
func TestSkillManageLegacyFlatShape(t *testing.T) {
	c, local := manageHome(t, Options{})
	_, err := c.Manage().Run(json.RawMessage(`{"action":"create","name":"flat","content":` + quote(skillMD("flat", "body")) + `}`))
	if err != nil {
		t.Fatalf("flat create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(local, "flat", "SKILL.md")); err != nil {
		t.Errorf("flat shape did not write: %v", err)
	}
}

// quote renders s as a JSON string literal.
func quote(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(data)
}
