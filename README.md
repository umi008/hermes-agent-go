# hermes-agent-go

A scoped Go port of [Hermes Agent](https://github.com/NousResearch/hermes-agent)
by Nous Research. This is a learning and portfolio project, not a replacement
for upstream.

**Status:** V1 Core, V2 Tools, V3 State, V4 Extensibility, V5 Interfaces and V6 Packaging done: `hermes-go chat` is interactive (REPL, slash commands, Ctrl-C) or one-shot with `-q`, `hermes-go serve` speaks the OpenAI-compatible API, `hermes-go cron` runs `jobs.json` schedules, `hermes-go gateway` serves Telegram and Discord, `hermes-go tui` drives the upstream Ink TUI, and the agent can write its own skills with `skill_manage`.

**Goals:**

- Ship a single CGO-free binary.
- Keep on-disk compatibility with an existing Hermes home where practical:
  `config.yaml`, `state.db`, skills, and `jobs.json`.
- Grow verticals one at a time.

## Why?

Mostly for fun and out of curiosity. Hermes is a large, well-designed agent:
an agent loop, tools, memory, skills, a gateway, and cron. Rebuilding it in
another language is the best way to understand how it really works.

It is also a learning project. Each vertical is built spec-first and test-first,
and the decisions are documented, so the repo works as a record of how an agent
like this is put together.

It is not meant to replace upstream. Upstream remains the reference, and this
port stays compatible with its on-disk formats, so you can try both side by side.

## Why Go and not Rust?

Rust would be a fine choice too. For this project, though, Go fits better:

- **Simpler single binary.** `CGO_ENABLED=0 go build` gives one static binary
  for every OS with no extra toolchain. The SQLite driver, with FTS5, is pure
  Go as well.
- **Faster compilation.** Edit-build-test loops take seconds, which matters in
  a project built in many small steps.
- **The workload is I/O-bound.** An agent spends its time waiting on LLM APIs,
  subprocesses, and network calls. Goroutines, channels, and `context`
  cancellation map directly onto streaming, parallel tool calls, and
  interrupts. Rust's raw speed and zero-cost abstractions buy little here.
- **A lower barrier for contributors.** Go is quick to read and learn, which
  helps if a community grows around the project.
- **A mature ecosystem for this domain.** HTTP/SSE, WebSocket, an MCP SDK,
  Telegram and Discord libraries, and Docker clients are all available.

Rust's real advantages are memory safety without a GC, a richer type system,
and finer control over performance. They matter less for a glue-heavy,
network-bound agent than shipping speed and simplicity do.

## Install

```sh
# Linux and macOS
curl -fsSL https://raw.githubusercontent.com/umi008/hermes-agent-go/main/scripts/install.sh | sh
```

```powershell
# Windows (PowerShell)
irm https://raw.githubusercontent.com/umi008/hermes-agent-go/main/scripts/install.ps1 | iex
```

```sh
# From source
go install github.com/umi008/hermes-agent-go/cmd/hermes-go@latest
```

Then set a provider key, run `hermes-go doctor`, and start with
`hermes-go chat`. [docs/GETTING_STARTED.md](docs/GETTING_STARTED.md) walks
through it, including providers and where files live.

## Usage

```sh
hermes-go doctor                     # check home, config, provider and optional tools
hermes-go chat                       # interactive REPL; -q "..." for one message
hermes-go serve --addr 127.0.0.1:8787  # OpenAI-compatible API
hermes-go cron add --schedule 30m --prompt "status report"
hermes-go cron serve                 # tick every 60s
hermes-go gateway                    # Telegram: gateway.telegram.token or TELEGRAM_BOT_TOKEN
hermes-go gateway --platform discord # Discord: discord.token or DISCORD_BOT_TOKEN
hermes-go tui --tui-dir path/to/tui  # upstream ui-tui build; without it, prints the URL to attach
```

With Docker (the home lives in the `/data` volume; `serve` must listen on
`0.0.0.0` inside the container, which requires an API key):

```sh
docker run --rm -it -v hermes:/data ghcr.io/umi008/hermes-go chat   # published from v0.2.0
docker build -t hermes-go .
docker run --rm -it -v hermes:/data hermes-go chat
docker run --rm -p 8787:8787 -v hermes:/data -e HERMES_API_KEY=... hermes-go serve --addr 0.0.0.0:8787
```

Every command reads the Hermes home (`HERMES_HOME`, or `-p <profile>`) and its
`config.yaml`. Run `hermes-go <command> -h` for its flags.

## Docs

- [docs/GETTING_STARTED.md](docs/GETTING_STARTED.md): install and first chat.
- [docs/PROGRESS.md](docs/PROGRESS.md): project status, start here for contributing.
- [docs/ROADMAP.md](docs/ROADMAP.md): milestones.
- [docs/PARITY.md](docs/PARITY.md): what is in and out of scope.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): the upstream architecture.
- [docs/WIRE_FORMATS.md](docs/WIRE_FORMATS.md): formats kept compatible.
- [docs/TUI_PROTOCOL.md](docs/TUI_PROTOCOL.md): can upstream `ui-tui` be reused?
- [docs/OPEN_QUESTIONS.md](docs/OPEN_QUESTIONS.md): pending decisions.

**Contributing:** contributions are welcome. Pick a `planned` row in
[docs/PARITY.md](docs/PARITY.md) and open an issue first.

**License:** MIT. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
