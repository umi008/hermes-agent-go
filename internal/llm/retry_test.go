package llm

import (
	"net/http"
	"testing"
	"time"
)

// TestParseRetryAfter pins both forms RFC 7231 allows and the clamping rule:
// a provider asking to wait "in the past" means wait nothing, not go back in
// time.
func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
		tol  time.Duration
		ok   bool
	}{
		{name: "seconds", in: "3", want: 3 * time.Second, ok: true},
		{name: "fractional seconds", in: "1.5", want: 1500 * time.Millisecond, ok: true},
		{name: "zero", in: "0", want: 0, ok: true},
		{name: "negative clamps", in: "-5", want: 0, ok: true},
		{name: "padded", in: "  7  ", want: 7 * time.Second, ok: true},
		{name: "http date", in: time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat), want: 90 * time.Second, tol: 5 * time.Second, ok: true},
		{name: "http date in the past clamps", in: time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), want: 0, ok: true},
		{name: "garbage", in: "soon", ok: false},
		{name: "empty", in: "", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseRetryAfter(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if diff := got - tt.want; diff > tt.tol || diff < -tt.tol {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
