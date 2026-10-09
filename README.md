# codex-accounts

Per-terminal ChatGPT accounts for Codex, with one shared local conversation store.
macOS and Linux, Apple Silicon/ARM64 and Intel/AMD64. No Python, Node.js, or Go
runtime to install. Homebrew installs this compiled CLI and a pinned official
native Codex package, including its bundled tools and resources.
Homebrew also installs tmux for concurrent conversation continuation.

Version 0.3.0 adds numbered device-code/browser authentication, grouped
continuation menus, and `--set-default` for choosing the machine's default login.

Unreleased development changes preserve upstream help/version/doctor commands
and prefer an independently installed Codex on `PATH`. These changes are being
tested locally and are not included in the published 0.3.0 archives.

This is an independent open-source local utility, not an OpenAI product. The
Homebrew `codex-accounts` package has no GUI, history server, cross-device
synchronization, or background daemon. The repository also contains a separate,
opt-in `codex-history-sync` executable described below; release archives do not
include or enable it.

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
codex account             # Current account, workspace and remaining limits
codex account kakadais     # Another account, with the same local conversations
codex account ryu --login  # Re-login, choosing device code or browser
codex account ryu --browser # Explicit browser re-login
codex account ryu --device # Explicit device-code re-login
codex account --list
codex account default     # Unset account overrides and show the default account
codex account ryu --set-default # Make ryu the machine default, and return this shell to default
codex resume --all
codex continue            # Select from quota and other interrupted conversations
```

Each terminal exports its own `CODEX_ACCOUNT`, `CODEX_HOME`, and
`CODEX_SQLITE_HOME`. Switching one terminal does not alter another. A child
terminal naturally inherits its parent's environment until you switch it.
The original `codex_account` function remains available. Failed or cancelled
login never emits environment changes. Network/service failures retain existing
credentials and do not trigger a replacement login.

With no arguments, `codex account` only shows the current environment, email,
plan, workspace and limits. It does not prompt for authentication.

For `codex account NAME`, the login menu appears when credentials are missing
or invalid, or when `--login` is requested. Enter `1` (or press Enter) for a
device code, `2` for the browser, or `0` to cancel. Invalid input asks again.
`--browser` and `--device` explicitly request re-login
without the menu; `--device-auth` is also accepted. These options also work with
`codex account NAME --set-default`. Browser sign-in runs native `codex login`; device
sign-in adds `--device-auth`, following the official
[Codex login commands](https://learn.chatgpt.com/docs/developer-commands#codex-login).

An executable cannot change its parent shell's environment. Consequently plain
`codex-accounts account ryu` validates/logs in and displays details; per-terminal
selection requires the shell integration, or an explicit `--account ryu` on the
command being run. No global "current account" file is used.

## Set the machine's default login

```sh
codex account ryu --set-default
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
The legacy spelling `codex account NAME default` remains accepted.
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
codex continue             # Numbered conversations and numbered batch actions
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

The menu numbers each conversation, followed by numbered actions for the quota
group, the other group and all listed work. Action numbers follow the last
conversation number. Enter one number or several separated by commas (such as
`1,3`). Enter `0` to cancel. Invalid input asks again without starting a job;
Enter or EOF also cancels. The `quota`, `other`, `all` and `q` shortcuts remain
accepted, and explicit command-line flags remain available.

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

### Optional standalone history relay (development)

`go build ./cmd/codex-history-sync` builds a separate macOS/Linux executable.
It requires no Python or SQLite installation. It does not wrap `codex`, change
account selection, or run during native CLI startup or updates. No Homebrew
release is made by building or installing this executable.

Each worker uses a 256 MiB soft Go memory limit unless `GOMEMLIMIT` is explicitly
set. A single large history bundle can exceed that limit while being processed;
the limit mainly prevents retaining large heaps between transfers. Initial
ingestion checks and compresses all histories and can take several minutes.

Each participant keeps its own native conversation store. One participant also
holds a private relay store; other participants connect over SSH. The relay's
`serve` command accepts only history-storage requests over standard input and
output. It does not expose a network listener or execute arbitrary commands.

Create `~/.codex-history-sync/v2/config.json` with private file permissions:

```json
{
  "node": "workstation",
  "hub_ssh": "history-host",
  "hub_store": "~/.codex-history-sync/v2/hub",
  "hub_binary": "~/.local/bin/codex-history-sync",
  "interval_seconds": 120,
  "enabled": true
}
```

