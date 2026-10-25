package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// fallbackAgent wires a primary and the given fallbacks with a fixed clock.
func fallbackAgent(primary *scriptClient, now *time.Time, events *[]Event, fbs ...Route) *Agent {
	return New(Options{
		Client: primary, Model: "p-model", Provider: "primary", BaseURL: "https://p.example",
		Fallbacks: fbs, Backoff: noWait, Now: func() time.Time { return *now },
		OnEvent: func(e Event) {
			if e.Kind == EventFallback {
				*events = append(*events, e)
			}
		},
	})
}

func apiErr(status int) error { return &llm.APIError{Provider: "primary", Status: status, Body: "x"} }

func TestFallbackOnRateLimitIsImmediateAndArmsCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	var events []Event
	primary := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){fail(apiErr(429))}}
	fb := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){text("from fb"), text("again fb")}}
	a := fallbackAgent(primary, &now, &events, Route{Client: fb, Provider: "backup", Model: "b-model", BaseURL: "https://b.example"})

	res, err := a.Run(context.Background(), "hi")
	if err != nil || res.Text != "from fb" {
		t.Fatalf("Run = %q, %v", res.Text, err)
	}
	if len(primary.requests) != 1 {
		t.Errorf("primary called %d times, want 1 (rate limit falls back without retries)", len(primary.requests))
	}
	if fb.requests[0].Model != "b-model" {
		t.Errorf("fallback request model = %q", fb.requests[0].Model)
	}
	want := "⚠️ Model fallback: p-model via primary unavailable (rate limit); using b-model via backup. Primary retry eligible in ~60 s; recovery is not guaranteed."
	if len(events) != 1 || events[0].Text != want {
		t.Fatalf("events = %+v", events)
	}

	// Inside the cooldown the next turn stays on the fallback.
	now = now.Add(30 * time.Second)
	if res, err := a.Run(context.Background(), "next"); err != nil || res.Text != "again fb" {
		t.Fatalf("second Run = %q, %v", res.Text, err)
	}
	if len(primary.requests) != 1 || len(events) != 1 {
		t.Errorf("primary calls %d, events %d: want the fallback kept during cooldown", len(primary.requests), len(events))
	}
}

func TestPrimaryRestoredNextTurnAfterCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	var events []Event
	primary := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){fail(apiErr(429)), text("primary back")}}
	fb := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){text("from fb")}}
	a := fallbackAgent(primary, &now, &events, Route{Client: fb, Provider: "backup", Model: "b-model"})

	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(61 * time.Second)
	res, err := a.Run(context.Background(), "next")
	if err != nil || res.Text != "primary back" {
		t.Fatalf("Run = %q, %v", res.Text, err)
	}
	if primary.requests[1].Model != "p-model" {
		t.Errorf("restored request model = %q", primary.requests[1].Model)
	}
	want := "✅ Primary model restored: p-model via primary; fallback b-model via backup is no longer active."
	if len(events) != 2 || events[1].Text != want {
		t.Fatalf("events = %+v", events)
	}
}

func TestServerErrorsFallBackAfterRetriesWithoutCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	var events []Event
	primary := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){
		fail(apiErr(500)), fail(apiErr(500)), fail(apiErr(500)), text("primary next turn"),
	}}
	fb := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){text("from fb")}}
	a := fallbackAgent(primary, &now, &events, Route{Client: fb, Provider: "backup", Model: "b-model"})

	if res, err := a.Run(context.Background(), "hi"); err != nil || res.Text != "from fb" {
		t.Fatalf("Run = %q, %v", res.Text, err)
	}
	if len(primary.requests) != DefaultMaxAttempts {
		t.Errorf("primary called %d times, want %d", len(primary.requests), DefaultMaxAttempts)
	}
	if want := "⚠️ Model fallback: p-model via primary unavailable (provider server error); using b-model via backup."; events[0].Text != want {
		t.Errorf("notice = %q", events[0].Text)
	}
	// No cooldown for server errors: the next turn goes back to the primary.
	if res, err := a.Run(context.Background(), "next"); err != nil || res.Text != "primary next turn" {
		t.Fatalf("second Run = %q, %v", res.Text, err)
	}
}

