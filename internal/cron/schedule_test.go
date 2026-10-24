package cron

import (
	"strings"
	"testing"
	"time"
)

// at is a fixed UTC instant, the clock every schedule test runs on.
func at(day, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC)
}

func TestParseSchedule(t *testing.T) {
	now := at(24, 8, 0)
	tests := []struct {
		name    string
		in      string
		kind    string
		minutes int
		expr    string
		runAt   string
		display string
		wantErr string
	}{
		{name: "interval", in: "30m", kind: KindInterval, minutes: 30, display: "every 30m"},
		{name: "interval every prefix", in: "every 2h", kind: KindInterval, minutes: 120, display: "every 120m"},
		{name: "interval days", in: "1d", kind: KindInterval, minutes: 1440, display: "every 1440m"},
		{name: "interval bare unit", in: "hour", kind: KindInterval, minutes: 60, display: "every 60m"},
		{name: "interval trims spaces", in: "  30m ", kind: KindInterval, minutes: 30, display: "every 30m"},
		{name: "cron", in: "0 9 * * *", kind: KindCron, expr: "0 9 * * *", display: "0 9 * * *"},
		{name: "cron ranges lists steps", in: "*/15 9-17 * * 1-5", kind: KindCron, expr: "*/15 9-17 * * 1-5", display: "*/15 9-17 * * 1-5"},
		{
			name: "one shot iso", in: "2026-11-01T09:00:00Z", kind: KindOnce,
			runAt: "2026-11-01T09:00:00+00:00", display: "once at 2026-11-01 09:00",
		},
		{
			name: "one shot iso offset", in: "2026-11-01T09:00:00+02:00", kind: KindOnce,
			runAt: "2026-11-01T09:00:00+02:00", display: "once at 2026-11-01 09:00",
		},
		{
			name: "one shot date", in: "2026-11-01", kind: KindOnce,
			runAt: "2026-11-01T00:00:00+00:00", display: "once at 2026-11-01 00:00",
		},
		{
			name: "one shot delay", in: "in 30m", kind: KindOnce,
			runAt: "2026-10-24T08:30:00+00:00", display: "once in 30m",
		},
		{name: "natural phrase unsupported", in: "every monday 9am", wantErr: "invalid schedule"},
		{name: "unknown word", in: "banana", wantErr: "invalid schedule"},
		{name: "six fields rejected", in: "0 9 * * * *", wantErr: "invalid schedule"},
		{name: "hour out of range", in: "0 25 * * *", wantErr: "invalid cron expression"},
		{name: "zero step", in: "*/0 * * * *", wantErr: "invalid cron expression"},
		{name: "named day rejected", in: "0 9 * * MON", wantErr: "invalid schedule"},
		{name: "bad timestamp", in: "2026-11-01T99:00:00Z", wantErr: "invalid timestamp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSchedule(tt.in, now)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Kind != tt.kind || got.Minutes != tt.minutes || got.Expr != tt.expr ||
				got.RunAt != tt.runAt || got.Display != tt.display {
				t.Errorf("got %+v, want kind=%s minutes=%d expr=%q run_at=%q display=%q",
					got, tt.kind, tt.minutes, tt.expr, tt.runAt, tt.display)
			}
		})
	}
}

func TestParseScheduleNaiveTimestampUsesNowZone(t *testing.T) {
	zone := time.FixedZone("X", 3600)
	now := time.Date(2026, 10, 24, 8, 0, 0, 0, zone)
	got, err := ParseSchedule("2026-11-01T09:00", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-11-01T09:00:00+01:00"; got.RunAt != want {
		t.Errorf("run_at = %q, want %q", got.RunAt, want)
	}
}

func TestNextRun(t *testing.T) {
	now := at(24, 8, 0) // Saturday 2026-10-24
	tests := []struct {
		name      string
		schedule  string
		lastRunAt string
		want      string
		wantOK    bool
	}{
		{name: "interval from now", schedule: "30m", want: "2026-10-24T08:30:00+00:00", wantOK: true},
		{
			name: "interval anchors on last run", schedule: "30m",
			lastRunAt: "2026-10-24T07:10:00+00:00", want: "2026-10-24T07:40:00+00:00", wantOK: true,
		},
		// 2026-10-24 is a Saturday, so the weekday 1 (Monday) expression lands two days out.
		{name: "cron today", schedule: "0 9 * * *", want: "2026-10-24T09:00:00+00:00", wantOK: true},
		{name: "cron is strictly after", schedule: "0 9 * * *", lastRunAt: "2026-10-24T09:00:00+00:00", want: "2026-10-25T09:00:00+00:00", wantOK: true},
		{name: "cron weekday", schedule: "0 9 * * 1", want: "2026-10-26T09:00:00+00:00", wantOK: true},
		{name: "once never run", schedule: "2026-10-24T09:30:00Z", want: "2026-10-24T09:30:00+00:00", wantOK: true},
		{name: "once already run", schedule: "2026-10-24T09:30:00Z", lastRunAt: "2026-10-24T09:30:00+00:00", wantOK: false},
		// The 120 s grace is the only window in which a past one-shot is still eligible.
		{name: "once within grace", schedule: "2026-10-24T07:59:00Z", want: "2026-10-24T07:59:00+00:00", wantOK: true},
		{name: "once past grace", schedule: "2026-10-24T07:00:00Z", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := ParseSchedule(tt.schedule, now)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := s.NextRun(tt.lastRunAt, now)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("next = %q,%v, want %q,%v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestGrace(t *testing.T) {
	now := at(24, 8, 0)
	tests := []struct {
		schedule string
		want     time.Duration
	}{
		// Half the period, clamped to [120s, 2h].
		{schedule: "1m", want: 120 * time.Second},
		{schedule: "30m", want: 15 * time.Minute},
		{schedule: "0 9 * * *", want: 2 * time.Hour},
		{schedule: "1d", want: 2 * time.Hour},
	}
	for _, tt := range tests {
		s, err := ParseSchedule(tt.schedule, now)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Grace(now); got != tt.want {
			t.Errorf("%s: grace = %v, want %v", tt.schedule, got, tt.want)
		}
	}
}

func TestCronExpressionBounds(t *testing.T) {
	now := at(24, 8, 7)
	tests := []struct {
		expr string
		want string
	}{
		{"*/15 * * * *", "2026-10-24T08:15:00+00:00"},
		{"5/15 * * * *", "2026-10-24T08:20:00+00:00"},
		{"0 0 1 * *", "2026-11-01T00:00:00+00:00"},
		{"30 2 * * *", "2026-10-25T02:30:00+00:00"},
		{"0 9 1-7 * 6", "2026-10-24T09:00:00+00:00"}, // day-of-month OR day-of-week: today is a Saturday
		{"0 9 * * 0,6", "2026-10-24T09:00:00+00:00"},
		{"0 9 31 2 *", ""}, // impossible: no Feb 31
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			s, err := ParseSchedule(tt.expr, now)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := s.NextRun("", now)
			if got != tt.want {
				t.Fatalf("next = %q (ok=%v), want %q", got, ok, tt.want)
			}
		})
	}
}
