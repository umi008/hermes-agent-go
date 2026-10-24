// Package todo ports upstream tools/todo_tool.py: an in-memory, revisioned
// task list for multi-step work, one per session.
package todo

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

// Upstream limits.
const (
	MaxContentChars = 4000
	MaxItems        = 256
	truncMarker     = "… [truncated]"
	// InjectionHeader marks the list re-injected after context compression.
	InjectionHeader = "[Your active task list was preserved across context compression]"
)

var validStatuses = []string{"pending", "in_progress", "completed", "cancelled"}

// Item is one task. List position is priority; Parent nests a subtask.
type Item struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
	Parent  string `json:"parent,omitempty"`
}

// Store is one session's list.
type Store struct {
	mu       sync.Mutex
	items    []Item
	revision int
}

// New returns an empty store.
func New() *Store { return &Store{} }

// Read returns a copy of the list and its revision.
func (s *Store) Read() ([]Item, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.items), s.revision
}

// Write replaces the list (or merges by id) and bumps the revision when it
// changed. An invalid item rejects the whole write.
func (s *Store) Write(todos []map[string]any, merge bool) ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next []Item
	var err error
	if merge {
		next, err = mergeItems(slices.Clone(s.items), todos)
	} else {
		next, err = freshItems(todos)
	}
	if err != nil {
		return nil, err
	}
	if len(next) > MaxItems {
		next = next[:MaxItems]
	}
	sanitizeParents(next)
	if !slices.Equal(next, s.items) {
		s.revision++
	}
	s.items = next
	return slices.Clone(s.items), nil
}

