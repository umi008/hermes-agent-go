// Package sessionsearch ports upstream tools/session_search_tool.py: search,
// read and scroll past sessions in the state store. Results are database
// rows, never an LLM summary.
package sessionsearch

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/umi008/hermes-agent-go/internal/state"
	"github.com/umi008/hermes-agent-go/internal/tools"
)

// Upstream limits and source classes.
var (
	hiddenSources  = []string{"kanban", "subagent", "tool"}
	demotedSources = []string{"cron"}
)

const (
	readMaxContent   = 2000
	discoverScan     = 300
	excludeCap       = 20
	compactPrefixOne = "[CONTEXT COMPACTION"
	compactPrefixTwo = "[CONTEXT SUMMARY]:"
)

//go:embed schema.json
var schema string

var relativeRE = regexp.MustCompile(`(?i)^(\d+)\s*(h|d|w)$`)

// Options bind the tool to a store.
type Options struct {
	Store *state.Store
	// Current returns the running session id, whose own messages are
	// already in context and so are skipped.
	Current func() string
	// Profile names the active profile in @session links; "" omits it.
	Profile string
	Now     func() time.Time
}

// Tool returns session_search.
func Tool(o Options) tools.Tool {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Current == nil {
		o.Current = func() string { return "" }
	}
	return tools.Tool{
		Name:    "session_search",
		Toolset: "session_search",
		Description: "Search or read past Hermes conversations (FTS5), or scroll inside one. Four shapes, picked by args: `query` = discovery " +
			"(top-N matching sessions, top result fully hydrated); `session_id` + " +
			"`around_message_id` = scroll (window of messages around an anchor); " +
			"`session_id` alone = read a whole session — how you resolve an " +
			"`@session:<profile>/<id>` link (split on '/' into profile + id); no " +
			"args = browse recent sessions. Results are actual DB messages, no LLM. " +
			"Searches conversation history ONLY — when the user gave a direct " +
			"source (URL, file, contact, live system), inspect that first; never " +
			"conclude 'not found' from history alone. Use for questions about past " +
			"conversations: 'what did we do about X', 'where did we leave Y'. When " +
			"referring the user to a session, write its `link` value verbatim " +
			"inline (it renders as a titled link).",
		Parameters: json.RawMessage(schema),
		Available:  func() bool { return o.Store != nil },
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			return handle(ctx, o, raw)
		},
	}
}

type args struct {
	Query       string          `json:"query"`
	Limit       json.RawMessage `json:"limit"`
	Sort        string          `json:"sort"`
	Detail      string          `json:"detail"`
	After       string          `json:"after"`
	Before      string          `json:"before"`
	Exclude     json.RawMessage `json:"exclude_session_ids"`
	SessionID   string          `json:"session_id"`
	Around      json.RawMessage `json:"around_message_id"`
	WindowSize  json.RawMessage `json:"window"`
	RoleFilter  string          `json:"role_filter"`
	ProfileName string          `json:"profile"`
}

func handle(ctx context.Context, o Options, raw json.RawMessage) (string, error) {
	var a args
	if err := tools.Decode(raw, &a); err != nil {
		return "", err
	}
	if a.ProfileName != "" && a.ProfileName != o.Profile {
		return "", fail("reading another profile's sessions is not supported yet")
	}
	if sid := strings.TrimSpace(a.SessionID); sid != "" {
		if len(a.Around) > 0 && string(a.Around) != "null" {
			return scroll(ctx, o, sid, a.Around, clampInt(a.WindowSize, 5, 1, 20))
		}
		return read(ctx, o, sid)
	}
	limit := clampInt(a.Limit, 3, 1, 10)
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return browse(ctx, o, limit)
	}
	after, err := parseBound(a.After, o.Now())
	if err != nil {
		return "", fail(err.Error())
	}
	before, err := parseBound(a.Before, o.Now())
	if err != nil {
		return "", fail(err.Error())
	}
	var roles []string
	for _, r := range strings.Split(a.RoleFilter, ",") {
		if r = strings.TrimSpace(r); r != "" {
			roles = append(roles, r)
		}
	}
	sort := strings.ToLower(strings.TrimSpace(a.Sort))
	if sort != "newest" && sort != "oldest" {
		sort = ""
	}
	detail := "adaptive"
	if strings.EqualFold(strings.TrimSpace(a.Detail), "full") {
		detail = "full"
	}
	return discover(ctx, o, query, roles, limit, sort, detail, after, before, excludeIDs(a.Exclude))
}

