# Roadmap

Proposed milestone order for verticals V1–V6. Each milestone runs
spec → tests → implementation → verification, and ends with the owner's
approval before the next one starts.

Effort is measured in **sessions**: focused working sessions on a Claude Pro
plan, roughly 1–2 hours each. These are rough guesses, not commitments.

## Order and rationale

Each milestone builds on the previous one, so a working agent appears as early
as possible:

1. Talk to an LLM.
2. Act through tools.
3. Remember.
4. Extend.
5. Expose through more interfaces.
6. Ship.

| # | Milestone | Content | Depends on | Effort |
|---|---|---|---|---|
| M1.1 | Home + config | `internal/home` (resolution, profiles); `internal/config` (YAML deep-merge, unknown-key preservation, `.env`) | — | 1–2 |
| M1.2 | LLM clients | `internal/llm` (provider-neutral types and `Client` interface); OpenAI-compatible client with SSE streaming; Anthropic client with streaming and `cache_control` plan; recorded-fixture tests | M1.1 | 3–4 |
| M1.3 | Prompt builder | 3-tier system prompt; `SOUL.md`; context files (`AGENTS.md`, `HERMES.md`, `CLAUDE.md`); byte-stable within a session | M1.1 | 1–2 |
| M1.4 | Agent loop + CLI one-shot | Loop with iteration budget, retries, `context` cancellation, steer channel, typed event stream; `hermes-go chat -q "..."` | M1.2, M1.3 | 2–3 |
| M2.1 | Tool registry | `Tool` interface, toolsets, availability checks, result-size caps, parallel batch execution | M1.4 | 1–2 |
| M2.2 | File + terminal tools | `read_file`, `write_file`, `patch`, `search_files`; local `terminal` with timeout, CWD tracking, and interrupt | M2.1 | 2–3 |
| M2.3 | Approval | Dangerous-pattern detection ported from upstream; per-session and permanent allowlist; fail-closed when unattended | M2.2 | 1–2 |
| M2.4 | Web + Docker | `web_search` / `web_extract` (one backend); Docker terminal backend | M2.2 | 2 |
| M3.1 | Sessions DB | `state.db` read/write at schema v32 against a fixture DB created by upstream; FTS5 search; `session_search` | M1.4 | 3 |
| M3.2 | Memory | `MEMORY.md` / `USER.md` store with frozen snapshot; `memory` tool | M3.1 | 1 |
| M3.3 | Compression | Tool-result pruning, LLM summary, deterministic fallback, prompt rebuild only on compression | M3.1 | 2 |
| M4.1 | Skills | Discovery, frontmatter parse, `skills_list` / `skill_view` / `skill_manage`, prompt index | M2.1 | 2 |
| M4.2 | MCP client | stdio and streamable HTTP via the official Go MCP SDK (verify with Context7 first) | M2.1 | 2 |
| M4.3 | Shell hooks | `hooks:` config, stdin/stdout JSON contract, exit code 2 blocks | M2.1 | 1 |
| M5.1 | Interactive CLI | REPL, slash-command subset, interrupt with Ctrl-C | M3.x | 2 |
| M5.2 | API server | `/v1/chat/completions` (streaming) and `/v1/models` | M3.1 | 2 |
| M5.3 | Cron | `jobs.json` round-trip, scheduler (at-most-once, tick lock), output files | M3.1 | 2 |
| M5.4 | Gateway + Telegram | Gateway core (sessions per chat, approvals over chat); Telegram adapter | M3.x | 3 |
| M5.5 | Discord | Discord adapter | M5.4 | 1–2 |
| M5.6 | TUI backend | `hermes-gateway-v1` WebSocket server for upstream `ui-tui` (decision pending, see TUI_PROTOCOL) | M5.4 | 3–4 |
| M6 | Packaging | Dockerfile (distroless static), goreleaser, GitHub Actions CI (lint, race tests, release) | any | 1–2 |

**Total:** about 37–48 sessions.

M6 CI is cheap and protects every later milestone, so it is worth pulling a
minimal CI workflow (gofmt, vet, golangci-lint, `go test -race`) forward into
M1.1.
