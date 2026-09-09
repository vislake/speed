---
title: Design decisions
weight: 7
description: "The repository's decision records: how architecture-decision records relate to the design pages in this column, and the public narrative of each one."
---

# Design decisions

This column's per-module design pages are distilled from the
repository's internal design documents (`docs/internal/`) and each
module's `AGENTS.md`, and link back to them through their Source
sections. This page covers a different record: **architecture decision
records (ADRs)** — decisions taken while turning the design documents
into code, when the design met reality and something had to give.

## How ADRs work here

Each ADR is one decision, one file, written as Context → Decision →
Consequences, in English, under `docs/adr/` — outside `docs/internal/`
deliberately, because ADRs document the repository's public shape and
are read by tooling as well as people: the license scanner, for
instance, refuses a weak-copyleft dependency until the dependency
manifest names an ADR that adjudicates it.

Three patterns produce an ADR:

- the design documents contain an internal contradiction, and
  implementing the interface exposed it (0001, 0002);
- an implementation detail of the design documents forces a decision
  the documents never made (0003).

The ADR is written in the same change as the fix, and the original
design documents are corrected in that same change, so the record
always reflects the decision the code embodies — not the one the
documents once described.

## ADR 0001: `Module.Migrations()` returns `embed.FS`

The `Module` interface is the wiring contract every module implements,
and it lives in `pkgcore` — the dependency floor. The design documents
specified `Migrations() dbkit.MigrationSet`, which would force
`pkgcore` to import `dbkit`. `dbkit` already imports `pkgcore` (for
the tenant-context primitives and the module contract itself), so the
interface as specified could not compile: an import cycle.

The decision: `Migrations()` returns the standard library's
`embed.FS` — deliberately the thinnest type that could work. How a
module's migrations are interpreted (dialect selection, versioning) is
`dbkit.MigrationRegistry`'s business, applied after the interface
boundary. Consequences: `pkgcore` keeps zero import-path dependency on
`dbkit`; dialect logic stays out of the `Module` interface; the design
documents were corrected in the same change.

[Read ADR 0001](https://github.com/vislake/speed/blob/main/docs/adr/0001-module-migrations-return-embed-fs.md)

## ADR 0002: tenant-context primitives live in `pkgcore`

Earlier drafts put the tenant-context primitives — `WithTenant`,
`TenantFromContext`, `WithSystemContext` and friends — in a `tenancy`
package. But `dbkit.Repository[T]` must resolve the tenant from the
context on every read and fail closed when none is present, and
`tenancy` depends on `dbkit` (for its GORM tenant-isolation plugin).
That, too, was a two-package import cycle that could not compile:
`dbkit -> tenancy -> dbkit`.

The decision: the raw primitives live in `pkgcore`, the one package
both `dbkit` and `tenancy` already depend on. `tenancy` layers the
richer wrapper on top — its `WithSystemContext` additionally publishes
an audit event, machinery that has no reason to live in `pkgcore`.
Consequences: `dbkit` implements fail-closed, tenant-scoped
repositories without importing `tenancy`; `tenancy` stays free to
depend on `dbkit`; business code written before `tenancy` existed
calls the `pkgcore` primitives directly.

[Read ADR 0002](https://github.com/vislake/speed/blob/main/docs/adr/0002-tenant-context-primitives-live-in-pkgcore.md)

## ADR 0003: accept MPL-2.0 for `vault/api` in `go/pki/signer/vault`

The repository's license policy bans GPL/AGPL-family dependencies and
requires an ADR-adjudicated evaluation before any MPL/LGPL-family
("weak copyleft") dependency enters the tree. The policy was never
consulted when `go/pki/signer/vault`, the optional HashiCorp Vault
Transit signing backend, was built on `github.com/hashicorp/vault/api`;
a manifest regeneration surfaced the dependency's unambiguous MPL-2.0
license.

The decision: accept MPL-2.0 for that one dependency, in that one
opt-in subpackage. MPL-2.0 is file-level weak copyleft — calling the
library does not place the repository's own code under MPL, no
`vault/api` file is modified, nothing outside the subpackage imports
it, no permissively licensed Vault client exists as a substitute, and
dropping the backend would remove shipped capability rather than
resolve risk. Not a blanket acceptance: any future MPL/LGPL dependency
needs its own adjudication. Consequences: the manifest entry carries
the ADR reference the scanner requires; importers of the subpackage
inherit the reasoning, everyone else is unaffected.

[Read ADR 0003](https://github.com/vislake/speed/blob/main/docs/adr/0003-accept-mpl2-for-pki-signer-vault.md)

## The shape of a new ADR

A decision that needs recording looks like the three above: it changes
what shipped code does, it was reached against a documented
alternative, and someone will later ask why. The ADR names the
context, states the decision, and lists consequences — and lands in
the same change as the code that embodies it.

## Source

- [docs/adr/0001-module-migrations-return-embed-fs.md](https://github.com/vislake/speed/blob/main/docs/adr/0001-module-migrations-return-embed-fs.md)
- [docs/adr/0002-tenant-context-primitives-live-in-pkgcore.md](https://github.com/vislake/speed/blob/main/docs/adr/0002-tenant-context-primitives-live-in-pkgcore.md)
- [docs/adr/0003-accept-mpl2-for-pki-signer-vault.md](https://github.com/vislake/speed/blob/main/docs/adr/0003-accept-mpl2-for-pki-signer-vault.md)
- [docs/internal/13-documentation-standards.md](https://github.com/vislake/speed/blob/main/docs/internal/13-documentation-standards.md)
- [docs/internal/20-quality-and-security.md](https://github.com/vislake/speed/blob/main/docs/internal/20-quality-and-security.md)
