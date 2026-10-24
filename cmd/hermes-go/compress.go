package main

import (
	"log/slog"
	"strconv"
	"time"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/compress"
	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/llm"
)

var _ agent.Compressor = (*compress.Compressor)(nil)

// newCompressor reads the upstream compression and model.context_length
// keys; compression.enabled false turns compaction off. The summary call
// uses the main client and model.
func newCompressor(cfg *config.Config, client llm.Client, model string, now func() time.Time, logger *slog.Logger) agent.Compressor {
	if isFalse(cfg, "compression", "enabled") {
		return nil
	}
	ctxLen, _ := cfg.Int("model", "context_length")
	lastN, _ := cfg.Int("compression", "protect_last_n")
	return compress.New(compress.Options{
		Client:        client,
		Model:         model,
		ContextLength: ctxLen,
		Threshold:     cfgFloat(cfg, "compression", "threshold"),
		ProtectLastN:  lastN,
		Now:           now,
		Logger:        logger,
	})
}

func cfgFloat(cfg *config.Config, path ...string) float64 {
	v, ok := cfg.Get(path...)
	if !ok {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}
