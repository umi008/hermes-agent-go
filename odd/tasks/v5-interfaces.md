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
| T4 skill_manage | S7 | worker | in progress | |
| T5 API server | S2 | inline | pending | |
| T6 cron | S3 | inline | pending | |
| T7 gateway core + Telegram | S4 | inline | pending | |
| T8 Discord | S5 | inline | pending | |
| T9 TUI backend | S6 | decision pending | pending | |

Route evidence: the parent writes sequentially; each unit depends on the chat runtime refactor in T3.

## Log

- L1 (2026-10-24, owner): "me voy a dormir, continúa trabajando por todas las fases hasta las 7am, ahí es tu límite, no preguntes o esperes algo de mi atención o respuesta hasta ese momento, incluso si llegas a la M6 ship, tienes full control y decision"
- L2 (2026-10-24, owner): "continua"
- 2026-10-24: T1 ports upstream `rewind_in_transaction` (soft archive with `active = 0`, `rewind_count + 1`, counters recomputed from active rows) and `delete_session_if_empty`. Compaction carriers and turn leases are not ported; the Go store has neither.
- 2026-10-24: T3 splits `runChat` into a `chatRuntime` (config, endpoint, tools, store) and per-session state (recorder, agent, prompt). /new ends the session with `new_session` and rebuilds the prompt; exit ends it with `cli_close`; a session that saved nothing is dropped. Tools and hooks read the live session id. Ctrl-C reaches the CLI as `Env.Interrupt`: it cancels the running turn (the partial turn stays in history, like upstream) and exits when idle; in `-q` mode it cancels the query. Tests drive the REPL through stdin with a fake model, including an interrupt over a hanging request and clarify answers by number, free text, skip and multi-select.
