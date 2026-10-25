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
| T2 delegate_task | S2 | inline | done | see Log |
| T3 auxiliary routing | S3 | inline | done | see Log |
| T4 shell hook events | S4 | inline | done | see Log |
| T5 MCP resources, prompts, reconnect | S5 | inline | planned | |
| T6 API server endpoints | S6 | inline | planned | |
| T7 smart approval | S7 | inline | planned | |
| T8 TUI canonical wire | S8 | inline | planned | |
| T9 Retry-After, max_tokens continuation | S9 | inline | planned | |
| T10 patch strategies, .gitignore | S10 | inline | planned | |

## Log

- L1 (2026-10-24, owner): "Continuemos, sigamos trabajando en que tenga todas las funcionalidades de hermes"
- L2 T1: `provider.FallbackChain` merges `fallback_providers` then `fallback_model`, drops entries without provider and model, dedupes by provider, model and base URL, and skips unresolvable entries with a warning. `agent.Options.Fallbacks`: 429 and 402 switch at once and arm the primary cooldown (60 s doubling, 4 h cap); 401, 403 and 404 switch at once; 408 and 5xx after the retries; a plain 400 never. Each switch restarts the retry budget, skips an entry equal to the current backend, and emits upstream's "⚠️ Model fallback: ..." notice; the next turn restores the primary with "✅ Primary model restored: ..." unless the cooldown runs. Not ported: classifier opt-in 400s (format errors), provider `reset_at`, `fallback.min_switch_reset_seconds`, credential pools, the init banner. Checks: `go test -race ./...`, vet, golangci-lint green.
- L3 T2: `internal/tools/delegate` ports `delegate_task` (toolset `delegation`): the `tasks` array or legacy `goal`/`context`, `delegation.max_concurrent_children` (10) caps the batch, children run in parallel and return upstream's result entries (status and exit reason rules, tokens, `tool_trace` without raw data, `cost_status: unknown`) with the 24000-char head/tail summary cap. The child gets upstream's leaf system prompt, the goal as its first message, the registry minus `delegate_task`, `clarify`, `memory`, `send_message`, `cronjob_manage`, `delegation.max_iterations` (250), a `subagent` session row under the caller's session, and approvals that deny unless `delegation.subagent_auto_approve`. `delegation.model`/`provider`/`base_url`/`api_key` pin another model. Not ported: background dispatch and `action` list/steer/stop, orchestrator children, cost estimates, the summary spill file, per-child terminal sessions. Checks: `go test -race ./...`, vet, golangci-lint green.
- L4 T3: `provider.Auxiliary` resolves `auxiliary.<task>` as upstream `_resolve_task_provider_model`: a base_url or a non-auto provider pins a route (a bare base_url is `custom`), `auto` with a model keeps the main client and swaps the model, unset inherits the main model; an unresolvable block falls back to the main route with a warning. The compression summary is the port's only side call: it uses that route and `auxiliary.compression.timeout` (120 s default, 300 s floor), and a timed-out summary takes the deterministic fallback. Children compress through their own route. Not ported: the auto discovery chain, per-task fallback chains, `extra_body`, `reasoning_effort`, the provider's default aux model. Checks: `go test -race ./...`, vet, golangci-lint green.
- L5 T4: `pre_llm_call`, `post_llm_call`, `on_session_start`, `on_session_end`, `subagent_start` and `subagent_stop` fire as upstream. `agent.TurnHooks` runs on every `Run`: `on_session_start` before the first turn, `pre_llm_call` once per turn (hook `context` strings joined by blank lines and appended to the user message sent to the model; `OnMessage` persists the typed text and `Rewind` returns it), `post_llm_call` and a per-turn `on_session_end` with upstream's exit flags and `turn_exit_reason` (`text_response(finish_reason=stop)`, `max_iterations_reached(n/max)`, `interrupted_by_user`, `all_retries_exhausted_no_response`). `hooks.Lifecycle` builds the payloads in upstream key order with `tool_name`/`tool_input` null and `conversation_history` in the OpenAI chat shape. Children fire `subagent_start` (parent session as `session_id`, `child_subagent_id` `sa-<index>-<hex8>`, role `leaf`) and `subagent_stop` after the summary cap with `delegate.StopHistory`; child turns fire the turn events under the child session with the parent's platform. Not ported: hook context spill, child summary redaction, `task_id`/`turn_id` values, the shutdown `on_session_end`. Checks: `go test -race ./...`, vet, golangci-lint green.
