package main

import (
	"log/slog"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/compress"
	"github.com/umi008/hermes-agent-go/internal/config"
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
			c := newCompressor(config.FromMap(raw), nil, "m", time.Now, slog.New(slog.DiscardHandler))
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
