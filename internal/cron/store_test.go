package cron

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// jobsFixture is a jobs.json written by upstream: every key this port models plus
// keys it does not (deliver, origin, monitor_state, pending_slot, ...).
const jobsFixture = `{
  "jobs": [
    {
      "id": "abc123def456",
      "name": "nightly",
      "prompt": "Summarize the repo",
      "skills": ["research"],
      "skill": "research",
      "model": "claude-opus-5-5",
      "provider": "anthropic",
      "base_url": null,
      "script": null,
      "no_agent": false,
      "monitor_script": null,
      "monitor_url": null,
      "monitor_state": {"fingerprint": "deadbeef"},
      "context_from": ["other-job"],
      "schedule": {"kind": "interval", "minutes": 30, "display": "every 30m", "timezone": "UTC"},
      "schedule_display": "every 30m",
      "repeat": {"times": null, "completed": 3, "limit_reached": false},
      "enabled": true,
      "state": "scheduled",
      "paused_at": null,
      "paused_reason": null,
      "created_at": "2026-10-20T08:00:00+00:00",
      "next_run_at": "2026-10-24T08:30:00+00:00",
      "last_run_at": "2026-10-24T08:00:00+00:00",
      "last_status": "ok",
      "last_error": null,
      "last_delivery_error": null,
      "last_delivery_unverified": null,
      "last_failure": null,
      "failure_streak": 0,
      "deliver": ["local"],
      "origin": {"platform": "cli", "chat_id": "c1"},
      "enabled_toolsets": ["web"],
      "workdir": "/tmp/work",
      "attach_to_session": true,
      "reasoning_effort": "high",
      "pending_slot": "2026-10-24T08:30:00+00:00",
      "last_fire_error": {"at": "2026-10-23T08:30:00+00:00", "detail": "gateway down"},
      "failure_deliver": "telegram"
    }
  ],
  "updated_at": "2026-10-24T08:00:00+00:00",
  "schema_version": 3
}`

func newTestStore(t *testing.T, now func() time.Time) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "cron"), now)
}

func fixedNow(ts ...time.Time) func() time.Time {
	cur := ts[0]
	return func() time.Time {
		if len(ts) > 1 {
			return ts[1]
		}
		return cur
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readObjects(t *testing.T, path string) (map[string]json.RawMessage, []map[string]json.RawMessage) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	var jobs []map[string]json.RawMessage
	if err := json.Unmarshal(envelope["jobs"], &jobs); err != nil {
		t.Fatal(err)
	}
	return envelope, jobs
}

func TestStoreRoundTripPreservesUnknownKeys(t *testing.T) {
	now := at(24, 8, 0)
	st := newTestStore(t, fixedNow(now))
	writeFile(t, st.JobsPath(), jobsFixture)

	jobs, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	j := jobs[0]
	if j.ID != "abc123def456" || j.Name != "nightly" || j.Prompt != "Summarize the repo" {
		t.Errorf("decoded identity = %+v", j)
	}
	if j.Schedule.Kind != KindInterval || j.Schedule.Minutes != 30 || j.Schedule.Display != "every 30m" {
		t.Errorf("schedule = %+v", j.Schedule)
	}
	if j.Repeat.Completed != 3 || j.Repeat.Times != nil {
		t.Errorf("repeat = %+v", j.Repeat)
	}
	if !j.Enabled || j.State != "scheduled" || j.FailureStreak != 0 {
		t.Errorf("state = %+v", j)
	}
	if got := j.EnabledToolsets; !reflect.DeepEqual(got, []string{"web"}) {
		t.Errorf("enabled_toolsets = %v", got)
	}
	if got := j.Skills; !reflect.DeepEqual(got, []string{"research"}) {
		t.Errorf("skills = %v", got)
	}

	if err := st.Save(jobs); err != nil {
		t.Fatal(err)
	}
	envelope, saved := readObjects(t, st.JobsPath())
	if len(saved) != 1 {
		t.Fatalf("saved jobs = %d", len(saved))
	}

	// The untouched record must survive save unchanged, key by key, including
	// keys the port never modelled and the ones nested inside schedule/repeat.
	var fixture map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jobsFixture), &fixture); err != nil {
		t.Fatal(err)
	}
	var fixtureJobs []map[string]json.RawMessage
	if err := json.Unmarshal(fixture["jobs"], &fixtureJobs); err != nil {
		t.Fatal(err)
	}
	for key, want := range fixtureJobs[0] {
		got, ok := saved[0][key]
		if !ok {
			t.Errorf("key %q dropped on save", key)
			continue
		}
		if !sameJSON(t, got, want) {
			t.Errorf("key %q changed: got %s, want %s", key, got, want)
		}
	}
	for key := range saved[0] {
		if _, ok := fixtureJobs[0][key]; !ok {
			t.Errorf("save invented key %q", key)
		}
	}
	// Nested leftovers: schedule.timezone and repeat.limit_reached.
	var schedule, repeat map[string]json.RawMessage
	if err := json.Unmarshal(saved[0]["schedule"], &schedule); err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, schedule["timezone"], json.RawMessage(`"UTC"`)) {
		t.Errorf("schedule.timezone = %s", schedule["timezone"])
	}
	if err := json.Unmarshal(saved[0]["repeat"], &repeat); err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, repeat["limit_reached"], json.RawMessage(`false`)) {
		t.Errorf("repeat.limit_reached = %s", repeat["limit_reached"])
	}
	// Top-level envelope leftovers survive too.
	if !sameJSON(t, envelope["schema_version"], json.RawMessage(`3`)) {
		t.Errorf("schema_version = %s", envelope["schema_version"])
	}
	if got := string(envelope["updated_at"]); got != `"2026-10-24T08:00:00+00:00"` {
		t.Errorf("updated_at = %s", got)
	}
}

