package gateway_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/gateway"
	"github.com/umi008/hermes-agent-go/internal/tools/approval"
)

// fakeAdapter feeds inbounds from a channel, exactly like a platform's receive
// loop: handle must return quickly or this loop stops reading.
type fakeAdapter struct {
	ch chan gateway.Inbound
}

func (f *fakeAdapter) Name() string { return "telegram" }

func (f *fakeAdapter) Run(ctx context.Context, handle func(gateway.Inbound) error) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case in := <-f.ch:
			if err := handle(in); err != nil {
				return err
			}
		}
	}
}

// fakeSession is one conversation driven by the test.
type fakeSession struct {
	id     string
	source string
	out    io.Writer
	f      *fakeFactory

	mu     sync.Mutex
	inputs []string
	ended  string
}

func (s *fakeSession) ID() string { return s.id }

func (s *fakeSession) Run(ctx context.Context, input string) error {
	s.mu.Lock()
	s.inputs = append(s.inputs, input)
	s.mu.Unlock()
	if fn := s.f.behaviour(); fn != nil {
		return fn(ctx, s, input)
	}
	_, _ = io.WriteString(s.out, "reply:"+input)
	return nil
}

func (s *fakeSession) End(reason string) {
	s.mu.Lock()
	s.ended = reason
	s.mu.Unlock()
	s.f.markEnded()
}

func (s *fakeSession) taken() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.inputs...)
}

type fakeFactory struct {
	mu       sync.Mutex
	sessions []*fakeSession
	run      func(ctx context.Context, s *fakeSession, input string) error
	ended    int
}

func (f *fakeFactory) New(_ context.Context, source string, out io.Writer) gateway.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeSession{id: fmt.Sprintf("sess-%d", len(f.sessions)+1), source: source, out: out, f: f}
	f.sessions = append(f.sessions, s)
	return s
}

func (f *fakeFactory) behaviour() func(context.Context, *fakeSession, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.run
}

func (f *fakeFactory) setBehaviour(fn func(context.Context, *fakeSession, string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.run = fn
}

func (f *fakeFactory) markEnded() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended++
}

func (f *fakeFactory) opened() []*fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeSession(nil), f.sessions...)
}

func (f *fakeFactory) endedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ended
}

// harness runs a Core over the fake adapter and records everything a chat saw.
type harness struct {
	t       *testing.T
	core    *gateway.Core
	adapter *fakeAdapter
	factory *fakeFactory
	logs    *bytes.Buffer

	mu    sync.Mutex
	sent  map[string][]string
	sendE error

	cancel  context.CancelFunc
	stopped chan struct{} // closed once Run returned
	runErr  error
}

type harnessOptions struct {
	maxChunk     int
	allowedChats []string
}

func newHarness(t *testing.T, o harnessOptions) *harness {
	t.Helper()
	logs := &bytes.Buffer{}
	h := &harness{
		t:       t,
		adapter: &fakeAdapter{ch: make(chan gateway.Inbound, 8)},
		factory: &fakeFactory{},
		logs:    logs,
		sent:    map[string][]string{},
		stopped: make(chan struct{}),
	}
	core, err := gateway.New(gateway.Options{
		NewSession:   h.factory.New,
		Logger:       slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:          func() time.Time { return time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC) },
		MaxChunk:     o.maxChunk,
		AllowedChats: o.allowedChats,
	})
	if err != nil {
		t.Fatalf("new core: %v", err)
	}
	h.core = core
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		err := core.Run(ctx, h.adapter)
		h.mu.Lock()
		h.runErr = err
		h.mu.Unlock()
		close(h.stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.stopped:
		case <-time.After(5 * time.Second):
			t.Errorf("core did not stop")
		}
	})
	return h
}

func (h *harness) inbound(chatID, text string) gateway.Inbound {
	return gateway.Inbound{
		Platform: "telegram",
		ChatID:   chatID,
		UserID:   "user-" + chatID,
		UserName: "Person " + chatID,
		Text:     text,
		Send: func(_ context.Context, text string) error {
			h.mu.Lock()
			h.sent[chatID] = append(h.sent[chatID], text)
			err := h.sendE
			h.mu.Unlock()
			return err
		},
	}
}

func (h *harness) deliver(chatID, text string) {
	h.t.Helper()
	select {
	case h.adapter.ch <- h.inbound(chatID, text):
	case <-time.After(5 * time.Second):
		h.t.Fatalf("adapter did not accept the message")
	}
}

func (h *harness) messages(chatID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sent[chatID]...)
}

func (h *harness) sessions() []*fakeSession { return h.factory.opened() }

func (h *harness) stop() error {
	h.cancel()
	select {
	case <-h.stopped:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.runErr
	case <-time.After(5 * time.Second):
		return errors.New("core did not stop")
	}
}

// waitFor polls until cond holds, so the tests never depend on scheduler order.
func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) waitForMessages(chatID string, n int) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("%d message(s) in chat %s", n, chatID), func() bool {
		return len(h.messages(chatID)) >= n
	})
}

