# Wire formats to keep compatible

On-disk formats the Go port must read and write so a user can run both
implementations against the same Hermes home. All citations point at upstream
`NousResearch/hermes-agent` at commit `bda3a363e9` (2026-10-24).

Items marked **UNVERIFIED** were seen only partially; confirm them before
implementing the vertical that needs them.

## Compatibility policy

| Format | Go port stance |
|---|---|
| Hermes home resolution, profiles | Must match exactly (V1). |
| `config.yaml` | Read full file, merge with Go defaults, preserve unknown keys on save (V1). |
| `.env` | Read and write with the same tokenizer rules (V1). |
| `state.db` | Read and write the upstream schema at `SCHEMA_VERSION` 32. Never run destructive migrations (V3). |
| `memories/*.md` | Byte-compatible (V3). |
| `skills/**/SKILL.md` | Read-compatible. Write only agent-created skills (V4). |
| `cron/jobs.json` | Read and write, preserving unknown job keys (V5). |
| `auth.json`, pairing, gateway runtime files | Out of scope until the vertical that needs them. |

**Golden rule:** unknown keys and columns are preserved, never dropped. Upstream
evolves fast, so the Go port must round-trip data it does not understand.

## 1. Hermes home and profiles

- **Resolution order:** context-local override, then the `HERMES_HOME` env var, then the platform default (`hermes_constants.py:188-195`, `:237-246`).
- **Platform default** (`hermes_constants.py:51-58`):
  - Windows: `%LOCALAPPDATA%\hermes<suffix>`, falling back to `~\AppData\Local\hermes<suffix>`.
  - Everything else: `~/.hermes<suffix>`.
  - `<suffix>` is `HERMES_DATA_DIR_SUFFIX`, empty by default. **Do not hardcode `~/.hermes`.**
- **Named profile:** `<root>/profiles/<name>/`.
  - The name must match `^[a-z0-9][a-z0-9_-]{0,63}$` (`hermes_constants.py:395`, `hermes_cli/profiles.py:30`).
  - `default` maps to the root itself (`hermes_cli/profiles.py:433-437`).
- **Active profile:** stored in `<root>/active_profile` (`hermes_cli/profiles.py:296-297`). Deleted profiles leave a tombstone at `profiles/.deleted` (`hermes_constants.py:313-330`).
- **Profile skeleton dirs** (`hermes_cli/profiles.py:35`): `memories sessions skills skins logs plans workspace cron home`.
- **Per-profile state:** `state.db`, `memories/`, and `cron/` are all anchored at the profile home (`cron/jobs.py:76-90`).

## 2. `config.yaml`

- **Path:** `<home>/config.yaml` (`hermes_cli/config.py:431-433`).
- **Defaults:** `DEFAULT_CONFIG` (`hermes_cli/config_defaults.py:33`).
- **Version:** `_config_version: 52` (`config_defaults.py:2716`).
- **Load** (`config.py:2088`, `_deep_merge` at `:1487`):
  - Defaults are deep-merged with the user file, then env refs are expanded.
  - A `None` value over a dict section is ignored.
  - The file is read as `utf-8-sig` and its top level must be a mapping.
- **Save** (`config.py:2476-2520`):
  - Values equal to the defaults are stripped, and `_config_version` is always kept.
  - The write is atomic and then permissions are tightened.
- **Corrupt file:** a backup is made and the run aborts (`config.py:442-465`). `HERMES_IGNORE_USER_CONFIG=1` bypasses it.
- **Migrations:** numbered steps in `hermes_cli/config_migrations.py`; the latest is `migrate_to_52`.
  - `SUPPORT_FLOOR_VERSION = 12`. Below it, the file is untouched and defaults merge at read time.
  - New keys need no bump, because the deep-merge fills them (`hermes_cli/AGENTS.md:75-76`).
  - **Go stance:** the Go port does NOT migrate. It reads any version ≥ 12. If the stamp is older than 52, it warns and suggests running upstream `hermes` once.

### Keys the Go port needs first (V1–V3)

