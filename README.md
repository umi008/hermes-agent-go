# hermes-agent-go

A scoped Go port of [Hermes Agent](https://github.com/NousResearch/hermes-agent)
by Nous Research. This is a learning and portfolio project, not a replacement
for upstream.

**Status:** discovery done (V0). No Go code yet.

**Goals:**

- Ship a single CGO-free binary.
- Keep on-disk compatibility with an existing Hermes home where practical:
  `config.yaml`, `state.db`, skills, and `jobs.json`.
- Grow verticals one at a time.

**Docs:**

- [docs/PROGRESS.md](docs/PROGRESS.md): start here.
- [docs/ROADMAP.md](docs/ROADMAP.md): milestones.
- [docs/PARITY.md](docs/PARITY.md): what is in and out of scope.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): the upstream architecture.
- [docs/WIRE_FORMATS.md](docs/WIRE_FORMATS.md): formats kept compatible.
- [docs/TUI_PROTOCOL.md](docs/TUI_PROTOCOL.md): can upstream `ui-tui` be reused?
- [docs/OPEN_QUESTIONS.md](docs/OPEN_QUESTIONS.md): pending decisions.

**Contributing:** contributions are welcome. Pick a `planned` row in
[docs/PARITY.md](docs/PARITY.md) and open an issue first.

**License:** MIT. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
