package cron

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	// cronFieldRE matches one numeric cron field: digits, "*", ranges, lists and
	// steps. Named months and weekdays are excluded on purpose, so "0 9 * * MON"
	// is reported as an unsupported schedule rather than a bad expression.
	cronFieldRE = regexp.MustCompile(`^[0-9*,/-]+$`)
	// datePrefixRE spots the "2026-11-01" a bare date or naive timestamp starts with.
	datePrefixRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)
)

// cronExpr is a parsed 5-field expression:
// minute hour day-of-month month day-of-week.
type cronExpr struct {
	minute, hour, dom, month, dow map[int]bool
	domAll, dowAll                bool // field was "*", so the other day field wins
}

// parseCronExpr parses the five fields of a cron expression.
func parseCronExpr(fields []string) (cronExpr, error) {
	var e cronExpr
	var err error
	if e.minute, _, err = parseCronField(fields[0], 0, 59); err != nil {
		return e, err
	}
	if e.hour, _, err = parseCronField(fields[1], 0, 23); err != nil {
		return e, err
	}
	if e.dom, e.domAll, err = parseCronField(fields[2], 1, 31); err != nil {
		return e, err
	}
	if e.month, _, err = parseCronField(fields[3], 1, 12); err != nil {
		return e, err
	}
	if e.dow, e.dowAll, err = parseCronField(fields[4], 0, 7); err != nil {
		return e, err
	}
	// Both 0 and 7 are Sunday.
	if e.dow[7] {
		delete(e.dow, 7)
		e.dow[0] = true
	}
	return e, nil
}

// parseCronField expands one field into its value set. base may be "*", a
// number, or a range; an optional "/step" applies to either.
func parseCronField(spec string, lo, hi int) (map[int]bool, bool, error) {
	set := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		if part == "" {
			return nil, false, fmt.Errorf("empty field")
		}
		base, stepText, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n <= 0 {
				return nil, false, fmt.Errorf("step %q must be a positive integer", stepText)
			}
			step = n
		}
		from, to := lo, hi
		switch {
		case base == "*":
			// The whole field, already in from and to.
		case strings.Contains(base, "-"):
			left, right, _ := strings.Cut(base, "-")
			var err error
			if from, err = strconv.Atoi(left); err != nil {
				return nil, false, fmt.Errorf("%q is not a number", left)
			}
			if to, err = strconv.Atoi(right); err != nil {
				return nil, false, fmt.Errorf("%q is not a number", right)
			}
		default:
			n, err := strconv.Atoi(base)
			if err != nil {
				return nil, false, fmt.Errorf("%q is not a number", base)
			}
			from, to = n, n
			if hasStep {
				// "5/15" runs from 5 up to the field's limit, as croniter does.
				to = hi
			}
		}
		if from < lo || to > hi || from > to {
			return nil, false, fmt.Errorf("%q is outside %d-%d", part, lo, hi)
		}
		for v := from; v <= to; v += step {
			set[v] = true
		}
	}
	return set, spec == "*", nil
}

// cronExpr re-parses the expression each time it is needed. A malformed stored
// expression yields an empty matcher, so the job simply has no next run.
func (s Schedule) cronExpr() cronExpr {
	expr, err := parseCronExpr(strings.Fields(s.Expr))
	if err != nil {
		return cronExpr{}
	}
	return expr
}

// next is the first matching minute strictly after base, in base's timezone.
// A day that cannot match is skipped whole, and an hour that cannot match is
// skipped whole, so the search stays cheap; cronSearchYears bounds it.
func (e cronExpr) next(base time.Time) (time.Time, bool) {
	loc := base.Location()
	t := base.Truncate(time.Minute)
	if !t.After(base) {
		t = t.Add(time.Minute)
	}
	limit := t.AddDate(cronSearchYears, 0, 0)
	for t.Before(limit) {
		if !e.month[int(t.Month())] || !e.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			continue
		}
		if !e.hour[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc).Add(time.Hour)
			continue
		}
		if !e.minute[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

// dayMatches applies the cron day rule: when both day fields are restricted a
// day matching either one is a match (the OR croniter and Vixie cron use); when
// only one is restricted, that one decides.
func (e cronExpr) dayMatches(t time.Time) bool {
	domMatch := e.dom[t.Day()]
	dowMatch := e.dow[int(t.Weekday())]
	switch {
	case e.domAll && e.dowAll:
		return true
	case e.domAll:
		return dowMatch
	case e.dowAll:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}
