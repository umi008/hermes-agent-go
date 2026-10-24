package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/gateway"
)

// testAPI is a fake Bot API: it serves the scripted getUpdates pages and
// records every sendMessage.
type testAPI struct {
	t     *testing.T
	mu    sync.Mutex
	pages [][]update
	polls []pollBody
	sends []sendBody
	// failSends is how many sendMessage calls answer 429 before succeeding.
	failSends int
	paths     []string
}

func (a *testAPI) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.paths = append(a.paths, r.URL.Path)
		a.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			var p pollBody
			_ = json.Unmarshal(body, &p)
			a.mu.Lock()
			a.polls = append(a.polls, p)
			var page []update
			if len(a.pages) > 0 {
				page, a.pages = a.pages[0], a.pages[1:]
			}
			a.mu.Unlock()
			writeResult(w, page)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var s sendBody
			_ = json.Unmarshal(body, &s)
			a.mu.Lock()
			a.sends = append(a.sends, s)
			failing := a.failSends > 0
			if failing {
				a.failSends--
			}
			a.mu.Unlock()
			if failing {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":2}}`)
				return
			}
			writeResult(w, true)
		default:
			a.t.Errorf("unexpected Bot API path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func writeResult(w http.ResponseWriter, result any) {
	raw, _ := json.Marshal(result)
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true,"result":`+string(raw)+`}`)
}

// newAdapter builds an adapter against the fake API and records the retry
// delays instead of sleeping them.
func newAdapter(t *testing.T, a *testAPI, o Options) (*Adapter, *[]time.Duration, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(a.handler())
	t.Cleanup(srv.Close)
	var mu sync.Mutex
	delays := []time.Duration{}
	o.APIBase = srv.URL
	o.Token = "TEST-TOKEN"
	o.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		return ctx.Err()
	}
	adapter, err := New(o)
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return adapter, &delays, srv
}

// runAdapter serves the adapter until the returned cancel is called.
func runAdapter(t *testing.T, a *Adapter, handle func(gateway.Inbound) error) (context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, handle) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v after cancel", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return cancel, done
}

func textUpdate(id, chatID, userID int64, text string) update {
	return update{UpdateID: id, Message: &message{
		MessageID: id,
		From:      &user{ID: userID, FirstName: "Ada", LastName: "Lovelace"},
		Chat:      chat{ID: chatID, Type: "private"},
		Text:      text,
	}}
}

func TestNewValidationAndDefaults(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("a token-less adapter was accepted")
	}
	a, err := New(Options{Token: " T ", APIBase: "https://example.test/"})
	if err != nil {
		t.Fatal(err)
	}
	if a.base != "https://example.test" {
		t.Errorf("base = %q, want the trailing slash trimmed", a.base)
	}
	if a.o.PollTimeout != defaultPollTimeout || a.o.Attempts != defaultAttempts {
		t.Errorf("defaults = %v/%d", a.o.PollTimeout, a.o.Attempts)
	}
	if a.Name() != "telegram" {
		t.Errorf("Name = %q", a.Name())
	}
}

func TestRunDeliversTextAndReplies(t *testing.T) {
	api := &testAPI{pages: [][]update{{textUpdate(11, 7, 42, "hi")}, {}}}
	a, _, _ := newAdapter(t, api, Options{PollTimeout: time.Second})

	inbox := make(chan gateway.Inbound, 4)
	cancel, _ := runAdapter(t, a, func(in gateway.Inbound) error {
		inbox <- in
		return nil
	})

	var in gateway.Inbound
	select {
	case in = <-inbox:
	case <-time.After(5 * time.Second):
		t.Fatal("no inbound message")
	}
	if in.Platform != "telegram" || in.ChatID != "7" || in.UserID != "42" || in.Text != "hi" {
		t.Errorf("inbound = %+v", in)
	}
	if in.UserName != "Ada Lovelace" {
		t.Errorf("UserName = %q", in.UserName)
	}

	if err := in.Send(context.Background(), "pong"); err != nil {
		t.Fatalf("send: %v", err)
	}
	api.mu.Lock()
	sends := append([]sendBody(nil), api.sends...)
	polls := append([]pollBody(nil), api.polls...)
	api.mu.Unlock()
	if len(sends) != 1 || sends[0].ChatID != "7" || sends[0].Text != "pong" {
		t.Fatalf("sends = %+v", sends)
	}
	if !sends[0].DisableWebPagePreview {
		t.Error("sendMessage should disable the web page preview")
	}

	// The second poll resumes after the delivered update.
	for i := 0; i < 100 && len(polls) < 2; i++ {
		time.Sleep(10 * time.Millisecond)
		api.mu.Lock()
		polls = append([]pollBody(nil), api.polls...)
		api.mu.Unlock()
	}
	if len(polls) < 2 {
		t.Fatalf("only %d polls", len(polls))
	}
	if polls[0].Offset != 0 {
		t.Errorf("first offset = %d, want 0", polls[0].Offset)
	}
	if polls[1].Offset != 12 {
		t.Errorf("second offset = %d, want update_id+1", polls[1].Offset)
	}
	if len(polls[0].AllowedUpdates) != 1 || polls[0].AllowedUpdates[0] != "message" {
		t.Errorf("allowed_updates = %v", polls[0].AllowedUpdates)
	}
	cancel()
}

func TestRunIgnoresNonTextAndBots(t *testing.T) {
	botMsg := textUpdate(1, 7, 99, "beep")
	botMsg.Message.From.IsBot = true
	noText := update{UpdateID: 2, Message: &message{MessageID: 2, From: &user{ID: 5}, Chat: chat{ID: 7}}}
	api := &testAPI{pages: [][]update{{botMsg, noText}, {textUpdate(3, 7, 42, "real")}, {}}}
	a, _, _ := newAdapter(t, api, Options{PollTimeout: time.Second})

	inbox := make(chan gateway.Inbound, 4)
	runAdapter(t, a, func(in gateway.Inbound) error {
		inbox <- in
		return nil
	})

	select {
	case in := <-inbox:
		if in.Text != "real" {
			t.Fatalf("first delivered message = %q, want the text one", in.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the text message never arrived")
	}
	select {
	case in := <-inbox:
		t.Fatalf("an ignored update was delivered: %+v", in)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSendRetriesOn429WithRetryAfter(t *testing.T) {
	api := &testAPI{failSends: 1}
	a, delays, _ := newAdapter(t, api, Options{PollTimeout: time.Second})

	in := a.sender("7")
	if err := in(context.Background(), "hello"); err != nil {
		t.Fatalf("send: %v", err)
	}
	api.mu.Lock()
	sends := len(api.sends)
	api.mu.Unlock()
	if sends != 2 {
		t.Fatalf("sendMessage calls = %d, want 2 (one 429, one success)", sends)
	}
	d := *delays
	if len(d) != 1 || d[0] != 2*time.Second {
		t.Fatalf("retry delays = %v, want [2s] from retry_after", d)
	}
}

func TestSendSplitsLongText(t *testing.T) {
	api := &testAPI{}
	a, _, _ := newAdapter(t, api, Options{PollTimeout: time.Second})

	long := strings.Repeat("a", MaxMessageLength+10)
	if err := a.sender("7")(context.Background(), long); err != nil {
		t.Fatalf("send: %v", err)
	}
	api.mu.Lock()
	sends := append([]sendBody(nil), api.sends...)
	api.mu.Unlock()
	if len(sends) != 2 {
		t.Fatalf("sendMessage calls = %d, want 2", len(sends))
	}
	for i, s := range sends {
		if len([]rune(s.Text)) > MaxMessageLength {
			t.Errorf("chunk %d is %d runes", i, len([]rune(s.Text)))
		}
	}
	if len([]rune(sends[0].Text))+len([]rune(sends[1].Text)) != len([]rune(long)) {
		t.Error("the chunks do not add up to the original text")
	}
	if sends[0].ChatID != "7" || sends[1].ChatID != "7" {
		t.Errorf("chat ids = %q %q", sends[0].ChatID, sends[1].ChatID)
	}
	// The token goes in the URL path, as the Bot API requires.
	api.mu.Lock()
	paths := append([]string(nil), api.paths...)
	api.mu.Unlock()
	if len(paths) == 0 || !strings.HasPrefix(paths[0], "/botTEST-TOKEN/") {
		t.Errorf("request path = %v, want /bot<token>/...", paths)
	}
}
