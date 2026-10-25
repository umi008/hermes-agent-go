# V8 Distribution and onboarding

Objective: a new user installs `hermes-go` with one command, checks the setup with `hermes-go doctor`, and reaches a first answer from a short guide.
Branch: `dev`. Delivery: small commits on `dev`; merge to `main` and the `v0.2.0` tag are the owner's call. Test runner: `go test ./...` (`-race` with cgo locally); scripts are checked by running them against the published `v0.1.0` release in a scratch directory.

## Specs

- S1 Install script for Linux and macOS: `curl -fsSL https://raw.githubusercontent.com/umi008/hermes-agent-go/main/scripts/install.sh | sh` downloads the latest GitHub Release archive for the host OS and architecture, verifies it against `checksums.txt`, and installs `hermes-go` into `~/.local/bin` (override with `HERMES_GO_INSTALL_DIR`). `HERMES_GO_VERSION` pins a version. A checksum mismatch aborts without installing.
- S2 Install script for Windows: `irm https://raw.githubusercontent.com/umi008/hermes-agent-go/main/scripts/install.ps1 | iex` does the same with the zip archive, installs into `%LOCALAPPDATA%\hermes-go\bin`, and adds that directory to the user PATH.
- S3 `go install github.com/umi008/hermes-agent-go/cmd/hermes-go@latest` is documented as the source install; it works because the binary is CGO-free.
- S4 `hermes-go doctor [-p profile]` reports, one line per check, the version, the Hermes home, `config.yaml` (missing, parsed, `_config_version`), the resolved provider and model with whether an API key is set (never the key), and optional tools (Tavily key for web tools, `docker` when `terminal.backend: docker`, `git` for `search_files`). It reads only, never writes, and exits 1 when a required check fails (home unusable, config unparsable, no provider).
- S5 `docs/GETTING_STARTED.md`: install, set a provider key, `doctor`, first `chat`, then pointers to `serve`, `gateway`, `cron` and `tui`. README gets an Install section that links it.
- S6 The release workflow also publishes the Docker image to `ghcr.io/umi008/hermes-go` on `v*` tags.

Assumptions:
- No `setup` wizard: the port never writes `config.yaml` (decision 2026-10-24); the guide and `doctor` replace it until the owner reopens that decision.
- Scripts are served from `main` on raw.githubusercontent.com, so they reach users after the next merge.
- Homebrew tap, Scoop bucket, signing and SBOM stay out of V8.

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 doctor | S4 | inline | todo | |
| T2 install.sh | S1 | inline | todo | |
| T3 install.ps1 | S2 | inline | todo | |
| T4 getting started, README | S3, S5 | inline | todo | |
| T5 GHCR image | S6 | inline | todo | |

## Log

- L1 (2026-10-25, owner): "Revisa que se tiene y que falta de hacer, tambien revisa el repo de hermes en el commit que estamos revisando, para establecer toda una ola de cosas nuevas que siguen, no se si por verticales o como vaya a ser, tmb el cosas como \"como instalar y usar\" en el repo o en general, no se si via curl o via como"
- L2 (2026-10-25, owner): "documenta todo esto planear nuevo y arrancas con v8"
- L3: the plan for V8-V12 is in `docs/ROADMAP.md`. `v0.1.0` (42000ed) predates V7; `dev` is 50 commits ahead, so `v0.2.0` is the first release a script install gets with V7.
