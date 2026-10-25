package tuigateway

import "testing"

// TestInfoCarriesInventory pins what the banner renders: the tool and skill
// inventory ride on the snapshot's info, and the slash catalog counts the same
// skills, so "0 tools" stops being a lie about a full registry.
func TestInfoCarriesInventory(t *testing.T) {
	h, _ := newHarnessStore(t, nil)
	h.srv.o.Info.Tools = map[string][]string{
		"files": {"patch", "read_file"},
		"web":   {"web_search"},
	}
	h.srv.o.Info.Skills = map[string][]string{"testing": {"tdd"}}
	h.ready()
	sid := h.createSession()

	res, _ := h.call("session.resume", map[string]any{"session_id": sid})["result"].(map[string]any)
	info, ok := res["info"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot has no info: %v", res)
	}
	tools, _ := info["tools"].(map[string]any)
	if len(tools) != 2 {
		t.Fatalf("info.tools = %v", info["tools"])
	}
	files, _ := tools["files"].([]any)
	if len(files) != 2 || files[0] != "patch" || files[1] != "read_file" {
		t.Errorf("info.tools.files = %v", tools["files"])
	}
	skills, _ := info["skills"].(map[string]any)
	if len(skills) != 1 {
		t.Errorf("info.skills = %v", info["skills"])
	}

	cat, _ := h.call("commands.catalog", map[string]any{})["result"].(map[string]any)
	if cat["skill_count"] != float64(1) {
		t.Errorf("skill_count = %v, want the inventory's count", cat["skill_count"])
	}
	if catalogSkills, _ := cat["skills"].(map[string]any); len(catalogSkills) != 1 {
		t.Errorf("catalog skills = %v", cat["skills"])
	}
}

// TestInfoInventoryDefaultsToEmpty pins that an absent inventory answers empty
// maps rather than nulls, so the client's flatten never panics.
func TestInfoInventoryDefaultsToEmpty(t *testing.T) {
	h, _ := newHarnessStore(t, nil)
	h.ready()
	sid := h.createSession()
	info, _ := h.call("session.resume", map[string]any{"session_id": sid})["result"].(map[string]any)["info"].(map[string]any)
	for _, key := range []string{"tools", "skills"} {
		m, ok := info[key].(map[string]any)
		if !ok || len(m) != 0 {
			t.Errorf("info.%s = %v, want an empty object", key, info[key])
		}
	}
}

// TestInventoryIsResolvedPerRequest pins the seam the runtime needs: the tool
// registry exists only after the server is built, so the inventory must be
// read per request and win over the static Info fields.
func TestInventoryIsResolvedPerRequest(t *testing.T) {
	h, _ := newHarnessStore(t, nil)
	h.ready()
	sid := h.createSession()

	tools := map[string][]string{"files": {"read_file"}}
	h.srv.o.Inventory = func() Inventory {
		return Inventory{Tools: tools, Skills: map[string][]string{"testing": {"tdd"}}}
	}
	first, _ := h.call("session.resume", map[string]any{"session_id": sid})["result"].(map[string]any)
	info, _ := first["info"].(map[string]any)
	if got, _ := info["tools"].(map[string]any); len(got) != 1 {
		t.Fatalf("info.tools = %v", info["tools"])
	}

	// A later request sees what the registry offers now, not what it offered
	// when the server was built.
	tools["web"] = []string{"web_search"}
	second, _ := h.call("session.resume", map[string]any{"session_id": sid})["result"].(map[string]any)
	info, _ = second["info"].(map[string]any)
	if got, _ := info["tools"].(map[string]any); len(got) != 2 {
		t.Fatalf("info.tools after the registry grew = %v", info["tools"])
	}
	if cat, _ := h.call("commands.catalog", map[string]any{})["result"].(map[string]any); cat["skill_count"] != float64(1) {
		t.Errorf("skill_count = %v", cat["skill_count"])
	}
}
