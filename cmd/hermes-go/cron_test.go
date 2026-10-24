package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cronClock is a hand-moved clock, so cron tests never sleep.
type cronClock struct{ t time.Time }

func (c *cronClock) Now() time.Time { return c.t }

func (c *cronClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// cronHome is an isolated Hermes home whose Env reads a hand-moved clock.
type cronHome struct {
	*fakeEnv
	clock *cronClock
}

func (c *cronHome) env() Env {
	e := envAt(c.fakeEnv, c.clock.Now)
	return e
}

// cronEnv lays out a temp home for cron tests.
func cronEnv(t *testing.T, configYAML string) *cronHome {
	t.Helper()
	return &cronHome{
		fakeEnv: newHome(t, configYAML),
		clock:   &cronClock{t: time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)},
	}
}

// envAt builds the Env of f with a different clock.
func envAt(f *fakeEnv, now func() time.Time) Env {
	return Env{
		GOOS:        "linux",
		Getenv:      func(k string) string { return f.vars[k] },
		LookupEnv:   func(k string) (string, bool) { v, ok := f.vars[k]; return v, ok },
		UserHomeDir: func() (string, error) { return f.vars["HOME"], nil },
		Getwd:       func() (string, error) { return f.cwd, nil },
		Now:         now,
		Stdin:       strings.NewReader(f.stdin),
		Stdout:      &f.stdout,
		Stderr:      &f.stderr,
	}
}

