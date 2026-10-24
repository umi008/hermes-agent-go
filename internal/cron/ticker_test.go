package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	runs []string
	err  error
	// during runs after every dispatch, so a test can observe jobs.json at
	// dispatch time (at-most-once: the advance must already be persisted).
	during func(job *Job)
	doc    string
}

func (f *fakeRunner) Run(_ context.Context, job *Job) (string, error) {
	f.runs = append(f.runs, job.ID)
	if f.during != nil {
		f.during(job)
	}
	if f.err != nil {
		return "## Error\n\nboom\n", f.err
	}
	if f.doc != "" {
		return f.doc, nil
	}
	return "# Cron Job: " + job.Name + "\n\nok\n", nil
}

// mutableClock is the injected clock every ticker test moves by hand.
type mutableClock struct{ t time.Time }

func (c *mutableClock) Now() time.Time { return c.t }

func (c *mutableClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newTicker(t *testing.T, clock *mutableClock, runner *fakeRunner, keep int) (*Ticker, *Store) {
	t.Helper()
	st := NewStore(filepath.Join(t.TempDir(), "cron"), clock.Now)
	return &Ticker{Store: st, Now: clock.Now, Runner: runner, Keep: keep}, st
}

func addJob(t *testing.T, st *Store, opts JobOptions) *Job {
	t.Helper()
	job, err := NewJob(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Add(job); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestTickFiresOncePerSlot(t *testing.T) {
	clock := &mutableClock{at(24, 8, 0)}
	var seenNext, seenLast string
	runner := &fakeRunner{}
	tk, st := newTicker(t, clock, runner, DefaultOutputRetention)
	job := addJob(t, st, JobOptions{Name: "nightly", Schedule: "30m", Prompt: "report", Now: clock.Now()})
	if job.NextRunAt != "2026-10-24T08:30:00+00:00" {
		t.Fatalf("created next_run_at = %q", job.NextRunAt)
	}

	// Not due yet.
	if n, err := tk.Tick(context.Background()); n != 0 || err != nil {
		t.Fatalf("early tick = %d, %v", n, err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("ran early: %v", runner.runs)
	}

	clock.Advance(31 * time.Minute)
	// At dispatch time the advance must already be persisted (at-most-once).
	runner.during = func(j *Job) {
		persisted, err := st.Find(j.ID)
		if err != nil {
			t.Fatal(err)
		}
		seenNext, seenLast = persisted.NextRunAt, persisted.LastRunAt
	}
	n, err := tk.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(runner.runs) != 1 {
		t.Fatalf("tick = %d runs = %v", n, runner.runs)
	}
	// The persisted marker is the next occurrence, strictly after the tick, so a
	// process that dies mid-run does not replay the job on the next tick.
	if seenNext != "2026-10-24T09:01:00+00:00" {
		t.Errorf("next_run_at at dispatch = %q, want the next occurrence after the tick", seenNext)
	}
	if seenNext <= "2026-10-24T08:31:00+00:00" {
		t.Errorf("next_run_at at dispatch = %q, want it strictly after the tick time", seenNext)
	}
	if seenLast != "" {
		t.Errorf("last_run_at at dispatch = %q, want empty", seenLast)
	}
	got, err := st.Find(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastStatus != "ok" || got.LastRunAt != "2026-10-24T08:31:00+00:00" || got.FailureStreak != 0 {
		t.Errorf("after run = %+v", got)
	}
	if got.NextRunAt != "2026-10-24T09:01:00+00:00" {
		t.Errorf("next_run_at = %q", got.NextRunAt)
	}
	if got.Repeat.Completed != 1 {
		t.Errorf("repeat.completed = %d", got.Repeat.Completed)
	}

	// The same instant never re-fires.
	if n, err := tk.Tick(context.Background()); n != 0 || err != nil {
		t.Fatalf("repeat tick = %d, %v", n, err)
	}
	if len(runner.runs) != 1 {
		t.Fatalf("fired twice: %v", runner.runs)
	}
}

func TestTickLockSerializesTickers(t *testing.T) {
	clock := &mutableClock{at(24, 8, 0)}
	runner := &fakeRunner{}
	tk, st := newTicker(t, clock, runner, DefaultOutputRetention)
	job := addJob(t, st, JobOptions{Schedule: "1m", Prompt: "report", Now: clock.Now()})
	clock.Advance(2 * time.Minute)

	// A live lock (fresh mtime) makes the tick a no-op.
	writeFile(t, st.TickLockPath(), "12345 2026-10-24T08:02:00+00:00")
	if n, err := tk.Tick(context.Background()); n != 0 || err != nil {
		t.Fatalf("locked tick = %d, %v", n, err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("ran under a live lock: %v", runner.runs)
	}
	if _, err := os.Stat(st.TickLockPath()); err != nil {
		t.Errorf("live lock removed: %v", err)
	}

	// A lock older than 10 minutes is stale and may be broken.
	stale := clock.Now().Add(-11 * time.Minute)
	if err := os.Chtimes(st.TickLockPath(), stale, stale); err != nil {
		t.Fatal(err)
	}
	n, err := tk.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(runner.runs) != 1 {
		t.Fatalf("stale lock tick = %d runs = %v", n, runner.runs)
	}
	if _, err := os.Stat(st.TickLockPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock not released after the tick: %v", err)
	}
	if got, _ := st.Find(job.ID); got.LastStatus != "ok" {
		t.Errorf("job did not run: %+v", got)
	}
}

func TestTickFailureBookkeeping(t *testing.T) {
	clock := &mutableClock{at(24, 8, 0)}
	runner := &fakeRunner{err: errors.New("provider down")}
	tk, st := newTicker(t, clock, runner, DefaultOutputRetention)
	job := addJob(t, st, JobOptions{Schedule: "30m", Prompt: "report", Now: clock.Now()})
	clock.Advance(30 * time.Minute)

	if n, _ := tk.Tick(context.Background()); n != 1 {
		t.Fatalf("tick = %d", n)
	}
	got, _ := st.Find(job.ID)
	if got.LastStatus != "error" || got.LastError != "provider down" || got.FailureStreak != 1 {
		t.Errorf("after failure = %+v", got)
	}
	if got.LastFailure == nil || !strings.Contains(got.LastFailure.Detail, "provider down") {
		t.Errorf("last_failure = %+v", got.LastFailure)
	}
	// The failed run still leaves a transcript for the operator.
	docs, err := filepath.Glob(filepath.Join(st.OutputDir(job.ID), "*.md"))
	if err != nil || len(docs) != 1 {
		t.Fatalf("outputs = %v, %v", docs, err)
	}

	// A later success clears the streak but keeps the sticky last_failure.
	runner.err = nil
	clock.Advance(30 * time.Minute)
	if n, _ := tk.Tick(context.Background()); n != 1 {
		t.Fatalf("second tick = %d", n)
	}
	got, _ = st.Find(job.ID)
	if got.LastStatus != "ok" || got.FailureStreak != 0 || got.LastError != "" {
		t.Errorf("after success = %+v", got)
	}
	if got.LastFailure == nil {
		t.Error("last_failure should survive a later success")
	}
}

func TestTickRetiresExpiredOneShot(t *testing.T) {
	clock := &mutableClock{at(24, 8, 0)}
	runner := &fakeRunner{}
	tk, st := newTicker(t, clock, runner, DefaultOutputRetention)
	job := addJob(t, st, JobOptions{Schedule: "2026-10-24T08:00:00Z", Prompt: "report", Now: clock.Now()})
	if job.NextRunAt != "2026-10-24T08:00:00+00:00" {
		t.Fatalf("next_run_at = %q", job.NextRunAt)
	}

	// 121 s late: past the 120 s one-shot grace, so it must not fire.
	clock.Advance(time.Hour)
	if n, err := tk.Tick(context.Background()); n != 0 || err != nil {
		t.Fatalf("tick = %d, %v", n, err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("expired one-shot fired: %v", runner.runs)
	}
	got, _ := st.Find(job.ID)
	if got.State != "completed" || got.Enabled || got.NextRunAt != "" {
		t.Errorf("retired one-shot = %+v", got)
	}

	// A one-shot inside the grace window still fires.
	clock2 := &mutableClock{at(24, 8, 0)}
	runner2 := &fakeRunner{}
	tk2, st2 := newTicker(t, clock2, runner2, DefaultOutputRetention)
	addJob(t, st2, JobOptions{Schedule: "2026-10-24T08:00:00Z", Prompt: "report", Now: clock2.Now()})
	clock2.Advance(90 * time.Second)
	if n, _ := tk2.Tick(context.Background()); n != 1 {
		t.Fatalf("grace tick = %d", n)
	}
	if len(runner2.runs) != 1 {
		t.Fatalf("one-shot inside grace did not fire")
	}
}

func TestTickStopsAtRepeatLimit(t *testing.T) {
	clock := &mutableClock{at(24, 8, 0)}
	runner := &fakeRunner{}
	tk, st := newTicker(t, clock, runner, DefaultOutputRetention)
	job := addJob(t, st, JobOptions{Schedule: "1m", Prompt: "report", Repeat: 1, Now: clock.Now()})
	clock.Advance(time.Minute)
	if n, _ := tk.Tick(context.Background()); n != 1 {
		t.Fatalf("tick = %d", n)
	}
	got, _ := st.Find(job.ID)
	if got.State != "completed" || got.Enabled || got.NextRunAt != "" {
		t.Errorf("completed job = %+v", got)
	}
	clock.Advance(time.Hour)
	if n, _ := tk.Tick(context.Background()); n != 0 {
		t.Fatalf("completed job re-fired: %d", n)
	}
}

func TestTickCatchUpFiresMissedRecurringOnce(t *testing.T) {
	clock := &mutableClock{at(24, 8, 0)}
	runner := &fakeRunner{}
	tk, st := newTicker(t, clock, runner, DefaultOutputRetention)
	job := addJob(t, st, JobOptions{Schedule: "30m", Prompt: "report", Now: clock.Now()})
	// Six hours of downtime: far beyond the 15 min catch-up window.
	clock.Advance(6 * time.Hour)
	if n, _ := tk.Tick(context.Background()); n != 1 {
		t.Fatalf("catch-up tick = %d", n)
	}
	got, _ := st.Find(job.ID)
	if got.Repeat.Completed != 1 {
		t.Errorf("missed occurrences were replayed: completed = %d", got.Repeat.Completed)
	}
	if got.NextRunAt != "2026-10-24T14:30:00+00:00" {
		t.Errorf("next_run_at = %q, want one period from now", got.NextRunAt)
	}
}

func TestTickOutputRetention(t *testing.T) {
	clock := &mutableClock{at(24, 8, 0)}
	runner := &fakeRunner{doc: "# doc\n"}
	tk, st := newTicker(t, clock, runner, 3)
	job := addJob(t, st, JobOptions{Schedule: "1m", Prompt: "report", Now: clock.Now()})
	dir := st.OutputDir(job.ID)
	for _, name := range []string{
		"2026-10-20_08-00-00.md", "2026-10-21_08-00-00.md", "2026-10-22_08-00-00.md",
		"2026-10-23_08-00-00.md", "2026-10-24_07-00-00.md",
	} {
		writeFile(t, filepath.Join(dir, name), "old\n")
	}
	clock.Advance(2 * time.Minute)
	if n, _ := tk.Tick(context.Background()); n != 1 {
		t.Fatalf("tick = %d", n)
	}
	docs, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Fatalf("retained %d files: %v", len(docs), docs)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-10-24_08-02-00.md")); err != nil {
		t.Errorf("newest run output missing: %v", err)
	}
}

func TestRunNowIgnoresSchedule(t *testing.T) {
	clock := &mutableClock{at(24, 8, 0)}
	runner := &fakeRunner{}
	tk, st := newTicker(t, clock, runner, DefaultOutputRetention)
	job := addJob(t, st, JobOptions{Name: "nightly", Schedule: "0 9 * * *", Prompt: "report", Now: clock.Now()})

	if err := tk.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 1 {
		t.Fatalf("runs = %v", runner.runs)
	}
	got, _ := st.Find(job.ID)
	if got.LastStatus != "ok" || got.NextRunAt != "2026-10-25T09:00:00+00:00" {
		t.Errorf("after run now = %+v", got)
	}
	if err := tk.RunNow(context.Background(), "missing"); err == nil {
		t.Error("unknown id accepted")
	}
}

func TestSaveOutputPathAndPrune(t *testing.T) {
	clock := &mutableClock{at(24, 8, 0)}
	st := NewStore(filepath.Join(t.TempDir(), "cron"), clock.Now)
	path, err := st.SaveOutput("job1", clock.Now(), "# doc\n", 2)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(st.Dir, "output", "job1", "2026-10-24_08-00-00.md")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "# doc\n" {
		t.Fatalf("content = %q, %v", data, err)
	}
	for _, name := range []string{"2026-10-23_08-00-00.md", "2026-10-23_09-00-00.md", "2026-10-23_10-00-00.md"} {
		writeFile(t, filepath.Join(st.OutputDir("job1"), name), "# old\n")
	}
	if n := st.PruneOutput("job1", 2); n != 2 {
		t.Errorf("pruned = %d, want 2", n)
	}
	if n := st.PruneOutput("job1", 0); n != 0 {
		t.Errorf("keep=0 pruned %d files", n)
	}
}
