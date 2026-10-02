# codex-accounts

Per-terminal ChatGPT accounts for Codex, with one shared local conversation store.
macOS and Linux, Apple Silicon/ARM64 and Intel/AMD64. No Python, Node.js, or Go
runtime to install. Homebrew installs this compiled CLI and a pinned official
native Codex package, including its bundled tools and resources.
Homebrew also installs tmux for concurrent conversation continuation.

Browser sign-in selection and grouped continuation are development changes on
`main` and in local builds. The Homebrew release remains 0.2.1 until the next
release is published.

This is an independent open-source local utility, not an OpenAI product. It has
no GUI, history server, cross-device synchronization, or background daemon.

## Install

```sh
brew install codeasy-digix/tap/codex-accounts
codex-accounts doctor
```

Use it immediately without modifying a shell profile:

```sh
codex-accounts --account ryu
codex-accounts --account kakadais resume --all
codex-accounts --account default
```

The first use of a nickname offers device-code or browser sign-in. Device-code
sign-in also works over SSH; browser sign-in opens the browser on the machine
running Codex. Sign in to the intended ChatGPT account. A new nickname is a new
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
codex account ryu --login  # Re-login, choosing device code or browser
codex account ryu --browser # Explicit browser re-login
codex account ryu --device # Explicit device-code re-login
codex account --list
codex account default     # Unset account overrides and show the default account
codex account ryu default # Make ryu the machine default, and return this shell to default
codex resume --all
codex continue            # Select from quota and other interrupted conversations
```

Each terminal exports its own `CODEX_ACCOUNT`, `CODEX_HOME`, and
`CODEX_SQLITE_HOME`. Switching one terminal does not alter another. A child
terminal naturally inherits its parent's environment until you switch it.
The original `codex_account` function remains available. Failed or cancelled
login never emits environment changes. Network/service failures retain existing
credentials and do not trigger a replacement login.

The login menu appears only when credentials are missing or invalid, or when
`--login` is requested. Enter `1` (or press Enter) for a device code, `2` for the
browser, or `q` to cancel. `--browser` and `--device` explicitly request re-login
without the menu; `--device-auth` is also accepted. These options also work with
`codex account NAME default`. Browser sign-in runs native `codex login`; device
sign-in adds `--device-auth`, following the official
[Codex login commands](https://learn.chatgpt.com/docs/developer-commands#codex-login).

An executable cannot change its parent shell's environment. Consequently plain
`codex-accounts account ryu` validates/logs in and displays details; per-terminal
selection requires the shell integration, or an explicit `--account ryu` on the
command being run. No global "current account" file is used.

## Set the machine's default login

```sh
codex account ryu default
```

This validates or signs in `ryu`, backs up the previous default credentials
and configuration under `~/.codex-accounts/.default-backups/`, and atomically
points `~/.codex/auth.json` at `~/.codex-accounts/ryu/auth.json`. The shared home,
conversations, SQLite state, and other account credentials keep their locations.
Native Codex writes `cli_auth_credentials_store = "file"` to the shared config,
preserving unrelated TOML settings and honoring managed authentication policy.
The default and named login refer to one credential file: no active refresh-token
copy is made. Backups are private files (0600) inside private directories (0700).

`codex account default` still means "use the machine's current default in this
terminal"; it does not change which nickname is the machine default.
`codex account` reports the default nickname, email, and limits. `--list` marks
the designated nickname with `(default)`.

New terminals with no account overrides and native clients using `~/.codex` use
the designated login. Restart the GUI app after switching: already-running
clients can cache the previous authentication. Finish or stop work using the
previous default before switching it; this command does not migrate running
jobs to another account. Other terminals with a named account stay selected.
If a GUI client has its own external login, provider, or `CODEX_HOME`, that
client's own account settings still apply. `CODEX_SHARED_HOME` changes the store
targeted by this command; it cannot redirect an independently configured GUI.

## Continue stopped conversations

```sh
codex continue             # Two groups; choose numbers, quota, other, all, or q
codex continue --list      # List only; no account request or model turn
codex continue --json      # Machine-readable list
codex continue --quota --list # Only account usage/rate-limit interruptions
codex continue --other --list # Only other interrupted conversations
codex continue 1,3         # Continue selected list numbers without an input prompt
codex continue <UUID>      # Continue one listed conversation
codex continue --quota --all # Continue all idle quota interruptions
codex continue --other --all # Continue all idle other interruptions
codex continue --all       # Continue both groups
```

This is a deterministic command in the external `codex-accounts` program. The
shell routes `codex continue` to that program directly, just like `codex account`.
It scans, filters, prints the list and reads your selection without launching
Codex, signing in, or asking a model to find interrupted work. Only a selection
starts native Codex to resume the chosen conversation(s).

The list is built from structured events in `~/.codex/sessions`, in two groups:

- **Quota:** saved account usage/rate-limit errors that have not been resolved.
- **Other:** user interruptions, network/authentication/execution errors, and
  started turns that have no completion marker and no active writer after the
  process exits. Retryable stream errors disappear after a successful completion.

User text mentioning limits and 100% usage snapshots do not count as interrupted
jobs. A later successful turn removes a conversation from the list. Archived
conversations and unreadable logs are excluded; unreadable
files are counted in a warning. Active conversation writers and already-running
continuation jobs are excluded from the numbered list (`activeSkipped` in JSON).
A private cache avoids reparsing unchanged logs; writer status is checked on
every invocation even when the interruption itself is cached. JSON includes each
conversation's `category` (`quota` or `other`) and a `counts` object for the
requested groups. This uses saved execution state, not a model's judgment of
whether the user's broader objective is finished.

If an older custom `codex()` function is loaded, put the `shell-init` line after
it. Otherwise it may forward `continue` to the native Codex prompt. You can
always bypass shell functions with `codex-accounts continue --list`.

Each selected conversation resumes with `codex exec resume UUID continue` in
its original working directory, using this terminal's selected account and
native permission settings. No approval or sandbox bypass is added. Concurrent
jobs receive separate tmux sessions named `codex-continue-<id>-<suffix>`; the CLI
prints each session's attach command. Existing conversation writers and already
launched jobs are skipped, and the stored state is checked again before launch.
Parallel jobs use the same account quota and can edit the same project, so select
individual jobs when their changes depend on one another.

Sessions close automatically when each job exits. Private output logs and JSON
status (`completed`/`failed`, account, exit code, times) remain under
`~/.codex-accounts/.continue-jobs/<UUID>.log` and `<UUID>.json`. Failures and a new
quota stop can be selected again; successful continuations update the original
conversation. This is a single continuation request, not a retry loop or a
guarantee that every user objective has finished.

## Storage and compatibility

| Data | Location |
| --- | --- |
| Default account | `~/.codex/auth.json`; after designation, a link to one named credential file |
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
| `CODEX_ACCOUNTS_TMUX_SOCKET` | Optional tmux socket name for isolated continuation sessions |

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
go run ./cmd/package -version 0.2.1
```

The packager builds all four binaries, produces archives and SHA256SUMS, and
generates `dist/codex-accounts.rb` using checked-in native-runtime checksums.
See [release instructions](docs/releasing.md) for the tag/release/tap procedure.
CI runs unit and Bash/Zsh integration tests on macOS/Linux and an unauthenticated
actual-native-runtime handshake. No live account or model request is needed.

Codex and the package's upstream tools retain their own licenses. See
[third-party notices](THIRD_PARTY_NOTICES.md).
