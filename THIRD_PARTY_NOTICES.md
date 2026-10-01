# Third-party software

The account wrapper is MIT licensed. It uses only the Go standard library.
Compiled Go binaries include the Go runtime under Go's BSD license; the release
archives include `third_party/GO_LICENSE`.

The Homebrew formula downloads an unmodified official OpenAI Codex native package
from the pinned `rust-v0.159.3` GitHub release and verifies its SHA-256 digest.
Codex is Apache-2.0 licensed; `third_party/CODEX_LICENSE` and
`third_party/CODEX_NOTICE` are included with this utility. The complete native
package's bundled resource notices/licenses are retained under
`libexec/codex/codex-resources`. Those resources may carry other licenses, including
LGPL-2.1, BSD, and MIT; see the upstream package's own notices and source URLs.

Upstream projects:

- https://github.com/openai/codex/tree/rust-v0.159.3
- https://github.com/golang/go

This local account utility does not host users' credentials, provide an account
sharing service, or change upstream service limits.