func TestChainAdvancesAndSkipsTheCurrentBackend(t *testing.T) {
	now := time.Unix(1000, 0)
	var events []Event
	primary := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){fail(apiErr(401))}}
	same := &scriptClient{}
	first := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){fail(apiErr(404))}}
	second := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){text("second")}}
	a := fallbackAgent(primary, &now, &events,
		Route{Client: same, Provider: "Primary", Model: "P-Model", BaseURL: "https://p.example/"},
		Route{Client: first, Provider: "one", Model: "m1"},
		Route{Client: second, Provider: "two", Model: "m2"},
	)
	res, err := a.Run(context.Background(), "hi")
	if err != nil || res.Text != "second" {
		t.Fatalf("Run = %q, %v", res.Text, err)
	}
	if len(same.requests) != 0 {
		t.Error("a fallback that resolves to the current backend must be skipped")
	}
	if len(events) != 2 || !strings.Contains(events[0].Text, "(authentication failed); using m1 via one.") ||
		!strings.Contains(events[1].Text, "m1 via one unavailable (model not found); using m2 via two.") {
		t.Fatalf("events = %+v", events)
	}
}

func TestExhaustedChainReturnsTheLastError(t *testing.T) {
	now := time.Unix(1000, 0)
	var events []Event
	primary := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){fail(apiErr(429))}}
	fb := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){fail(apiErr(402))}}
	a := fallbackAgent(primary, &now, &events, Route{Client: fb, Provider: "backup", Model: "b-model"})
	_, err := a.Run(context.Background(), "hi")
	var ae *llm.APIError
	if !errors.As(err, &ae) || ae.Status != 402 {
		t.Fatalf("err = %v", err)
	}
}

func TestBadRequestDoesNotFallBack(t *testing.T) {
	now := time.Unix(1000, 0)
	var events []Event
	primary := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){fail(apiErr(400))}}
	fb := &scriptClient{}
	a := fallbackAgent(primary, &now, &events, Route{Client: fb, Provider: "backup", Model: "b-model"})
	if _, err := a.Run(context.Background(), "hi"); err == nil {
		t.Fatal("want the 400 returned")
	}
	if len(fb.requests) != 0 || len(events) != 0 {
		t.Errorf("a plain 400 must not fall back: %d requests, %d events", len(fb.requests), len(events))
	}
}

func TestNoFallbacksKeepsRetryBehavior(t *testing.T) {
	primary := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){fail(apiErr(429)), text("ok")}}
	a := New(Options{Client: primary, Model: "m", Backoff: noWait})
	if res, err := a.Run(context.Background(), "hi"); err != nil || res.Text != "ok" {
		t.Fatalf("Run = %q, %v", res.Text, err)
	}
	if len(primary.requests) != 2 {
		t.Errorf("requests = %d, want a retry on 429 without fallbacks", len(primary.requests))
	}
}

func TestServedNamesTheActiveRoute(t *testing.T) {
	now := time.Unix(1000, 0)
	var events []Event
	primary := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){fail(apiErr(429))}}
	fb := &scriptClient{replies: []func(llm.Request, func(llm.Event)) (llm.Response, error){text("from fb")}}
	a := fallbackAgent(primary, &now, &events, Route{Client: fb, Provider: "backup", Model: "b-model"})
	if p, m := a.Served(); p != "primary" || m != "p-model" {
		t.Fatalf("Served before = %q, %q", p, m)
	}
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if p, m := a.Served(); p != "backup" || m != "b-model" {
		t.Fatalf("Served after fallback = %q, %q", p, m)
	}
}