// FormatForInjection renders the active (pending/in_progress) items, keeping
// a parent while any descendant is active. Empty when nothing is active.
func (s *Store) FormatForInjection() string {
	items, _ := s.Read()
	children := map[string][]Item{}
	for _, it := range items {
		if it.Parent != "" {
			children[it.Parent] = append(children[it.Parent], it)
		}
	}
	markers := map[string]string{"completed": "[x]", "in_progress": "[>]", "pending": "[ ]", "cancelled": "[~]"}
	var render func(it Item, depth int, out *[]string) bool
	render = func(it Item, depth int, out *[]string) bool {
		var kids []string
		activeKid := false
		for _, k := range children[it.ID] {
			activeKid = render(k, depth+1, &kids) || activeKid
		}
		keep := it.Status == "pending" || it.Status == "in_progress" || activeKid
		if keep {
			*out = append(*out, fmt.Sprintf("%s- %s %s. %s (%s)", strings.Repeat("  ", depth), markers[it.Status], it.ID, it.Content, it.Status))
			*out = append(*out, kids...)
		}
		return keep
	}
	lines := []string{InjectionHeader}
	for _, it := range items {
		if it.Parent == "" {
			render(it, 0, &lines)
		}
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// str mirrors Python str() on a decoded JSON value.
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func capContent(c string) string {
	if utf8.RuneCountInString(c) > MaxContentChars {
		return string([]rune(c)[:MaxContentChars-utf8.RuneCountInString(truncMarker)]) + truncMarker
	}
	return c
}

func normStatus(v any) string { return strings.ToLower(strings.TrimSpace(str(v))) }

func validate(t map[string]any) (Item, error) {
	if t == nil {
		return Item{ID: "?", Content: "(invalid item)", Status: "pending"}, nil
	}
	id := strings.TrimSpace(str(t["id"]))
	if id == "" {
		id = "?"
	}
	content := strings.TrimSpace(str(t["content"]))
	if content == "" {
		return Item{}, fmt.Errorf("Todo item '%s' has empty or missing content. Each item must have a non-empty description.", id) //nolint:staticcheck,revive // upstream message
	}
	status := "pending"
	if st, ok := t["status"]; ok {
		status = normStatus(st)
	}
	if !slices.Contains(validStatuses, status) {
		status = "pending"
	}
	it := Item{ID: id, Content: capContent(content), Status: status}
	if p := strings.TrimSpace(str(t["parent"])); p != "" && p != id {
		it.Parent = p
	}
	return it, nil
}

// dedupe keeps the last occurrence of each id, in its position.
func dedupe(todos []map[string]any) []map[string]any {
	last := map[string]int{}
	for i, t := range todos {
		key := fmt.Sprintf("__invalid_%d", i)
		if t != nil {
			key = strings.TrimSpace(str(t["id"]))
		}
		if key == "" {
			key = "?"
		}
		last[key] = i
	}
	idx := make([]int, 0, len(last))
	for _, i := range last {
		idx = append(idx, i)
	}
	slices.Sort(idx)
	out := make([]map[string]any, len(idx))
	for k, i := range idx {
		out[k] = todos[i]
	}
	return out
}

func freshItems(todos []map[string]any) ([]Item, error) {
	var out []Item
	for _, t := range dedupe(todos) {
		it, err := validate(t)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return normalizeOrder(out), nil
}

func mergeItems(items []Item, todos []map[string]any) ([]Item, error) {
	pos := map[string]int{}
	for i, it := range items {
		if _, ok := pos[it.ID]; !ok {
			pos[it.ID] = i
		}
	}
	for _, t := range dedupe(todos) {
		if t == nil {
			continue
		}
		id := strings.TrimSpace(str(t["id"]))
		if id == "" {
			continue
		}
		i, ok := pos[id]
		if !ok {
			it, err := validate(t)
			if err != nil {
				return nil, err
			}
			pos[it.ID] = len(items)
			items = append(items, it)
			continue
		}
		cur := &items[i]
		if c := str(t["content"]); c != "" {
			cur.Content = capContent(strings.TrimSpace(c))
		}
		if st := normStatus(t["status"]); st != "" && slices.Contains(validStatuses, st) {
			cur.Status = st
		}
		if p, ok := t["parent"]; ok {
			cur.Parent = strings.TrimSpace(str(p))
		}
	}
	// Duplicate ids already in the list collapse to their first occurrence.
	seen := map[string]bool{}
	var out []Item
	for _, it := range items {
		if !seen[it.ID] {
			seen[it.ID] = true
			out = append(out, items[pos[it.ID]])
		}
	}
	return normalizeOrder(out), nil
}

// normalizeOrder lifts the in_progress item ahead of an earlier pending one
// in flat lists.
func normalizeOrder(items []Item) []Item {
	active, firstPending := -1, -1
	for i, it := range items {
		if it.Parent != "" {
			return items
		}
		if it.Status == "in_progress" && active < 0 {
			active = i
		}
		if it.Status == "pending" && firstPending < 0 {
			firstPending = i
		}
	}
	if active < 0 || firstPending < 0 || firstPending > active {
		return items
	}
	it := items[active]
	items = slices.Delete(items, active, active+1)
	return slices.Insert(items, firstPending, it)
}

// sanitizeParents drops dangling parents and breaks cycles.
func sanitizeParents(items []Item) {
	byID := map[string]int{}
	for i, it := range items {
		byID[it.ID] = i
	}
	for i := range items {
		if _, ok := byID[items[i].Parent]; items[i].Parent != "" && !ok {
			items[i].Parent = ""
		}
	}
	for i := range items {
		seen := map[string]bool{items[i].ID: true}
		node := items[i]
		for node.Parent != "" {
			if seen[node.Parent] {
				items[i].Parent = ""
				break
			}
			seen[node.Parent] = true
			node = items[byID[node.Parent]]
		}
	}
}

// Tools returns todo_list bound to the store.
func (s *Store) Tools() []tools.Tool {
	return []tools.Tool{{
		Name:    "todo_list",
		Toolset: "todo",
		Description: "Track a task list for multi-step work (3+ steps). Use for complex tasks " +
			"with 3+ steps or when the user provides multiple tasks. " +
			"For 'all N items' tasks, enumerate every instance as its own checklist " +
			"item so none are silently dropped. " +
			"Call with no parameters to read the current list.\n" +
			"List order is priority. Only ONE item in_progress at a time. " +
			"Break large phases into subtasks via parent. " +
			"Mark an item completed only after the work is verified done, never " +
			"based on intent. If something fails, cancel it and add a revised " +
			"item. Always returns the full current list.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"todos":{"type":"array","description":"Task items to write.","items":{"type":"object","properties":{` +
			`"id":{"type":"string"},"content":{"type":"string","description":"Task description"},` +
			`"status":{"type":"string","enum":["pending","in_progress","completed","cancelled"]},` +
			`"parent":{"type":"string","description":"Optional id of another item, making this a nested subtask. Omit for top-level."}},` +
			`"required":["id","content","status"]}},` +
			`"merge":{"type":"boolean","description":"true: update existing items by id, add new ones. false (default): replace the entire list with a fresh plan.","default":false}` +
			`},"required":[]}`),
		Handler: s.handle,
	}}
}

var pyTypes = map[byte]string{'{': "dict", '"': "str", 't': "bool", 'f': "bool"}

type summary struct {
	Total      int `json:"total"`
	Pending    int `json:"pending"`
	InProgress int `json:"in_progress"`
	Completed  int `json:"completed"`
	Cancelled  int `json:"cancelled"`
}

func (s *Store) handle(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Todos json.RawMessage `json:"todos"`
		Merge bool            `json:"merge"`
	}
	if err := tools.Decode(raw, &a); err != nil {
		return "", err
	}
	var items []Item
	var rev int
	todos := strings.TrimSpace(string(a.Todos))
	if todos == "" || todos == "null" {
		items, rev = s.Read()
	} else {
		if todos[0] != '[' {
			name, ok := pyTypes[todos[0]]
			if !ok {
				name = "int"
				if strings.ContainsAny(todos, ".eE") {
					name = "float"
				}
			}
			return "", tools.Error("todos must be a list, got " + name)
		}
		var elems []json.RawMessage
		if err := json.Unmarshal(a.Todos, &elems); err != nil {
			return "", tools.Error("Invalid arguments: " + err.Error())
		}
		list := make([]map[string]any, len(elems))
		for i, e := range elems {
			_ = json.Unmarshal(e, &list[i]) // non-objects stay nil
		}
		var err error
		if items, err = s.Write(list, a.Merge); err != nil {
			return "", tools.Error(err.Error())
		}
		_, rev = s.Read()
	}
	sum := summary{Total: len(items)}
	for _, it := range items {
		switch it.Status {
		case "pending":
			sum.Pending++
		case "in_progress":
			sum.InProgress++
		case "completed":
			sum.Completed++
		case "cancelled":
			sum.Cancelled++
		}
	}
	if items == nil {
		items = []Item{}
	}
	return tools.Result(struct {
		Todos    []Item  `json:"todos"`
		Revision int     `json:"revision"`
		Summary  summary `json:"summary"`
	}{items, rev, sum})
}
