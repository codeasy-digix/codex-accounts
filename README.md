# codex-accounts — retired

All maintained implementation, tests, installation and operational documentation
now belong to [kakadais/codexm](https://github.com/kakadais/codexm). This repository
contains no current executable source or release workflow.

Use `codexm account` for account management and the explicit
`codexm history-sync` subcommand for the optional history relay. Official `codex`
is not wrapped. Existing credentials under `~/.codex-accounts` and conversations
under `~/.codex` remain user data and must not be deleted during retirement.

The pre-consolidation source remains in Git at
`b8a7b1284de68f8507247544369190b3f8196cec`. Existing published historical releases
are retained for provenance and rollback, not recommended for new installation.
No new tag, release, Homebrew or npm package is published by this retirement.

See [product deployment and ownership](https://github.com/kakadais/codexm/blob/main/docs/deployment.md)
and [history relay operations](https://github.com/kakadais/codexm/blob/main/docs/history-sync.md).
