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
| T5 MCP resources, prompts, reconnect | S5 | inline | done | see Log |
| T6a /v1/responses | S6 | inline | done | see Log |
| T6b /v1/runs | S6 | inline | planned | |
| T6c /api/sessions | S6 | inline | planned | |
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
- L6 T5: `mcpclient` registers upstream's frozen `list_resources`, `read_resource`, `list_prompts` and `get_prompt` schemas per server, only for an advertised capability family and unless `tools.resources`/`tools.prompts` is false; include/exclude do not apply and a native tool of the same name wins. Handlers render as upstream (`resources` with `description`/`mimeType` only when set, `read_resource` text joined by newlines and blobs as the embedded-resource notice, `prompts` with `arguments`, `get_prompt` `messages` plus `description`), with `Missing required parameter '<p>'` and `MCP server '<s>' is not connected`. Each server has a supervisor: the initial connect retries transient failures three times (permanent ones such as a missing command, no endpoint, 401/403 fail at once), a drop reconnects with 1 s doubling ±20 % backoff capped at 60 s up to five times, then parks and probes every 300 s; a served call resets the budget. A call waits up to 15 s for a reconnect in flight; a mid-call drop replays only read-only tools and utilities, other tools get upstream's outcome-unknown message. Not ported: `tools/list_changed` refresh after reconnect, keepalive pings, the event tools, auth/session-expired recovery handlers, prompt argument `required: null`. Checks: `go test -race ./...`, vet, golangci-lint green.
- L7 T6a: `POST/GET/DELETE /v1/responses` port upstream: `input` string or item array, `instructions`, `previous_response_id` or `conversation` (exclusive), `conversation_history`, `store`, `truncation: auto` (last 100 messages), upstream's error messages and envelope. Records live in a SQLite LRU (`hermes-go/response_store.db`, 100 rows, memory fallback) with the replay transcript, instructions and session id, so a chain continues the same session (`X-Hermes-Session-Id`). Streaming emits upstream's event sequence with sha256 item ids, 50 ms text batching, 10 s keepalives, function_call/function_call_output items and the trimmed terminal snapshot; a disconnect stores an incomplete snapshot. `gateway.api_server.max_concurrent_runs` (10, 0 off) caps chat completions and responses with upstream's 429. `agent.Options.History` seeds prior messages. Gaps: no reasoning items, generic failure text, system/developer history dropped, truncation keeps no compaction summary, compact JSON, no idempotency keys, `X-Hermes-Session-Key` or multimodal input, session source `api`. RED observed only for the agent History test; the apiserver tests were written with the code.
