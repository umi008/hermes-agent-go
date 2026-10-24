# V2 Tools

Objective: the agent acts through tools — registry, file tools, local terminal with background processes, dangerous-command approval, todo/clarify, web via Tavily, and a Docker terminal backend — wired into `hermes-go chat`.
Branch: `dev`. Delivery: small commits pushed to `dev`; `main` only after owner approval. Test runner: `go test -race ./...`.

## Specs

- S1 Registry: tools register name, toolset, JSON schema, handler, and an availability check; unknown tool → `{"error":"Unknown tool: X"}`; handler failure → `{"error":"Tool execution failed: ..."}`; error text capped at 2048 chars + `"… [truncated]"`; results over 100,000 chars are cut (read_file exempt); calls in one turn run in parallel.
- S2 File tools (upstream contracts): `read_file` (`N|content` lines, offset 1-indexed default 1, limit default/max 2000, long lines cut at 2000 + `... [truncated]`, 100,000-char budget with `next_offset`), `write_file` (creates parents; refuses to overwrite an existing file not read in this session or changed since), `patch` (`old_string`/`new_string`/`replace_all`, fuzzy strategies subset, unified diff in result), `search_files` (content/files targets, glob, limit/offset).
- S3 Terminal: `terminal` runs a command in the local shell, timeout default 180 s, foreground max 600 s, cwd persists between calls, `workdir` applies to one call, output capped at 50,000 chars (40 % head / 60 % tail marker), ANSI stripped; `background: true` starts a process managed by `process_manage` (list, poll, log, wait, kill, write, submit).
- S4 Approval: hardline patterns always blocked; dangerous patterns ported from upstream (RE2-compatible subset) require approval; `command_allowlist` from config approves; one-shot `-q` mode denies by default (upstream `single_query_mode`).
- S5 `todo_list` and `clarify` with upstream schemas and result shapes; `clarify` without an interactive user returns a tool error.
- S6 Web: `web_search` / `web_extract` through Tavily (`TAVILY_API_KEY`); unavailable without the key.
- S7 Docker backend: `terminal` can run inside a long-lived container instead of the host.
- S8 CLI: `hermes-go chat` exposes the tools to the model; toolsets selectable.

Assumptions: Tavily is the single web backend; patch fuzzy strategies beyond exact/line-trimmed/whitespace-normalized/indentation-flexible are later; results over the cap are truncated instead of persisted to disk; `search_files` is pure Go (no ripgrep, no .gitignore parsing in V2).

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 internal/tools registry | S1 | inline | done | af4bcfb |
| T2 file tools | S2 | inline | done | dcd0679 |
| T3 terminal + process_manage | S3 | inline | done | c5a21b7 |
| T4 approval | S4 | inline | done | 282427b |
| T5 todo_list + clarify | S5 | inline | done | f780850 |
| T6 web (Tavily) | S6 | inline | done | a8e0797 |
| T7 docker backend | S7 | inline | done | 18fb19c |
| T8 CLI wiring | S8 | inline | done | c3ea22f |

Route evidence: the parent writes sequentially; units are independent but small enough that inline is cheaper than starting writers.

## Log

- L1 (2026-10-24, owner): "si" (approving V1 and the start of V2 Tools).
- L2 Upstream map from a read-only explorer: registry `tools/registry.py:691`, error shape `:1057`, caps `tools/budget_config.py:10-20`, file tools `tools/file_tools.py:1151-1428`, terminal `tools/terminal_tool.py:1580-1721`, process `process_registry.py:2574-2623`, approval `tools/approval_detection.py:107-482`, `tools/approval.py:1151-1194`, todo `tools/todo_tool.py:229-336`, clarify `tools/clarify_tool.py:136-200`, web Tavily `plugins/web/tavily/provider.py`.
- 2026-10-24: T1-T8 done; gates and CI green (docker integration test runs on ubuntu CI). V2 merged to main.
