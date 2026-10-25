package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestChatFallsBackWhenThePrimaryIsRateLimited(t *testing.T) {
	var primaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		primaryCalls.Add(1)
		http.Error(w, `{"error":{"message":"slow down"}}`, http.StatusTooManyRequests)
	}))
	t.Cleanup(primary.Close)
	backup := deltasModel(t, "from ", "backup")

	f := newHome(t, modelConfig(primary.URL)+
		"fallback_providers:\n  - {provider: custom, model: spare, base_url: "+backup.URL+"}\n")
	code := Run(context.Background(), []string{"chat", "-q", "hi"}, f.env())
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, f.stderr.String())
	}
	if f.stdout.String() != "from backup\n" {
		t.Fatalf("stdout = %q", f.stdout.String())
	}
	if primaryCalls.Load() != 1 {
		t.Errorf("primary calls = %d, want 1", primaryCalls.Load())
	}
	want := "⚠️ Model fallback: tiny via custom unavailable (rate limit); using spare via custom. Primary retry eligible in ~60 s"
	if !strings.Contains(f.stderr.String(), want) {
		t.Errorf("stderr = %q", f.stderr.String())
	}
}
