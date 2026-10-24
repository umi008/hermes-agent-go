# V7 Parity

Objective: close the PARITY rows marked `later` and the deliberate gaps of V1-V5 that upstream users notice, one small feature per task.
Branch: `dev`. Delivery: small commits pushed to `dev`, merged to `main` at the end of the vertical. Test runner: `go test ./...` (`-race` with cgo locally).

## Specs

- S1 Fallback providers: `fallback_providers` (and legacy `fallback_model`) from `config.yaml`; when the primary provider is exhausted, the agent moves to the next entry of the chain, as upstream.
- S2 `delegate_task`: subagents, as upstream `tools/delegate_tool*.py`.
- S3 Auxiliary side-LLM routing: compression summaries and other side calls use the auxiliary model when configured.
- S4 Other shell hook events beyond `pre_tool_call` and `post_tool_call`.
- S5 MCP resources and prompts utility tools; reconnect with backoff.
- S6 API server: `/v1/responses`, `/v1/runs`, `/api/sessions`.
- S7 Smart (LLM) approval gate.
- S8 TUI canonical wire: `session_authority`, resume, session list, slash commands.
- S9 Agent loop robustness: Retry-After honoring, `max_tokens` continuation.
- S10 File tools: further fuzzy `patch` strategies, `.gitignore` filtering in `search_files`.

Assumptions:
- Order follows user-visible value: S1 first (a single provider outage stops the agent today), then S2.
- Rows marked `out` in PARITY stay out unless the owner reopens them.

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 fallback providers | S1 | inline | done | see Log |
| T2 delegate_task | S2 | inline | planned | |
| T3 auxiliary routing | S3 | inline | planned | |
| T4 shell hook events | S4 | inline | planned | |
| T5 MCP resources, prompts, reconnect | S5 | inline | planned | |
| T6 API server endpoints | S6 | inline | planned | |
| T7 smart approval | S7 | inline | planned | |
| T8 TUI canonical wire | S8 | inline | planned | |
| T9 Retry-After, max_tokens continuation | S9 | inline | planned | |
| T10 patch strategies, .gitignore | S10 | inline | planned | |

## Log

- L1 (2026-10-24, owner): "Continuemos, sigamos trabajando en que tenga todas las funcionalidades de hermes"
- L2 T1: `provider.FallbackChain` merges `fallback_providers` then `fallback_model`, drops entries without provider and model, dedupes by provider, model and base URL, and skips unresolvable entries with a warning. `agent.Options.Fallbacks`: 429 and 402 switch at once and arm the primary cooldown (60 s doubling, 4 h cap); 401, 403 and 404 switch at once; 408 and 5xx after the retries; a plain 400 never. Each switch restarts the retry budget, skips an entry equal to the current backend, and emits upstream's "⚠️ Model fallback: ..." notice; the next turn restores the primary with "✅ Primary model restored: ..." unless the cooldown runs. Not ported: classifier opt-in 400s (format errors), provider `reset_at`, `fallback.min_switch_reset_seconds`, credential pools, the init banner. Checks: `go test -race ./...`, vet, golangci-lint green.
