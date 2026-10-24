# Upstream architecture summary

A map of how upstream Hermes Agent (Python) is built, written to guide the Go
port. Citations point at `NousResearch/hermes-agent` at commit `bda3a363e9`.
"UNVERIFIED" marks claims taken from an `AGENTS.md` without tracing the code.

## Size of the upstream codebase

Python lines, counted with `wc -l` over each area recursively:

| Area | Lines | Files |
|---|---|---|
| `hermes_cli/` | 295,744 | 763 |
| `agent/` | 145,446 | 394 |
| `tools/` | 126,960 | 361 |
| `gateway/` | 122,652 | 325 |
| `tui_gateway/` | 46,543 | 117 |
| `hermes_state*.py` | 26,621 | — |
| `cron/` | 19,143 | 45 |

Roughly 790k lines, excluding `plugins/`. The Go port targets a narrow,
well-defined subset of this (see [PARITY.md](PARITY.md)).

## Two invariants that shape everything

1. **Per-conversation prompt caching.**
   - The system prompt and the toolset stay byte-stable for the whole conversation. Context compression is the only sanctioned mutation (`AGENTS.md`, `agent/AGENTS.md:57-63`).
   - Memory is snapshotted at session start.
   - Skills inject as user messages, never into the system prompt.
2. **Narrow core.** Every tool schema is sent on every API call, so new capability belongs at the edges: skills, plugins, service-gated tools.

## Agent turn loop

- **Entry point:**
  - `AIAgent` is a facade (`run_agent.py`, 1563 lines) built by `agent/agent_init.py::init_agent`.
  - A turn is `agent/conversation_loop.py::run_conversation` (`:1785`), run under a session turn lease (`agent/turn_facade_lease.py`).
- **Loop bounds:**
  - The loop runs while `api_call_count < max_iterations` and the iteration budget remains (`conversation_loop.py:1724-1725`). `max_iterations` defaults to 500 and is shared with subagents.
  - An inner retry loop handles API errors (`:1578`).
- **Phases** (`agent/AGENTS.md:38-46`): each phase is a `turn_*.py` sibling.
  - Preflight: `turn_preflight*`, `turn_iteration_prep`, `turn_scripted_prelude`.
  - Request: `turn_request_assembly`, `turn_api_request`, `turn_api_call`, `turn_api_error`.
  - Response: `turn_response_intake`, `turn_response_check`, `turn_empty_response`.
  - Tools: `turn_tool_round`, `turn_tool_validation`.
  - Recovery: `turn_overflow`, `turn_truncation`, `turn_context_compaction`, `turn_recovery*`, `turn_stop_gates`.
  - Finish: `turn_usage`, `turn_final_response`, `turn_finalizer`.
- **Messages:**
  - The internal format is OpenAI-style `{role: system|user|assistant|tool}`. Reasoning lives in `assistant.reasoning`.
  - Role alternation is strict. The only exception is `/steer`, which adds a user row after a tool result.
- **Interrupts** (`agent/interrupt_control.py`):
  - The controls are `interrupt`, `hard_interrupt`, `steer`, and `redirect`.
  - Each sets a per-thread bit (`tools/interrupt.py`) and fans it out to tool worker threads (`:142-155`).
  - Terminal backends poll the bit too (`tools/environments/base.py:263-266`).
  - **Go mapping:** `context.Context` cancellation for interrupt, plus a steer channel for the rest.
- **Tool dispatch:**
  - The call chain is `agent/tool_executor.py` → `model_tools.handle_function_call` (`model_tools.py:855`) → `ToolRegistry`.
  - Agent-level tools such as `todo_list`, `memory`, `session_search`, and `clarify` dispatch through a table, `INLINE_TOOL_EXECUTORS` (`agent/inline_tool_executors.py:253-313`).
  - Tool batches run in parallel on a bounded pool (`tool_executor.py:244-251`).
  - Pre-dispatch guardrails (`:671`) and plugin pre/post hooks (`model_tools.py:748-851`) run around each call.
- **Streaming:**
  - Delivery to stream callbacks is `agent/stream_delivery.py`.
  - A stateful scrubber strips `<think>` and `<memory-context>` spans that are split across deltas (`conversation_loop.py:354-361`).
  - A mid-stream network cut triggers a "continue exactly" recovery (`:992-1018`).

## Providers and LLM clients

- **Transport ABC:** `agent/transports/base.py:12-30` defines `ProviderTransport` with four steps: `convert_messages` → `convert_tools` → `build_kwargs` → `normalize_response`.
  - The ABC deliberately does NOT own client construction, streaming, caching, or retries.
  - Normalized types live in `transports/types.py`.
