# Getting started

Install `hermes-go`, give it a model, and get a first answer in about two
minutes. It reads the same Hermes home as upstream, so an existing
`config.yaml` works as is.

## Quick path

1. Install (pick one):

   ```sh
   # Linux and macOS
   curl -fsSL https://raw.githubusercontent.com/umi008/hermes-agent-go/main/scripts/install.sh | sh
   ```

   ```powershell
   # Windows (PowerShell)
   irm https://raw.githubusercontent.com/umi008/hermes-agent-go/main/scripts/install.ps1 | iex
   ```

   ```sh
   # From source, any OS with Go 1.27+
   go install github.com/umi008/hermes-agent-go/cmd/hermes-go@latest
   ```

2. Point it at a model. The shortest path is an OpenRouter key and a model in
   `config.yaml` (see [Where things live](#where-things-live) for the path):

   ```sh
   export OPENROUTER_API_KEY=sk-or-...
   ```

   ```yaml
   model:
     provider: openrouter
     default: deepseek/deepseek-v4.1-flash
   ```

3. Check the setup. Every line should be `ok` or `warn`:

   ```sh
   hermes-go doctor
   ```

4. Chat:

   ```sh
   hermes-go chat -q "hello"   # one message
   hermes-go chat              # interactive; /help lists the commands
   ```

## Install options

| Method | Installs to | Notes |
|---|---|---|
| `install.sh` | `~/.local/bin/hermes-go` | Verifies the archive against `checksums.txt`. Prints a PATH hint when the directory is not on PATH. |
| `install.ps1` | `%LOCALAPPDATA%\hermes-go\bin\hermes-go.exe` | Verifies the checksum and adds the directory to the user PATH. |
| `go install` | `$(go env GOPATH)/bin` | Builds from source; the binary is CGO-free, so no C toolchain is needed. |
| Release archive | anywhere | Download from [Releases](https://github.com/umi008/hermes-agent-go/releases) and unpack. |
| Docker | image | See the README. |

Both scripts read these environment variables:

| Variable | Effect |
|---|---|
| `HERMES_GO_VERSION` | Install this tag (for example `v0.2.0`) instead of the latest. |
| `HERMES_GO_INSTALL_DIR` | Install into this directory. |
| `HERMES_GO_DOWNLOAD_URL` | Download release assets from this base URL (a mirror). |
| `HERMES_GO_NO_MODIFY_PATH=1` | Windows only: leave the user PATH alone. |

To update, run the same command again. To uninstall, delete the binary.

## Providers

`hermes-go` picks the provider from `model.provider`, `--provider`, or the
first API key it finds in the environment.

| Provider | Key variable | Notes |
|---|---|---|
| `openrouter` | `OPENROUTER_API_KEY` | Any OpenRouter model. |
| `anthropic` | `ANTHROPIC_API_KEY` | Messages API with prompt caching. |
| `deepseek` | `DEEPSEEK_API_KEY` | |
| `ollama-cloud` | `OLLAMA_API_KEY` | |
| `lmstudio` | none | Local server at `http://localhost:1234/v1` (`LM_BASE_URL`). |
| `custom` | none | Any OpenAI-compatible server: set `model.base_url`. |

Keys can also live in `.env` inside the Hermes home.

## Where things live

| What | Path |
|---|---|
| Hermes home | `~/.hermes` on Linux and macOS, `%LOCALAPPDATA%\hermes` on Windows, or `HERMES_HOME` |
| Config and secrets | `<home>/config.yaml`, `<home>/.env` |
| Sessions and memory written by the port | `<home>/hermes-go/` (upstream's own files are never written) |
| Profiles | `<home>/profiles/<name>`, selected with `-p <name>` |

## Next steps

| Want to | Run |
|---|---|
| Serve the OpenAI-compatible API | `hermes-go serve --addr 127.0.0.1:8787` |
| Talk to it from Telegram or Discord | `hermes-go gateway` (`--platform discord`) |
| Run scheduled prompts | `hermes-go cron add --schedule 30m --prompt "status"`, then `hermes-go cron serve` |
| Use the upstream terminal UI | `hermes-go tui --tui-dir path/to/ui-tui` |

Run `hermes-go <command> -h` for each command's flags. If something fails,
`hermes-go doctor` names the check that broke.
