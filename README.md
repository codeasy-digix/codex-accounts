# codex-accounts (retired distribution)

Account and worker management now belongs to
[codexm](https://github.com/kakadais/codexm), an independent terminal manager.
The official `codex` command runs directly, without an account shell wrapper or
an account-helper/daemon prerequisite.

Current commands are:

```sh
codexm account --help
codexm account --list
codexm account dongbu
codexm setup --manager inyoung
codexm daemon status
codexm daemon restart
```

The saved default applies to new/imported codexm jobs. Existing jobs keep their
accounts; the manager account is separate. Native Codex authentication and the
parent shell stay unchanged. Existing account credentials and conversation IDs
are retained during migration.

## Distribution status

Automatic tag releases and the old Homebrew packager are retired. Replacement
Homebrew/npm packages are prepared locally in the codexm repository; this change
publishes nothing and does not remove already published historical archives or
alter the official `@openai/codex` package. See [the retirement policy](docs/releasing.md)
and [codexm distribution preparation](https://github.com/kakadais/codexm/blob/main/docs/distribution.md).

This repository preserves the compatibility source, tests and the separately
managed, opt-in `codex-history-sync` executable. History sync is not bundled into
codexm and is not enabled by this migration. The complete previous guide is in
[archived legacy usage](docs/legacy-usage.md); it is a record of retired behavior,
not a current installation guide.