- **API modes:** `chat_completions`, `anthropic_messages`, `codex_responses` (OpenAI Responses API), `bedrock_converse`, plus `vertex` and `codex_app_server`.
- **Anthropic:** `anthropic_adapter.py` uses `messages.stream()` and falls back to `create()` (`:814-882`).
- **Provider profiles are plugins:** 42 directories under `plugins/model-providers/` (anthropic, openrouter, nous, gemini, ollama-cloud, lmstudio, custom, …).
- **Supporting modules:**
  - Model metadata: `agent/model_metadata*.py` and `models_dev.py`.
  - Credential pool: `credential_pool*.py`.
  - Fallback and cooldown: `fallback_cooldown.py`.
  - Error classification: `error_classifier.py`.
- **Auxiliary side-LLM:** each task (compression, title, vision, …) can pin its own model under `auxiliary:` (`agent/AGENTS.md:99-105`).
- **Prompt caching** (`agent/prompt_caching.py`):
  - Anthropic `cache_control` uses 4 breakpoints: the static system prefix, the end of the system prompt, and the last 2 non-system messages. Without a static prefix it uses 1 system breakpoint plus the last 3 messages (`:1-5`).
  - The TTL is 5m or 1h (`:117`, `:164`).
  - The function is idempotent: it strips old markers before applying new ones (`:345-377`).
  - The stable prefix is tracked in `agent/prompt_cache_boundary.py`.

## Prompt builder

`agent/system_prompt.py::build_system_prompt_parts` (`:736`) returns three
tiers, joined with `"\n\n"`:

1. **Stable:**
   - `SOUL.md` or the default identity (`:550`).
   - Help guidance.
   - Tool-use guidance blocks (`:559-586`).
   - Pinned auto-load skills.
   - The coding-posture brief.
2. **Context:**
   - The caller's `system_message`.
   - Project context files: `HERMES.md`, the `AGENTS.md` chain, `CLAUDE.md`, and `.cursorrules`. They are read from the CWD with caps (`agent/prompt_builder.py:1679`).
   - The workspace snapshot.
3. **Volatile (still frozen per session):**
   - The skills index.
   - The memory snapshot.
   - Plugin sections.
   - The active-profile line.
   - A timestamp.
   - Runtime-environment hints.

The prompt is cached on the agent and rebuilt only after compression
(`:814`). `ephemeral_system_prompt` is injected at API-call time only.

## Tools

- **Registry:** `tools/registry.py`.
  - A `ToolEntry` (`:216`) holds `name, toolset, schema, handler, check_fn, requires_env, is_async, max_result_size_chars, execution_timeout, …`.
  - Tools register themselves at import time (`:691`).
  - `check_fn` answers availability and is cached (`:342`).
- **Toolsets:** `toolsets.py`. `TOOLSETS` holds per-platform `hermes-*` bundles, each the core set plus extras.
- **`_HERMES_CORE_TOOLS`** (`toolsets.py:11-40`), about 42 names:

| Group | Tools |
|---|---|
| Web | `web_search`, `web_extract` |
| Terminal | `terminal`, `process_manage` |
| Files | `read_file`, `write_file`, `patch`, `search_files` |
| Media | `vision_analyze`, `image_generate`, `text_to_speech` |
| Skills | `skills_list`, `skill_view`, `skill_manage` |
| Browser | `browser_exec`, `browser_cdp`, `browser_dialog`, `browser_vault` |
| State | `todo_list`, `memory`, `session_search` |
| Agent | `clarify`, `execute_code`, `delegate_task`, `cronjob_manage` |
| Kanban | 14 `kanban_*` tools |
| Other | `computer_use`, `manage_connections` |

- **Terminal backends:** `tools/environments/base.py::BaseEnvironment` (`:263`).
  - Subclasses implement `_run_bash()` and `cleanup()`.
  - The base owns snapshot sourcing, CWD tracking, interrupts, and timeouts.
  - Backends: `local`, `docker`, `ssh`, `modal`, `daytona`, `vercel_sandbox`, plus plugin-provided ones (`tools/terminal_tool_backends.py:182-271`).
