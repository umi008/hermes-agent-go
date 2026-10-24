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

## Known V2 gaps (deliberate, tracked)

- No grep-safe variant of `search_files` output and no secret-URL refusal in `web_extract`.
- No smart (LLM) approval and no gateway approvals; `chat -q` has no prompter, so dangerous commands are blocked unless `approvals.single_query_mode: approve` or `approvals.mode: off`.
- `patch` ports exact and whitespace-tolerant matching only; upstream's further fuzzy strategies are not ported. No `.gitignore` filtering in `search_files`.
- Docker: killing `docker exec` does not stop the process inside the container; the container is removed on exit.
- Command deobfuscation is limited to upstream's normalization; no parser-limit block.
- `clarify` returns "not available" until the interactive CLI (V5).

## Known V1 gaps (deliberate, tracked)

- `fallback_providers` is not wired. Only the primary provider is used.
- There is no Retry-After honoring. Backoff is upstream's jittered exponential (5s base, 120s cap).
- There is no `max_tokens` continuation. A truncated reply is returned as is.
- The timestamp line covers the same-day form only. The "Today's date" rebuild line and the Session ID trailer are not ported.
- Context-file prompt-injection scanning (`_scan_context_content`) is not ported.
- The dynamic context-file cap (6% of the context window) is not ported. Only the fixed 20,000 chars, or `context_file_max_chars`, applies.
- Tool guidance, help guidance, skills index, memory, and runtime hints in the prompt arrive with V2–V4.

## Next

- V3 State (see ROADMAP.md).

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
| 2026-10-24 | V1 adopted the OPEN_QUESTIONS recommendations as assumptions: binary `hermes-go`, the same home resolution as upstream (read-only in V1), both provider families, CI in M1.1 | The owner was away. Every one of these is reversible. |

## Open questions

See [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md).
