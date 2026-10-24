package cron

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// jobsLockName is the cross-process lock around a jobs.json write.
	jobsLockName = ".jobs.lock"
	// tickLockName guards the ticker.
	tickLockName = ".tick.lock"
	// jobsLockTimeout is how long a writer waits for the jobs lock.
	jobsLockTimeout = 30 * time.Second
	// tickLockStale is how old a tick lock must be before a later tick may break
	// it: a killed ticker leaves its lock behind.
	tickLockStale = 10 * time.Minute
	// outputTimeLayout names a run document.
	outputTimeLayout = "2006-01-02_15-04-05"
)

// Store is the cron directory: jobs.json, the locks and the run documents.
type Store struct {
	// Dir is <hermes home>/cron.
	Dir string
	// now is the injected clock, so tests never depend on wall time.
	now func() time.Time
}

// NewStore opens the cron store at dir. now is the clock every write stamps.
func NewStore(dir string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{Dir: dir, now: now}
}

// JobsPath is the jobs.json path.
func (s *Store) JobsPath() string { return filepath.Join(s.Dir, "jobs.json") }

// TickLockPath is the ticker lock's path.
func (s *Store) TickLockPath() string { return filepath.Join(s.Dir, tickLockName) }

// OutputDir is one job's run-document directory.
func (s *Store) OutputDir(jobID string) string {
	return filepath.Join(s.Dir, "output", jobID)
}

// Load reads jobs.json. A missing file is an empty store, not an error.
func (s *Store) Load() ([]*Job, error) {
	env, err := s.readEnvelope()
	if err != nil {
		return nil, err
	}
	return env.jobs, nil
}

// Save writes jobs.json atomically: a temp file in the same directory, synced
// and renamed over the old one, so a reader never sees a partial store.
func (s *Store) Save(jobs []*Job) error {
	return s.withLock(func() error { return s.save(jobs) })
}

// Add appends one job and persists the store.
func (s *Store) Add(job *Job) error {
	if job == nil {
		return errors.New("cron: add a nil job")
	}
	return s.withLock(func() error {
		jobs, err := s.Load()
		if err != nil {
			return err
		}
		return s.save(append(jobs, job))
	})
}

// Find returns the job with id, or nil when there is none.
func (s *Store) Find(id string) (*Job, error) {
	jobs, err := s.Load()
	if err != nil {
		return nil, err
	}
	for _, job := range jobs {
		if job.ID == id {
			return job, nil
		}
	}
	return nil, nil
}

// Update edits the job list under the lock. fn reports whether to persist.
func (s *Store) Update(fn func(jobs []*Job) (bool, error)) error {
	if fn == nil {
		return errors.New("cron: update with no function")
	}
	return s.withLock(func() error {
		jobs, err := s.Load()
		if err != nil {
			return err
		}
		save, err := fn(jobs)
		if err != nil || !save {
			return err
		}
		return s.save(jobs)
	})
}

// Remove deletes the job with id, reporting whether it existed.
func (s *Store) Remove(id string) (bool, error) {
	removed := false
	err := s.withLock(func() error {
		jobs, err := s.Load()
		if err != nil {
			return err
		}
		kept := make([]*Job, 0, len(jobs))
		for _, job := range jobs {
			if job.ID == id {
				removed = true
				continue
			}
			kept = append(kept, job)
		}
		if !removed {
			return nil
		}
		return s.save(kept)
	})
	return removed, err
}

// SaveOutput writes one run document and prunes the job's older documents.
// keep is a positive retention cap; zero or less keeps everything.
func (s *Store) SaveOutput(jobID string, now time.Time, doc string, keep int) (string, error) {
	dir := s.OutputDir(jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cron: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, now.Format(outputTimeLayout)+".md")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		return "", fmt.Errorf("cron: write %s: %w", path, err)
	}
	s.PruneOutput(jobID, keep)
	return path, nil
}

// PruneOutput keeps the newest keep run documents and reports how many it
// removed. A non-positive keep disables pruning, as upstream's opt-out does.
func (s *Store) PruneOutput(jobID string, keep int) int {
	if keep <= 0 {
		return 0
	}
	dir := s.OutputDir(jobID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		names = append(names, entry.Name())
	}
	// Document names start with a timestamp, so newest-first is reverse lexical.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	pruned := 0
	for i := keep; i < len(names); i++ {
		if err := os.Remove(filepath.Join(dir, names[i])); err == nil {
			pruned++
		}
	}
	return pruned
}

// envelope is jobs.json: the modelled job list plus every other top-level key.
type envelope struct {
	jobs  []*Job
	other map[string]json.RawMessage
}

func (s *Store) readEnvelope() (envelope, error) {
	env := envelope{other: map[string]json.RawMessage{}}
	data, err := os.ReadFile(s.JobsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return env, nil
	}
	if err != nil {
		return env, fmt.Errorf("cron: read %s: %w", s.JobsPath(), err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return env, fmt.Errorf("cron: decode %s: %w", s.JobsPath(), err)
	}
	if jobData, ok := raw["jobs"]; ok {
		if err := json.Unmarshal(jobData, &env.jobs); err != nil {
			return env, fmt.Errorf("cron: decode jobs in %s: %w", s.JobsPath(), err)
		}
	}
	delete(raw, "jobs")
	env.other = raw
	return env, nil
}

// save writes the envelope as it stands, keeping every key it does not model.
func (s *Store) save(jobs []*Job) error {
	env, err := s.readEnvelope()
	if err != nil {
		return err
	}
	if jobs == nil {
		jobs = []*Job{}
	}
	out := make(map[string]json.RawMessage, len(env.other)+2)
	for key, value := range env.other {
		out[key] = value
	}
	jobData, err := json.Marshal(jobs)
	if err != nil {
		return fmt.Errorf("cron: encode jobs: %w", err)
	}
	out["jobs"] = jobData
	updated, err := json.Marshal(stamp(s.now()))
	if err != nil {
		return fmt.Errorf("cron: encode updated_at: %w", err)
	}
	out["updated_at"] = updated
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("cron: encode %s: %w", s.JobsPath(), err)
	}
	return s.writeAtomic(append(data, '\n'))
}

// writeAtomic lands data through a temp file plus rename.
func (s *Store) writeAtomic(data []byte) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("cron: create %s: %w", s.Dir, err)
	}
	tmp, err := os.CreateTemp(s.Dir, ".jobs-*.tmp")
	if err != nil {
		return fmt.Errorf("cron: temp file in %s: %w", s.Dir, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // a no-op once the rename succeeded
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cron: write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cron: sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cron: close %s: %w", name, err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("cron: chmod %s: %w", name, err)
	}
	if err := os.Rename(name, s.JobsPath()); err != nil {
		return fmt.Errorf("cron: rename %s: %w", name, err)
	}
	return nil
}

// withLock runs fn holding the cross-process jobs lock.
func (s *Store) withLock(fn func() error) error {
	release, err := s.lockJobs()
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// lockJobs takes the jobs lock by exclusive create and returns its release.
func (s *Store) lockJobs() (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("cron: create %s: %w", s.Dir, err)
	}
	path := filepath.Join(s.Dir, jobsLockName)
	deadline := time.Now().Add(jobsLockTimeout)
	for {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(file, "%d %s\n", os.Getpid(), stamp(s.now()))
			_ = file.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("cron: lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("cron: timed out waiting for %s", path)
		}
		// A writer that died mid-save leaves the lock behind: break it once it is
		// clearly older than the timeout.
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > jobsLockTimeout {
			_ = os.Remove(path)
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}
}