| Key | Line in `config_defaults.py` | Notes |
|---|---|---|
| `model` | 34 | String or `{default\|model, provider, base_url}` (`cli-config.yaml.example:72-86`). |
| `providers`, `fallback_providers`, `fallback` | 35-40 | Provider resolution. |
| `toolsets` | 42 | Default `["hermes-cli"]`. |
| `agent` | 75 | `max_turns` (null = unlimited), `gateway_timeout: 1800`. |
| `terminal` | 297 | `backend: local`, `cwd: "."`, `timeout: 180`, `env_passthrough`. |
| `tool_output` | 554 | `max_bytes: 50000`, `max_lines: 2000`, `max_line_length: 2000`. |
| `approvals`, `command_allowlist`, `security` | 1664, 1702, 1768 | Dangerous-command approval. |
| `compression` | 576 | `enabled`, `threshold: 0.50`, `target_ratio: 0.20`, `protect_last_n`. |
| `memory` | 1310 | `memory_char_limit: 2200`, `user_char_limit: 1375`, `nudge_interval: 10`. |
| `skills` | 1447 | `external_dirs`, `trusted_project_dirs`, `template_vars`, `inline_shell: false`. |
| `database` | 45 | `journal_mode: wal`. |
| `mcp_servers` | not in defaults | `{<name>: {command, args[], env{}, url?, sampling{}}}` (`cli-config.yaml.example:1597-1619`, `hermes_cli/mcp_config.py:210-242`). |
| `cron` | 1799 | `output_retention` (default 50, `cron/jobs.py:3358`). |
| `gateway`, `telegram`, `discord`, `streaming` | 2099, 1625, 1540, 2246 | V5. |

## 3. `.env`

- **Path:** `<home>/.env` (`config.py:472-474`). Holds **secrets only** (`hermes_cli/AGENTS.md:82`).
- **Tokenizer** (`agent/secret_scope.py:294-309`):
  - Values may be bare, `'single'` (no escapes), or `"double"` (only `\"` and `\\` escape).
  - Everything after the first `=` is opaque.

## 4. `state.db` (SQLite)

- **Path:** `<home>/state.db` (`hermes_state.py:180,203`).
- **Pragmas** (`hermes_state.py:806-816`): WAL with fallback to delete mode, `foreign_keys=ON`, and a `busy_timeout` per connection.
- **Versions:**
  - `SCHEMA_VERSION = 32` (`hermes_state_common.py:289`), stored in the single-row `schema_version` table.
  - `FTS_STORAGE_VERSION = 3` (`hermes_state_common.py:312`), stored in `state_meta.fts_storage_version`.
- **Healing:** upstream runs `executescript(SCHEMA_SQL)` and then additive `ALTER` statements (`_reconcile_columns`) on every writable open (`hermes_state_schema.py:947-1137`).
- **DDL sources:** `hermes_state_common.py` — `SCHEMA_SQL` at `:452-565`, runtime tables at `:586-748`, indexes and triggers at `:750-870`, FTS at `:941-1080`.

### Tables the Go port reads and writes (V3)

- **`schema_version`:** `(version INTEGER NOT NULL)`.
- **`system_prompts`:** `(hash TEXT PRIMARY KEY, prompt TEXT NOT NULL)`.
- **`sessions`:** about 70 columns.
  - Core: `id, source, user_id, model, model_config (JSON), system_prompt, system_prompt_hash, parent_session_id, started_at REAL, ended_at REAL, end_reason, title`.
  - Counters: `message_count, tool_call_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens`.
  - Context: `cwd, git_branch, profile_name, archived, pinned, transcript_revision`.
  - Every column is listed in `hermes_state_common.py:452-520`. The Go port must INSERT only the columns it knows and rely on the column defaults for the rest.
- **`messages`:**
  - Core: `id INTEGER PK AUTOINCREMENT, session_id, role, content, tool_call_id, tool_calls (JSON), tool_name, timestamp REAL, token_count, finish_reason`.
  - Reasoning: `reasoning, reasoning_content, reasoning_details`.
  - Display and lifecycle: `active, compacted, api_content, display_*, message_uid`, and more (`hermes_state_common.py:521-545`).
- **`session_model_usage`:** per-model token and cost rollup, `PRIMARY KEY (session_id, model, billing_provider, billing_base_url, billing_mode, task)`.
- **`state_meta`:** `(key TEXT PRIMARY KEY, value TEXT)`.
- Timestamps are REAL epoch seconds, and JSON columns are text.

