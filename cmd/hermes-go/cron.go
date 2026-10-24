package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/umi008/hermes-agent-go/internal/cron"
	"github.com/umi008/hermes-agent-go/internal/home"
)

// cronUsage is the `hermes-go cron` help text.
const cronUsage = `Usage: hermes-go cron <list|add|remove|run|tick|serve> [flags]

  list                 List scheduled jobs
  add                  Create a job: --schedule, --prompt, optional --name, --repeat
  remove <id>          Delete a job
  run <id>             Run a job now, ignoring its schedule
  tick                 Run the jobs that are due, once
  serve                Tick every 60s until interrupted

Run "hermes-go cron add -h" for the add flags.
`

// cronTickInterval is how often `cron serve` ticks, as upstream.
const cronTickInterval = 60 * time.Second

// cronCmd runs the `hermes-go cron` subcommands.
func cronCmd(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 {
		_, _ = io.WriteString(env.Stderr, cronUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		_, _ = io.WriteString(env.Stdout, cronUsage)
		return 0
	case "list":
		return cronList(env, args[1:])
	case "add":
		return cronAdd(ctx, env, args[1:])
	case "remove":
		return cronRemove(env, args[1:])
	case "run":
		return cronRun(ctx, env, args[1:])
	case "tick":
		return cronTick(ctx, env, args[1:])
	case "serve":
		return cronServe(ctx, env, args[1:])
	}
	_, _ = fmt.Fprintf(env.Stderr, "hermes-go: unknown cron subcommand %q\n\n%s", args[0], cronUsage)
	return 2
}

// cronStore opens the profile's cron store with the injected clock.
func cronStore(env Env, profile string) (*cron.Store, error) {
	h, err := home.Resolver{GOOS: env.GOOS, Getenv: env.Getenv, UserHomeDir: env.UserHomeDir}.Resolve(profile)
	if err != nil {
		return nil, err
	}
	return cron.NewStore(filepath.Join(h.Dir, "cron"), envNow(env)), nil
}

// envNow is the process clock, defaulting to the wall clock.
func envNow(env Env) func() time.Time {
	if env.Now != nil {
		return env.Now
	}
	return time.Now
}

// cronList prints one line per job.
func cronList(env Env, args []string) int {
	f, ok := parseCronArgs(env, "cron list", args, nil)
	if !ok {
		return f.code
	}
	store, err := cronStore(env, f.profile)
	if err != nil {
		return cronFail(env, err)
	}
	jobs, err := store.Load()
	if err != nil {
		return cronFail(env, err)
	}
	if len(jobs) == 0 {
		_, _ = fmt.Fprintln(env.Stdout, "No scheduled jobs")
		return 0
	}
	_, _ = fmt.Fprintf(env.Stdout, "Scheduled jobs (%d):\n", len(jobs))
	for _, job := range jobs {
		next := "-"
		if at, parsed := time.Parse(cron.TimeLayout, job.NextRunAt); parsed == nil {
			next = at.Format("2006-01-02 15:04")
		}
		_, _ = fmt.Fprintf(env.Stdout, "  %s  %s  %s  next %s  %s\n",
			job.ID, job.Name, job.ScheduleDisplay, next, job.State)
	}
	return 0
}

// cronAdd creates one job from the command line.
func cronAdd(_ context.Context, env Env, args []string) int {
	var name, schedule, prompt string
	var repeat int
	f, ok := parseCronArgs(env, "cron add", args, func(fs *flag.FlagSet) {
		fs.StringVar(&name, "name", "", "job name (default: derived from the prompt)")
		fs.StringVar(&schedule, "schedule", "", `when to run: "30m", "0 9 * * *", "2026-11-01T09:00:00Z", "in 30m"`)
		fs.StringVar(&prompt, "prompt", "", "prompt to send on every run")
		fs.IntVar(&repeat, "repeat", 0, "run at most N times (0 = forever)")
	})
	if !ok {
		return f.code
	}
	if schedule == "" {
		_, _ = fmt.Fprintf(env.Stderr, "hermes-go: cron add needs --schedule\n\n%s", cronUsage)
		return 2
	}
	if strings.TrimSpace(prompt) == "" {
		_, _ = fmt.Fprintf(env.Stderr, "hermes-go: cron add needs --prompt\n\n%s", cronUsage)
		return 2
	}
	store, err := cronStore(env, f.profile)
	if err != nil {
		return cronFail(env, err)
	}
	job, err := cron.NewJob(cron.JobOptions{
		Name:     name,
		Schedule: schedule,
		Prompt:   prompt,
		Repeat:   repeat,
		Now:      envNow(env)(),
	})
	if err != nil {
		return cronFail(env, err)
	}
	if err := store.Add(job); err != nil {
		return cronFail(env, err)
	}
	_, _ = fmt.Fprintf(env.Stdout, "Created cron job %s (%s): %s\n", job.Name, job.ID, job.ScheduleDisplay)
	return 0
}

// cronRemove deletes one job.
func cronRemove(env Env, args []string) int {
	f, rest, ok := parseCronCommand(env, "cron remove", args)
	if !ok {
		return f.code
	}
	if len(rest) != 1 {
		_, _ = fmt.Fprintf(env.Stderr, "hermes-go: cron remove needs a job id\n\n%s", cronUsage)
		return 2
	}
	store, err := cronStore(env, f.profile)
	if err != nil {
		return cronFail(env, err)
	}
	removed, err := store.Remove(rest[0])
	if err != nil {
		return cronFail(env, err)
	}
	if !removed {
		return cronFail(env, fmt.Errorf("no cron job with id %q", rest[0]))
	}
	_, _ = fmt.Fprintf(env.Stdout, "Removed cron job %s\n", rest[0])
	return 0
}

// cronRun dispatches one job now, ignoring its schedule.
func cronRun(ctx context.Context, env Env, args []string) int {
	f, rest, ok := parseCronCommand(env, "cron run", args)
	if !ok {
		return f.code
	}
	if len(rest) != 1 {
		_, _ = fmt.Fprintf(env.Stderr, "hermes-go: cron run needs a job id\n\n%s", cronUsage)
		return 2
	}
	store, err := cronStore(env, f.profile)
	if err != nil {
		return cronFail(env, err)
	}
	job, err := store.Find(rest[0])
	if err != nil {
		return cronFail(env, err)
	}
	if job == nil {
		return cronFail(env, fmt.Errorf("no cron job with id %q", rest[0]))
	}
	rt, err := newCronRuntime(ctx, env, f.profile, f.verbose)
	if err != nil {
		return cronFail(env, err)
	}
	defer rt.close()
	ticker := cronTicker(rt, store)
	if err := ticker.RunNow(ctx, job.ID); err != nil {
		return cronFail(env, err)
	}
	_, _ = fmt.Fprintf(env.Stdout, "Cron job %q (%s): ok\n", job.Name, job.ID)
	return 0
}

// cronTick fires every due job once.
func cronTick(ctx context.Context, env Env, args []string) int {
	f, ok := parseCronArgs(env, "cron tick", args, nil)
	if !ok {
		return f.code
	}
	rt, err := newCronRuntime(ctx, env, f.profile, f.verbose)
	if err != nil {
		return cronFail(env, err)
	}
	defer rt.close()
	store, err := cronStore(env, f.profile)
	if err != nil {
		return cronFail(env, err)
	}
	ran, err := cronTicker(rt, store).Tick(ctx)
	if err != nil {
		return cronFail(env, err)
	}
	reportTick(env, ran)
	return 0
}

// cronServe ticks now and then every cronTickInterval until ctx is done.
func cronServe(ctx context.Context, env Env, args []string) int {
	f, ok := parseCronArgs(env, "cron serve", args, nil)
	if !ok {
		return f.code
	}
	rt, err := newCronRuntime(ctx, env, f.profile, f.verbose)
	if err != nil {
		return cronFail(env, err)
	}
	defer rt.close()
	store, err := cronStore(env, f.profile)
	if err != nil {
		return cronFail(env, err)
	}
	ticker := cronTicker(rt, store)
	// Ctrl-C stops the loop the same way a cancelled context does.
	if env.Interrupt != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-env.Interrupt:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	for {
		ran, err := ticker.Tick(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return 0
		case err != nil:
			_, _ = fmt.Fprintf(env.Stderr, "hermes-go: cron tick: %v\n", err)
		default:
			reportTick(env, ran)
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(cronTickInterval):
		}
	}
}

// reportTick prints one line per tick.
func reportTick(env Env, ran int) {
	if ran == 0 {
		_, _ = fmt.Fprintln(env.Stdout, "No jobs due")
		return
	}
	_, _ = fmt.Fprintf(env.Stdout, "Ran %d job(s)\n", ran)
}

// cronTicker wires the store and the agent runner to a ticker, honouring
// cron.output_retention from config.yaml.
func cronTicker(rt *chatRuntime, store *cron.Store) *cron.Ticker {
	keep := cron.DefaultOutputRetention
	if n, ok := rt.cfg.Int("cron", "output_retention"); ok {
		keep = n
	}
	return &cron.Ticker{
		Store:  store,
		Now:    rt.env.Now,
		Runner: cronRunner{rt: rt},
		Keep:   keep,
	}
}

// newCronRuntime builds the shared chat runtime a run, tick or serve needs:
// config, endpoint, tools and the store, with no interactive session.
func newCronRuntime(ctx context.Context, env Env, profile string, verbose bool) (*chatRuntime, error) {
	level := slog.LevelWarn
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: level}))
	return newChatRuntime(ctx, chatFlags{profile: profile, verbose: verbose, querySet: true}, env, logger, false)
}

