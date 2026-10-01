# Release procedure

The consumer installs prebuilt binaries; there are no runtime language dependencies.
All development/release commands run from the repository root.

1. Test with `go test -race ./...` and `go vet ./...`. Run native integration by
   setting `CODEX_ACCOUNTS_INTEGRATION_RUNTIME` to a verified official Codex binary.
2. When updating Codex, change `packaging/codex-runtime.json` using the hashes from
   the official release's `codex-package_SHA256SUMS`. Update the default
   `codexVersion` and third-party notices/licenses. Check all four targets and
   account/SQLite compatibility. The generated release pins that runtime.
3. Commit and push. Tag `vX.Y.Z` and push the tag. The release workflow first
   runs macOS/Linux tests and native protocol integration, then builds four
   `CGO_ENABLED=0` archives and publishes their SHA256SUMS and formula.
4. Copy the release's `codex-accounts.rb` into
   `kakadais/homebrew-tap/Formula/codex-accounts.rb`, run `brew style` and
   `brew test`, then commit and push the tap. The tap uses the official upstream
   native package as a checksummed resource, so no Cask, Python, npm, compiler,
   or existing Codex installation is required.
5. Verify `brew install kakadais/tap/codex-accounts` on macOS and Linux, and
   test `doctor`, default status, and one existing account without a model turn.

For a local release, `go run ./cmd/package -version X.Y.Z` generates exactly the
same artifacts. Upload them with `gh release create vX.Y.Z dist/*.tar.gz
dist/SHA256SUMS dist/codex-accounts.rb --verify-tag --notes-file <file>`.

The included workflow uses the repository's ordinary `GITHUB_TOKEN` with
`contents: write`. No user authentication tokens or additional service accounts
are needed. Updating a separate tap is deliberately a maintainer commit; this
avoids storing a broad organization PAT in a build workflow.

Homebrew's core Formula repository is not required. The public tap is immediately
installable through `brew install kakadais/tap/codex-accounts`.
