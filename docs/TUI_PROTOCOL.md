# TUI protocol: can the Go port reuse `ui-tui` unchanged?

**Short answer:** not strictly unchanged. The wire protocol is reusable, but the
launch path is tied to Python. Either a bootstrap shim or a small TypeScript
patch is needed (verified 2026-10-24 at upstream `bda3a363e9`).

## Verified facts

- **JSON-RPC 2.0.**
  - Responses carry `"jsonrpc": "2.0"` (`tui_gateway/server.py:898,903`).
  - Events are notifications of the form `{"jsonrpc":"2.0","method":"event","params":{"type","payload"}}` (`server.py:720`, `tui_gateway/entry.py:342`).
  - Error codes:
    - `-32700`: parse error (`entry.py:371`).
    - `-32000`: handler error (`entry.py:388`).
    - `4000`/`4001`: app errors (`tui_gateway/AGENTS.md:39-52`).
- **Two framings carry the same messages.**
  - **stdio:** newline-delimited JSON (`entry.py:350-390`). The first message is `gateway.ready`.
  - **WebSocket:** `/api/ws` (`tui_gateway/ws.py`). It reuses `server.dispatch` verbatim and pings every 15 s.
- **Requests flow in both directions.**
  - The server can send requests to the client, with ids `srq-<n>`. They block the agent thread until the client answers (`tui_gateway/AGENTS.md:22-30`).
  - A client opts in by sending `client.capabilities {server_requests: true}`.
  - After a reconnect, the client replays missed events with `session.events.since`.
- **There is a machine-readable contract:** `apps/shared/src/gateway-contract.openrpc.json`, OpenRPC 1.3.2 with `info.version: "1"`. Its counts, which I checked:

| Contract section | Count |
|---|---|
| `methods` (legacy and shared) | 254 |
| `x-canonical-methods` (`hermes-gateway-v1`) | 48 |
| `x-server-requests` | 16 |
| `x-notifications` (events) | 78 |

  The source of truth is Pydantic, in `tui_gateway/contracts/`. The models use `extra="forbid"`, so a payload with an unknown key is rejected with `4000`.

## How Ink connects today

`ui-tui/CANONICAL_GATEWAY.md:3-8` says Ink "no longer starts
`tui_gateway.entry` or owns an agent loop". The connection goes like this:

1. **Bootstrap.** `ui-tui/src/localGatewayBootstrap.ts` runs `python ui-tui/scripts/gateway_bootstrap.py --start|--recover`, using `HERMES_PYTHON` or `python3`.
   - The script imports `pm.environments` and `gateway.status`.
   - It prints a JSON grant: `{url, protocols, profile_id, instance_id}`.
2. **Connect.** `ui-tui/src/gatewayClient.ts:600-617` opens a WebSocket with the subprotocols `hermes-gateway-v1` and `hermes-gateway-ticket.<ticket>`.
3. **Handshake.** The client calls `runtime.describe` and requires `session_create` to advertise the `tui` source (`gatewayClient.ts:191,859`).
4. **Explicit URL.** `HERMES_TUI_GATEWAY_URL` (`gatewayClient.ts:57`) skips the bootstrap and connects to the given URL directly, but the handshake still applies.

The stdio `tui_gateway.entry` path is still used by the desktop e2e harness
(`apps/desktop/e2e/real-session-builder.ts:74`) and by the dashboard PTY.

## Python leaking into the protocol

- **Bootstrap:** the daemon is found through Python, using `HERMES_PYTHON` and `gateway_bootstrap.py`.
- **Error text:** error strings mention `hermes pm repair`.
- **Payloads:** the `gateway.ready` and startup-failure payloads carry a `python` field (`gatewayClient.ts:248,405`).
- **Slash commands:** they run in a Python `_SlashWorker`, and `command.dispatch` returns directives such as `{type: "skill"}`.

## Spike: the explicit-URL path needs no ticket (2026-10-24, upstream `bda3a363e9`)

- With `HERMES_TUI_GATEWAY_URL` set, `start()` skips the bootstrap and calls `startAttachedGateway(attachUrl)` with **no subprotocols** (`ui-tui/src/gatewayClient.ts:734-748`). Only the bootstrap grant path passes `grant.protocols`, the ticket included (`:560-565`).
- The wire is negotiated from the server's first frame: a `gateway.ready` event with `session_authority: true` switches the client to the canonical wire and `runtime.describe` (`gatewayClient.ts:189-193,224-233`); without it the client stays on the legacy wire. Upstream sends `{skin, change_events, heartbeat, replay_epoch, session_authority?}` (`tui_gateway/ws.py:548-553`).
- Credentials, if any, ride in the URL query string; the client redacts it in logs (`gatewayClient.ts:70-80`).
- Ink itself is a Node app (`ui-tui/package.json`: `start` runs `tsx src/entry.tsx`); Python is only reached through `localGatewayBootstrap.ts`, which the explicit URL skips.

**Consequence:** option A works without touching upstream and without Python: `hermes-go` serves `/api/ws` on loopback and launches Ink with `HERMES_TUI_GATEWAY_URL=ws://127.0.0.1:<port>/api/ws?token=<random>`. The remaining cost is the method surface, not the handshake.

