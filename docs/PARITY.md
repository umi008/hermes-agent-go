# Parity matrix

This matrix tracks which upstream Hermes Agent features the Go port covers, which
vertical owns each one, and where the upstream code lives.

Status values:

- `planned`: in scope, not started yet.
- `partial`: in scope, partly implemented.
- `done`: in scope and implemented.
- `out`: out of scope for now.
- `later`: in scope, but deferred past the first pass of its vertical.

Update the Status column as verticals land. Upstream paths are relative to
`NousResearch/hermes-agent` at `bda3a363e9`.

## Core (V1)

| Feature | Status | Upstream |
|---|---|---|
| Hermes home resolution (`HERMES_HOME`, `LOCALAPPDATA` on Windows, `HERMES_DATA_DIR_SUFFIX`) | done | `hermes_constants.py:51-58,188-246` |
| Profiles (`profiles/<name>`, `active_profile`, name regex) | done | `hermes_cli/profiles.py`, `hermes_constants.py:93-101,395` |
| `config.yaml` load and deep-merge, unknown keys preserved | done | `hermes_cli/config.py:1487,2088,2476` |
| Config migrations | out (read v ≥ 12, warn) | `hermes_cli/config_migrations.py` |
| `.env` secrets | done | `agent/secret_scope.py:294-309` |
| Provider resolution (`model`, `providers`, `fallback_providers`) | done (fallback chain: rate limit, billing, auth and missing model switch at once, transient errors after the retries; turn-scoped with the primary rate-limit cooldown; no credential pools, `min_switch_reset_seconds` or provider `reset_at`) | `plugins/model-providers/`, `agent/agent_init*.py` |
| OpenAI chat completions client, streaming | done | `agent/transports/chat_completions.py` |
| Anthropic Messages client, streaming | done | `agent/transports/anthropic.py`, `agent/anthropic_adapter.py` |
| Anthropic prompt caching (4 breakpoints, TTL) | done | `agent/prompt_caching.py` |
| OpenAI Responses / Codex, Bedrock, Vertex, Gemini native | out | `agent/transports/codex.py`, `bedrock.py`, … |
| Credential pool, OAuth providers | out | `agent/credential_pool*.py`, `hermes_cli/auth.py` |
| System prompt builder (3 tiers, SOUL.md, context files) | done | `agent/system_prompt.py:736`, `agent/prompt_builder.py:1679` |
| Agent loop: iterations, retries, interrupt, steer | done | `agent/conversation_loop.py`, `agent/interrupt_control.py` |
| Observable tool-call event stream | done | `agent/stream_delivery.py` |
| `<think>` scrubbing across deltas | done | `agent/conversation_loop.py:354-361` |
| Auxiliary side-LLM routing | partial (`auxiliary.compression`: provider, model, base_url, api_key, key_env, api_mode, timeout with the 300 s floor; the port has no other side calls; no auto discovery chain, per-task fallback chain, extra_body or reasoning_effort) | `agent/auxiliary_client.py` |

## Tools (V2)

