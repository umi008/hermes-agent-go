package cron

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// Runner dispatches one job and returns the run document to persist. The
// document is written whatever the outcome, so a failed run still leaves a
// transcript for the operator.
type Runner interface {
	Run(ctx context.Context, job *Job) (string, error)
}

// DefaultOutputRetention is cron.output_retention's default: keep the newest 50
// run documents per job.
const DefaultOutputRetention = 50

// Ticker fires due jobs once per Tick.
type Ticker struct {
	Store  *Store
	Now    func() time.Time
	Runner Runner
	// Keep is the per-job run-document retention. Zero means
	// DefaultOutputRetention; a negative value keeps everything.
	Keep int
}

// Tick fires every due job once and reports how many it dispatched. A job is due
// when its stored next_run_at has arrived; the slot is persisted as consumed
// before dispatch, so a crash mid-run can never fire it twice.
func (t *Ticker) Tick(ctx context.Context) (int, error) {
	release, ok, err := t.lock()
	if err != nil || !ok {
		return 0, err
	}
	defer release()

	jobs, err := t.Store.Load()
	if err != nil {
		return 0, err
	}
	now := t.now()
	due := make([]*Job, 0, len(jobs))
	pending := make([]string, 0, len(jobs))
	changed := false
	for _, job := range jobs {
		if !job.Enabled || job.State == "paused" || job.State == "cancelled" || job.NextRunAt == "" {
			continue
		}
		next, ok := parseStamp(job.NextRunAt)
		if !ok || next.After(now) {
			continue
		}
		if job.Schedule.Kind == KindOnce && (job.LastRunAt != "" || now.Sub(next) > oneshotGrace) {
			// Already fired, or missed past its grace: retire instead of firing.
			retire(job)
			changed = true
			continue
		}
		due = append(due, job)
		pending = append(pending, job.NextRunAt)
	}
	if len(due) > 0 {
		for _, job := range due {
			// At-most-once: mark the slot consumed before dispatch, and mark it with
			// the next occurrence rather than now, so a process that dies mid-run
			// does not replay the job on the following tick.
			if next, ok := job.Schedule.NextRun(stamp(now), now); ok {
				job.NextRunAt = next
			} else {
				// No next occurrence: a one-shot. An empty next_run_at means "not due".
				job.NextRunAt = ""
			}
		}
		changed = true
	}
	if changed {
		if err := t.Store.Save(jobs); err != nil {
			return 0, err
		}
	}
	for i, job := range due {
		doc, runErr := t.Runner.Run(ctx, job)
		if doc != "" {
			// Run documents are best effort, as upstream's are: a failed write must
			// not stop the bookkeeping below.
			_, _ = t.Store.SaveOutput(job.ID, now, doc, t.keep())
		}
		t.finish(job, pending[i], runErr, now)
	}
	if len(due) > 0 {
		if err := t.Store.Save(jobs); err != nil {
			return 0, err
		}
	}
	return len(due), nil
}

// RunNow dispatches one job regardless of its schedule, recording last_run_at
// and advancing next_run_at the same way a scheduled run does.
func (t *Ticker) RunNow(ctx context.Context, id string) error {
	jobs, err := t.Store.Load()
	if err != nil {
		return err
	}
	var job *Job
	for _, candidate := range jobs {
		if candidate.ID == id {
			job = candidate
			break
		}
	}
	if job == nil {
		return fmt.Errorf("no cron job with id %q", id)
	}
	now := t.now()
	previous := job.NextRunAt
	// At-most-once: the consumed slot is durable before the run starts, and it
	// already points past now so a crash mid-run cannot replay the job.
	if next, ok := job.Schedule.NextRun(stamp(now), now); ok {
		job.NextRunAt = next
	} else {
		job.NextRunAt = ""
	}
	if err := t.Store.Save(jobs); err != nil {
		return err
	}
	doc, runErr := t.Runner.Run(ctx, job)
	if doc != "" {
		_, _ = t.Store.SaveOutput(job.ID, now, doc, t.keep())
	}
	t.finish(job, previous, runErr, now)
	if err := t.Store.Save(jobs); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("cron job %s failed: %w", id, runErr)
	}
	return nil
}

// finish records the outcome of one run and arms the next one.
func (t *Ticker) finish(job *Job, previousNext string, runErr error, now time.Time) {
	at := stamp(now)
	job.LastRunAt = at
	if runErr != nil {
		job.LastStatus = "error"
		job.LastError = runErr.Error()
		job.FailureStreak++
		// The most recent failure stays visible after a later success.
		job.LastFailure = &Failure{At: at, Detail: runErr.Error()}
	} else {
		job.LastStatus = "ok"
		job.LastError = ""
		job.FailureStreak = 0
	}
	if job.Repeat == nil {
		job.Repeat = &Repeat{}
	}
	job.Repeat.Completed++
	if times := job.Repeat.Times; times != nil && *times > 0 && job.Repeat.Completed >= *times {
		retire(job)
		return
	}
	// The run consumed a slot. A job that fired early consumes the pending slot;
	// one that fired late re-anchors on now, which collapses a long outage into a
	// single catch-up run instead of a burst of replays.
	base := now
	if pending, ok := parseStamp(previousNext); ok && pending.After(now) {
		base = pending
	}
	next, ok := job.Schedule.NextRun(stamp(base), now)
	if !ok {
		if job.Schedule.Kind == KindOnce {
			retire(job)
			return
		}
		// A recurring schedule that cannot compute a next run stays enabled so a
		// transient failure is not silently turned into "completed".
		job.State = "error"
		job.NextRunAt = ""
		return
	}
	job.NextRunAt = next
	job.State = "scheduled"
}

// retire marks a job finished: it has no further run and never fires again.
func retire(job *Job) {
	job.State = "completed"
	job.Enabled = false
	job.NextRunAt = ""
}

// keep is the effective run-document retention.
func (t *Ticker) keep() int {
	if t.Keep == 0 {
		return DefaultOutputRetention
	}
	return t.Keep
}

func (t *Ticker) now() time.Time {
	if t.Now == nil {
		return time.Now()
	}
	return t.Now()
}

// lock takes the tick lock. ok is false when another ticker holds a live lock,
// in which case this tick does nothing. A lock left by a ticker that died is
// broken once it is older than tickLockStale.
func (t *Ticker) lock() (release func(), ok bool, err error) {
	path := t.Store.TickLockPath()
	if err := os.MkdirAll(t.Store.Dir, 0o755); err != nil {
		return nil, false, fmt.Errorf("cron: create %s: %w", t.Store.Dir, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(file, "%d %s\n", os.Getpid(), stamp(t.now()))
			_ = file.Close()
			return func() { _ = os.Remove(path) }, true, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, false, fmt.Errorf("cron: tick lock %s: %w", path, err)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			// The holder released it between our create and stat: retry once.
			continue
		}
		// A future mtime is clock skew, not a dead ticker: keep the lock.
		if age := t.now().Sub(info.ModTime()); age < tickLockStale {
			return nil, false, nil
		}
		_ = os.Remove(path)
	}
	return nil, false, nil
}
