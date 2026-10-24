# Open questions

Questions marked **Owner** are product decisions for the project owner.
Questions marked **Spike** need a technical experiment before the vertical that
depends on them.

## Owner decisions

1. **Binary name and config isolation.** Should the binary be `hermes-go`, so it
   never collides with upstream `hermes` on PATH? Should it share the Hermes
   home by default, or use its own `HERMES_HOME` until V3 is proven safe? The
   recommendation is `hermes-go` plus an opt-in to share the home.
2. **Write safety on a shared `state.db`.** Writing from two implementations
   risks corrupting the user's real history. Should the Go port open an
   upstream DB read-only until a "write enabled" flag is set?
3. **First provider to support.** Options are OpenRouter (OpenAI-compatible),
   the Anthropic API directly, or a local Ollama or LM Studio. This decides the
   first end-to-end target in M1.4.
4. **TUI strategy.** The options are in TUI_PROTOCOL: (A) explicit URL, (B) a
   bootstrap shim, or (C) a native Go TUI. Deciding is only needed before M5.6.
5. **CI timing.** Should a minimal CI workflow land in M1.1, before V6?

## Spikes

1. **CJK FTS trigger.** Does an upstream DB that has `messages_fts_cjk` triggers
   (it needs `libfts5_cjk.so`) reject writes when opened from
   `modernc.org/sqlite`? This must be resolved before M3.1.
2. **Hermes home on Windows.** The upstream default on Windows is
   `%LOCALAPPDATA%\hermes`, not `~/.hermes`. Confirm what the owner's existing
   install uses (`C:\Users\PC\AppData\Local\hermes` appears on PATH).
3. **Go SDK versions.** Verify with Context7 which Go SDKs are current before
   M1.2 and M4.2: the official MCP Go SDK, plus the `anthropic-sdk-go` and
   `openai-go` SDKs, or plain `net/http`.
4. **TUI handshake.** Confirm how `HERMES_TUI_GATEWAY_URL` obtains the ticket
   subprotocol without the Python bootstrap. This must be resolved before M5.6.
5. **Exact predicates.** Read the exact predicates of the deferred indexes past
   `hermes_state_common.py:861` and the trigram FTS column list.