### Tables the Go port leaves alone (gateway / runtime ledger)

`runtime_epoch, session_admissions, worker_executions, worker_receipts,
gateway_routing, gateway_hygiene_state, conversation_generations,
gateway_heartbeats, compression_locks, session_turn_leases, async_delegations`
(`hermes_state_common.py:586-748`).

### Triggers with consequences

- **`sessions_runtime_ledger_delete_guard`:** aborts a session DELETE while `session_admissions` or `worker_executions` rows reference the session.
- **`messages_transcript_revision_update`:** bumps `sessions.transcript_revision` when message content is updated.
- **`messages_message_uid_insert`:** fills `message_uid` with `lower(hex(randomblob(16)))` when it is NULL.
- **`messages_display_*`:** maintain `display_order` and `display_identity`.

The Go port relies on these triggers instead of re-implementing them.

### FTS5

- **`messages_fts`:** `fts5(content, tool_name, tool_calls, content='messages_fts_src', content_rowid='id')`. Its source view truncates tool-role content to its first 8192 chars.
- **`messages_fts_trigram`:** `tokenize='trigram'`. Its view excludes tool rows and sessions whose `source` is `cron` or `subagent`. The exact column list is **UNVERIFIED** (`hermes_state_common.py:1070-1080`).
- **`messages_fts_cjk`:** optional. It needs the loadable extension `lib/libfts5_cjk.so` (`hermes_state_fts.py:42-56,94-97`).
- **Triggers:** each FTS table is maintained by insert, delete, and update triggers, gated by `state_meta` keys `fts_rebuild_high_water` and `fts_rebuild_progress`.

**Verified for Go (2026-10-24):** `modernc.org/sqlite v1.61.0` (SQLite 3.54.1)
with `CGO_ENABLED=0` supports `fts5`, the `porter unicode61` tokenizer, and the
`trigram` tokenizer.

**Risk:** the CJK tokenizer is a loadable C extension, and the pure-Go driver
cannot load it. If an upstream DB has `messages_fts_cjk` and its triggers, every
write to `messages` fails under Go. This needs a spike in V3 (see
OPEN_QUESTIONS).

## 5. Memory

- **Files:** `<home>/memories/MEMORY.md` (agent notes) and `USER.md` (user profile) (`tools/memory_tool.py:35-37`, `tools/memory_tool_store.py:298-301`).
- **Format:** entries are joined by `"\n§\n"` (`memory_tool_store.py:25`).
  - Read as `utf-8-sig`, split on the full delimiter, strip each entry, and drop empty ones (`:660-667`).
  - Write is atomic: a `.mem_*` temp file, then rename (`:681`).
- **Limits:** counted in characters on the joined string (`:310`), configured as `memory.memory_char_limit` and `memory.user_char_limit`.
  - Over-limit writes are refused, but existing over-limit files are only warned about.
- **Prompt snapshot:** memory is frozen at session start. Mid-session writes do not change the system prompt, which keeps prompt caching intact.
- **Locking:** `fcntl` / `msvcrt` file locks (`memory_tool.py:19-28`). The lock file name is **UNVERIFIED**.
- **Persona:** `SOUL.md` lives at `<home>/SOUL.md` (`agent/context_file_sources.py:90`).

## 6. Skills

- **Layout:** `<home>/skills/[<category>/[<sub>/]]<skill>/SKILL.md`.
  - Optional support dirs: `references/ templates/ assets/ scripts/` (`agent/skill_utils.py:32`).
  - Scan depth is at most 6.
  - Excluded dirs: `.git .github .hub .archive .curator_backups .locks .venv venv node_modules site-packages __pycache__ .tox .nox .pytest_cache .mypy_cache .ruff_cache` (`agent/skill_utils.py:24-28`).
- **Extra roots:**
  - `skills.external_dirs`.
  - `skills.create_dir`.
  - `<repo>/.hermes/skills` and `<repo>/.agents/skills`, for trusted dirs only.
- **Frontmatter** (`agent/skill_utils.py:67-87`):
  - A BOM is stripped, and the file must start with `---`.
  - The closing fence is the regex `\n---\s*\n`.
  - The content is YAML; on a YAML error the parser falls back to `key: value` lines.