func fail(msg string) error { return tools.ErrorWith(msg, map[string]any{"success": false}) }

// obj is a JSON object that keeps key order, for upstream-identical output.
type obj []kv

type kv struct {
	k string
	v any
}

func (o obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(p.k)
		v, err := json.Marshal(p.v)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func ok(fields ...kv) (string, error) {
	return tools.Result(append(obj{{"success", true}}, fields...))
}

func clampInt(raw json.RawMessage, def, lo, hi int) int {
	v := def
	var f float64
	var s string
	switch {
	case json.Unmarshal(raw, &f) == nil:
		v = int(f)
	case json.Unmarshal(raw, &s) == nil:
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			v = n
		}
	}
	return max(lo, min(v, hi))
}

func excludeIDs(raw json.RawMessage) []string {
	var items []string
	var one string
	if json.Unmarshal(raw, &one) == nil {
		items = []string{one}
	} else {
		var list []any
		_ = json.Unmarshal(raw, &list)
		for _, it := range list {
			s, _ := it.(string)
			items = append(items, s)
		}
	}
	var out []string
	for _, it := range items {
		if it = strings.TrimSpace(it); it != "" && !slices.Contains(out, it) {
			out = append(out, it)
		}
		if len(out) >= excludeCap {
			break
		}
	}
	return out
}

// parseBound ports _parse_iso_bound: an ISO date/datetime (UTC when it has
// no zone) or a relative 7d/24h/2w duration back from now.
func parseBound(v string, now time.Time) (*int64, error) {
	text := strings.TrimSpace(v)
	if text == "" {
		return nil, nil
	}
	if m := relativeRE.FindStringSubmatch(text); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		unit := map[string]int64{"h": 3600, "d": 86400, "w": 604800}[strings.ToLower(m[2])]
		ts := now.Unix() - n*unit
		return &ts, nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, text, time.UTC); err == nil {
			ts := t.Unix()
			return &ts, nil
		}
	}
	return nil, fmt.Errorf("invalid time bound: '%s' (expected ISO date/datetime like 2026-07-01, or a relative duration like 7d, 24h, 2w)", v)
}

func formatTime(ts float64) string {
	if ts == 0 {
		return "unknown"
	}
	return time.Unix(0, int64(ts*1e9)).Format("January 02, 2006 at 03:04 PM")
}

func (o Options) link(sessionID string) string {
	if o.Profile == "" || o.Profile == "custom" {
		return "@session:" + sessionID
	}
	return "@session:" + o.Profile + "/" + sessionID
}