func TestTwoChatsKeepTwoSessions(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	h.deliver("1", "hi")
	h.deliver("2", "yo")
	h.waitForMessages("1", 1)
	h.waitForMessages("2", 1)

	if got := h.messages("1"); got[0] != "reply:hi" {
		t.Errorf("chat 1 reply = %q", got[0])
	}
	if got := h.messages("2"); got[0] != "reply:yo" {
		t.Errorf("chat 2 reply = %q", got[0])
	}
	sessions := h.sessions()
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want one per chat", len(sessions))
	}
	if sessions[0].id == sessions[1].id {
		t.Errorf("chats share session %q", sessions[0].id)
	}
	if sessions[0].source != "telegram" {
		t.Errorf("session source = %q, want the platform name", sessions[0].source)
	}

	// A second turn in the same chat reuses that chat's session.
	h.deliver("1", "again")
	h.waitForMessages("1", 2)
	if got := h.sessions(); len(got) != 2 {
		t.Fatalf("sessions = %d after a second turn, want 2", len(got))
	}
	if got := h.messages("1")[1]; got != "reply:again" {
		t.Errorf("second reply = %q", got)
	}
}

func TestMidTurnMessageQueues(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	h.factory.setBehaviour(func(ctx context.Context, s *fakeSession, input string) error {
		if input != "one" {
			_, _ = io.WriteString(s.out, "reply:"+input)
			return nil
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, _ = io.WriteString(s.out, "reply:"+input)
		return nil
	})

	h.deliver("1", "one")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never started")
	}
	// The adapter keeps receiving while the turn is stuck: this delivery must
	// not block on the turn.
	h.deliver("1", "two")
	if got := h.sessions()[0].taken(); len(got) != 1 {
		t.Fatalf("inputs while the turn runs = %v, want only the first", got)
	}

	close(release)
	h.waitForMessages("1", 2)
	if got := h.sessions()[0].taken(); strings.Join(got, ",") != "one,two" {
		t.Errorf("turn order = %v", got)
	}
	if got := h.messages("1"); got[0] != "reply:one" || got[1] != "reply:two" {
		t.Errorf("replies = %v", got)
	}
}

func TestLongReplyIsSplit(t *testing.T) {
	h := newHarness(t, harnessOptions{maxChunk: 10})
	const body = "alpha beta gamma delta epsilon"
	h.factory.setBehaviour(func(_ context.Context, s *fakeSession, _ string) error {
		_, _ = io.WriteString(s.out, body)
		return nil
	})
	h.deliver("1", "long")
	h.waitForMessages("1", 4)

	for i, got := range h.messages("1") {
		if len([]rune(got)) > 10 {
			t.Errorf("chunk %d is %d runes: %q", i, len([]rune(got)), got)
		}
	}
	if joined := strings.Join(h.messages("1"), " "); joined != body {
		t.Errorf("chunks do not reassemble: %q", joined)
	}
}

func TestApprovalOnce(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	choices := make(chan approval.Choice, 1)
	h.factory.setBehaviour(func(ctx context.Context, s *fakeSession, _ string) error {
		choice, err := h.core.Approve(ctx, approval.Request{Command: "git push --force", Description: "force push"})
		if err != nil {
			return err
		}
		choices <- choice
		_, _ = io.WriteString(s.out, "ran")
		return nil
	})

	h.deliver("1", "push it")
	h.waitForMessages("1", 1)
	prompt := h.messages("1")[0]
	for _, want := range []string{"DANGEROUS COMMAND: force push", "git push --force", "[o]nce  |  [s]ession  |  [d]eny", "Choice [o/s/D]:"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q: %q", want, prompt)
		}
	}
	// The answer is the next message from the chat, never a new turn.
	h.deliver("1", "o")
	select {
	case choice := <-choices:
		if choice != approval.Once {
			t.Fatalf("choice = %v, want Once", choice)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approval never resolved")
	}
	h.waitForMessages("1", 3)
	if got := h.messages("1")[1]; !strings.Contains(got, "Allowed once") {
		t.Errorf("confirmation = %q", got)
	}
	if got := h.messages("1")[2]; got != "ran" {
		t.Errorf("turn reply = %q", got)
	}
	if got := h.sessions()[0].taken(); len(got) != 1 || got[0] != "push it" {
		t.Errorf("approval answer started a turn: %v", got)
	}
}

