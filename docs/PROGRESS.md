# Progress

This file is the single resume point. Read it first on every session start.

## Project

**hermes-agent-go** is a scoped Go port of
[NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent), built
as a learning and portfolio project, not as a replacement. It keeps on-disk
compatibility with the Hermes home where practical.

- **Upstream reference:** commit `bda3a363e9` (2026-10-24). The read-only clone
  is at `C:\Users\PC\Desktop\codigo\hermes-agent`.
- **This repo:** `C:\Users\PC\Desktop\codigo\hermes-agent-go`, published on
  GitHub as `umi008/hermes-agent-go`.
- **Branches:**
  - `main` is stable and holds approved verticals only.
  - `dev` is the integration branch. Work lands here.

## Workflow

- Each vertical runs spec → tests → implementation → verification, then stops
  for the owner's approval.
- Gates on every change: `gofmt`, `go vet`, `golangci-lint run`, and
  `go test -race ./...`.
- Commits follow Conventional Commits and stay small.

## Toolchain (verified 2026-10-24)

- Go 1.27.0 on windows/amd64.
- golangci-lint 2.15.0.
- `modernc.org/sqlite` v1.61.0 (SQLite 3.54.1) works with `CGO_ENABLED=0`,
  including FTS5 with the `porter unicode61` and `trigram` tokenizers.

## Done

- **V0 Discovery (2026-10-24).** These docs:
  - [ARCHITECTURE.md](ARCHITECTURE.md)
  - [PARITY.md](PARITY.md)
  - [WIRE_FORMATS.md](WIRE_FORMATS.md)
  - [TUI_PROTOCOL.md](TUI_PROTOCOL.md)
  - [ROADMAP.md](ROADMAP.md)
  - [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md)

- **V1 Core (2026-10-24, merged to `main`).** Feature doc:
  [odd/tasks/v1-core.md](../odd/tasks/v1-core.md).
  - `internal/home`: home and profile resolution.
  - `internal/config`: `config.yaml` deep merge, `${VAR}` refs, and the `.env` overlay.
  - `internal/provider`: the provider auto ladder and named `providers:` entries.
  - `internal/llm`: neutral types, plus the `<think>` scrubber.
  - `internal/llm/openai`: an OpenAI-compatible SSE client.
  - `internal/llm/anthropic`: the official SDK, with 4 cache breakpoints.
  - `internal/prompt`: 3 tiers, `SOUL.md`, context files, and truncation.
  - `internal/agent`: the loop, with retries, steering, and parallel tools.
  - `cmd/hermes-go`: `hermes-go chat -q "..." [-p profile] [-m model] [--provider name] [-v]`.

- **V2 Tools (2026-10-24, merged to `main`).** Feature doc:
  [odd/tasks/v2-tools.md](../odd/tasks/v2-tools.md).
  - `internal/tools`: registry, toolsets, availability checks, upstream result caps and error shape.
  - `internal/tools/files`: `read_file`, `write_file`, `patch`, `search_files`, with stale-read guards.
  - `internal/tools/terminal`: `terminal` with persistent cwd and env, `process_manage`, local and Docker backends.
  - `internal/tools/approval`: hardline and dangerous-command detection (upstream patterns via `regexp2`), the approval gate.
  - `internal/tools/todo`, `internal/tools/clarify`, `internal/tools/web` (Tavily).
  - `cmd/hermes-go`: tools wired into `chat`, `--toolsets` filter, `-v` logs tool calls.

- **V3 State (2026-10-24, merged to `main`).** Feature doc:
  [odd/tasks/v3-state.md](../odd/tasks/v3-state.md).
  - `internal/state`: a pure-Go SQLite store built from the upstream schema v32 DDL, with FTS5 search, upstream query sanitization and snippets.
  - `internal/tools/sessionsearch`: `session_search` with discover, read, scroll and browse shapes.
  - `internal/tools/memory`: `memory` over `MEMORY.md` and `USER.md`, upstream limits and errors, a frozen snapshot in the prompt.
  - `internal/compress`: threshold, lean tail, tool-result pruning, LLM summary, deterministic fallback; the agent compacts before a call, at most 3 times per run.
  - `cmd/hermes-go`: `chat` records sessions and messages, injects memory and tool guidance, reads `compression.*` and `model.context_length`.

