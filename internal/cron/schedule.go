// Package cron ports upstream Hermes Agent's cron store and scheduler: the
// jobs.json envelope, schedule parsing and the at-most-once tick (cron/jobs.py,
// cron/AGENTS.md).
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule kinds, as stored under schedule.kind.
const (
	KindOnce     = "once"
	KindInterval = "interval"
	KindCron     = "cron"
)

// TimeLayout is the timestamp shape upstream writes: Python's isoformat() with
// a numeric UTC offset, never a trailing "Z".
const TimeLayout = "2006-01-02T15:04:05-07:00"

const (
	// oneshotGrace is the window in which a past one-shot is still eligible.
	// A one-shot older than this is retired as missed instead of fired.
	oneshotGrace = 120 * time.Second
	// minGrace floors the catch-up window of a frequent job; maxGrace caps it
	// for a sparse one. The window is half the period in between.
	minGrace = 2 * time.Minute
	maxGrace = 2 * time.Hour
	// cronSearchYears bounds the search for the next cron match, so an
	// impossible expression ("0 9 31 2 *") terminates instead of spinning.
	cronSearchYears = 5
)

// stamp formats t the way upstream stores timestamps.
func stamp(t time.Time) string { return t.Format(TimeLayout) }

// parseStamp reads a stored timestamp, reporting false when it is not ISO.
func parseStamp(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Schedule is a parsed schedule. Kind says which field carries it: Minutes for
// an interval, Expr for a cron expression, RunAt for a one-shot.
type Schedule struct {
	Kind    string // KindOnce, KindInterval or KindCron
	Minutes int    // interval length, KindInterval only
	Expr    string // 5-field expression, KindCron only
	RunAt   string // one-shot instant, KindOnce only
	Display string // human wording, e.g. "every 30m"

	// extra keeps schedule keys this port does not model (timezone, ...) so a
	// write never drops them.
	extra rawObject
}

// ParseSchedule parses the schedule strings `cron add` accepts, as upstream
// parse_schedule: an interval ("30m", "every 2h", "hour"), a 5-field cron
// expression, an ISO timestamp, or "in 30m". now anchors both "in ..." and the
// timezone of a naive timestamp.
func ParseSchedule(in string, now time.Time) (Schedule, error) {
	schedule := strings.TrimSpace(in)
	if schedule == "" {
		return Schedule{}, fmt.Errorf("invalid schedule %q: it is empty", in)
	}
	lower := strings.ToLower(schedule)

	// "every X" is either an interval or a natural day/time phrase, which this
	// port does not support: upstream needs croniter for those.
	if strings.HasPrefix(lower, "every ") {
		rest := strings.TrimSpace(schedule[len("every "):])
		if minutes, ok := parseDuration(rest); ok {
			return intervalSchedule(minutes), nil
		}
		return Schedule{}, fmt.Errorf(
			"invalid schedule %q: weekday/time phrases like \"every monday 9am\" are not supported; use a cron expression such as \"0 9 * * 1\"", in)
	}

	// Five numeric fields are a cron expression. Letters (named months or
	// weekdays) never reach the parser, so they fall through to the error below
	// instead of being reported as a malformed expression.
	if fields := strings.Fields(schedule); len(fields) == 5 {
		numeric := true
		for _, f := range fields {
			if !cronFieldRE.MatchString(f) {
				numeric = false
				break
			}
		}
		if numeric {
			if _, err := parseCronExpr(fields); err != nil {
				return Schedule{}, fmt.Errorf("invalid cron expression %q: %w", schedule, err)
			}
			return Schedule{Kind: KindCron, Expr: schedule, Display: schedule}, nil
		}
	}

	// An ISO timestamp (or a bare date) is a one-shot.
	if strings.Contains(schedule, "T") || datePrefixRE.MatchString(schedule) {
		runAt, err := parseTimestamp(schedule, now)
		if err != nil {
			return Schedule{}, fmt.Errorf("invalid timestamp %q: %w", schedule, err)
		}
		return Schedule{
			Kind:    KindOnce,
			RunAt:   stamp(runAt),
			Display: "once at " + runAt.Format("2006-01-02 15:04"),
		}, nil
	}

	// "in 30m" is the explicit one-shot-by-duration form.
	if strings.HasPrefix(lower, "in ") {
		text := strings.TrimSpace(schedule[len("in "):])
		minutes, ok := parseDuration(text)
		if !ok {
			return Schedule{}, fmt.Errorf("invalid schedule %q: use e.g. \"in 30m\" or \"in 2h\"", in)
		}
		return Schedule{
			Kind:    KindOnce,
			RunAt:   stamp(now.Add(time.Duration(minutes) * time.Minute)),
			Display: "once in " + text,
		}, nil
	}

	// A bare duration is a recurring interval.
	if minutes, ok := parseDuration(schedule); ok {
		return intervalSchedule(minutes), nil
	}

	return Schedule{}, fmt.Errorf(
		"invalid schedule %q. Use an interval (\"30m\", \"every 2h\"), a cron expression (\"0 9 * * *\"), a timestamp (\"2026-11-01T09:00:00\") or a delay (\"in 30m\")", in)
}

func intervalSchedule(minutes int) Schedule {
	return Schedule{Kind: KindInterval, Minutes: minutes, Display: fmt.Sprintf("every %dm", minutes)}
}

// NextRun is the next scheduled instant, rendered like every stored timestamp.
// lastRunAt anchors a recurring schedule so a restart cannot re-anchor it, and
// carries the "already fired" state of a one-shot. An interval anchors on its
// last run; a cron expression matches strictly after it.
func (s Schedule) NextRun(lastRunAt string, now time.Time) (string, bool) {
	if s.Kind == KindOnce {
		// A one-shot fires at most once, and only inside its grace window.
		if lastRunAt != "" {
			return "", false
		}
		runAt, ok := parseStamp(s.RunAt)
		if !ok || now.Sub(runAt) > oneshotGrace {
			return "", false
		}
		return s.RunAt, true
	}

	base := now
	if lastRunAt != "" {
		if parsed, ok := parseStamp(lastRunAt); ok {
			base = parsed
		}
	}
	switch s.Kind {
	case KindInterval:
		if s.Minutes <= 0 {
			return "", false
		}
		return stamp(base.Add(time.Duration(s.Minutes) * time.Minute)), true
	case KindCron:
		expr := s.cronExpr()
		next, ok := expr.next(base)
		if !ok {
			return "", false
		}
		return stamp(next), true
	}
	return "", false
}

// Grace is how late a run may start and still count as on time: half the
// period, clamped to [2m, 2h]. Frequent jobs fast-forward, daily ones catch up.
func (s Schedule) Grace(now time.Time) time.Duration {
	period := s.cadence(now)
	if period <= 0 {
		return minGrace
	}
	grace := period / 2
	if grace < minGrace {
		return minGrace
	}
	if grace > maxGrace {
		return maxGrace
	}
	return grace
}

// cadence is the period between two consecutive runs, zero when unknown.
func (s Schedule) cadence(now time.Time) time.Duration {
	switch s.Kind {
	case KindInterval:
		return time.Duration(s.Minutes) * time.Minute
	case KindCron:
		first, ok := s.NextRun("", now)
		if !ok {
			return 0
		}
		second, ok := s.NextRun(first, now)
		if !ok {
			return 0
		}
		from, okFrom := parseStamp(first)
		to, okTo := parseStamp(second)
		if !okFrom || !okTo {
			return 0
		}
		return to.Sub(from)
	}
	return 0
}

// --- durations ---------------------------------------------------------------

// parseDuration reads an interval length in minutes: "30m", "2h", "1d", or the
// bare unit words upstream accepts ("hour", "day", "minute").
func parseDuration(in string) (int, bool) {
	text := strings.ToLower(strings.TrimSpace(in))
	if text == "" {
		return 0, false
	}
	// Bare unit words are exactly one unit.
	switch text {
	case "min", "minute", "minutes":
		return 1, true
	case "hr", "hrs", "hour", "hours":
		return 60, true
	case "day", "days":
		return 1440, true
	}
	multiplier := 1
	switch {
	case strings.HasSuffix(text, "minutes"):
		text = strings.TrimSuffix(text, "minutes")
	case strings.HasSuffix(text, "minute"):
		text = strings.TrimSuffix(text, "minute")
	case strings.HasSuffix(text, "hours"):
		text = strings.TrimSuffix(text, "hours")
		multiplier = 60
	case strings.HasSuffix(text, "hour"):
		text = strings.TrimSuffix(text, "hour")
		multiplier = 60
	case strings.HasSuffix(text, "hrs"):
		text = strings.TrimSuffix(text, "hrs")
		multiplier = 60
	case strings.HasSuffix(text, "hr"):
		text = strings.TrimSuffix(text, "hr")
		multiplier = 60
	case strings.HasSuffix(text, "days"):
		text = strings.TrimSuffix(text, "days")
		multiplier = 1440
	case strings.HasSuffix(text, "day"):
		text = strings.TrimSuffix(text, "day")
		multiplier = 1440
	case strings.HasSuffix(text, "m"):
		text = strings.TrimSuffix(text, "m")
	case strings.HasSuffix(text, "h"):
		text = strings.TrimSuffix(text, "h")
		multiplier = 60
	case strings.HasSuffix(text, "d"):
		text = strings.TrimSuffix(text, "d")
		multiplier = 1440
	default:
		return 0, false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, false
	}
	value, err := strconv.Atoi(text)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value * multiplier, true
}

// --- timestamps --------------------------------------------------------------

// naiveTimestampLayouts are the shapes accepted without an offset; they are
// read in now's timezone, as upstream does with the configured Hermes zone.
var naiveTimestampLayouts = []string{
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseTimestamp(in string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, in); err == nil {
		return t, nil
	}
	for _, layout := range naiveTimestampLayouts {
		if t, err := time.ParseInLocation(layout, in, now.Location()); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("expected an ISO-8601 instant such as 2026-11-01T09:00:00")
}
