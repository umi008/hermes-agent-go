package approval

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeReviewer is a scripted guardian.
type fakeReviewer struct {
	verdict     Verdict
	calls       int
	command     string
	description string
}

func (f *fakeReviewer) Review(_ context.Context, command, description string) Verdict {
	f.calls++
	f.command, f.description = command, description
	return f.verdict
}

// recordingPrompter answers a fixed choice and records every request, so a
// test can see what the human was offered.
type recordingPrompter struct {
	choice Choice
	err    error
	reqs   []Request
}

func (p *recordingPrompter) Approve(_ context.Context, r Request) (Choice, error) {
	p.reqs = append(p.reqs, r)
	return p.choice, p.err
}

func (p *recordingPrompter) calls() int { return len(p.reqs) }

// sessionCtx is the per-session context the breaker tallies under.
func sessionCtx(key string) context.Context {
	return WithSession(context.Background(), key)
}

// smartGate builds a gate whose guardian answers verdict.
func smartGate(t *testing.T, p Prompter, reviewer *fakeReviewer, mutate func(*Options)) *Gate {
	t.Helper()
	o := Options{Detector: newDetector(t), Prompter: p, Smart: reviewer, BreakerThreshold: 3}
	if mutate != nil {
		mutate(&o)
	}
	return New(o)
}

const breakerText = " CIRCUIT BREAKER: 2 consecutive commands were blocked by the security reviewer. " +
	"STOP attempting variations of this operation. Report the blocked operation to the user and either ask them " +
	"to run it manually or use /approve."

// TestSmartApprovesOnlyThatCommand pins the approve verdict: the command runs
// without asking the human and nothing is remembered, so the next command in
// the same detector category is assessed again.
func TestSmartApprovesOnlyThatCommand(t *testing.T) {
	reviewer := &fakeReviewer{verdict: VerdictApprove}
	p := &recordingPrompter{choice: Deny}
	g := smartGate(t, p, reviewer, nil)
	if err := g.Check(sessionCtx("s1"), "git push --force"); err != nil {
		t.Fatalf("approved command blocked: %v", err)
	}
	if p.calls() != 0 {
		t.Fatalf("the guardian's approval still asked the human: %+v", p.reqs)
	}
	if len(g.session) != 0 {
		t.Fatalf("a smart approval must not persist a session choice: %v", g.session)
	}
	if reviewer.calls != 1 || reviewer.command != "git push --force" ||
		!strings.Contains(reviewer.description, "force push") {
		t.Fatalf("reviewer saw %+v", reviewer)
	}
	// The same command is reviewed again: the approval was one operation, so
	// the escalated second turn reaches the human (who denies it here).
	reviewer.verdict = VerdictEscalate
	if err := g.Check(sessionCtx("s1"), "git push --force"); err == nil ||
		!strings.Contains(err.Error(), "User denied this command") {
		t.Fatalf("escalated command = %v", err)
	}
	if reviewer.calls != 2 || p.calls() != 1 {
		t.Fatalf("reviewer calls %d, prompter calls %d", reviewer.calls, p.calls())
	}
}

// TestSmartDenyReducesThePrompt pins the deny verdict with a human present:
// they are offered one operation, and their answer never persists.
func TestSmartDenyReducesThePrompt(t *testing.T) {
	for _, choice := range []Choice{Once, Session, Always} {
		reviewer := &fakeReviewer{verdict: VerdictDeny}
		p := &recordingPrompter{choice: choice}
		var persisted []string
		g := smartGate(t, p, reviewer, func(o *Options) {
			o.Persist = func(key string) error { persisted = append(persisted, key); return nil }
		})
		if err := g.Check(sessionCtx("s1"), "git push --force"); err != nil {
			t.Fatalf("choice %v blocked: %v", choice, err)
		}
		if len(p.reqs) != 1 || !p.reqs[0].SmartDenied {
			t.Fatalf("choice %v: the prompt must be marked as a smart deny: %+v", choice, p.reqs)
		}
		if len(g.session) != 0 || len(persisted) != 0 {
			t.Fatalf("choice %v persisted a smart-denied command: session %v, always %v", choice, g.session, persisted)
		}
	}
}

