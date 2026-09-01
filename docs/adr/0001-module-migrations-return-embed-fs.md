# ADR 0001: `Module.Migrations()` returns `embed.FS`, not a `dbkit`-specific type

- **Status**: Accepted
- **Date**: 2026-09-01
- **Context of discovery**: found and fixed during Round 1 implementation of `go/pkgcore`, confirmed by independent gap-check audit.

## Context

`docs/internal/01-architecture.md` originally specified the `Module` interface with:

```go
Migrations() dbkit.MigrationSet
```

`docs/internal/01-architecture.md`'s own dependency graph states `dbkit --> pkgcore` (dbkit depends on pkgcore, not the reverse). The `Module` interface is defined in package `pkgcore` — it is the wiring contract every module, including business modules that never touch `dbkit` directly, implements. If `Migrations()` returned a type defined in `dbkit`, package `pkgcore` would have to import `dbkit` to reference that type in the interface signature. Since `dbkit` already imports `pkgcore` (for the tenant-context primitives and the `Module`/`Registry` contract itself), this would create a two-package import cycle: `pkgcore -> dbkit -> pkgcore`. Go does not allow import cycles; this would not compile.

The inconsistency was actually already visible within the original document: the comment directly above the field read "资产声明（全部用 embed.FS，与模块代码同版本）" ("asset declarations, all using embed.FS, versioned with the module's code") while the type on the very next line contradicted that comment.

## Decision

`Module.Migrations()` returns the standard library's `embed.FS` directly:

```go
Migrations() embed.FS
```

`dbkit.MigrationRegistry` (built in a later round) consumes `embed.FS` values from each module and applies its own dialect-specific interpretation and aggregation logic. `pkgcore` needs no knowledge of what `dbkit` does with the filesystem it receives — the contract is intentionally the thinnest type that could work, which is exactly what keeps `pkgcore` free of a `dbkit` dependency.

## Consequences

- `pkgcore` has zero import-path dependency on `dbkit`, preserving the dependency floor's isolation.
- Any module-specific migration interpretation (dialect selection, versioning scheme) is entirely `dbkit`'s responsibility, not encoded in the `Module` interface.
- The original design documents (`docs/internal/01-architecture.md`) have been corrected to match this decision as of the same change that produced this ADR.