// jobsFile reads the cron store of f straight off disk.
func jobsFile(t *testing.T, f *cronHome) map[string]any {
	t.Helper()
	path := filepath.Join(f.vars["HERMES_HOME"], "cron", "jobs.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func jobsOf(t *testing.T, f *cronHome) []map[string]any {
	t.Helper()
	doc := jobsFile(t, f)
	raw, _ := doc["jobs"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, j := range raw {
		if m, ok := j.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func TestCronAddListRemove(t *testing.T) {
	f := cronEnv(t, "model:\n  provider: custom\n  default: tiny\n  base_url: http://127.0.0.1:9\n")
	ctx := context.Background()

	if code := cronCmd(ctx, []string{"list"}, f.env()); code != 0 {
		t.Fatalf("empty list exit %d, stderr: %s", code, f.stderr.String())
	}
	if out := f.stdout.String(); !strings.Contains(out, "No scheduled jobs") {
		t.Errorf("empty list output = %q", out)
	}

	f.stdout.Reset()
	code := cronCmd(ctx, []string{"add", "--name", "nightly", "--schedule", "30m", "--prompt", "Summarize the repo", "--repeat", "3"}, f.env())
	if code != 0 {
		t.Fatalf("add exit %d, stderr: %s", code, f.stderr.String())
	}
	if out := f.stdout.String(); !strings.Contains(out, "Created cron job") || !strings.Contains(out, "nightly") {
		t.Errorf("add output = %q", out)
	}
	jobs := jobsOf(t, f)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %v", jobs)
	}
	job := jobs[0]
	id, _ := job["id"].(string)
	if len(id) != 12 || job["name"] != "nightly" || job["prompt"] != "Summarize the repo" {
		t.Errorf("job = %v", job)
	}
	if job["schedule_display"] != "every 30m" || job["next_run_at"] != "2026-10-24T08:30:00+00:00" {
		t.Errorf("schedule = %v / %v", job["schedule_display"], job["next_run_at"])
	}
	if repeat, _ := job["repeat"].(map[string]any); repeat["times"] != float64(3) {
		t.Errorf("repeat = %v", job["repeat"])
	}

	f.stdout.Reset()
	if code := cronCmd(ctx, []string{"list"}, f.env()); code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, f.stderr.String())
	}
	out := f.stdout.String()
	for _, want := range []string{id, "nightly", "every 30m", "2026-10-24 08:30", "scheduled"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}

	f.stdout.Reset()
	if code := cronCmd(ctx, []string{"remove", id}, f.env()); code != 0 {
		t.Fatalf("remove exit %d, stderr: %s", code, f.stderr.String())
	}
	if out := f.stdout.String(); !strings.Contains(out, "Removed cron job "+id) {
		t.Errorf("remove output = %q", out)
	}
	if jobs := jobsOf(t, f); len(jobs) != 0 {
		t.Errorf("jobs left = %v", jobs)
	}

	f.stderr.Reset()
	if code := cronCmd(ctx, []string{"remove", id}, f.env()); code != 1 {
		t.Errorf("remove missing exit = %d", code)
	}
	if err := f.stderr.String(); !strings.Contains(err, "no cron job with id") {
		t.Errorf("remove missing stderr = %q", err)
	}
}

func TestCronAddRejectsBadInput(t *testing.T) {
	f := cronEnv(t, "")
	ctx := context.Background()

	f.stderr.Reset()
	if code := cronCmd(ctx, []string{"add", "--name", "x", "--prompt", "p"}, f.env()); code != 2 {
		t.Errorf("missing schedule exit = %d", code)
	}
	if err := f.stderr.String(); !strings.Contains(err, "cron add needs --schedule") {
		t.Errorf("stderr = %q", err)
	}

	f.stderr.Reset()
	if code := cronCmd(ctx, []string{"add", "--schedule", "30m", "--prompt", "p"}, f.env()); code != 0 {
		t.Fatalf("add without name exit = %d, stderr: %s", code, f.stderr.String())
	}
	if jobs := jobsOf(t, f); jobs[0]["name"] != "p" {
		t.Errorf("name derived from prompt = %v", jobs[0]["name"])
	}

	f.stderr.Reset()
	if code := cronCmd(ctx, []string{"add", "--schedule", "banana", "--prompt", "p"}, f.env()); code != 1 {
		t.Errorf("bad schedule exit = %d", code)
	}
	if err := f.stderr.String(); !strings.Contains(err, "invalid schedule") {
		t.Errorf("stderr = %q", err)
	}

	f.stderr.Reset()
	if code := cronCmd(ctx, []string{"bogus"}, f.env()); code != 2 {
		t.Errorf("unknown subcommand exit = %d", code)
	}
	if err := f.stderr.String(); !strings.Contains(err, "cron <list|add|remove|run|tick|serve>") {
		t.Errorf("usage = %q", err)
	}
}

func TestCronRun(t *testing.T) {
	srv, seen := echoModel(t)
	f := cronEnv(t, modelConfig(srv.URL))
	ctx := context.Background()

	if code := cronCmd(ctx, []string{"add", "--name", "nightly", "--schedule", "0 9 * * *", "--prompt", "report the news"}, f.env()); code != 0 {
		t.Fatalf("add exit %d, stderr: %s", code, f.stderr.String())
	}
	id, _ := jobsOf(t, f)[0]["id"].(string)
	f.clock.Advance(5 * time.Minute)

	f.stdout.Reset()
	if code := cronCmd(ctx, []string{"run", id}, f.env()); code != 0 {
		t.Fatalf("run exit %d, stderr: %s", code, f.stderr.String())
	}
	out := f.stdout.String()
	if !strings.Contains(out, "nightly") || !strings.Contains(out, "ok") {
		t.Errorf("run output = %q", out)
	}

	// The model saw the stored prompt.
	calls := seen()
	if len(calls) != 1 {
		t.Fatalf("model calls = %d", len(calls))
	}
	last := calls[0][len(calls[0])-1]
	if !strings.Contains(last, "report the news") || !strings.Contains(last, "scheduled cron job") {
		t.Errorf("prompt sent = %q", last)
	}

	docs, err := filepath.Glob(filepath.Join(f.vars["HERMES_HOME"], "cron", "output", id, "*.md"))
	if err != nil || len(docs) != 1 {
		t.Fatalf("output files = %v, %v", docs, err)
	}
	if filepath.Base(docs[0]) != "2026-10-24_08-05-00.md" {
		t.Errorf("output name = %q", filepath.Base(docs[0]))
	}
	data, err := os.ReadFile(docs[0])
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	for _, want := range []string{
		"# Cron Job: nightly", "**Job ID:** " + id, "**Schedule:** 0 9 * * *",
		"## Prompt", "report the news", "## Response", "re: ",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("transcript lacks %q:\n%s", want, doc)
		}
	}

	job := jobsOf(t, f)[0]
	if job["last_status"] != "ok" || job["last_run_at"] != "2026-10-24T08:05:00+00:00" {
		t.Errorf("job after run = %v", job)
	}
	if job["failure_streak"] != float64(0) {
		t.Errorf("failure_streak = %v", job["failure_streak"])
	}

	f.stderr.Reset()
	if code := cronCmd(ctx, []string{"run", "nope"}, f.env()); code != 1 {
		t.Errorf("unknown id exit = %d", code)
	}
	if err := f.stderr.String(); !strings.Contains(err, "no cron job with id") {
		t.Errorf("stderr = %q", err)
	}
}

