package main

import (
	"log/slog"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/compress"
	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/llm"
)

func TestNewCompressorReadsConfig(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want int // threshold tokens; -1 means disabled
	}{
		{"defaults", "", 192_000},
		{"disabled", "compression:\n  enabled: false\n", -1},
		{"disabled string", "compression:\n  enabled: \"false\"\n", -1},
		{"window and fraction", "model:\n  context_length: 1000000\ncompression:\n  threshold: 0.6\n", 600_000},
		{"string fraction", "model:\n  context_length: 1000000\ncompression:\n  threshold: \"0.7\"\n", 700_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := config.Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			c := newCompressor(config.FromMap(raw), nil, agent.Route{Model: "m"}, time.Now, slog.New(slog.DiscardHandler))
			if tt.want < 0 {
				if c != nil {
					t.Fatal("compression should be off")
				}
				return
			}
			if got := c.(*compress.Compressor).ThresholdTokens(); got != tt.want {
				t.Fatalf("threshold %d want %d", got, tt.want)
			}
		})
	}
}

type namedClient struct{ llm.Client }

func TestAuxiliaryRoute(t *testing.T) {
	main := agent.Route{Client: namedClient{}, Provider: "anthropic", Model: "big"}
	logger := slog.New(slog.DiscardHandler)
	lookup := func(string) (string, bool) { return "", false }
	cfg := func(yaml string) *config.Config {
		raw, err := config.Parse([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		return config.FromMap(raw)
	}

	if c, m := auxiliaryRoute(cfg(""), lookup, "compression", main, logger); c != main.Client || m != "big" {
		t.Errorf("unset: %v %q", c, m)
	}
	if c, m := auxiliaryRoute(cfg("auxiliary: {compression: {model: small}}\n"), lookup, "compression", main, logger); c != main.Client || m != "small" {
		t.Errorf("model only: %v %q", c, m)
	}
	c, m := auxiliaryRoute(cfg("auxiliary: {compression: {base_url: \"http://127.0.0.1:9/v1\", model: tiny}}\n"), lookup, "compression", main, logger)
	if c == main.Client || c == nil || m != "tiny" {
		t.Errorf("pinned: %v %q", c, m)
	}
	if c, m := auxiliaryRoute(cfg("auxiliary: {compression: {provider: nowhere, model: x}}\n"), lookup, "compression", main, logger); c != main.Client || m != "big" {
		t.Errorf("unresolvable: %v %q", c, m)
	}
}