- **V4 Extensibility (2026-10-24, merged to `main`).** Feature doc:
  [odd/tasks/v4-extensibility.md](../odd/tasks/v4-extensibility.md).
  - `internal/skills`: SKILL.md discovery under `<hermes>/skills` and `skills.external_dirs`, `skills_list`, `skill_view`, and the prompt index, matched to upstream output on a fixture.
  - `internal/tools/mcpclient`: MCP over stdio, streamable HTTP and SSE on the official Go SDK; tools register as `mcp__<server>__<tool>` in toolset `mcp-<server>`, with upstream schema normalization, include/exclude filters, timeouts, a safe stdio env and credential redaction.
  - `internal/hooks`: `pre_tool_call` and `post_tool_call` shell hooks with the upstream stdin JSON, block/modify/approve directives, exit code 2, `fail_closed`, matchers and timeouts; outcomes match 100 upstream verdicts.
  - `cmd/hermes-go`: `skills.*`, `mcp_servers` and `hooks:` config; `chat --accept-hooks`.

- **V5 Interfaces (in progress).** Feature doc:
  [odd/tasks/v5-interfaces.md](../odd/tasks/v5-interfaces.md).
  - M5.1 interactive CLI: `hermes-go chat` without `-q` is a REPL with a slash-command subset; Ctrl-C cancels the running turn and exits when idle; `clarify` and dangerous-command approval are answered from the terminal; `/retry` and `/undo` soft-archive turns in the store like upstream.
  - Session seam: one `chatRuntime` per process serves many `chatSession`s, each with its own id, recorder, agent, output sink and hooks. The API server, cron and the gateways open one session per request, job or chat, and the REPL keeps its own in `rt.cur`.
  - `internal/gateway/adapter.go` is the platform contract: adapters speak their wire protocol (Telegram long polling, Discord Gateway WebSocket), the core owns sessions, per-chat queues and approvals inside the chat.
  - `skill_manage`: create, patch (targeted or whole-file), write_file, remove_file and delete, applied as atomic `operations` batches and registered only for interactive sessions, as upstream.
  - M5.2 API server: `hermes-go serve` answers `/v1/models` and `/v1/chat/completions`, streaming and not, behind a bearer token from `api_server.api_key` or `HERMES_API_KEY`, with one session per request.
  - M5.3 cron: `hermes-go cron` round-trips `jobs.json` preserving keys the port does not model, parses interval, 5-field cron and one-shot schedules, ticks behind `cron/.tick.lock` with at-most-once dispatch, and writes and prunes `cron/output/`.
  - M5.4 and M5.5 gateways: `hermes-go gateway` serves Telegram (long polling) and Discord (Gateway WebSocket), one session per chat, a per-chat queue, chunked replies and approvals answered in the chat.

- **V6 Packaging (2026-10-24, on `dev`).**
  - `hermes-go version` (and `--version`) prints the version injected with `-X main.version`; development builds say `dev`.
  - `.goreleaser.yaml`: static binaries for linux, darwin and windows on amd64 and arm64, tar.gz (zip on Windows) with LICENSE, NOTICE and README, `checksums.txt`. Checked with `goreleaser check` and a local `release --snapshot`.
  - `.github/workflows/release.yml`: a `v*` tag runs the tests, then GoReleaser, and publishes a GitHub release.
  - `Dockerfile`: distroless static, nonroot, home in the `/data` volume; CI builds it and smoke-tests `version`, `cron list` and `cron add`.

## Known V6 gaps (deliberate, tracked)

- No container image is published; the image is built in CI only. No Homebrew tap, Scoop bucket, signing or SBOM.
- The distroless image has no shell, so the local `terminal` backend fails inside it.
- `serve` in a container must listen on `0.0.0.0`, which requires an API key.

## Known V5 gaps (deliberate, tracked)