On the participant that hosts the relay, omit `hub_ssh`. `home` defaults to
`~/.codex`, and `store` defaults to `~/.codex-history-sync/v2`. Credentials, GUI
catalogs, remote connection settings, and GUI workspace assignments are never
transported. Store history privately; it can contain project content and secrets
that were already present in the conversations.

```sh
codex-history-sync plan       # Inspect planned transfers
codex-history-sync sync       # One exchange and safe publication attempt
codex-history-sync status     # Last cycle, pending imports, durable conflicts
codex-history-sync conflicts  # Recorded divergent versions and selected winners
```

For an existing, enabled relay, prepare a system service as its owner:

```sh
python3 tools/history_sync_service.py
sudo ~/.codex-history-sync/v2/service/install-system.sh
```

The preparation tool uses only the Python standard library; the installed
service runs the existing Go executable directly without Python or tmux.
macOS uses a LaunchDaemon under `/Library/LaunchDaemons`; Linux uses a systemd
system unit enabled for `multi-user.target`. Both run as the conversation
owner, start at boot, and restart a stopped worker. Administrator access is
required once. On a FileVault Mac, disk unlocking still happens before the OS
can start services.

The installer verifies the prepared executable, configuration and unit hashes,
backs up a previous service definition, and gracefully stops only this owner's
existing history worker before activation. It refuses overlapping workers.
Existing history, account configuration and relay credentials are retained.

```sh
# macOS
sudo launchctl print system/kr.digix.codex-history-sync
sudo launchctl bootout system/kr.digix.codex-history-sync
# Linux
systemctl status codex-history-sync
sudo systemctl stop codex-history-sync
```

To disable automatic startup, use `sudo launchctl disable
system/kr.digix.codex-history-sync` on macOS, or `sudo systemctl disable --now
codex-history-sync` on Linux, and set `enabled` to `false` in the relay config.
Changing a relay requires moving its immutable objects and head index first,
then updating the participants' configuration. Merely pointing at an empty
directory does not migrate the existing relay's retained revisions.

The same conversation ID remains the same conversation. Selection uses actual
persisted event timestamps, not filesystem modification times. Equal timestamps
with different content remain a visible conflict; the destination keeps its
existing conversation. No conflict creates a new conversation ID. Losing
versions and the conflict ledger remain in the relay; a quiet next cycle does
not erase them. Identical histories, acknowledged uploads and pending downloads
are cached to avoid retransferring unchanged payloads.

Native publication is conservative: while Codex GUI, CLI, or app-server
processes are running, incoming history stays in the separate pending store.
This includes a persistent app-server daemon even when it has no active turn.
The worker never stops these processes. While publication is paused, unchanged
pending objects are not repeatedly decoded; new heads are still received and
validated. Cached objects receive full validation when publication resumes.
After the native processes have exited, a later cycle
can publish supported conversations. A history bundle alone does not synchronize
project working trees, worktrees, external attachments, or inherited rollout
dependencies; unsupported or incomplete dependencies stay pending with a reason.
The native reader can restore execution permissions from saved turn metadata.
Publication therefore also requires those permissions to match the destination
thread. New threads use read-only, on-request permissions by default. An operator
can instead select a destination policy for new threads using `new_thread_policy`:

```json
"new_thread_policy": {
  "sandbox": {"type": "disabled"},
  "approval": "never",
  "config_sha256": "SHA256_OF_DESTINATION_CONFIG_TOML"
}
```

The example is suitable only when deliberately choosing the destination's
existing full-access, never-ask policy. Incoming saved permissions must match
the selected policy. The installer verifies the hash of the destination's
`config.toml` before preparation and before commit; a changed configuration
defers new threads until the operator reviews and updates the pin. This pin
detects file changes; it does not evaluate selected profiles, project settings,
managed requirements, or runtime overrides. Existing threads keep their local
permissions, and no native configuration is changed. Unsupported or mismatched
histories remain in the relay and pending store with a reason.

The destination's native history backfill must also be complete. Working
directories under the source home are mapped to the destination home in the
native index. This does not create or synchronize those project directories.
Because raw history keeps its original provenance, resetting or rebuilding the
native index can restore a historical source working directory; recheck paths
before continuing work after such a rebuild.

