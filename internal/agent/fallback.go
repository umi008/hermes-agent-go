package agent

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// Route is one model backend the agent can call: the primary, built from
// Options.Client and Options.Model, or an entry of Options.Fallbacks.
type Route struct {
	Client   llm.Client
	Provider string
	Model    string
	BaseURL  string
}

// sameBackend matches upstream BackendIdentity: provider, model and base URL,
// compared case-insensitively and without a trailing slash.
func (r Route) sameBackend(o Route) bool {
	norm := func(s string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/")) }
	return norm(r.Provider) == norm(o.Provider) && norm(r.Model) == norm(o.Model) && norm(r.BaseURL) == norm(o.BaseURL)
}

// failover is how an error relates to the fallback chain, after upstream
// agent/error_classifier.py and turn_recovery.py.
type failover struct {
	// label is upstream's operator-facing reason text.
	label string
	// eager switches on the first failure instead of after the retries.
	eager bool
	// rateLimit arms the primary cooldown when leaving it.
	rateLimit bool
}

// classifyFailover returns the failover class of err; ok is false when the
// error never moves to a fallback (a plain 400, a cancelled context).
func classifyFailover(err error) (failover, bool) {
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		switch s := apiErr.Status; {
		case s == 429:
			return failover{label: "rate limit", eager: true, rateLimit: true}, true
		case s == 402:
			return failover{label: "billing or quota exhausted", eager: true, rateLimit: true}, true
		case s == 401 || s == 403:
			return failover{label: "authentication failed", eager: true}, true
		case s == 404:
			return failover{label: "model not found", eager: true}, true
		case s == 408:
			return failover{label: "request timeout"}, true
		case s == 503 || s == 529:
			return failover{label: "provider overloaded"}, true
		case s >= 500:
			return failover{label: "provider server error"}, true
		}
		return failover{}, false
	}
	if llm.IsRetryable(err) {
		return failover{label: "provider failure"}, true
	}
	return failover{}, false
}

// Upstream arms the primary cooldown at 60 s, doubling per rate-limited
// switch, capped at 4 h (agent/fallback_cooldown.py).
const (
	cooldownBase = 60 * time.Second
	cooldownCap  = 4 * time.Hour
)

// fallbackState is the agent's position in the chain. Fallback is
// turn-scoped: restorePrimary runs at the start of every Run.
type fallbackState struct {
	active        Route
	index         int
	activated     bool
	cooldownUntil time.Time
	backoffCount  int
}

func (a *Agent) primary() Route {
	return Route{Client: a.opts.Client, Provider: a.opts.Provider, Model: a.opts.Model, BaseURL: a.opts.BaseURL}
}

// restorePrimary reinstalls the primary unless its rate-limit cooldown is
// still running, as upstream restore_primary_runtime.
func (a *Agent) restorePrimary() {
	fb := &a.fallback
	if !fb.activated {
		fb.index = 0
		return
	}
	if a.opts.Now().Before(fb.cooldownUntil) {
		return
	}
	prev := fb.active
	fb.active, fb.index, fb.activated, fb.backoffCount = a.primary(), 0, false, 0
	a.opts.Logger.Info("primary runtime restored", "model", fb.active.Model, "provider", fb.active.Provider)
	a.emit(Event{Kind: EventFallback, Text: fmt.Sprintf(
		"✅ Primary model restored: %s via %s; fallback %s via %s is no longer active.",
		fb.active.Model, fb.active.Provider, prev.Model, prev.Provider)})
}

// activateFallback switches to the next usable chain entry and reports
// whether it did, as upstream try_activate_fallback.
func (a *Agent) activateFallback(f failover) bool {
	fb := &a.fallback
	if fb.index >= len(a.opts.Fallbacks) {
		return false
	}
	var cooldown time.Duration
	if f.rateLimit && !fb.activated {
		cooldown = min(cooldownBase*time.Duration(math.Pow(2, float64(fb.backoffCount))), cooldownCap)
		fb.backoffCount++
		fb.cooldownUntil = a.opts.Now().Add(cooldown)
	}
	for fb.index < len(a.opts.Fallbacks) {
		next := a.opts.Fallbacks[fb.index]
		fb.index++
		if next.Client == nil || next.sameBackend(fb.active) {
			continue
		}
		prev := fb.active
		fb.active, fb.activated = next, true
		notice := fmt.Sprintf("⚠️ Model fallback: %s via %s unavailable (%s); using %s via %s.",
			prev.Model, prev.Provider, f.label, next.Model, next.Provider)
		if cooldown > 0 {
			notice += fmt.Sprintf(" Primary retry eligible in ~%d s; recovery is not guaranteed.",
				int(math.Ceil(fb.cooldownUntil.Sub(a.opts.Now()).Seconds())))
		}
		a.opts.Logger.Info("fallback activated", "from", prev.Model, "to", next.Model, "reason", f.label)
		a.emit(Event{Kind: EventFallback, Text: notice})
		return true
	}
	return false
}
