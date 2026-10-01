# codex-accounts

Per-terminal ChatGPT accounts for Codex, with one shared local conversation store.
macOS and Linux, Apple Silicon/ARM64 and Intel/AMD64. No Python, Node.js, or Go
runtime to install. Homebrew installs this compiled CLI and a pinned official
native Codex package, including its bundled tools and resources.

This is an independent open-source local utility, not an OpenAI product. It has
no GUI, history server, cross-device synchronization, or background daemon.

## Install

```sh
brew install codeasy-org/tap/codex-accounts
codex-accounts doctor
```

Use it immediately without modifying a shell profile:

```sh
codex-accounts --account ryu
codex-accounts --account kakadais resume --all
codex-accounts --account default
```

The first use of a nickname displays OpenAI's device-code login instructions.
Sign in to the intended ChatGPT account in your browser. A new nickname is a new
credential store; no account's token is copied to another nickname.

## `codex account` in each terminal

Add this **once**, after any existing Codex shell functions, to `~/.zshrc`:

```sh
eval "$(codex-accounts shell-init zsh)"
```

For Bash, add `eval "$(codex-accounts shell-init bash)"` to `~/.bashrc` (or the
profile your login shell actually reads). Open a new terminal, or evaluate the
line in the current terminal. Then:

```sh
codex account ryu          # Check/login, select this terminal, and show details
codex account             # Current environment, email, plan, workspace, limits
codex account kakadais     # Another account, with the same local conversations
codex account ryu --login  # Explicit device-code re-login
codex account --list
codex account default     # Unset account overrides and show the default account
codex resume --all
```

Each terminal exports its own `CODEX_ACCOUNT`, `CODEX_HOME`, and
`CODEX_SQLITE_HOME`. Switching one terminal does not alter another. A child
terminal naturally inherits its parent's environment until you switch it.
The original `codex_account` function remains available. Failed or cancelled
login never emits environment changes. Network/service failures retain existing
credentials and do not trigger a replacement login.

An executable cannot change its parent shell's environment. Consequently plain
`codex-accounts account ryu` validates/logs in and displays details; per-terminal
selection requires the shell integration, or an explicit `--account ryu` on the
command being run. No global "current account" file is used.

## Storage and compatibility

| Data | Location |
| --- | --- |
| Default account | Original `~/.codex` authentication store |
| Named credentials | Real `~/.codex-accounts/<name>/auth.json`, mode 0600 |
| Account directories | Mode 0700 |
| Conversations, archives, memory, attachments, history | Shared `~/.codex` |
| Shared SQLite and its WAL files | `~/.codex`, via native `sqlite_home` |
| Config, skills, rules, `AGENTS.md` | Links to the same local `~/.codex` |

No individual SQLite database or WAL file is symlinked. Existing user files or
links to other locations are rejected instead of overwritten. Existing account
directories from the earlier `.codex_rc` helper are supported without migration.
Authentication files, machine caches, and MCP credential stores remain separate.
Changing the selected account changes billing/limits, while the conversation
content continues locally. Simultaneously editing the same conversation remains
subject to native Codex's writer coordination.

Named accounts use Codex's native file-based ChatGPT authentication and token
refresh. Ambient `OPENAI_API_KEY`, `CODEX_API_KEY`, and `CODEX_ACCESS_TOKEN` are
removed only from named-account child processes. `default` restores the original
native credential/provider behavior, including API-key environment variables.

Account details and quota windows are fetched through the official local
[Codex app-server account endpoints](https://learn.chatgpt.com/docs/app-server#auth-endpoints).
The query issues no model turn. Remaining percentage is `100 - usedPercent`;
5h/week labels are used only for the corresponding reported window durations.
All returned quota buckets are displayed. Missing limits are shown as unavailable,
not as 0%. Reset times use the machine's local timezone. Workspace names are
displayed only when the login claim identifies the selected organization.

The short-lived app-server process exits after the account request; normal Codex
execution replaces the wrapper process and preserves terminal I/O and exit codes.

### Optional environment variables

| Variable | Purpose |
| --- | --- |
| `CODEX_SHARED_HOME` | Shared local store (default `~/.codex`) |
| `CODEX_ACCOUNTS_DIR` | Separate credentials root (default `~/.codex-accounts`) |
| `CODEX_ACCOUNTS_RUNTIME` | Explicit native executable override for development |

Never put credential directories inside the shared store or a tracked repository.
When shell-init overrides an older `codex()` function, commands use this package's
bundled runtime and account feature. Earlier custom sync hooks are not invoked.
Installation itself does not edit your shell profiles or existing helpers.

## Upgrade and uninstall

```sh
brew update
brew upgrade codex-accounts
brew uninstall codex-accounts
```

Remove the shell-init line when uninstalling. Homebrew does not delete your
accounts, default credentials, or conversations. No upstream `codex` executable
is overwritten: only `codex-accounts` is installed in Homebrew's `bin` directory.
The `codex` command name is provided by the opt-in shell function. Bundled Codex
versions are pinned and tested; updating an independently installed Codex does
not change this package. A package release updates the tested native runtime.

## Development and releases

Development needs Go 1.25+ only. The consumer needs neither Go nor a C compiler.

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build .
go run ./cmd/package -version 0.1.0
```

The packager builds all four binaries, produces archives and SHA256SUMS, and
generates `dist/codex-accounts.rb` using checked-in native-runtime checksums.
See [release instructions](docs/releasing.md) for the tag/release/tap procedure.
CI runs unit and Bash/Zsh integration tests on macOS/Linux and an unauthenticated
actual-native-runtime handshake. No live account or model request is needed.

Codex and the package's upstream tools retain their own licenses. See
[third-party notices](THIRD_PARTY_NOTICES.md).
