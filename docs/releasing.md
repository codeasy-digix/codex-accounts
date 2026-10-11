# Distribution retired

New account-management distribution belongs to [kakadais/codexm](https://github.com/kakadais/codexm).
This repository no longer builds Homebrew formulas or publishes account releases.
Tag pushes cannot invoke a release job. Compatibility and history-sync tests remain.

The historical install instructions in README describe existing releases only.
Do not add `codex` shell functions for new installations. Use `codexm account NAME`
and open a job through codexm; official `codex` retains its own command and updates.
Named credentials and local conversations keep their existing locations.

This source change prepares the migration. Existing GitHub releases and the
published Homebrew tap are preserved until the replacement is actually released.
No separate npm account package is defined in this repository. The official
`@openai/codex` npm package is unrelated and must remain available.

For a future cutover, first verify the codexm archives and migration, then retire
the old tap formula and publish only the independently named codexm package.
Do not publish or delete registry packages as part of preparation.
