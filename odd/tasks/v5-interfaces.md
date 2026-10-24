# V5 Interfaces

Objective: the agent is reachable beyond one-shot `chat -q`: an interactive CLI, an OpenAI-compatible API server, cron jobs and chat gateways, with upstream wire shapes.
Branch: `dev`. Delivery: small commits pushed to `dev`, merged to `main` at the end of the vertical. Test runner: `go test ./...` (cgo is off locally, so no `-race`).

## Specs

- S1 Interactive CLI (M5.1): "REPL, slash-command subset, interrupt with Ctrl-C". Includes `clarify` answered from the terminal.
- S2 API server (M5.2): "`/v1/chat/completions` (streaming) and `/v1/models`".
- S3 Cron (M5.3): "`jobs.json` round-trip, scheduler (at-most-once, tick lock), output files".
- S4 Gateway + Telegram (M5.4): "Gateway core (sessions per chat, approvals over chat); Telegram adapter".
- S5 Discord (M5.5): "Discord adapter".
- S6 TUI backend (M5.6): "`hermes-gateway-v1` WebSocket server for upstream `ui-tui` (decision pending, see TUI_PROTOCOL)".
- S7 `skill_manage`, deferred from V4 T3: upstream hides it in single-query sessions, so it lands with the interactive CLI. Scope: toolset `skills`; the advertised call shape is one `operations` array, and the legacy flat single-op shape is also accepted. Ops: `create`, `patch` (`old_string`/`new_string`/`replace_all`, optional `file_path`), `patch` with `content` (whole-file rewrite), `write_file`, `remove_file`, `delete` (sole op only). Batches apply in order and roll back every touched file when one op fails. Skipped deliberately: the staged write-approval gate, the JSONL audit ledger, the background-review preflight, the curator `absorbed_into` guard, the skill linter and `skills_sync`; no project tier, no plugin skills.

Assumptions:
- The slash-command subset is /new (/reset), /clear, /history, /retry, /undo [N], /title, /status, /model (show only), /tools, /toolsets, /skills, /help, /quit (/exit). Text follows upstream `locales/en.yaml` where the Go port has the same behavior.
- No prompt_toolkit features: no line editing, completion, history file, prefill after /undo, or busy-input modes. Lines typed during a turn queue for after it.
- One chat runtime process shares config, endpoint, tools and the store; each session owns its agent, recorder and output sink. The API server, cron and gateway verticals reuse that seam instead of building their own agent.

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 state rewind + empty-session drop | S1 | inline | done | 335726e |
| T2 agent Reset/Rewind | S1 | inline | done | 1a381e7 |
| T3 interactive CLI: REPL, slash commands, Ctrl-C, clarify | S1 | inline | done | 1a73d7a |
| T4 skill_manage | S7 | worker, finished inline | done | see Log |
| T5 API server | S2 | worker, finished inline | done | see Log |
| T6 cron | S3 | worker, finished inline | done | see Log |
| T7 gateway core + Telegram | S4 | worker, finished inline | done | see Log |
| T8 Discord | S5 | worker, finished inline | done | see Log |
| T9 TUI backend | S6 | inline | in progress | |

Route evidence: the parent owns the shared seams (`cmd/hermes-go/chat_session.go`, `internal/gateway/adapter.go`, the `run.go` dispatch and the docs) and writes them inline. T4-T8 run in parallel as workers on disjoint surfaces: T4 `internal/skills` + the tools registration, T5 `internal/apiserver` + `serve.go`, T6 `internal/cron` + `cron.go`, T7 `internal/gateway` (except `adapter.go`) + `internal/telegram` + `gateway.go`, T8 `internal/discord`. No worker edits `run.go`; each reports the dispatch snippet the parent pastes. All five workers died on the same model error; the parent finished their partial work inline in small steps instead of relaunching in a loop.

## Log

- L1 (2026-10-24, owner): "me voy a dormir, continúa trabajando por todas las fases hasta las 7am, ahí es tu límite, no preguntes o esperes algo de mi atención o respuesta hasta ese momento, incluso si llegas a la M6 ship, tienes full control y decision"
- L2 (2026-10-24, owner): "continua"
- 2026-10-24: T1 ports upstream `rewind_in_transaction` (soft archive with `active = 0`, `rewind_count + 1`, counters recomputed from active rows) and `delete_session_if_empty`. Compaction carriers and turn leases are not ported; the Go store has neither.
- 2026-10-24: T3 splits `runChat` into a `chatRuntime` (config, endpoint, tools, store) and per-session state (recorder, agent, prompt). /new ends the session with `new_session` and rebuilds the prompt; exit ends it with `cli_close`; a session that saved nothing is dropped. Tools and hooks read the live session id. Ctrl-C reaches the CLI as `Env.Interrupt`: it cancels the running turn (the partial turn stays in history, like upstream) and exits when idle; in `-q` mode it cancels the query. Tests drive the REPL through stdin with a fake model, including an interrupt over a hanging request and clarify answers by number, free text, skip and multi-select.
- 2026-10-24: session seam (`1e3f85d`): one `chatRuntime`, many `chatSession`s with their own hooks wrapper. The independent verifier found a real race on `state.Store.lastTS`, made reachable by the seam; it is now guarded by a mutex with a concurrent test.
- 2026-10-24: cron at-most-once: the worker persisted the tick instant as `next_run_at` before dispatch, so a crash mid-run replayed the job. Fixed to upstream's semantics: persist the next occurrence after the tick before firing.
- 2026-10-24: `gateway --platform discord` was missing although `internal/discord` was done; wired with a test over the platform selection and its token errors. README gained a Usage section.
- 2026-10-24: verification: `go test ./...` green, `go vet` clean, `golangci-lint` 0 issues; smoke of `serve -h`, `cron -h|list|add`, `gateway -h` against a scratch `HERMES_HOME`. Telegram and Discord were not exercised against live APIs (no tokens); their tests use fake servers.
- L3 (2026-10-24, owner, on the T9 options A/B/C): "\"A\" creo, lo q sea más parecido a hermes original, recuerda q así es el. plan"
- 2026-10-24: T9 takes option A: `hermes-go` serves `/api/ws` on loopback and the upstream Ink TUI attaches through `HERMES_TUI_GATEWAY_URL`, unmodified. Fidelity to upstream wins over a smaller surface.
