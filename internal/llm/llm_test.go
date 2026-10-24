package llm

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestSystemText(t *testing.T) {
	r := Request{System: []string{"a", "", "b"}}
	if got := r.SystemText(); got != "a\n\nb" {
		t.Fatalf("got %q", got)
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, false},
		{"wrapped canceled", fmt.Errorf("x: %w", context.Canceled), false},
		{"429", &APIError{Status: 429}, true},
		{"529 overloaded", &APIError{Status: 529}, true},
		{"500", fmt.Errorf("call: %w", &APIError{Status: 500}), true},
		{"400", &APIError{Status: 400}, false},
		{"401", &APIError{Status: 401}, false},
		{"stream cut", fmt.Errorf("read: %w", ErrStreamInterrupted), true},
		{"other", errors.New("boom"), false},
	}
	for _, tt := range tests {
		if got := IsRetryable(tt.err); got != tt.want {
			t.Errorf("%s: IsRetryable = %v, want %v", tt.name, got, tt.want)
		}
	}
}