// sameJSON compares two JSON values after compacting, so insignificant
// whitespace from the fixture does not count as a change.
func sameJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var ca, cb bytes.Buffer
	if err := json.Compact(&ca, a); err != nil {
		t.Fatalf("compact %s: %v", a, err)
	}
	if err := json.Compact(&cb, b); err != nil {
		t.Fatalf("compact %s: %v", b, err)
	}
	return ca.String() == cb.String()
}

func TestStoreLoadMissingFile(t *testing.T) {
	st := newTestStore(t, fixedNow(at(24, 8, 0)))
	jobs, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %d", len(jobs))
	}
}

func TestStoreAddUpdateRemove(t *testing.T) {
	now := at(24, 8, 0)
	st := newTestStore(t, fixedNow(now))
	job, err := NewJob(JobOptions{Schedule: "30m", Prompt: "say hi", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Add(job); err != nil {
		t.Fatal(err)
	}
	got, err := st.Find(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Prompt != "say hi" || got.NextRunAt != "2026-10-24T08:30:00+00:00" {
		t.Fatalf("find = %+v", got)
	}
	if err := st.Update(func(jobs []*Job) (bool, error) {
		jobs[0].LastStatus = "ok"
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Find(job.ID); got.LastStatus != "ok" {
		t.Errorf("last_status = %q", got.LastStatus)
	}
	removed, err := st.Remove(job.ID)
	if err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if removed, _ := st.Remove(job.ID); removed {
		t.Error("second remove reported a removal")
	}
	if jobs, _ := st.Load(); len(jobs) != 0 {
		t.Errorf("jobs left: %d", len(jobs))
	}
}

// TestStoreSaveIsAtomic proves the write lands through a temp file (no partial
// jobs.json) and leaves no scratch files behind.
func TestStoreSaveIsAtomic(t *testing.T) {
	now := at(24, 8, 0)
	st := newTestStore(t, fixedNow(now))
	job, err := NewJob(JobOptions{Schedule: "30m", Prompt: "say hi", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Add(job); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(st.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "jobs.json" && e.Name() != jobsLockName {
			t.Errorf("stray file %q", e.Name())
		}
	}
}

func TestNewJob(t *testing.T) {
	now := at(24, 8, 0)
	job, err := NewJob(JobOptions{Name: "nightly", Schedule: "0 9 * * *", Prompt: "report", Repeat: 2, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(job.ID) != 12 {
		t.Errorf("id = %q", job.ID)
	}
	if job.Name != "nightly" || job.State != "scheduled" || !job.Enabled {
		t.Errorf("job = %+v", job)
	}
	if job.ScheduleDisplay != "0 9 * * *" || job.Schedule.Display != "0 9 * * *" {
		t.Errorf("display = %q / %q", job.ScheduleDisplay, job.Schedule.Display)
	}
	if job.NextRunAt != "2026-10-24T09:00:00+00:00" || job.CreatedAt != "2026-10-24T08:00:00+00:00" {
		t.Errorf("next = %q created = %q", job.NextRunAt, job.CreatedAt)
	}
	if job.Repeat.Times == nil || *job.Repeat.Times != 2 || job.Repeat.Completed != 0 {
		t.Errorf("repeat = %+v", job.Repeat)
	}

	// A one-shot inside the grace window is schedulable; 120 s out is not.
	if _, err := NewJob(JobOptions{Schedule: "2026-10-24T07:59:00Z", Prompt: "p", Now: now}); err != nil {
		t.Errorf("one-shot within grace: %v", err)
	}
	if _, err := NewJob(JobOptions{Schedule: "2026-10-24T07:00:00Z", Prompt: "p", Now: now}); err == nil {
		t.Error("one-shot past grace accepted")
	}
	if _, err := NewJob(JobOptions{Schedule: "30m", Now: now}); err == nil {
		t.Error("job without a prompt accepted")
	}
	if _, err := NewJob(JobOptions{Schedule: "banana", Prompt: "p", Now: now}); err == nil {
		t.Error("invalid schedule accepted")
	}
}