Publication preserves raw history bytes and the stable conversation ID. It
prepares a native immutable rollout generation and its matching projection,
then changes the selected native state pointer. Previous generations remain
recoverable. Existing local pins, project associations, archive state and
permission settings remain local. GUI indexes are rebuilt by the native app;
the relay never copies another device's GUI database or marks a local thread
as an SSH thread. Keep GUI SSH connections disabled when the connected machines
contain replicas with identical conversation IDs.

This adapter checks the reviewed native storage layout and defers unsupported
schemas. Treat a Codex storage-format update as requiring validation before
publication, even if the relay can still retain the incoming raw history.

### Optional environment variables

| Variable | Purpose |
| --- | --- |
| `CODEX_SHARED_HOME` | Shared local store (default `~/.codex`) |
| `CODEX_ACCOUNTS_DIR` | Separate credentials root (default `~/.codex-accounts`) |
| `CODEX_ACCOUNTS_RUNTIME` | Explicit native executable override; otherwise use the shared daemon package, then `codex` on `PATH`, then the bundled fallback |
| `CODEX_ACCOUNTS_TMUX_SOCKET` | Optional tmux socket name for isolated continuation sessions |

Never put credential directories inside the shared store or a tracked repository.
When shell-init overrides an older `codex()` function, commands use this package's
account feature and selected native runtime. Earlier custom sync hooks are not invoked.
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
The `codex` command name is provided by the opt-in shell function. Only `account`
and `continue` are extension commands; help, version, doctor, and other
commands pass to the selected native CLI. The default environment retains native
daemon behavior. Named accounts use `--no-daemon` to isolate their credentials.
Installation updates use the shared installation home without changing the
account selected in the parent shell.

The runtime is chosen for each invocation: `CODEX_ACCOUNTS_RUNTIME`, the official
package under `~/.codex/packages/app-server-daemon`, an installed `codex` on
`PATH`, then the pinned bundled fallback. Default and named accounts use the same
package regardless of npm, NVM, or Homebrew PATH order. An explicit override
remains authoritative. Recursive shell shims and relative PATH entries are skipped.

When a managed daemon PID file exists, selection runs the native read-only
`app-server daemon version` command. If an update has installed a newer package
while an older daemon is still running, account sessions use that running
release. Selection never starts, restarts, updates, or logs into a server. An
unreadable version response or missing running release produces an error rather
than silently choosing an incompatible CLI.

With a managed package selected, `codex update` forwards to the native
`app-server daemon update` command in the shared installation home. This updates
the one package used by both the CLI and daemon; it may interrupt running daemon
work and retains upstream confirmation. Normal launches do not run an updater.
Without a managed package, updates retain the selected CLI's native behavior.

If npm reports a successful update but `codex --version` stays old, compare it
with `command codex --version` and `codex-accounts doctor --json`. The doctor's
selected path, `runtimeSource`, and version identify the actual shared package.
`command codex` bypasses the account wrapper and can still select another CLI
from PATH; it is not the unified account entry point.
Older controllers that always select their bundled runtime need a controller
update; sourcing the shell configuration alone does not add the new resolver.
The pinned Codex version in `codex-accounts --version` describes the bundled
fallback, not the runtime currently selected.

Without a managed daemon package, an independent Homebrew CLI can provide the
initial runtime:

```sh
brew install --cask codex
codex --version
codex update
```

Bundled copies can be misidentified as a different Homebrew package, and an
app-contained CLI may not support self-update. Once the official daemon package
is installed, account execution and `codex update` use that shared package.
The extension never runs an updater during normal commands.
`codex-accounts doctor --json` reports the actual chosen path, source, and version.
The bundled fallback stays pinned until a package release updates
it. No GUI restart or account change is performed by runtime selection.

## Development and releases

Development needs Go 1.25+ only. The consumer needs neither Go nor a C compiler.

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build .
go run ./cmd/package -version 0.3.0
```

The packager builds all four binaries, produces archives and SHA256SUMS, and
generates `dist/codex-accounts.rb` using checked-in native-runtime checksums.
See [release instructions](docs/releasing.md) for the tag/release/tap procedure.
CI runs unit and Bash/Zsh integration tests on macOS/Linux and an unauthenticated
actual-native-runtime handshake. No live account or model request is needed.

Codex and the package's upstream tools retain their own licenses. See
[third-party notices](THIRD_PARTY_NOTICES.md).

## Support

Contact [support@digix.kr](mailto:support@digix.kr), or report a bug through
[GitHub Issues](https://github.com/codeasy-digix/codex-accounts/issues).
