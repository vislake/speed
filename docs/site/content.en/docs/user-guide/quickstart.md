---
title: Quickstart
weight: 2
description: Generate a bootable starter project with saasctl new, migrate its database and run it — the current, real way to try speed from a local checkout.
aliases: ["/docs/quickstart"]
---

# Quickstart

From nothing to a running project: clone the repository, materialize
a starter project with `saasctl new`, migrate its database and boot
it. You need a Go toolchain and nothing else.

> [!NOTE]
> **No usable release exists yet.** v0.0.1's Go modules were published —
> 21 module tags served by the Go module proxy — then the tags were
> deleted and the version voided; the npm packages never published. Of
> that version the proxy's immutable cache still serves only part of
> the module set (17 of the 21 modules resolve; admin, ai-gateway,
> integration and saasctl do not), while the complete tree stays
> reachable in this repository's history at the publish commit. Either
> way it is voided and unsupported. The local-checkout path below —
> clone plus `go run` — is the only real way to try speed today.
> Generated starter projects carry the transition-state shape this
> implies: version strings overridden by `replace` directives to the
> checkout (zero pseudo-versions and the few real versions mixed), and
> no `go.sum` until the first consumer-side `go mod tidy`.

## 1. Get a checkout

```sh
git clone https://github.com/vislake/speed
cd speed
```

The repository root is a Go workspace (`go.work`), not a Go module —
the note at the bottom of this page explains how to run Go commands
from it. `saasctl` runs straight from the checkout with `go run`.

## 2. Materialize a starter project with `saasctl new`

`saasctl` is speed's consumer-facing CLI: it materializes a bootable
starter project from an embedded template tree of real files, with
every demo-specific piece removed. Its host seams (authn's
`MembershipReader`, org's `SubjectResolver`, the config resolver) are
left unwired and fail closed, each named by a doc comment as your
first task.

```sh
go run ./go/saasctl new --speed-root . ../my-app
```

`--speed-root` names the checkout the generated `go.mod` will point at.
When omitted, saasctl falls back to the `SPEED_ROOT` variable, then to
a walk up the working directory for a `go.work` listing `go/pkgcore`.
The target directory must not already exist (an existing empty
directory is accepted). The directory's base name becomes the go.mod
module path; a name that cannot serve as one is refused before
anything is created.

### Choosing modules with `--with`

Five modules (`pkgcore`, `dbkit`, `tenancy`, `config`,
`observability`) are always present — there is no flag to remove them.
Three more are switchable — `authn`, `rbac`, `org` — and `--with` is
positive selection with closure validation: selecting `rbac` or `org`
without `authn` is refused, naming `authn` as implied. There is no
`--without`: closing a module means not listing it.
`go/pki` is not a fourth choice — it rides along silently whenever
`authn` is selected, as authn's signing-key source; the concrete
require set each selection produces is in the [module index](/docs/user-guide/modules/).

```sh
# the default: the full {authn, rbac, org} selection
go run ./go/saasctl new --speed-root . ../my-app

# authn only — no org tree, no role-based access control
go run ./go/saasctl new --speed-root . --with=authn ../my-app-lite

# bare config-only skeleton, no switchable module at all
go run ./go/saasctl new --speed-root . --with="" ../my-app-bare
```

## The four `saasctl` commands

| Command | What it does |
|---|---|
| `saasctl new [flags] <target-directory>` | Materializes the project skeleton into a new directory from saasctl's embedded template tree, substituting the app's module path and the resolved speed-checkout path. Exit codes: 0 success/help, 2 usage error, 1 execution error. |
| `saasctl upgrade --version vX.Y.Z [go.mod]` | Rewrites a consumer `go.mod`'s `github.com/vislake/speed/go/*` requires to one target version, in place, byte-for-byte preserving everything else (third-party requires with their `// indirect` markers, `replace` blocks, comments, formatting). `--version` is required and validated against the release-version grammar; a second run over an already-rewritten file is a no-op. Rewrites Go `go.mod` files only. |
| `saasctl db migrate [go.mod]` | Applies the SQL migrations of exactly the migration-shipping modules the project's `go.mod` requires — `authn`, `config`, `org`, `pki` and `rbac`, in alphabetical order; 34 files for the full selection (`authn` 12, `config` 1, `org` 9, `pki` 9, `rbac` 3) — to the project's SQLite database, one transaction per module, every file recorded in `schema_migrations`. Idempotent; an existing database file with no `schema_migrations` ledger is refused. |
| `saasctl config print [go.mod]` | Renders how a generated project's bootstrap configuration resolves — one row per `APP_*`/`PORT` variable the generated `cmd/server/config.go` parses, showing the resolved value and its provenance, secret rows `[redacted]` whatever the environment holds; malformed input is refused exactly when the app would refuse to boot. |

## 3. Migrate the database and run

`saasctl` is not installed anywhere yet, so its subcommands take an
explicit `go.mod` argument when you are not inside the project's own
directory:

```sh
# still from inside the speed checkout
go run ./go/saasctl db migrate ../my-app/go.mod
go run ./go/saasctl config print ../my-app/go.mod

# then boot the generated app from its own directory
cd ../my-app
go run .
```

The boot defaults: standalone deployment mode, a SQLite database `app.db`
in the current directory (`APP_DB_PATH` overrides), port 8080 (`PORT`
overrides). The generated app applies its own migrations at every
startup — the selected db component runs them in the assembly's
`Verify` stage, idempotent through the `schema_migrations` ledger —
and `db migrate` is the operator-driven twin of that step, for when
you want the schema ready before the first boot. CLI-then-boot and
boot-only agree.

From the moment the skeleton boots, `/healthz` and
`/api/v1/config/public` answer 200, and registering an account through
`/api/v1/authn/register` answers 201. A password sign-in cannot
succeed on the skeleton as shipped: it has no membership store, authn
re-verifies membership through the host-injected `MembershipReader`
on every sign-in, and a nil reader fails closed — login answers 401
`authn.invalid_credentials` either way. Wiring that seam is the
generated code's named first task.

## Running Go commands in this repository

From the repository root, module-scoped commands take the full import
path; from inside the module they take `./...`:

```sh
# from the repository root
go build github.com/vislake/speed/go/authn/...

# equivalently, from inside the module
cd go/authn && go build ./... && go vet ./...
```

## Next steps

- [Reference app walkthrough](../walkthrough-reference-app/) — drive a
  fully wired, demo-seeded product over real HTTP to see sign-in,
  permissions and tenant isolation behave.
- [Operating a generated project](../operating/) — deployment modes in
  brief, and the `upgrade`/`db migrate`/`config print` commands that
  maintain a project day to day.
- [User guides](/docs/user-guide/) — read by domain: identity and
  access, tenancy and organizations, billing, notifications and more.
- [For AI Agents](/docs/ai-agents/) — what an agent should read first
  when doing the integrating.

## Source

- [saasctl AGENTS.md](https://github.com/vislake/speed/blob/main/go/saasctl/AGENTS.md) —
  the authoritative contract for all four commands.
