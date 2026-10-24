# V1 Core

Objective: a Go agent core that reads an existing Hermes home, talks to OpenAI-compatible and Anthropic providers, builds the system prompt, and runs an interruptible agent loop behind `hermes-go chat -q`.
Branch: `dev`. Delivery: small commits pushed to `dev`; `main` only after owner approval. Test runner: `go test -race ./...`.

## Specs

- S1 Home: resolve HERMES_HOME like upstream (`HERMES_HOME` env, else `%LOCALAPPDATA%\hermes<suffix>` on Windows, else `~/.hermes<suffix>`; suffix from `HERMES_DATA_DIR_SUFFIX`). Profiles live in `<root>/profiles/<name>`, names match `^[a-z0-9][a-z0-9_-]{0,63}$`, `default` maps to the root, sticky `<root>/active_profile` applies when no explicit profile is given, a missing profile dir is an error.
- S2 Config: load `config.yaml` (utf-8-sig, top level must be a mapping), deep-merge over Go defaults (dict over dict recurses, null over a section is ignored, otherwise replace), expand `${VAR}` / `${env:VAR}` in string values only, leave unresolved and non-env refs verbatim, keep unknown keys.
- S3 Env: parse `<home>/.env` with upstream tokenizer rules; `.env` values override the process environment for Hermes lookups.
- S4 Providers: resolve provider + model + base URL + API key from config (`model`, `providers:`) and env, following the upstream auto ladder subset (explicit, config `model.provider`, OpenRouter key, first provider with a usable env key).
- S5 LLM clients: provider-neutral `Client` with streaming; OpenAI-compatible chat completions over SSE; Anthropic Messages via the official Go SDK with up to 4 `cache_control` breakpoints (system + last messages), idempotent.
- S6 Prompt: 3 tiers joined by blank lines; identity from `SOUL.md` or the upstream default identity; one project context type (`.hermes.md`/`HERMES.md`, AGENTS.md chain to git root, `CLAUDE.md`, `.cursorrules`), each capped at 20,000 chars; built once per session.
- S7 Loop: iteration budget, retry on transient errors, context cancellation (interrupt), steer messages injected between iterations, typed event stream, tool execution through a consumer-side interface.
- S8 CLI: `hermes-go chat -q "..."` streams one answer to stdout; `-p/--profile`, `-m/--model`, `--provider` flags.

Assumptions (owner did not answer OPEN_QUESTIONS yet, all reversible): binary `hermes-go`; home resolution identical to upstream, V1 reads only (`config.yaml`, `.env`, `SOUL.md`); both OpenAI-compatible and Anthropic supported; minimal CI in M1.1.

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 CI + lint config | — | inline | done | d4917f8 |
| T2 internal/home | S1 | inline | done | 379cd05 |
| T3 internal/config + env | S2, S3 | inline | done | 8a5aa92 |
| T4 internal/llm types + providers | S4 | inline | done | 7c10ab4 |
| T5 OpenAI-compatible client | S5 | inline | done | a670ea1 |
| T6 Anthropic client + caching | S5 | inline | done | e099a1f |
| T7 internal/prompt | S6 | inline | done | abf74d3 |
| T8 internal/agent loop | S7 | inline | done | 554f335 |
| T9 cmd/hermes-go chat | S8 | inline | done | 69a8205 |

Route evidence: one writer (the parent) works sequentially; units depend on each other, so no parallel writer reason applies.

## Log

- L1 (2026-10-24, owner): "Hablamos espanol aunque el prompt esté en inglés, puedes pensar en inglés, queremos hacer un rewrite completo de hermes pero en go, entonces sé que es algo grande, por lo que partiremos de lo inicial, y poco a poco iremos metiendo, igual esperemos que la comunidad crezca o algo así, entonces vas documentando absolutamente todo de lo q nos va faltando y eso, pero dale, tienes full access, creas una nueva carpeta, e inicializas repo de mi github, creas rama dev y main, y vas pusheando, puedes instalar go y lo que falte para el desarrollo."
- L2 (2026-10-24, owner): "ya hice el cd, dale continua y rifate, me voy del pc unas horas"
- L3 Decision: Anthropic uses the official `anthropic-sdk-go` (skill guidance: official SDK over raw HTTP); OpenAI-compatible uses plain `net/http` because it must speak to many non-OpenAI servers (OpenRouter, Ollama, LM Studio) with loose schemas.
- L4 Verification (2026-10-24): all nine tasks are done. Every commit passed gofmt, go vet, golangci-lint (0 issues), and `go test -race ./...`. The CLI end-to-end test drives `hermes-go chat -q` against an httptest OpenAI-compatible server inside an isolated temp `HERMES_HOME`, and checks that the system prompt carries the identity, AGENTS.md, Model, Provider, and Platform. The deliberate gaps are listed in docs/PROGRESS.md under "Known V1 gaps".