## Options for the Go port (decided: A)

| Option | Cost | Upstream TUI untouched? |
|---|---|---|
| A. Go server for `/api/ws` + `hermes-gateway-v1`. Launch Ink with `HERMES_TUI_GATEWAY_URL` pointing at Go. | Medium: canonical handshake plus about 30–40 methods. | Yes. Verified: the explicit URL needs no ticket (see the spike above). |
| B. Same Go server, plus a tiny `gateway_bootstrap` replacement that prints the same grant. | Medium, plus a small shim. | Yes, but the shim still has to be Python or be spawned under `HERMES_PYTHON`. |
| C. Native Go TUI (Bubble Tea). | High. | Not applicable. |

**Rough minimal surface** (estimate, not measured):
- about 30–40 methods;
- the 4 server requests `approval`, `clarify`, `sudo`, `secret`;
- about 20–25 events: `message.*`, `tool.*`, `reasoning.delta`, `session.*`, `status.update`, `error`.

A conformance test can validate Go payloads against the OpenRPC JSON schemas.

## Implementation (option A, canonical wire, 2026-10-24)

- `internal/tuigateway` serves `/api/ws?token=...`. `gateway.ready` carries `heartbeat: true`, `authority_epoch`, `replay_epoch`, `session_authority: true`, `skin: null` and `change_events: []`, so the client runs its canonical handshake and publishes its own ready.

### Canonical wire the port speaks

- `runtime.describe` takes no parameters and answers `instance_id`, `profile_id`, `authority_epoch`, `capabilities` (`["local-cli-create-v1"]`, only what is implemented), `session_create` (`sources: ["tui", "cli"]`, `parameters: []`) and `skin: null`.
- `session.list` answers `{scope, sessions}`. `scope` is `live` (this connection's sessions, newest first) or `stored` (the `state.db` rows, recency order); `limit` is 1-200 with 200 as the default, `title` is an exact stored-title filter that implies the stored scope, `include_hidden` admits hidden rows. Archived rows and delegate children stay out. A row carries `session_id`, `id`, `title`, `source`, `started_at`, `message_count`, `running`, `model`, `resolved_id` and `root_title`.
- `session.resume` takes upstream's closed parameter set (`session_id`, `title`, `latest`, `source`, `omit_messages`, `defer_history`, `display_projection`, …); an unknown key is refused by name in `data.fields`. It attaches an already live session when the live or the stored id is given again, so a resume never forks a conversation; otherwise it opens the stored session through `tuigateway.Options.ResumeSession`. The answer is `CanonicalSnapshot`: `session_id`, `stored_session_id`, `messages`, `display_messages`, `message_count`, `running`, `authority_epoch`, `replay_epoch`, `last_sequence`, `subscription_id`, `revision`, `execution_generation`, `pending`, `prompts`, `info`, `pending_connection`, `todo_state`, `open_requests`. A transcript row is `{role, text, timestamp, row_id}` plus `content`/`tool_call_id`/`name` on tool rows, `display_kind` and `reasoning` when present.
- Errors are canonical: code `4001` with `data.reason` (`invalid_params`, `not_found`, `unknown_method`, `unsupported`, `unavailable`) and `data.fields` for an unknown parameter; an unknown method is `-32601` with `unknown method: <m>` and `data.reason: unknown_method`.
- `commands.catalog`, `complete.slash` and `slash.exec` serve the slash menu over the gateway's own catalog (`/help`, `/model`, `/status`), which advertises only what `slash.exec` can answer. A command the CLI has and the gateway cannot run is not listed, and naming it answers `/x is not available from this gateway; run it in the CLI.`; an unknown name answers `Unknown command: /x (use /help)`. `command.resolve` and `command.dispatch` are not ported.
- Not spoken: the receipt ledger (`prompt.receipt`), event replay, pending admissions, `open_requests`, display projection and skins. The snapshot reports zero `last_sequence`/`revision`/`execution_generation` and empty queues, which the client reads as nothing in flight. A dropped connection parks its session for `Options.OrphanGrace` (20 s by default): a reconnect that resumes the stored id inside that window keeps the live conversation, one that does not is reaped and its stored transcript stays resumable. Only the connection that owns a session may submit, interrupt or close it, and `session.close` answers the same shape for an id that does not exist and one that belongs to somebody else, so it cannot be used to discover ids.
- The server requests are `approval` (choices `once`, `session`, `deny`) and `clarify` (one request per batch; `clarify.lock` per question, or a response frame with `answers`). Ids are `srq-<12 hex>`; a cancelled turn withdraws them with `request.cancel`.
- `prompt.submit` with `submission_id` is refused with 4000, as upstream's legacy contract does; the client then retries with legacy delivery.
- `hermes-go tui` launches `node --expose-gc <tui-dir>/dist/entry.js`. Build that product from an upstream checkout: install the workspace dependencies, `npm run build:ink` in `ui-tui`, then `node scripts/build/tui.mjs --source . --out <tui-dir>`.
