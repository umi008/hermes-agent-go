package tuigateway

import (
	"strings"
	"testing"
)

// TestCommandsCatalog pins the slash catalog: the commands this gateway can
// actually answer, their aliases and their categories.
func TestCommandsCatalog(t *testing.T) {
	h := newHarness(t, nil)
	h.ready()
	res, ok := h.call("commands.catalog", map[string]any{})["result"].(map[string]any)
	if !ok {
		t.Fatal("commands.catalog answered no result")
	}
	pairs, _ := res["pairs"].([]any)
	names := map[string]string{}
	for _, p := range pairs {
		pair, _ := p.([]any)
		if len(pair) != 2 {
			t.Fatalf("pair = %v", p)
		}
		name, _ := pair[0].(string)
		help, _ := pair[1].(string)
		if name == "" || help == "" {
			t.Errorf("pair = %v", pair)
		}
		names[name] = help
	}
	for _, want := range []string{"help", "model", "status"} {
		if _, present := names[want]; !present {
			t.Errorf("catalog lacks %q: %v", want, names)
		}
	}
	// A command the gateway cannot answer must not be advertised: an entry the
	// client offers and slash.exec then refuses is worse than no entry.
	for _, unwanted := range []string{"compress", "title", "undo"} {
		if _, present := names[unwanted]; present {
			t.Errorf("catalog advertises %q, which slash.exec cannot answer", unwanted)
		}
	}
	canon, _ := res["canon"].(map[string]any)
	if canon["?"] != "help" {
		t.Errorf("canon = %v", canon)
	}
	categories, _ := res["categories"].([]any)
	if len(categories) == 0 {
		t.Fatalf("categories = %v", res["categories"])
	}
	category, _ := categories[0].(map[string]any)
	if category["name"] == "" {
		t.Errorf("category = %v", category)
	}
	if catPairs, _ := category["pairs"].([]any); len(catPairs) == 0 {
		t.Errorf("category pairs = %v", category["pairs"])
	}
	if res["skill_count"] != float64(0) || res["warning"] != "" {
		t.Errorf("skills = %v / %v", res["skill_count"], res["warning"])
	}
}

// TestCompleteSlash pins prefix completion over command names and aliases.
func TestCompleteSlash(t *testing.T) {
	h := newHarness(t, nil)
	h.ready()
	res, _ := h.call("complete.slash", map[string]any{"text": "/he"})["result"].(map[string]any)
	if res["replace_from"] != float64(1) {
		t.Errorf("replace_from = %v", res["replace_from"])
	}
	items, _ := res["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", items)
	}
	item, _ := items[0].(map[string]any)
	if item["text"] != "/help" || item["kind"] != "command" || item["display"] == "" {
		t.Errorf("item = %v", item)
	}
	// Every command completes from a bare slash.
	res, _ = h.call("complete.slash", map[string]any{"text": "/"})["result"].(map[string]any)
	if items, _ := res["items"].([]any); len(items) != 3 {
		t.Errorf("bare slash = %v", items)
	}
	// An alias completes to its canonical name.
	res, _ = h.call("complete.slash", map[string]any{"text": "/?"})["result"].(map[string]any)
	if items, _ := res["items"].([]any); len(items) != 1 {
		t.Errorf("alias = %v", items)
	} else if item, _ := items[0].(map[string]any); item["text"] != "/help" {
		t.Errorf("alias item = %v", item)
	}
	// Arguments are not completed here, and an unknown prefix offers nothing.
	for _, text := range []string{"/zz", "/model so", "hello"} {
		res, _ = h.call("complete.slash", map[string]any{"text": text})["result"].(map[string]any)
		if items, _ := res["items"].([]any); len(items) != 0 {
			t.Errorf("%q = %v", text, items)
		}
	}
}

// TestSlashExec pins execution and its refusals.
func TestSlashExec(t *testing.T) {
	h := newHarness(t, nil)
	h.ready()
	sid := h.createSession()

	res, ok := h.call("slash.exec", map[string]any{"session_id": sid, "command": "/status"})["result"].(map[string]any)
	if !ok {
		t.Fatal("slash.exec answered no result")
	}
	output, _ := res["output"].(string)
	for _, want := range []string{"tiny", "session"} {
		if !strings.Contains(strings.ToLower(output), want) {
			t.Errorf("/status output = %q, want %q", output, want)
		}
	}

	resp := h.call("slash.exec", map[string]any{"session_id": sid, "command": "/help"})
	output, _ = resp["result"].(map[string]any)["output"].(string)
	for _, want := range []string{"/status", "/model", "/help"} {
		if !strings.Contains(output, want) {
			t.Errorf("/help output = %q, want %q", output, want)
		}
	}

	// A leading slash is optional, and an unknown command answers in the
	// output instead of failing the call: the client renders it as a notice.
	resp = h.call("slash.exec", map[string]any{"session_id": sid, "command": "model"})
	if output, _ := resp["result"].(map[string]any)["output"].(string); !strings.Contains(output, "tiny") {
		t.Errorf("/model = %v", resp)
	}
	resp = h.call("slash.exec", map[string]any{"session_id": sid, "command": "/nope"})
	if output, _ := resp["result"].(map[string]any)["output"].(string); !strings.Contains(output, "Unknown command: /nope") {
		t.Errorf("unknown = %v", resp)
	}

	// A command the gateway knows but cannot run says so, and says why.
	resp = h.call("slash.exec", map[string]any{"session_id": sid, "command": "/compress"})
	res, _ = resp["result"].(map[string]any)
	if output, _ := res["output"].(string); !strings.Contains(output, "/compress") || !strings.Contains(output, "not available") {
		t.Errorf("/compress = %v", res)
	}

	// The refusals are canonical.
	for _, params := range []map[string]any{
		{"session_id": sid},
		{"session_id": sid, "command": "  "},
		{"command": "/status"},
	} {
		errObj, _ := h.call("slash.exec", params)["error"].(map[string]any)
		data, _ := errObj["data"].(map[string]any)
		if errObj["code"] != float64(4001) || data["reason"] != "invalid_params" {
			t.Errorf("%v = %v", params, errObj)
		}
	}
	errObj, _ := h.call("slash.exec", map[string]any{"session_id": "nope", "command": "/status"})["error"].(map[string]any)
	if data, _ := errObj["data"].(map[string]any); errObj["code"] != float64(4001) || data["reason"] != "not_found" {
		t.Errorf("unknown session = %v", errObj)
	}
}
