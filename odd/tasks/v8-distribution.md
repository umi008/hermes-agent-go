# V8 Distribution and onboarding

Objective: a new user installs `hermes-go` with one command, checks the setup with `hermes-go doctor`, and reaches a first answer from a short guide.
Branch: `dev`. Delivery: small commits on `dev`; merge to `main` and the `v0.2.0` tag are the owner's call. Test runner: `go test ./...` (`-race` with cgo locally); scripts are checked by running them against the published `v0.1.0` release in a scratch directory.

## Specs

- S1 Install script for Linux and macOS: `curl -fsSL https://raw.githubusercontent.com/umi008/hermes-agent-go/main/scripts/install.sh | sh` downloads the latest GitHub Release archive for the host OS and architecture, verifies it against `checksums.txt`, and installs `hermes-go` into `~/.local/bin` (override with `HERMES_GO_INSTALL_DIR`). `HERMES_GO_VERSION` pins a version. A checksum mismatch aborts without installing.
- S2 Install script for Windows: `irm https://raw.githubusercontent.com/umi008/hermes-agent-go/main/scripts/install.ps1 | iex` does the same with the zip archive, installs into `%LOCALAPPDATA%\hermes-go\bin`, and adds that directory to the user PATH (keeping `%VAR%` entries unexpanded; `HERMES_GO_NO_MODIFY_PATH=1` opts out).
- S3 `go install github.com/umi008/hermes-agent-go/cmd/hermes-go@latest` is documented as the source install; it works because the binary is CGO-free.
- S4 `hermes-go doctor [-p profile]` reports, one line per check, the version, the Hermes home, `config.yaml` (missing, parsed, `_config_version`), the resolved provider and model with whether an API key is set (never the key), and optional tools (Tavily key for web tools, `docker` when `terminal.backend: docker`, `git` for `search_files`). It reads only, never writes, and exits 1 when a required check fails (home unusable, `.env` or config unreadable, no provider, or `terminal.backend: docker` without `docker` on PATH, since chat cannot start then).
- S5 `docs/GETTING_STARTED.md`: install, set a provider key, `doctor`, first `chat`, then pointers to `serve`, `gateway`, `cron` and `tui`. README gets an Install section that links it.
- S6 The release workflow also publishes the Docker image to `ghcr.io/umi008/hermes-go` on `v*` tags.

Assumptions:
- No `setup` wizard: the port never writes `config.yaml` (decision 2026-10-24); the guide and `doctor` replace it until the owner reopens that decision.
- Scripts are served from `main` on raw.githubusercontent.com, so they reach users after the next merge.
- Homebrew tap, Scoop bucket, signing and SBOM stay out of V8.

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 doctor | S4 | inline + verify | done | 5f9e030 |
| T2 install.sh | S1 | inline + verify | done | e318a38, 01c89b4 |
| T3 install.ps1 | S2 | inline + verify | done | 77a556f, 01c89b4 |
| T4 getting started, README | S3, S5 | inline + verify | done | 1ad57bb, 6761d1b |
| T5 GHCR image | S6 | inline + verify | done | d5a1c18, 8bae375, 01c89b4 |

## Log

- L1 (2026-10-25, owner): "Revisa que se tiene y que falta de hacer, tambien revisa el repo de hermes en el commit que estamos revisando, para establecer toda una ola de cosas nuevas que siguen, no se si por verticales o como vaya a ser, tmb el cosas como \"como instalar y usar\" en el repo o en general, no se si via curl o via como"
- L2 (2026-10-25, owner): "documenta todo esto planear nuevo y arrancas con v8"
- L3: the plan for V8-V12 is in `docs/ROADMAP.md`. `v0.1.0` (42000ed) predates V7; `dev` is 50 commits ahead, so `v0.2.0` is the first release a script install gets with V7.
- L4: T1-T5 landed. `doctor` checks pass against the owner's real home (read-only). install.sh ran in WSL Debian (python curl shim; WSL has no curl) against the published v0.1.0: latest, pinned, tampered checksum (nothing installed), missing version. install.ps1 ran in Windows PowerShell 5.1 through `iex`: latest, tampered checksum, missing version (shell stays alive), and the PATH write against a scratch HKCU key (ExpandString kept, idempotent). Not run: PowerShell 7, macOS, Docker (no local daemon; the CI docker job builds the new cross-compiling Dockerfile on push), the GHCR job (runs only on a tag). `go install ...@latest` works and now reports the tag; local builds stay `dev`.
- L5: independent verifier: no blockers. Advisories fixed in 01c89b4: signal exit and staged-file cleanup in install.sh, `latest` skipped for prerelease tags, raw registry PATH in install.ps1. S4 reworded: docker backend without docker and an unreadable `.env` are required failures, matching what `chat` does. Fixed on the way: 4 pre-existing lint findings in `internal/tuigateway` (8e79d87) that kept CI lint red.
- L6: pending, owner's call: push `dev`, merge to `main` (the scripts are served from `main`), tag `v0.2.0` (the first release with V7, `doctor` and the GHCR image).
- L7 (2026-10-25, owner): "hazlo todo tu"
- L8: `dev` pushed; CI green after one rerun (`TestBackgroundProcess` failed once on windows-latest in `internal/tools/terminal`, untouched since V2, and passed on rerun). Merged to `main` as 2b8d070 (CI green), tagged `v0.2.0`: GoReleaser and the GHCR job passed; six archives plus `checksums.txt`; `ghcr.io/umi008/hermes-go:0.2.0` pulls anonymously. The documented one-liners from `main` installed `hermes-go 0.2.0` in WSL (python curl shim) and Windows PowerShell 5.1, and `doctor` ran on both.