- **Dangerous commands:**
  - `tools/approval_detection.py` has `DANGEROUS_PATTERNS` (`:228`), regex based, and `detect_dangerous_command` (`:1548`).
  - `tools/approval.py`: `check_all_command_guards` (`:1151`) and `_run_approval_gate` (`:968`).
  - Modes: yolo, per-session approve, permanent allowlist, `smart` LLM gate (`:764`), and a denial circuit breaker.
  - Unattended contexts fail closed (`:606-658`).

## State, memory, compression

- **Sessions:** `hermes_state.py` and about 60 siblings, with SQLite and FTS5. See [WIRE_FORMATS.md](WIRE_FORMATS.md).
- **Built-in memory:** `MEMORY.md` and `USER.md` as a frozen snapshot (`tools/memory_tool.py:2-5`). Provider memory plugins sit behind `agent/memory_provider.py`.
- **Compression:**
  - `agent/context_compressor.py` triggers at 50% of the context window by default, with per-model thresholds (`:2040`).
  - The algorithm prunes old tool results with no LLM call, picks boundaries, and writes a structured summary with the auxiliary model, falling back to a deterministic summary (`agent/AGENTS.md:77-92`).
  - The gateway runs its own hygiene pass at 85%.

## Extensibility

- **Skills:**
  - `SKILL.md` with frontmatter.
  - The prompt index is built by `agent/prompt_builder.py::build_skills_system_prompt` (`:1244`).
  - The tools are `skills_list`, `skill_view`, and `skill_manage`.
  - There is also a hub (`tools/skills_hub*.py`) and a curator (`agent/curator.py`).
- **MCP client:** `tools/mcp_tool.py` on the Python `mcp` SDK.
  - Transports: stdio (`:160`), streamable HTTP (`:162-164`), and SSE (`:79`).
  - Extras: sampling, elicitation, and OAuth.
- **Plugin hooks:**
  - `VALID_HOOKS` (`hermes_cli/plugins.py:108`) includes `pre_tool_call`, `post_tool_call`, `transform_tool_result`, `pre_llm_call`, `post_llm_call`, `on_stream_*`, `pre_api_request`, and more.
  - Plugins export `register(ctx)`.
- **Shell hooks:** `agent/shell_hooks.py`.
  - Input is JSON on stdin; output is optional JSON on stdout, `{decision|action: block|modify}`.
  - Exit code 2 blocks `pre_tool_call`.
  - Hooks fail open, and consent is asked per `(event, command)`.

## Interfaces

- **Gateway:**
  - `gateway/run.py` is a facade with `run_*.py` phases.
  - The platform enum is at `gateway/config.py:217-242`, with about 28 platform plugins in `plugins/platforms/`.
  - One process can serve several profiles ("multiplex").
- **Cron:**
  - The store is `cron/jobs.py`; the scheduler is `cron/scheduler.py` plus 20 siblings.
  - It is at-most-once, with a tick lock and an inactivity watchdog (`cron/AGENTS.md:22-48`).
- **OpenAI-compatible API:** the `api_server` gateway platform, `gateway/platforms/api_server.py:59-81`.
  - OpenAI-style: `/v1/chat/completions`, `/v1/responses`, `/v1/models`.
  - Async runs: `/v1/runs/*`.
  - Sessions: `/api/sessions/*`.
  - There is a profile prefix, `/p/<profile>/v1/...`.
- **TUI:** see [TUI_PROTOCOL.md](TUI_PROTOCOL.md).

## Go port mapping (proposed)

| Upstream | Go package |
|---|---|
| `hermes_constants.py`, profiles | `internal/home` |
| `hermes_cli/config*.py` | `internal/config` |
| `agent/transports/*`, adapters | `internal/llm` (`openai`, `anthropic`), consumer-side `Client` interface |
| `agent/prompt_caching.py` | `internal/llm/anthropic` (cache plan) |
| `agent/system_prompt.py` | `internal/prompt` |
| `agent/conversation_loop.py` + `turn_*` | `internal/agent` (loop and event stream) |
| `tools/registry.py`, `toolsets.py` | `internal/tools` |
| `tools/environments/` | `internal/tools/terminal` (`local`, `docker`) |
| `tools/approval*.py` | `internal/approval` |
| `hermes_state*.py` | `internal/state` |
| `tools/memory_tool*.py` | `internal/memory` |
| `agent/context_compressor.py` | `internal/compress` |
| skills | `internal/skills` |
| `tools/mcp_tool.py` | `internal/mcp` |
| `gateway/` | `internal/gateway`, `internal/gateway/telegram`, … |
| `cron/` | `internal/cron` |
| `api_server.py` | `internal/apiserver` |
| `hermes` CLI | `cmd/hermes-go` |
