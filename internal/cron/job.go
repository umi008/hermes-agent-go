package cron

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Job is one record of jobs.json. Only the fields this port reads and writes are
// modelled; every other key upstream stores (model, deliver, origin, workdir,
// pending_slot, ...) is kept verbatim in extra and written back untouched.
type Job struct {
	ID              string
	Name            string
	Prompt          string
	Skills          []string
	Skill           string // legacy single-skill alias
	Schedule        Schedule
	ScheduleDisplay string
	Repeat          *Repeat
	Enabled         bool
	State           string
	CreatedAt       string
	NextRunAt       string
	LastRunAt       string
	LastStatus      string
	LastError       string
	LastFailure     *Failure
	FailureStreak   int
	EnabledToolsets []string

	extra map[string]json.RawMessage
}

// Repeat is the run budget: Times nil means forever, Completed counts runs.
type Repeat struct {
	Times     *int
	Completed int

	// extra keeps keys this port does not model (limit_reached, ...).
	extra rawObject
}

// Failure is the most recent failed run. It is sticky: a later success keeps it
// for the operator, as upstream does.
type Failure struct {
	At     string `json:"at"`
	Detail string `json:"detail"`
}

// jobKeys are the keys Job models itself; everything else in a record is a
// leftover.
var jobKeys = []string{
	"id", "name", "prompt", "skills", "skill", "schedule", "schedule_display",
	"repeat", "enabled", "state", "created_at", "next_run_at", "last_run_at",
	"last_status", "last_error", "last_failure", "failure_streak", "enabled_toolsets",
}

// JobOptions is what the CLI hands to NewJob.
type JobOptions struct {
	Name     string
	Schedule string
	Prompt   string
	Repeat   int // 0 runs forever
	Now      time.Time
}

// NewJob builds a schedulable job the way upstream's create_job does: a 12-hex
// id, the derived name, the schedule display, the first next_run_at and an
// enabled scheduled state.
func NewJob(opts JobOptions) (*Job, error) {
	prompt := strings.TrimSpace(opts.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("cron job needs a prompt")
	}
	now := opts.Now
	schedule, err := ParseSchedule(opts.Schedule, now)
	if err != nil {
		return nil, err
	}
	next, ok := schedule.NextRun("", now)
	if !ok {
		return nil, fmt.Errorf("invalid schedule %q: it has no next run from %s", opts.Schedule, stamp(now))
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = deriveName(prompt)
	}
	var times *int
	if opts.Repeat > 0 {
		repeat := opts.Repeat
		times = &repeat
	}
	return &Job{
		ID:              id,
		Name:            name,
		Prompt:          prompt,
		Skills:          []string{},
		Schedule:        schedule,
		ScheduleDisplay: schedule.Display,
		Repeat:          &Repeat{Times: times},
		Enabled:         true,
		State:           "scheduled",
		CreatedAt:       stamp(now),
		NextRunAt:       next,
		EnabledToolsets: []string{},
	}, nil
}

// newID is uuid4's hex form truncated to 12 characters, like upstream.
func newID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate cron job id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// deriveName is the fallback name: the prompt's first line, at most 50 runes.
func deriveName(prompt string) string {
	line := prompt
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	runes := []rune(line)
	if len(runes) > 50 {
		runes = runes[:50]
	}
	if len(runes) == 0 {
		return "cron job"
	}
	return string(runes)
}

// MarshalJSON writes the modelled fields plus every leftover key, so a job this
// port only read comes back byte-identical.
func (j *Job) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(jobKeys)+len(j.extra))
	for key, raw := range j.extra {
		out[key] = raw
	}
	put := func(key string, value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", key, err)
		}
		out[key] = raw
		return nil
	}
	// Optional strings are written as null when unset, the shape upstream uses.
	putNullEmpty := func(key, value string) {
		if value == "" {
			out[key] = json.RawMessage("null")
			return
		}
		_ = put(key, value)
	}
	for key, value := range map[string]any{
		"id":               j.ID,
		"name":             j.Name,
		"prompt":           j.Prompt,
		"skills":           j.Skills,
		"schedule":         j.Schedule,
		"schedule_display": j.ScheduleDisplay,
		"enabled":          j.Enabled,
		"state":            j.State,
		"created_at":       j.CreatedAt,
		"last_failure":     j.LastFailure,
		"failure_streak":   j.FailureStreak,
		"enabled_toolsets": j.EnabledToolsets,
	} {
		if err := put(key, value); err != nil {
			return nil, err
		}
	}
	putNullEmpty("skill", j.Skill)
	putNullEmpty("next_run_at", j.NextRunAt)
	putNullEmpty("last_run_at", j.LastRunAt)
	putNullEmpty("last_status", j.LastStatus)
	putNullEmpty("last_error", j.LastError)
	if j.Repeat != nil {
		if err := put("repeat", j.Repeat); err != nil {
			return nil, err
		}
	}
	return json.Marshal(out)
}