- Interactive CLI: plain line input (no prompt_toolkit editing, completion, history file, spinner or status bar), no /undo prefill, lines typed during a turn queue for after it. /model only shows the model. The approval menu has no [a]lways because the port never writes config.yaml; session approvals survive /new. Hook `approve` directives still follow config, not a prompt.
- `/retry` and `/undo` port the plain rewind (soft archive with `active = 0`); no compaction carriers, turn leases or memory-provider notifications.
- API server: no `/v1/responses`, `/v1/runs` or `/api/sessions`, no conversation resume (one session per request), and usage is reported only on the non-streaming path.
- Cron: no delivery targets, quotas, dependency chains, `context_from` or thread handoff; one job runs at a time, and a job never resumes a conversation.
- Gateway: no pairing flow, delivery ledger, rooms or webhooks; approvals are text menus rather than inline keyboards; no media, voice or topics. A chat session lives as long as the process.
- Server sessions leave `${HERMES_SESSION_ID}` and the memory tool's current session empty: those read one current session per runtime, which only the REPL sets.

## Known V4 gaps (deliberate, tracked)

- Skills: `skill_manage` writes them from interactive sessions (V5); `chat -q` still sees them read-only, as upstream. No project-tier or plugin skills, no `platform_disabled`, no inline shell, no usage counters.
- MCP: no resources or prompts utility tools, no reconnect or backoff, no OAuth, no sampling, no const-union schema collapse, no server-name toolset alias. Images and audio become a text notice instead of a cached file. Globs use `path.Match`, which differs from `fnmatch` on `/`.
- Hooks: only `pre_tool_call` and `post_tool_call` fire; other valid events are logged and skipped. No TTY consent prompt and no allowlist writes: a hook runs with `--accept-hooks`, `HERMES_ACCEPT_HOOKS`, `hooks_auto_accept`, or an approval already in upstream's `shell-hooks-allowlist.json`. An `approve` directive passes only when approvals are off or `single_query_mode: approve`. Matchers use RE2, not Python `re`. A timeout kills the hook process, not its process tree. No `hermes hooks` subcommands.

## Known V3 gaps (deliberate, tracked)

- The Go port writes `<hermes>/hermes-go/state.db` and `<hermes>/hermes-go/memories`, never upstream's files. Upstream sessions are not searchable yet.
- No session lineage (compression children, branches); each session is its own root. Compaction summaries are not persisted.
- `session_search`: no cross-profile reads, no title match, no CJK, ASCII `\w` in the sanitizer.
- `memory`: no threat scan, no cross-process file lock, no per-turn consolidation cap, no write approval gate.
- Compression: no model-metadata context lookup (config or 256K default), no lean session log or anchors, no secret redaction, no per-tool digest formatters, no image retirement, no cooldowns, `compression.max_attempts` is fixed at 3, the summary uses the main model.

## Known V2 gaps (deliberate, tracked)

- No grep-safe variant of `search_files` output and no secret-URL refusal in `web_extract`.
- No smart (LLM) approval and no gateway approvals; `chat -q` has no prompter (the interactive CLI asks), so dangerous commands are blocked unless `approvals.single_query_mode: approve` or `approvals.mode: off`.
- `patch` ports exact and whitespace-tolerant matching only; upstream's further fuzzy strategies are not ported. No `.gitignore` filtering in `search_files`.
- Docker: killing `docker exec` does not stop the process inside the container; the container is removed on exit.
- Command deobfuscation is limited to upstream's normalization; no parser-limit block.
- `clarify` returns "not available" in `chat -q`; the interactive CLI answers it.

## Known V1 gaps (deliberate, tracked)

- `fallback_providers` is not wired. Only the primary provider is used.
- There is no Retry-After honoring. Backoff is upstream's jittered exponential (5s base, 120s cap).
- There is no `max_tokens` continuation. A truncated reply is returned as is.
- The timestamp line covers the same-day form only. The "Today's date" rebuild line and the Session ID trailer are not ported.
- Context-file prompt-injection scanning (`_scan_context_content`) is not ported.
- The dynamic context-file cap (6% of the context window) is not ported. Only the fixed 20,000 chars, or `context_file_max_chars`, applies.
- Tool guidance, help guidance, skills index, memory, and runtime hints in the prompt arrive with V2–V4.