// TestSmartDenyDeniedByHuman pins the human denial: upstream's message, and
// a human denial never counts towards the guardian breaker.
func TestSmartDenyDeniedByHuman(t *testing.T) {
	reviewer := &fakeReviewer{verdict: VerdictDeny}
	p := &recordingPrompter{choice: Deny}
	g := smartGate(t, p, reviewer, func(o *Options) { o.BreakerThreshold = 2 })
	for i := 1; i <= 3; i++ {
		err := g.Check(sessionCtx("s1"), "git push --force")
		if err == nil || !strings.Contains(err.Error(), "User denied this command") {
			t.Fatalf("check %d = %v", i, err)
		}
		// Only the guardian denials count: the third human denial must not
		// have tripped a two-denial breaker on its own.
		if i < 2 && strings.Contains(err.Error(), "CIRCUIT BREAKER") {
			t.Fatalf("check %d tripped early: %v", i, err)
		}
	}
}

// TestSmartEscalatesToTheHuman pins the escalate verdict: the normal prompt,
// with the normal persistence.
func TestSmartEscalatesToTheHuman(t *testing.T) {
	reviewer := &fakeReviewer{verdict: VerdictEscalate}
	p := &recordingPrompter{choice: Session}
	g := smartGate(t, p, reviewer, nil)
	if err := g.Check(sessionCtx("s1"), "git push --force"); err != nil {
		t.Fatalf("escalated command blocked: %v", err)
	}
	if len(p.reqs) != 1 || p.reqs[0].SmartDenied {
		t.Fatalf("escalate must be a normal prompt: %+v", p.reqs)
	}
	if !g.session[p.reqs[0].Description] {
		t.Fatalf("a session choice must still persist: %v", g.session)
	}
}

// TestSmartDenyTimeoutKeepsTheDeny pins a prompter that cannot answer: the
// command stays blocked and the message is upstream's timeout text.
func TestSmartDenyTimeoutKeepsTheDeny(t *testing.T) {
	reviewer := &fakeReviewer{verdict: VerdictDeny}
	p := &recordingPrompter{err: errors.New("timeout")}
	g := smartGate(t, p, reviewer, nil)
	err := g.Check(sessionCtx("s1"), "git push --force")
	if err == nil || !strings.Contains(err.Error(), "timed out without user response") {
		t.Fatalf("timeout = %v", err)
	}
}

// TestSmartCircuitBreaker pins upstream's consecutive-denial breaker: after
// the threshold the denial carries the hard-stop text, and an approval resets
// the tally.
func TestSmartCircuitBreaker(t *testing.T) {
	reviewer := &fakeReviewer{verdict: VerdictDeny}
	p := &recordingPrompter{choice: Deny}
	g := smartGate(t, p, reviewer, func(o *Options) { o.BreakerThreshold = 2 })

	first := g.Check(sessionCtx("s1"), "git push --force")
	if first == nil || strings.Contains(first.Error(), "CIRCUIT BREAKER") {
		t.Fatalf("first denial = %v", first)
	}
	second := g.Check(sessionCtx("s1"), "git push --force")
	if second == nil || !strings.Contains(second.Error(), breakerText) {
		t.Fatalf("second denial = %v", second)
	}
	// An approval resets the tally, so the next denial starts over.
	reviewer.verdict = VerdictApprove
	if err := g.Check(sessionCtx("s1"), "git push --force"); err != nil {
		t.Fatalf("approval = %v", err)
	}
	reviewer.verdict = VerdictDeny
	again := g.Check(sessionCtx("s1"), "git push --force")
	if again == nil || strings.Contains(again.Error(), "CIRCUIT BREAKER") {
		t.Fatalf("denial after a reset = %v", again)
	}
}