| Feature | Status | Upstream |
|---|---|---|
| Tool registry, toolsets, `check_fn` | done | `tools/registry.py`, `toolsets.py`, `model_tools.py` |
| `read_file`, `write_file`, `patch`, `search_files` | partial (no fuzzy patch strategies beyond exact/whitespace, no .gitignore) | `tools/file_*.py` (UNVERIFIED names) |
| `terminal` (local backend), `process_manage` | done | `tools/terminal_tool*.py`, `tools/environments/local.py` |
| Dangerous-command detection and approval | done (116-command parity fixture; once/session/deny prompt in the interactive CLI) | `tools/approval_detection.py:228,1548`, `tools/approval.py` |
| Smart (LLM) approval gate | done (guardian verdicts, smart-deny menu, comment stripping, operator policy, denial breaker; no approval plugin hooks) | `tools/approval.py:764`, `tools/approval_smart.py` |
| `web_search`, `web_extract` | done (Tavily only) | `tools/web_*.py`, `plugins/web/` |
| Docker terminal backend | done | `tools/environments/docker.py` |
| SSH, Modal, Daytona, Vercel backends | out | `tools/environments/` |
| `todo_list`, `clarify` | done (clarify answered in the interactive CLI) | `agent/inline_tool_executors.py:253-313` |
| `delegate_task` (subagents) | done (synchronous batch, no background dispatch or list/steer/stop; leaf children only, sharing the parent's terminal session; no cost estimate or spill file) | `tools/delegate_tool*.py` |
| `vision_analyze`, `image_generate`, `text_to_speech` | out | `tools/` |
| Browser tools | out | `tools/browser*` |
| `execute_code` | out | `tools/code_execution*` |
| Kanban tools | out | `hermes_cli/kanban*.py`, `plugins/kanban/` |
| `computer_use`, `manage_connections` | out | `tools/` |

## State (V3)

| Feature | Status | Upstream |
|---|---|---|
| `state.db` sessions and messages, read and write at schema v32 (own `hermes-go/state.db`) | done | `hermes_state_common.py:452-565` |
| FTS5 `messages_fts` and `messages_fts_trigram` search | done (no CJK, no title match) | `hermes_state_common.py:941-1080` |
| CJK FTS (loadable extension) | out (risk, see OPEN_QUESTIONS) | `hermes_state_fts.py` |
| Gateway and runtime ledger tables | out | `hermes_state_common.py:586-748` |
| `session_search` tool | done (no lineage, no cross-profile) | `agent/inline_tool_executors.py` |
| Built-in memory: `MEMORY.md` and `USER.md` (own `hermes-go/memories`) | done (no threat scan, no file lock) | `tools/memory_tool.py`, `tools/memory_tool_store.py` |
| Memory provider plugins | out | `plugins/memory/` |
| Context compression (prune tool results, LLM summary, fallback) | partial (legacy prompt, no session log or anchors) | `agent/context_compressor.py` |
| Native Codex compaction | out | `agent/native_compaction.py` |

## Extensibility (V4)

| Feature | Status | Upstream |
|---|---|---|
| Skills discovery and frontmatter, `skills_list` / `skill_view` | done | `agent/skill_utils.py`, `tools/skills_tool.py` |
| `skill_manage` (create and patch agent skills) | done (interactive sessions only, as upstream; atomic `operations` batches; no staged write gate, ledger or linter) | `tools/skill_manager_tool.py` |
| Skills index in the system prompt | done | `agent/prompt_builder.py:1244` |
| Skills hub, curator, bundled sync | out | `tools/skills_hub*.py`, `agent/curator.py`, `tools/skills_sync.py` |
| MCP client over stdio | done | `tools/mcp_tool.py:160` |
| MCP client over streamable HTTP | done | `tools/mcp_tool.py:162-164` |
| MCP SSE transport | done | `tools/mcp_tool*.py` |
| Shell hooks (`hooks:` config) | done (`pre_tool_call`, `post_tool_call`, `pre_llm_call`, `post_llm_call`, `on_session_start`, `on_session_end`, `subagent_start`, `subagent_stop`) | `agent/shell_hooks.py` |
| MCP resources and prompts utility tools | done | `tools/mcp_tool_schema.py`, `tools/mcp_tool_handlers.py` |
| MCP reconnect with backoff | done | `tools/mcp_tool_server_run.py` |
| MCP sampling, OAuth, event tools | later | `tools/mcp_tool*.py` |
| Other shell hook events (`pre_api_request`, `pre_compress`, `pre_verify`, gateway and approval events, ...) | later | `agent/shell_hooks.py` |
| Python plugin system | out (no Python plugins in Go) | `hermes_cli/plugins.py` |

## Interfaces (V5)

| Feature | Status | Upstream |
|---|---|---|
| CLI REPL and one-shot mode | done (Ctrl-C cancels a turn; clarify and dangerous-command approval answered from the terminal) | `cli.py`, `hermes_cli/cli_*_mixin.py` |
| Slash commands (subset) | done (/new /reset /clear /history /retry /undo /title /status /model /tools /toolsets /skills /help /quit) | `hermes_cli/` slash registry |
| TUI backend speaking `hermes-gateway-v1` over WebSocket | done: legacy wire, `hermes-go tui` (canonical wire planned) | `tui_gateway/`, `gateway/session_controls.py` |
| Telegram adapter | done (long polling, text only; no inline keyboards, media, topics or voice) | `plugins/platforms/telegram/` |
| Discord adapter | done (Gateway WebSocket v10 and REST, text only; no slash commands, components or voice) | `plugins/platforms/discord/` |
| Gateway pairing and approvals | done (subset: sessions per chat, per-chat queue, approvals answered in the chat; no pairing, delivery ledger or rooms) | `gateway/pairing.py`, `tools/approval.py:126-196` |
| Other 20+ platforms | out | `plugins/platforms/` |
| Cron scheduler with `jobs.json` | done (interval, 5-field cron and one-shot schedules; at-most-once ticks behind a lock file; output files with retention; no delivery targets, quotas or threads) | `cron/jobs.py`, `cron/scheduler.py` |
| OpenAI-compatible API: `/v1/chat/completions`, `/v1/models` | done (streaming and non-streaming, bearer token, one session per request) | `gateway/platforms/api_server.py:59-81` |
| `/v1/responses` (store, chains, conversations, streaming, run cap) | done | `gateway/platforms/api_server.py` |
| `/v1/runs` (start, status, events with replay, stop) | done | `gateway/platforms/api_server_runs.py` |
| `/v1/runs` approval, clarify, steer, resolve-unknown, Idempotency-Key | later | `gateway/platforms/api_server_runs.py` |
| `/api/sessions` list, create, get, patch, delete, messages, fork | done | `gateway/platforms/api_server.py` |
| `/api/sessions/{id}/chat`, `/chat/stream`, model lock | done (one turn over a stored session, synchronous and SSE, with the persisted Browser model lock; no provider switching) | `gateway/platforms/api_server.py` |
| Compression lineage (tip projection, ancestor messages, lineage-wide flags) | later | `hermes_state_compression.py` |
| Desktop app, dashboard web UI | out | `apps/desktop/`, `web/` |
| ACP | out | — |

## Packaging (V6)

| Feature | Status | Upstream |
|---|---|---|
| Single CGO-free binary | done (`CGO_ENABLED=0`, pure-Go SQLite; `hermes-go version`) | — |
| Dockerfile (distroless) | done (static, nonroot, `HERMES_HOME=/data`; no shell, so the local `terminal` tool cannot run inside it) | `Dockerfile` |
| goreleaser, GitHub Actions CI | done (CI: vet, `-race` tests on Linux and Windows, lint, image build and smoke; release on `v*` tags: linux, darwin and windows on amd64 and arm64, checksums) | `.github/workflows/` |

## Out of scope (explicit)

- Desktop app
- Platform plugins beyond Telegram and Discord
- RL and trajectories
- `execute_code`
- ACP
- Browser automation
- Media generation
- Kanban
- Credential pools and OAuth flows
- Config migrations
