package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, name, desc string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestChatListsSkillsInPrompt(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	f := newHome(t, "model:\n  provider: custom\n  default: tiny\n  base_url: "+srv.URL+"\n"+
		"skills:\n  disabled: [off-skill]\n  external_dirs: [shared]\n")
	hermes := f.vars["HERMES_HOME"]
	writeSkill(t, filepath.Join(hermes, "skills", "coding", "review"), "review", "Review code.")
	writeSkill(t, filepath.Join(hermes, "skills", "off-skill"), "off-skill", "Disabled.")
	writeSkill(t, filepath.Join(hermes, "shared", "ext-skill"), "ext-skill", "From outside.")

	if code := Run(context.Background(), []string{"chat", "-q", "hi"}, f.env()); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
	}
	system, _ := body["messages"].([]any)[0].(map[string]any)["content"].(string)
	for _, want := range []string{"Scan the skills below", "<available_skills>", "    - review: Review code.", "    - ext-skill: From outside."} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if strings.Contains(system, "off-skill") {
		t.Error("disabled skill listed")
	}
	var names []string
	for _, tl := range body["tools"].([]any) {
		names = append(names, tl.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	if got := strings.Join(names, ","); !strings.Contains(got, "skill_view") || !strings.Contains(got, "skills_list") {
		t.Errorf("tools = %s", got)
	}
}

func TestExpandPath(t *testing.T) {
	abs, hermes, user := t.TempDir(), t.TempDir(), t.TempDir()
	lookup := func(k string) (string, bool) {
		if k == "SKILLS" {
			return abs, true
		}
		return "", false
	}
	tests := map[string]string{
		"rel":        filepath.Join(hermes, "rel"),
		"~/s":        filepath.Join(user, "s"),
		"$SKILLS/a":  filepath.Join(abs, "a"),
		"${SKILLS}":  abs,
		"$MISSING/x": filepath.Join(hermes, "$MISSING", "x"),
	}
	for in, want := range tests {
		if got := expandPath(in, hermes, user, lookup); got != want {
			t.Errorf("%q = %q want %q", in, got, want)
		}
	}
}