// UnmarshalJSON decodes the modelled fields and keeps every other key.
func (j *Job) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID              string     `json:"id"`
		Name            string     `json:"name"`
		Prompt          string     `json:"prompt"`
		Skills          []string   `json:"skills"`
		Skill           nullString `json:"skill"`
		Schedule        Schedule   `json:"schedule"`
		ScheduleDisplay string     `json:"schedule_display"`
		Repeat          *Repeat    `json:"repeat"`
		Enabled         bool       `json:"enabled"`
		State           string     `json:"state"`
		CreatedAt       string     `json:"created_at"`
		NextRunAt       nullString `json:"next_run_at"`
		LastRunAt       nullString `json:"last_run_at"`
		LastStatus      nullString `json:"last_status"`
		LastError       nullString `json:"last_error"`
		LastFailure     *Failure   `json:"last_failure"`
		FailureStreak   int        `json:"failure_streak"`
		EnabledToolsets []string   `json:"enabled_toolsets"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*j = Job{
		ID:              wire.ID,
		Name:            wire.Name,
		Prompt:          wire.Prompt,
		Skills:          wire.Skills,
		Skill:           string(wire.Skill),
		Schedule:        wire.Schedule,
		ScheduleDisplay: wire.ScheduleDisplay,
		Repeat:          wire.Repeat,
		Enabled:         wire.Enabled,
		State:           wire.State,
		CreatedAt:       wire.CreatedAt,
		NextRunAt:       string(wire.NextRunAt),
		LastRunAt:       string(wire.LastRunAt),
		LastStatus:      string(wire.LastStatus),
		LastError:       string(wire.LastError),
		LastFailure:     wire.LastFailure,
		FailureStreak:   wire.FailureStreak,
		EnabledToolsets: wire.EnabledToolsets,
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, key := range jobKeys {
		delete(raw, key)
	}
	j.extra = raw
	return nil
}

// MarshalJSON writes times, completed and then any leftover repeat keys.
func (r *Repeat) MarshalJSON() ([]byte, error) {
	var w objectWriter
	if err := w.field("times", r.Times); err != nil {
		return nil, err
	}
	if err := w.field("completed", r.Completed); err != nil {
		return nil, err
	}
	r.extra.appendTo(&w)
	return w.bytes(), nil
}

// UnmarshalJSON decodes the modelled repeat fields and keeps the rest.
func (r *Repeat) UnmarshalJSON(data []byte) error {
	var wire struct {
		Times     *int `json:"times"`
		Completed int  `json:"completed"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	var extra rawObject
	if err := json.Unmarshal(data, &extra); err != nil {
		return err
	}
	extra.del("times")
	extra.del("completed")
	*r = Repeat{Times: wire.Times, Completed: wire.Completed, extra: extra}
	return nil
}

// MarshalJSON writes kind, the kind's own fields, display, then any leftover
// schedule keys (timezone, ...) in their original order.
func (s Schedule) MarshalJSON() ([]byte, error) {
	var w objectWriter
	if err := w.field("kind", s.Kind); err != nil {
		return nil, err
	}
	for _, field := range []struct {
		key   string
		value any
		set   bool
	}{
		{"minutes", s.Minutes, s.Minutes != 0},
		{"expr", s.Expr, s.Expr != ""},
		{"run_at", s.RunAt, s.RunAt != ""},
	} {
		if !field.set {
			continue
		}
		if err := w.field(field.key, field.value); err != nil {
			return nil, err
		}
	}
	if err := w.field("display", s.Display); err != nil {
		return nil, err
	}
	s.extra.appendTo(&w)
	return w.bytes(), nil
}

// UnmarshalJSON decodes the modelled schedule fields and keeps the rest.
func (s *Schedule) UnmarshalJSON(data []byte) error {
	var wire struct {
		Kind    string `json:"kind"`
		Minutes int    `json:"minutes"`
		Expr    string `json:"expr"`
		RunAt   string `json:"run_at"`
		Display string `json:"display"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	var extra rawObject
	if err := json.Unmarshal(data, &extra); err != nil {
		return err
	}
	for _, key := range []string{"kind", "minutes", "expr", "run_at", "display"} {
		extra.del(key)
	}
	*s = Schedule{
		Kind: wire.Kind, Minutes: wire.Minutes, Expr: wire.Expr,
		RunAt: wire.RunAt, Display: wire.Display, extra: extra,
	}
	return nil
}

// objectWriter builds a JSON object field by field, in call order.
type objectWriter struct {
	buf   bytes.Buffer
	count int
}

func (w *objectWriter) field(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", key, err)
	}
	w.raw(key, raw)
	return nil
}

func (w *objectWriter) raw(key string, value json.RawMessage) {
	if w.count == 0 {
		w.buf.WriteByte('{')
	} else {
		w.buf.WriteByte(',')
	}
	w.count++
	name, err := json.Marshal(key)
	if err != nil {
		return
	}
	w.buf.Write(name)
	w.buf.WriteByte(':')
	w.buf.Write(value)
}

func (w *objectWriter) bytes() []byte {
	if w.count == 0 {
		return []byte("{}")
	}
	w.buf.WriteByte('}')
	return w.buf.Bytes()
}

// rawObject is a JSON object kept as ordered key/value pairs, so keys this port
// does not model survive a write byte for byte. Nested objects (schedule,
// repeat) are compared whole by the round-trip contract, so their key order
// matters, not just their content.
type rawObject struct {
	keys []string
	vals map[string]json.RawMessage
}

func (o *rawObject) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil // null: no leftovers
	}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := keyToken.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
		o.set(key, value)
	}
	_, err = dec.Token() // closing '}'
	return err
}

func (o *rawObject) set(key string, value json.RawMessage) {
	if o.vals == nil {
		o.vals = map[string]json.RawMessage{}
	}
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = value
}

func (o *rawObject) del(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o rawObject) appendTo(w *objectWriter) {
	for _, key := range o.keys {
		w.raw(key, o.vals[key])
	}
}

// nullString reads a string that upstream may have written as JSON null.
type nullString string

func (s *nullString) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*s = ""
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = nullString(value)
	return nil
}