- **Required fields** (`tools/skill_manager_tool.py:108-190`):
  - `name`: at most 64 chars, matching `^[a-z0-9][a-z0-9._-]*$`.
  - `description`: at most 1024 chars.
  - A non-empty body; the whole file is at most 100,000 chars.
- **Optional fields:**
  - `version, author, license, platforms`.
  - `metadata.hermes.{tags, category, related_skills, config}`, also accepted at the top level as a fallback.
  - `compatibility, deps, required_environment_variables, environments`.
- **agentskills.io:** compatibility is stated in `website/docs/user-guide/features/skills.md:9`.
- **Template vars:** `${HERMES_SKILL_DIR}` and `${HERMES_SESSION_ID}`. Inline shell `` !`cmd` `` runs only when `skills.inline_shell: true`.
- **Bookkeeping files:** `skills/.bundled_manifest` (`v2 name:hash` lines), `skills/.usage.json`, and `skills/.hub/`. The Go port leaves them alone, except that it may read `.usage.json`.

## 7. Cron (`jobs.json`)

- **Path:** `<home>/cron/jobs.json` (`cron/jobs.py:89-90`).
- **Envelope:** `{"jobs": [...], "updated_at": "<ISO>"}` (`cron/jobs.py:1536-1537`).
- **Locks:** `cron/.jobs.lock` is cross-process, with a 30 s timeout. `cron/.tick.lock` guards the ticker.
- **Run output:** `cron/output/<job_id>/<YYYY-mm-dd_HH-MM-SS>.md`, pruned to `cron.output_retention` (`cron/jobs.py:3387-3394`).
- **Job fields at creation** (`cron/jobs.py:1868-1914`):

```
id (uuid4 hex[:12]), name, prompt, skills[], skill, model, provider, base_url,
script, no_agent, monitor_script, monitor_url, monitor_state, context_from,
schedule {kind: once|interval|cron, run_at | minutes | expr, display},
schedule_display, repeat {times: int|null, completed}, enabled, state,
paused_at, paused_reason, created_at (ISO tz), next_run_at, last_run_at,
last_status, last_error, last_delivery_error, last_delivery_unverified,
last_failure {at, detail}, failure_streak, deliver, origin {...},
enabled_toolsets, workdir
```

  - Optional keys written only when set: `attach_to_session`, `reasoning_effort`, `failure_deliver`, `interpreter`.
  - Runtime-added keys: `pending_slot`, `last_fire_error`, and others. **Preserve them.**
- **Schedule parsing** (`cron/jobs.py:783-845`):
  - `30m` is an interval.
  - A 5-field expression or a natural phrase is a cron schedule.
  - An ISO timestamp is a one-shot.
- **Semantics** (`cron/AGENTS.md`):
  - At-most-once: `next_run_at` is advanced before dispatch.
  - The catch-up window is half the period, clamped to 120 s – 2 h.
  - Missed one-shots get a 120 s grace period.
  - The ticker runs every 60 s.

## 8. Other files (out of scope until needed)

- **`auth.json`:** `{"version":1,"providers":{...}}`, guarded by `auth.lock` (`hermes_cli/auth.py:493,737`). The per-provider shapes are **UNVERIFIED**.
- **`.anthropic_oauth.json`:** the Anthropic OAuth grant.
- **Gateway runtime:** `gateway.pid`, `gateway.lock`, `gateway_state.json`, `processes.json` (`gateway/status.py:41,310`).
- **Pairing:** `platforms/pairing/<platform>-{pending,approved}.json` (`gateway/pairing.py:59,353-362`).
- **Logs:** rotating files under `logs/` (`hermes_logging.py:397-400`).

| File | Level | Max size | Backups |
|---|---|---|---|
| `agent.log` | INFO+ | 5 MB | 3 |
| `errors.log` | WARNING+ | 2 MB | 2 |
| `gateway.log` | INFO+ | 5 MB | 3 |

- **Other databases:** `response_store.db`, `runs_idempotency.db`, `projects.db`, `verification_evidence.db`. Their purposes are **UNVERIFIED**; `response_store.db` probably belongs to the API server.