func metaBlock(s state.Session) obj {
	return obj{{"when", formatTime(s.StartedAt)}, {"source", s.Source}, {"model", nullStr(s.Model)}, {"title", nullStr(s.Title)}}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// shape ports _shape_message.
func shape(m state.Message, anchor int64, maxLen int) obj {
	e := obj{{"id", m.ID}, {"role", m.Role}, {"content", m.Content}, {"timestamp", m.Timestamp}}
	if m.ToolName != "" {
		e = append(e, kv{"tool_name", m.ToolName})
	}
	if m.ToolCalls != "" {
		e = append(e, kv{"tool_calls", json.RawMessage(m.ToolCalls)})
	}
	if m.ToolCallID != "" {
		e = append(e, kv{"tool_call_id", m.ToolCallID})
	}
	if anchor != 0 && m.ID == anchor {
		e = append(e, kv{"anchor", true})
	}
	if n := utf8.RuneCountInString(m.Content); maxLen > 0 && n > maxLen {
		e[2].v = string([]rune(m.Content)[:maxLen]) + "…"
		e = append(e, kv{"content_truncated", true}, kv{"original_content_chars", n})
	}
	return e
}

func shapeAll(ms []state.Message, anchor int64, maxLen int, skipSummaries bool) []obj {
	out := []obj{}
	for _, m := range ms {
		if skipSummaries && (strings.HasPrefix(m.Content, compactPrefixOne) || strings.HasPrefix(m.Content, compactPrefixTwo)) {
			continue
		}
		out = append(out, shape(m, anchor, maxLen))
	}
	return out
}

func discover(ctx context.Context, o Options, query string, roles []string, limit int, sort, detail string,
	after, before *int64, exclude []string) (string, error) {
	if roles == nil {
		roles = []string{"user", "assistant"}
	}
	hits, err := o.Store.Search(ctx, state.Query{Text: query, Roles: roles, ExcludeSources: hiddenSources,
		Limit: discoverScan, Sort: sort, After: after, Before: before})
	if err != nil {
		return "", fail("Search failed: " + err.Error())
	}
	slices.SortStableFunc(hits, func(a, b state.Hit) int {
		da, db := slices.Contains(demotedSources, a.Source), slices.Contains(demotedSources, b.Source)
		switch {
		case da == db:
			return 0
		case da:
			return 1
		}
		return -1
	})
	payload := func(results []obj, extra ...kv) (string, error) {
		fields := []kv{{"mode", "discover"}, {"query", query}, {"detail", detail}, {"results", results}, {"count", len(results)}}
		return ok(append(fields, extra...)...)
	}
	if len(hits) == 0 {
		return payload([]obj{}, kv{"message", "No matching sessions found. FTS5 ANDs all terms by default — " +
			"broaden with OR (`alpha OR beta`), exact-match with quoted " +
			"phrases, exclude with NOT, or prefix-match with `deploy*`."})
	}
	current := o.Current()
	var picked []state.Hit
	seen := map[string]bool{}
	for _, h := range hits {
		if len(picked) >= limit {
			break
		}
		if seen[h.SessionID] || h.SessionID == current || slices.Contains(exclude, h.SessionID) {
			continue
		}
		seen[h.SessionID] = true
		picked = append(picked, h)
	}
	results := []obj{}
	for _, h := range picked {
		full := detail == "full" || len(results) == 0
		entry, err := hydrate(ctx, o, h, full)
		if err != nil {
			return "", fail("Search failed: " + err.Error())
		}
		if entry != nil {
			results = append(results, entry)
		}
	}
	return payload(results, kv{"sessions_searched", len(picked)}, kv{"link_hint",
		"When referring the user to a session, write its `link` value " +
			"verbatim inline mid-sentence (it renders as a titled link) — never " +
			"as markdown, in backticks, on its own line, or next to the " +
			"title/id/date. To read more around a compact result, scroll: " +
			"session_search(session_id=..., around_message_id=match_message_id)."})
}

func hydrate(ctx context.Context, o Options, h state.Hit, full bool) (obj, error) {
	view, err := o.Store.Anchored(ctx, h.SessionID, h.ID, 5, 3)
	if err != nil {
		return nil, err
	}
	sess, err := o.Store.GetSession(ctx, h.SessionID)
	if err != nil {
		sess = state.Session{StartedAt: h.SessionStarted, Source: h.Source, Model: h.Model}
	}
	model := sess.Model
	if model == "" {
		model = "unknown"
	}
	detail, start, end := "compact", []obj{}, []obj{}
	var msgs []obj
	if full {
		detail = "full"
		start, end = shapeAll(view.Start, 0, 1200, true), shapeAll(view.End, 0, 1200, true)
		msgs = shapeAll(view.Messages, h.ID, 4000, false)
	} else {
		msgs = []obj{}
		for _, m := range view.Messages {
			if m.ID == h.ID {
				msgs = append(msgs, shape(m, h.ID, 4000))
			}
		}
	}
	return obj{
		{"session_id", h.SessionID}, {"when", formatTime(sess.StartedAt)}, {"source", sess.Source}, {"model", model},
		{"title", nullStr(sess.Title)}, {"matched_role", h.Role}, {"match_message_id", h.ID}, {"snippet", h.Snippet},
		{"bookend_start", start}, {"messages", msgs}, {"bookend_end", end},
		{"messages_before", view.Before}, {"messages_after", view.After}, {"detail", detail},
		{"link", o.link(h.SessionID)},
	}, nil
}

func read(ctx context.Context, o Options, sid string) (string, error) {
	const head, tail = 20, 10
	sess, err := o.Store.GetSession(ctx, sid)
	if err != nil {
		return "", fail("session_id not found in this profile: " + sid + ". If it belongs to another " +
			"profile, pass profile=<name> (or the @session:<profile>/<id> link).")
	}
	rows, err := o.Store.Messages(ctx, sid)
	if err != nil {
		return "", fail("failed to load session: " + err.Error())
	}
	shaped := shapeAll(rows, 0, readMaxContent, false)
	total := len(shaped)
	truncated := total > head+tail
	if truncated {
		shaped = append(shaped[:head:head], shaped[total-tail:]...)
	}
	fields := []kv{{"mode", "read"}, {"session_id", sid}, {"link", o.link(sid)}, {"session_meta", metaBlock(sess)},
		{"message_count", total}, {"truncated", truncated}, {"messages", shaped}}
	if truncated {
		fields = append(fields, kv{"message", fmt.Sprintf("Session has %d messages; showing first %d + last %d. "+
			"Pass around_message_id (any id above) to scroll the middle.", total, head, tail)})
	}
	return ok(fields...)
}

func scroll(ctx context.Context, o Options, sid string, rawAnchor json.RawMessage, window int) (string, error) {
	var f float64
	if json.Unmarshal(rawAnchor, &f) != nil {
		var s string
		if json.Unmarshal(rawAnchor, &s) != nil {
			return "", fail("scroll requires integer around_message_id")
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return "", fail("scroll requires integer around_message_id")
		}
		f = float64(n)
	}
	anchor := int64(f)
	if sid == o.Current() {
		return "", fail("scroll rejected: anchor lives in the current session lineage (already in your active context)")
	}
	sess, err := o.Store.GetSession(ctx, sid)
	if err != nil {
		return "", fail("session_id not found: " + sid)
	}
	view, err := o.Store.MessagesAround(ctx, sid, anchor, window)
	if err != nil {
		return "", fail("failed to load messages: " + err.Error())
	}
	if len(view.Messages) == 0 {
		return "", fail(fmt.Sprintf("around_message_id %d not in session_id %s", anchor, sid))
	}
	return ok(kv{"mode", "scroll"}, kv{"session_id", sid}, kv{"around_message_id", anchor},
		kv{"session_meta", metaBlock(sess)}, kv{"window", window},
		kv{"messages", shapeAll(view.Messages, anchor, 0, false)},
		kv{"messages_before", view.Before}, kv{"messages_after", view.After},
		kv{"hint", "Scroll forward: re-call with around_message_id = the LAST message's " +
			"id; backward: the FIRST message's id (the boundary message repeats " +
			"as an orientation marker). messages_before/messages_after < window " +
			"means you've hit that end of the session."})
}

func browse(ctx context.Context, o Options, limit int) (string, error) {
	sessions, err := o.Store.RecentSessions(ctx, limit+15, hiddenSources)
	if err != nil {
		return "", fail("Failed to list recent sessions: " + err.Error())
	}
	current := o.Current()
	results := []obj{}
	for _, s := range sessions {
		if s.ID == current {
			continue
		}
		if len(results) == limit {
			break
		}
		results = append(results, obj{{"session_id", s.ID}, {"link", o.link(s.ID)}, {"title", nullStr(s.Title)},
			{"source", s.Source}, {"started_at", s.StartedAt}, {"last_active", s.LastActive},
			{"message_count", s.MessageCount}, {"preview", s.Preview}})
	}
	return ok(kv{"mode", "browse"}, kv{"results", results}, kv{"count", len(results)}, kv{"message",
		fmt.Sprintf("Showing %d most recent sessions. Pass a query= to search, "+
			"or session_id+around_message_id to scroll.", len(results))})
}
