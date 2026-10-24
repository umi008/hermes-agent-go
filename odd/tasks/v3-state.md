# V3 State

Objective: sessions persist in an upstream-schema SQLite store with FTS5 search, the agent can recall past sessions (`session_search`) and keep durable notes (`memory`), and long conversations are compressed.
Branch: `dev`. Delivery: small commits pushed to `dev`, merged to `main` at the end of the vertical. Test runner: `go test -race ./...`.

## Specs

- S1 Store: a pure-Go SQLite store created from the exact upstream schema v32 DDL (tables, indexes, views, FTS5 and trigram tables, triggers), WAL, busy timeout. Sessions and messages are written with upstream column semantics (`message_uid`, `tool_call_uids`, counters, `ended_at`/`end_reason`, token counts). A store whose `schema_version` is not 32 opens read-only.
- S2 Search: upstream FTS5 query sanitization, `snippet(..., '>>>', '<<<', '...', 40)`, BM25 or newest/oldest order, role/source/time filters, OR-relaxed retry on a zero-hit multi-term query.
- S3 `session_search` tool with the upstream schema and its four shapes: discover, read, scroll, browse; result JSON keys in upstream order.
- S4 `memory` tool: `MEMORY.md` and `USER.md` in the Hermes home, upstream actions and limits, frozen snapshot injected into the system prompt.
- S5 Compression: when the context nears the model limit, prune old tool results, summarize the middle with the LLM, fall back deterministically when the summary fails.
- S6 CLI: `chat` records the session and its messages; tools wired.

Assumptions:
- The Go port writes its own store at `<hermes home>/hermes-go/state.db` (same schema) and never writes upstream's `state.db`. This is OPEN_QUESTIONS Owner 2: no write risk to the user's real history.
- Session lineage (compression children, branches) is not modelled: each session is its own lineage root.
- CJK bigram FTS is out. The trigram index is created but is used only for the zero-hit fallback.

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 internal/state store | S1 | inline | todo | |
| T2 search | S2 | inline | todo | |
| T3 session_search tool | S3 | inline | todo | |
| T4 memory tool | S4 | inline | todo | |
| T5 compression | S5 | inline | todo | |
| T6 CLI wiring | S6 | inline | todo | |

Route evidence: the parent writes sequentially. Every unit depends on T1, so none of them can run in parallel.

## Log

- L1 (2026-10-24, owner): "me voy a dormir, continúa trabajando por todas las fases hasta las 7am, ahí es tu límite, no preguntes o esperes algo de mi atención o respuesta hasta ese momento, incluso si llegas a la M6 ship, tienes full control y decision"
- 2026-10-24: the upstream schema was dumped from a fixture DB that upstream `SessionDB` created in a scratch dir (throwaway uv env, upstream repo untouched).