// TestSmartCircuitBreakerPerSession pins the tally's per-session scope.
func TestSmartCircuitBreakerPerSession(t *testing.T) {
	reviewer := &fakeReviewer{verdict: VerdictDeny}
	p := &recordingPrompter{choice: Deny}
	g := smartGate(t, p, reviewer, func(o *Options) { o.BreakerThreshold = 2 })
	_ = g.Check(sessionCtx("s1"), "git push --force")
	if err := g.Check(sessionCtx("s2"), "git push --force"); err == nil || strings.Contains(err.Error(), "CIRCUIT BREAKER") {
		t.Fatalf("another session tripped: %v", err)
	}
	if err := g.Check(sessionCtx("s1"), "git push --force"); err == nil || !strings.Contains(err.Error(), "CIRCUIT BREAKER") {
		t.Fatalf("the same session did not trip: %v", err)
	}
}

// TestSmartBreakerDisabled pins approvals.denial_breaker_threshold <= 0.
func TestSmartBreakerDisabled(t *testing.T) {
	reviewer := &fakeReviewer{verdict: VerdictDeny}
	p := &recordingPrompter{choice: Deny}
	g := smartGate(t, p, reviewer, func(o *Options) { o.BreakerThreshold = 0 })
	for i := 0; i < 5; i++ {
		if err := g.Check(sessionCtx("s1"), "git push --force"); err == nil || strings.Contains(err.Error(), "CIRCUIT BREAKER") {
			t.Fatalf("check %d = %v", i, err)
		}
	}
}

// TestSmartNeverSeesHardlineOrOff pins the ladder's early gates: a hardline
// command and an off mode never reach the guardian.
func TestSmartNeverSeesHardlineOrOff(t *testing.T) {
	reviewer := &fakeReviewer{verdict: VerdictApprove}
	g := smartGate(t, nil, reviewer, nil)
	if err := g.Check(sessionCtx("s1"), "rm -rf /"); err == nil ||
		!strings.Contains(err.Error(), "BLOCKED (hardline)") {
		t.Fatalf("hardline = %v", err)
	}
	if err := smartGate(t, nil, reviewer, func(o *Options) { o.Off = true }).Check(sessionCtx("s1"), "git push --force"); err != nil {
		t.Fatalf("off mode = %v", err)
	}
	if reviewer.calls != 0 {
		t.Fatalf("the guardian reviewed %d commands it must not see", reviewer.calls)
	}
}

// TestSmartSessionApprovalSkipsTheGuardian pins the existing session approval
// gate, which still answers before the guardian.
func TestSmartSessionApprovalSkipsTheGuardian(t *testing.T) {
	reviewer := &fakeReviewer{verdict: VerdictEscalate}
	p := &recordingPrompter{choice: Session}
	g := smartGate(t, p, reviewer, nil)
	if err := g.Check(sessionCtx("s1"), "git push --force"); err != nil {
		t.Fatalf("first = %v", err)
	}
	// The human granted the whole pattern, so the next command never reaches
	// the guardian.
	reviewer.calls, reviewer.verdict = 0, VerdictDeny
	if err := g.Check(sessionCtx("s1"), "git push --force"); err != nil {
		t.Fatalf("approved session command = %v", err)
	}
	if reviewer.calls != 0 || len(p.reqs) != 1 {
		t.Fatalf("an approved session command was reviewed again: %d calls", reviewer.calls)
	}
}

// TestSessionContext pins the session key helper.
func TestSessionContext(t *testing.T) {
	if got := SessionKey(sessionCtx("abc")); got != "abc" {
		t.Fatalf("key = %q", got)
	}
	if got := SessionKey(context.Background()); got != "" {
		t.Fatalf("missing key = %q", got)
	}
}
