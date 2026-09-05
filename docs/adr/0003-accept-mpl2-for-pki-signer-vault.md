# ADR 0003: Accept MPL-2.0 for `github.com/hashicorp/vault/api` in `go/pki/signer/vault`

- **Status**: Accepted
- **Date**: 2026-09-05
- **Context of discovery**: found while regenerating `tools/dependency-licenses.json` against the live tree (the manifest had drifted since `go/pki` round 4 added its `SignerRegistry` backends); `tools/license_scan.py` fails closed on any weak-copyleft dependency until an ADR adjudicates it, per its own header comment and `docs/internal/20-quality-and-security.md`'s license-compliance section.

## Context

`docs/internal/20-quality-and-security.md` states the repository's license policy plainly: GPL/AGPL-family dependencies are banned outright; MPL/LGPL-family dependencies ("weak copyleft") need individual evaluation, recorded in an ADR, before they may enter the tree. `tools/license_scan.py` enforces this mechanically — a manifest entry whose license falls in its `WEAK_COPYLEFT` set only passes if the entry carries an `adr` field naming an existing file under `docs/`.

`go/pki` round 4 added `SignerRegistry` (a `pkgcore.SeamRegistry[Signer]`) plus two optional KMS-backed signer backends: `go/pki/signer/vault` (HashiCorp Vault Transit) and `go/pki/signer/kmsaws` (AWS KMS). `go/pki/signer/vault` depends on `github.com/hashicorp/vault/api`, whose actual license — read directly from the module cache (`$GOMODCACHE/github.com/hashicorp/vault/api@v1.23.0/LICENSE`, which reads "Mozilla Public License, version 2.0") and cross-checked against `SPDX-License-Identifier: MPL-2.0` headers in the dependency's own source files (`auth.go`, `client.go`, and others) — is unambiguously **MPL-2.0**. This was never adjudicated when the dependency was added; it surfaced only when the license-manifest regeneration re-derived the expected dependency set from the live tree and hit the scanner's fail-closed weak-copyleft branch.

Verified facts about this dependency's isolation, checked against the live tree at the time of writing:

- `grep -rl "hashicorp/vault/api"` over `go/` finds exactly three non-test files, all inside `go/pki/signer/vault/` (`doc.go`, `signer.go`) plus one comment reference in `go/pki/jwks.go` that names the subpackage without importing it. No file outside `go/pki/signer/vault` imports the package.
- No other module, including `examples/reference-app` and `go/saasctl`, imports `github.com/hashicorp/vault/api` or `go/pki/signer/vault`.
- `go/pki`'s own `go.mod` lists `github.com/hashicorp/vault/api` as a direct (non-indirect) require, which is expected and correct: Go module requires are recorded per module, not per package, so any package inside the `go/pki` module — including its optional `signer/vault` subpackage — pulls the dependency into that module's `go.mod`. A consumer that imports only `go/pki`'s root package never imports `go/pki/signer/vault` and therefore never links `vault/api`'s code into its binary; a consumer that wants the Vault Transit backend imports `go/pki/signer/vault` explicitly and accepts the dependency deliberately, the same `database/sql`-style shape `CLAUDE.md`'s "Prefer a subpackage over a new module" rule already establishes for `go/pki/signer/kmsaws`.
- No semgrep rule under `tools/semgrep_rules/` mentions `vault`, `kmsaws`, or `SignerRegistry` by name today. The isolation this ADR relies on is structural (Go's own package/import-graph boundary and the subpackage split `CLAUDE.md`'s dependency-boundary rules already require of infrastructure backends), not lint-enforced. This ADR does not claim otherwise.

## Decision

Accept `github.com/hashicorp/vault/api`'s MPL-2.0 license for use inside `go/pki/signer/vault` only, on these grounds:

1. **MPL-2.0 is file-level weak copyleft, not project-level.** Its copyleft obligations (making modifications to MPL-licensed *files* available under MPL) attach to the files of the covered work itself; a program merely linking against an MPL library — which is exactly what `go/pki/signer/vault` does — is not thereby placed under MPL. No source file in `go/pki/signer/vault` is a modification of any `vault/api` file, so the repository incurs no MPL obligation on its own code.
2. **The dependency is confined to one opt-in subpackage nothing else imports.** A consumer who never imports `go/pki/signer/vault` (the default: `go/pki`'s own tests, `examples/reference-app`, every generated `saasctl new` skeleton) never links this dependency and is entirely unaffected by its license. This is the identical shape the "Prefer a subpackage over a new module" architecture rule already mandates for exactly this reason.
3. **The backend serves a real, named need** — Vault Transit as an alternative signing-key backend to the module's zero-dependency `LocalSigner`, per `docs/internal/22-pki.md` and `go/pki/AGENTS.md`'s round-4 entry — and no permissively-licensed Vault Go client exists as a substitute; HashiCorp's own `api` package is the standard client for the Vault HTTP API.
4. **No stronger alternative exists that avoids the dependency**: dropping the backend would remove a documented, round-4-shipped capability (`go/pki/AGENTS.md`'s own round-4 entry names it as landed, tested work) rather than resolve a real risk, since (1) and (2) already bound the risk to the one subpackage that opts in.

This acceptance is scoped to `github.com/hashicorp/vault/api` inside `go/pki/signer/vault` specifically. It is not a blanket acceptance of MPL/LGPL dependencies elsewhere in the tree — `tools/license_scan.py`'s policy (fail closed on weak copyleft absent its own ADR) stays exactly as strict as before for any other dependency; a future MPL/LGPL dependency needs its own adjudication.

## Consequences

- `tools/dependency-licenses.json`'s `github.com/hashicorp/vault/api` entry carries `"adr": "docs/adr/0003-accept-mpl2-for-pki-signer-vault.md"`, and `tools/license_scan.py` now passes against the live tree.
- Any consumer who imports `go/pki/signer/vault` inherits this ADR's reasoning along with the dependency; a consumer who does not import it is unaffected, per point 2 above.
- If a future round adds a lint rule fencing infrastructure-backend imports generally (there is none today), `go/pki/signer/vault`'s import of `vault/api` should be added to its allowlist rather than treated as a new violation — the isolation this ADR relies on is structural today, and a lint rule would only be restating it mechanically.
- Should HashiCorp relicense `vault/api`, or should the module ship code that modifies one of its files rather than merely calling it, this ADR's grounds (points 1 and 2) would need re-evaluation.
