package main

import (
	"log/slog"
	"strings"
	"time"

	"github.com/umi008/hermes-agent-go/internal/agent"
	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/tools/approval"
)

// Smart approval wiring: upstream tools/approval_context.py and
// tools/approval_smart.py resolve the mode, the guardian's route and the
// denial circuit breaker from the same `approvals` block.

// approvalsMode is upstream _normalize_approval_mode. YAML 1.1 parses a bare
// `off` as false, so a bool is the intended mode; an unknown string falls back
// to manual with a warning instead of silently failing every mode check.
func approvalsMode(cfg *config.Config, logger *slog.Logger) string {
	raw, ok := cfg.Get("approvals", "mode")
	if !ok {
		return "manual"
	}
	switch x := raw.(type) {
	case bool:
		if !x {
			return "off"
		}
	case string:
		switch normalized := strings.ToLower(strings.TrimSpace(x)); normalized {
		case "manual", "smart", "off":
			return normalized
		default:
			if normalized != "" {
				logger.Warn("unknown approvals.mode; defaulting to manual",
					"mode", x, "valid", "manual, smart, off")
			}
		}
	}
	return "manual"
}

// denialBreakerThreshold is approvals.denial_breaker_threshold: upstream's
// default is 3 and zero or negative disables the breaker.
func denialBreakerThreshold(cfg *config.Config) int {
	if n, ok := cfg.Int("approvals", "denial_breaker_threshold"); ok {
		return n
	}
	return 3
}

// approvalTimeout is auxiliary.approval.timeout, upstream's 30 s default.
func approvalTimeout(cfg *config.Config) time.Duration {
	if secs := cfgFloat(cfg, "auxiliary", "approval", "timeout"); secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 30 * time.Second
}

// smartReviewer builds the guardian-LLM reviewer when approvals.mode is smart.
// A nil reviewer keeps the gate manual, so an unset or manual mode never pays
// for an auxiliary call.
func smartReviewer(cfg *config.Config, lookup config.LookupFunc, main agent.Route, logger *slog.Logger) approval.Reviewer {
	if approvalsMode(cfg, logger) != "smart" {
		return nil
	}
	client, model := auxiliaryRoute(cfg, lookup, "approval", main, logger)
	return approval.NewSmart(approval.SmartOptions{
		Client: client, Model: model, Timeout: approvalTimeout(cfg),
		Policy: cfg.String("approvals", "smart_policy"), Logger: logger,
	})
}