// cronRunner is the ticker's dispatcher: it runs the job's prompt through the
// shared agent and renders the run document.
type cronRunner struct{ rt *chatRuntime }

// Run sends the job's prompt through a fresh session and returns the transcript
// to persist, whether the run succeeded or not.
func (r cronRunner) Run(ctx context.Context, job *cron.Job) (string, error) {
	session := r.rt.newSession(ctx, "cron", io.Discard)
	defer session.end("cron_run")
	prompt := cronPrompt(job)
	res, err := session.run(ctx, prompt)
	return cronDocument(job, prompt, res.Text, err, r.rt.env.Now()), err
}

// cronHint mirrors upstream's cron prompt header: the response is the report.
const cronHint = "[IMPORTANT: You are running as a scheduled cron job. " +
	"Your final response is the report the operator reads — do NOT try to deliver it yourself. " +
	"Execute the task now; never create or update a cron job because of recurring or future-schedule language in the task below.]"

// cronPrompt is the effective prompt: the cron hint plus the stored prompt.
func cronPrompt(job *cron.Job) string {
	return cronHint + "\n\n" + job.Prompt
}

// cronDocument renders the markdown run document saved under
// cron/output/<job id>/.
func cronDocument(job *cron.Job, prompt, response string, runErr error, now time.Time) string {
	if strings.TrimSpace(response) == "" {
		switch {
		case runErr != nil:
			response = "Error: " + runErr.Error()
		default:
			response = "(No response generated)"
		}
	}
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "# Cron Job: %s\n\n", job.Name)
	_, _ = fmt.Fprintf(&b, "**Job ID:** %s\n", job.ID)
	_, _ = fmt.Fprintf(&b, "**Run Time:** %s\n", now.Format("2006-01-02 15:04:05"))
	_, _ = fmt.Fprintf(&b, "**Schedule:** %s\n\n", job.ScheduleDisplay)
	_, _ = fmt.Fprintf(&b, "## Prompt\n\n%s\n\n", prompt)
	_, _ = fmt.Fprintf(&b, "## Response\n\n%s\n", response)
	return b.String()
}