func TestCronTick(t *testing.T) {
	srv, seen := echoModel(t)
	f := cronEnv(t, modelConfig(srv.URL))
	ctx := context.Background()

	if code := cronCmd(ctx, []string{"add", "--schedule", "30m", "--prompt", "ping"}, f.env()); code != 0 {
		t.Fatalf("add exit %d, stderr: %s", code, f.stderr.String())
	}

	f.stdout.Reset()
	if code := cronCmd(ctx, []string{"tick"}, f.env()); code != 0 {
		t.Fatalf("idle tick exit %d, stderr: %s", code, f.stderr.String())
	}
	if out := f.stdout.String(); !strings.Contains(out, "No jobs due") {
		t.Errorf("idle tick output = %q", out)
	}
	if len(seen()) != 0 {
		t.Fatalf("idle tick ran a job")
	}

	f.clock.Advance(30 * time.Minute)
	f.stdout.Reset()
	if code := cronCmd(ctx, []string{"tick"}, f.env()); code != 0 {
		t.Fatalf("tick exit %d, stderr: %s", code, f.stderr.String())
	}
	if out := f.stdout.String(); !strings.Contains(out, "Ran 1 job(s)") {
		t.Errorf("tick output = %q", out)
	}
	if len(seen()) != 1 {
		t.Fatalf("model calls = %d", len(seen()))
	}

	// At-most-once: the same instant never fires twice.
	f.stdout.Reset()
	if code := cronCmd(ctx, []string{"tick"}, f.env()); code != 0 {
		t.Fatalf("second tick exit %d", code)
	}
	if len(seen()) != 1 {
		t.Fatalf("fired twice: %d calls", len(seen()))
	}
	if out := f.stdout.String(); !strings.Contains(out, "No jobs due") {
		t.Errorf("second tick output = %q", out)
	}
}

func TestCronServe(t *testing.T) {
	srv, seen := echoModel(t)
	f := cronEnv(t, modelConfig(srv.URL))
	if code := cronCmd(context.Background(), []string{"add", "--schedule", "1m", "--prompt", "ping"}, f.env()); code != 0 {
		t.Fatalf("add exit %d, stderr: %s", code, f.stderr.String())
	}
	// A 1m job added at 08:00 is due at 08:01; move the clock so the first tick
	// has something to fire.
	f.clock.Advance(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Cancel once the first run reached the model: a fixed sleep races the
		// runtime startup and can cancel the run before it dispatches.
		deadline := time.Now().Add(10 * time.Second)
		for len(seen()) == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	code := cronCmd(ctx, []string{"serve"}, f.env())
	if code != 0 {
		t.Fatalf("serve exit %d, stderr: %s", code, f.stderr.String())
	}
	// The first tick fires before the wait, so the due job ran exactly once.
	if len(seen()) != 1 {
		t.Fatalf("model calls = %d", len(seen()))
	}
}
