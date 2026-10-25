package main

import (
	"log/slog"
	"strconv"
	"time"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/compress"
	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/provider"
)

var _ agent.Compressor = (*compress.Compressor)(nil)

// Upstream auxiliary.compression.timeout defaults to 120 s and is floored at
// 300 s, since reasoning models can take longer to summarize.
const (
	compressionTimeout      = 120 * time.Second
	compressionTimeoutFloor = 300 * time.Second
)

// newCompressor reads the upstream compression and model.context_length
// keys; compression.enabled false turns compaction off. The summary call
// goes to auxiliary.compression when that block pins a route or a model,
// and to main otherwise.
func newCompressor(cfg *config.Config, lookup config.LookupFunc, main agent.Route, now func() time.Time, logger *slog.Logger) agent.Compressor {
	if isFalse(cfg, "compression", "enabled") {
		return nil
	}
	ctxLen, _ := cfg.Int("model", "context_length")
	lastN, _ := cfg.Int("compression", "protect_last_n")
	client, model := auxiliaryRoute(cfg, lookup, "compression", main, logger)
	timeout := compressionTimeout
	if secs := cfgFloat(cfg, "auxiliary", "compression", "timeout"); secs > 0 {
		timeout = time.Duration(secs * float64(time.Second))
	}
	return compress.New(compress.Options{
		Client:        client,
		Model:         model,
		ContextLength: ctxLen,
		Threshold:     cfgFloat(cfg, "compression", "threshold"),
		ProtectLastN:  lastN,
		Now:           now,
		Logger:        logger,
		Timeout:       max(timeout, compressionTimeoutFloor),
	})
}

// auxiliaryRoute is the client and model for a side call of task. A block
// that fails to resolve falls back to the main route with a warning.
func auxiliaryRoute(cfg *config.Config, lookup config.LookupFunc, task string, main agent.Route, logger *slog.Logger) (llm.Client, string) {
	ep, model, pinned, err := provider.Auxiliary(cfg, lookup, task)
	switch {
	case err != nil:
		logger.Warn("auxiliary route unavailable; using the main model", "task", task, "err", err)
	case pinned:
		return newClient(ep), ep.Model
	case model != "":
		return main.Client, model
	}
	return main.Client, main.Model
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