## Next

- V5 Interfaces: complete except the TUI backend (T9), whose decision is still open (see [TUI_PROTOCOL.md](TUI_PROTOCOL.md)).
- Merge `dev` into `main` and tag the first release once the owner approves.

## Local dev notes

- `go test -race` needs cgo. On Windows, WinLibs MinGW is installed (`winget install BrechtSanders.WinLibs.POSIX.UCRT`). Run with `CGO_ENABLED=1` and its `mingw64/bin` on PATH. Release builds stay `CGO_ENABLED=0`.

## Decisions

| Date | Decision | Why |
|---|---|---|
| 2026-10-24 | Pure-Go SQLite (`modernc.org/sqlite`) | Gives a CGO-free single binary. FTS5 and trigram are verified. |
| 2026-10-24 | No config migrations in Go; read `_config_version` ≥ 12 and warn when the stamp is old | Upstream owns migrations, so duplicating them risks divergence. |
| 2026-10-24 | Preserve unknown YAML keys, JSON keys, and DB columns on every write | Upstream evolves fast, and dropping data the Go port does not know corrupts a shared home. |
| 2026-10-24 | Python plugin system is out of scope; extension is via skills, MCP, and shell hooks | No Python runtime in Go. |
| 2026-10-24 | Anthropic via official `anthropic-sdk-go` (v1.84.0); OpenAI-compatible via plain `net/http` | Official SDK where one exists; the OpenAI-compatible path must tolerate loose schemas from OpenRouter, Ollama, and LM Studio. |
| 2026-10-24 | SDK retries disabled; the agent loop owns retry policy | One retry policy for every provider, as upstream (`max_retries=0`). |
| 2026-10-24 | An explicit base URL with no provider resolves to `custom` | Upstream's default (OpenRouter) would send a local server's traffic to the wrong key. This deviates from upstream. |
| 2026-10-24 | Approval patterns run on `github.com/dlclark/regexp2` with a 250 ms timeout that fails closed | 18 upstream patterns need lookarounds or possessive quantifiers RE2 lacks; parity is checked against 116 upstream verdicts. |
| 2026-10-24 | Web tools use Tavily only | One backend keeps V2 small; others are plugins upstream. |
| 2026-10-24 | V3 writes its own `hermes-go/state.db` and `hermes-go/memories` with the upstream schema and formats | No write risk to the user's real history while the port is young; the paths live in one place (`statePath`, `openMemory`) and can move later. |
| 2026-10-24 | MCP client on the official `github.com/modelcontextprotocol/go-sdk` (v1.8.0) | Official SDK; it ships stdio, streamable HTTP and SSE client transports. |
| 2026-10-24 | Shell hooks never prompt or write the allowlist | `chat -q` has no TTY to ask, and the allowlist is upstream's file in the user's home. |
| 2026-10-24 | V1 adopted the OPEN_QUESTIONS recommendations as assumptions: binary `hermes-go`, the same home resolution as upstream (read-only in V1), both provider families, CI in M1.1 | The owner was away. Every one of these is reversible. |
| 2026-10-24 | One `chatRuntime` per process, one `chatSession` per conversation; hooks wrap the shared registry per session | The API server, cron and the gateways run many conversations over one store and one tool registry, and a hook payload must name the session that called the tool. The REPL keeps `rt.cur` and its behavior is unchanged. |
| 2026-10-24 | `internal/gateway/adapter.go` is the platform seam: adapters speak their wire protocol, the core owns sessions, queues and approvals | Telegram and Discord stay free of store and prompt code, and each adapter can land without touching the core. |
| 2026-10-24 | `skill_manage` ports the advertised `operations` array with atomic rollback, minus the staged write gate, audit ledger, linter and curator guard | Those parts depend on Python machinery (staged approvals, a JSONL ledger, skill sync) the port does not have; the contract the model sees is preserved. |

## Open questions

See [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md).
