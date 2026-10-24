# V4 Extensibility

Objective: the agent loads user skills, calls tools from MCP servers, and runs user shell hooks around tool calls, with upstream config keys and wire shapes.
Branch: `dev`. Delivery: small commits pushed to `dev`, merged to `main` at the end of the vertical. Test runner: `go test ./...` (cgo is off locally, so no `-race`).

## Specs

- S1 Skills (M4.1): "Discovery, frontmatter parse, `skills_list` / `skill_view` / `skill_manage`, prompt index". SKILL.md packages under `<hermes>/skills`, `skills.create_dir` and `skills.external_dirs`, resolved by upstream tier precedence. `skills_list` and `skill_view` return upstream JSON with keys in upstream order. The index block in the system prompt matches upstream byte for byte.
- S2 MCP client (M4.2): "stdio and streamable HTTP via the official Go MCP SDK (verify with Context7 first)". `mcp_servers` config; each server's tools register as `mcp_<server>_<tool>`.
- S3 Shell hooks (M4.3): "`hooks:` config, stdin/stdout JSON contract, exit code 2 blocks".

Assumptions:
- Skills are read-only in the Go port for now. Upstream hides `skill_manage` in single-query (`-q`) sessions (ONESHOT_HIDDEN_TOOLS), and every Go chat is single-query, so `skill_manage` waits for the interactive CLI in V5.
- No project-tier skills (they need upstream's trust gating), no plugin skills, no inline shell, no dependency installs, no usage counters, no credential files.

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 internal/skills: discovery, list, view, index | S1 | inline | done | c58e504 |
| T2 CLI skills wiring | S1 | inline | done | 8d53c4b |
| T3 skill_manage | S1 | deferred to V5 | pending | |
| T4 MCP client | S2 | inline | pending | |
| T5 shell hooks | S3 | inline | pending | |

Route evidence: the parent writes sequentially. Units are small, and the CLI wiring depends on each package.

## Log

- L1 (2026-10-24, owner): "me voy a dormir, continúa trabajando por todas las fases hasta las 7am, ahí es tu límite, no preguntes o esperes algo de mi atención o respuesta hasta ese momento, incluso si llegas a la M6 ship, tienes full control y decision"
- 2026-10-24: T1 is verified against testdata/upstream.json. The fixture was dumped from upstream `skills_list`, `skill_view` and `build_skills_system_prompt` over a copy of testdata/home, on Windows, in a scratch uv env (upstream repo untouched). It covers 3 list cases, 21 view cases and 3 index variants. A mutation of the fixture fails the test.
- 2026-10-24: T3 deferred: `skill_manage` is hidden in one-shot runs upstream, and the Go CLI only has one-shot runs.