// cronFlags are the shared `cron` options.
type cronFlags struct {
	profile string
	verbose bool
	code    int // set by parseCronArgs when parsing failed
}

// parseCronCommand parses only the shared cron flags and returns the positional
// arguments.
func parseCronCommand(env Env, name string, args []string) (cronFlags, []string, bool) {
	f := cronFlags{}
	rest, ok := parseCronArgsInto(env, name, args, &f, nil)
	return f, rest, ok
}

// parseCronArgs parses the shared cron flags plus extra's own flags.
func parseCronArgs(env Env, name string, args []string, extra func(*flag.FlagSet)) (cronFlags, bool) {
	f := cronFlags{}
	_, ok := parseCronArgsInto(env, name, args, &f, extra)
	return f, ok
}

func parseCronArgsInto(env Env, name string, args []string, f *cronFlags, extra func(*flag.FlagSet)) ([]string, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	for _, alias := range []string{"p", "profile"} {
		fs.StringVar(&f.profile, alias, "", "profile name")
	}
	fs.BoolVar(&f.verbose, "v", false, "log debug output to stderr")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			f.code = 0
		} else {
			f.code = 2
		}
		return nil, false
	}
	return fs.Args(), true
}

// cronFail reports err and returns the exit code for a runtime failure.
func cronFail(env Env, err error) int {
	_, _ = fmt.Fprintf(env.Stderr, "hermes-go: %v\n", err)
	return 1
}
