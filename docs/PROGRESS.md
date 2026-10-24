# Progress

This file is the single resume point. Read it first on every session start.

## Project

**hermes-agent-go** is a scoped Go port of
[NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent), built
as a learning and portfolio project, not as a replacement. It keeps on-disk
compatibility with the Hermes home where practical.

- **Upstream reference:** commit `bda3a363e9` (2026-10-24). The read-only clone
  is at `C:\Users\PC\Desktop\codigo\hermes-agent`.
- **This repo:** `C:\Users\PC\Desktop\codigo\hermes-agent-go`, published on
  GitHub as `umi008/hermes-agent-go`.
- **Branches:**
  - `main` is stable and holds approved verticals only.
  - `dev` is the integration branch. Work lands here.

## Workflow

- Each vertical runs spec → tests → implementation → verification, then stops
  for the owner's approval.
- Gates on every change: `gofmt`, `go vet`, `golangci-lint run`, and
  `go test -race ./...`.
- Commits follow Conventional Commits and stay small.

## Toolchain (verified 2026-10-24)

- Go 1.27.0 on windows/amd64.
- golangci-lint 2.15.0.
- `modernc.org/sqlite` v1.61.0 (SQLite 3.54.1) works with `CGO_ENABLED=0`,
  including FTS5 with the `porter unicode61` and `trigram` tokenizers.

## Done

- **V0 Discovery (2026-10-24).** These docs:
  - [ARCHITECTURE.md](ARCHITECTURE.md)
  - [PARITY.md](PARITY.md)
  - [WIRE_FORMATS.md](WIRE_FORMATS.md)
  - [TUI_PROTOCOL.md](TUI_PROTOCOL.md)
  - [ROADMAP.md](ROADMAP.md)
  - [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md)

## Next

- The owner answers the questions in OPEN_QUESTIONS.md.
- Then start M1.1, Home + config (see ROADMAP.md).

## Decisions

| Date | Decision | Why |
|---|---|---|
| 2026-10-24 | Pure-Go SQLite (`modernc.org/sqlite`) | Gives a CGO-free single binary. FTS5 and trigram are verified. |
| 2026-10-24 | No config migrations in Go; read `_config_version` ≥ 12 and warn when the stamp is old | Upstream owns migrations, so duplicating them risks divergence. |
| 2026-10-24 | Preserve unknown YAML keys, JSON keys, and DB columns on every write | Upstream evolves fast, and dropping data the Go port does not know corrupts a shared home. |
| 2026-10-24 | Python plugin system is out of scope; extension is via skills, MCP, and shell hooks | No Python runtime in Go. |

## Open questions

See [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md).