func TestApprovalDenyReachesToolLayer(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	det, err := approval.NewDetector("", "")
	if err != nil {
		t.Fatal(err)
	}
	gate := approval.New(approval.Options{Detector: det, Prompter: h.core})
	results := make(chan error, 1)
	h.factory.setBehaviour(func(ctx context.Context, _ *fakeSession, _ string) error {
		results <- gate.Check(ctx, "git push --force")
		return nil
	})

	h.deliver("1", "push it")
	h.waitForMessages("1", 1)
	h.deliver("1", "d")
	select {
	case err := <-results:
		if err == nil {
			t.Fatal("gate allowed a denied command")
		}
		for _, want := range []string{"User denied this command", `"status":"blocked"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("gate error = %v, want %q", err, want)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approval never resolved")
	}
	h.waitForMessages("1", 2)
	if got := h.messages("1")[1]; got != "✗ Denied" {
		t.Errorf("confirmation = %q", got)
	}
}

func TestApprovalRepromptsOnce(t *testing.T) {
	tests := []struct {
		name     string
		answers  []string
		want     approval.Choice
		wantSend int
	}{
		{"unrecognised then deny", []string{"maybe", "d"}, approval.Deny, 3},
		{"unrecognised then once", []string{"huh", "once"}, approval.Once, 3},
		{"unrecognised twice denies", []string{"maybe", "huh"}, approval.Deny, 3},
		{"session word", []string{"session"}, approval.Session, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			choices := make(chan approval.Choice, 1)
			h.factory.setBehaviour(func(ctx context.Context, _ *fakeSession, _ string) error {
				choice, err := h.core.Approve(ctx, approval.Request{Command: "rm -r build", Description: "recursive delete"})
				if err != nil {
					return err
				}
				choices <- choice
				return nil
			})
			h.deliver("1", "delete it")
			h.waitForMessages("1", 1)
			for _, answer := range tt.answers {
				h.deliver("1", answer)
			}
			select {
			case choice := <-choices:
				if choice != tt.want {
					t.Fatalf("choice = %v, want %v", choice, tt.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("approval never resolved")
			}
			h.waitForMessages("1", tt.wantSend)
			if len(tt.answers) > 1 {
				// The second send is the re-prompt, not a confirmation.
				if got := h.messages("1")[1]; !strings.Contains(got, "Reply [o]nce") {
					t.Errorf("re-prompt = %q", got)
				}
			}
		})
	}
}

func TestApprovalCancellationDenies(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	results := make(chan error, 1)
	h.factory.setBehaviour(func(ctx context.Context, _ *fakeSession, _ string) error {
		_, err := h.core.Approve(ctx, approval.Request{Command: "rm -r build", Description: "recursive delete"})
		results <- err
		return nil
	})
	h.deliver("1", "delete it")
	h.waitForMessages("1", 1)
	h.cancel()
	select {
	case err := <-results:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want a cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation never reached the approval")
	}
}

func TestApproveOutsideChatDenies(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	choice, err := h.core.Approve(context.Background(), approval.Request{Command: "x"})
	if err == nil || choice != approval.Deny {
		t.Fatalf("choice %v err %v, want Deny and an error", choice, err)
	}
}

func TestAllowlistDropsUnknownChat(t *testing.T) {
	h := newHarness(t, harnessOptions{allowedChats: []string{"1"}})

	h.deliver("2", "hello?")
	h.deliver("1", "hello")
	h.waitForMessages("1", 1)
	if got := h.messages("2"); len(got) != 0 {
		t.Errorf("chat 2 got %v, want nothing", got)
	}
	if got := h.sessions(); len(got) != 1 {
		t.Errorf("sessions = %d, want only the allowed chat", len(got))
	}
	if !strings.Contains(h.logs.String(), "gateway.allowed_chats") {
		t.Errorf("drop was not logged: %s", h.logs.String())
	}
}

func TestShutdownEndsSessions(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.deliver("1", "hi")
	h.deliver("2", "yo")
	h.waitForMessages("1", 1)
	h.waitForMessages("2", 1)

	if err := h.stop(); err != nil {
		t.Fatalf("run returned %v", err)
	}
	if got := h.factory.endedCount(); got != 2 {
		t.Fatalf("ended sessions = %d, want 2", got)
	}
	for _, s := range h.sessions() {
		s.mu.Lock()
		reason := s.ended
		s.mu.Unlock()
		if reason != "gateway_close" {
			t.Errorf("session %s ended with %q", s.id, reason)
		}
	}
}

func TestShutdownCancelsRunningTurn(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	started := make(chan struct{}, 1)
	finished := make(chan error, 1)
	h.factory.setBehaviour(func(ctx context.Context, _ *fakeSession, _ string) error {
		started <- struct{}{}
		<-ctx.Done()
		finished <- ctx.Err()
		return ctx.Err()
	})
	h.deliver("1", "slow")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never started")
	}
	if err := h.stop(); err != nil {
		t.Fatalf("run returned %v", err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("turn err = %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn was not cancelled")
	}
	if got := h.factory.endedCount(); got != 1 {
		t.Errorf("ended sessions = %d, want 1", got)
	}
	if got := h.messages("1"); len(got) != 0 {
		t.Errorf("cancelled turn sent %v", got)
	}
}

func TestTurnErrorIsReportedInChat(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.factory.setBehaviour(func(context.Context, *fakeSession, string) error {
		return errors.New("model exploded")
	})
	h.deliver("1", "hi")
	h.waitForMessages("1", 1)
	if got := h.messages("1")[0]; !strings.Contains(got, "model exploded") {
		t.Errorf("error reply = %q", got)
	}
}

func TestDefaultMaxChunkIsTelegramLimit(t *testing.T) {
	if gateway.DefaultMaxChunk != 4096 {
		t.Fatalf("DefaultMaxChunk = %d, want Telegram's 4096", gateway.DefaultMaxChunk)
	}
}
