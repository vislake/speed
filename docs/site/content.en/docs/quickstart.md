---
title: Quickstart
weight: 2
---

# Quickstart

> [!NOTE]
> **Nothing is published yet.** speed's first tagged release lands at
> milestone M4 (see [Status](../status/)); until then there is no
> `go get github.com/vislake/speed/...` and no `npm install @speed/...`
> from a registry. The path below — a local checkout plus `go run` — is
> the real, current way to try speed, not a simplification of a published
> flow.

## 1. Clone the repository

```sh
git clone https://github.com/vislake/speed
cd speed
```

The repository root is a Go workspace (`go.work`), not a Go module — see
the note on running Go commands below.

## 2. Generate a starter project with `saasctl new`

`saasctl` is speed's consumer-facing CLI: the tool that materializes a
bootable starter project from an embedded template tree, not a
synthesized skeleton. Run it straight from the checkout with `go run`:

```sh
go run ./go/saasctl new ../my-app --speed-root .
```

The target directory must not already exist (or must be empty). The
generated project's `go.mod` carries `replace` directives pointing at
`--speed-root` — the same transition-state shape every generated project
carries until the first real release, since nothing is on a registry to
depend on by version yet.

### `--with`: choosing which business modules are wired in

By default `new` wires the full switchable set. Five modules
(`pkgcore`, `dbkit`, `tenancy`, `config`, `observability`) are always
present — there is no flag to remove them. Three more are switchable,
with `--with` doing positive selection and downward-closure validation:
selecting `rbac` or `org` without `authn` is refused, naming `authn` as
implied (both need an authenticating layer). There is no `--without` —
closing a module means not listing it.

```sh
# the default: the full {authn, rbac, org} selection
go run ./go/saasctl new ../my-app --speed-root .

# authn only, no org tree or role-based access control
go run ./go/saasctl new ../my-app-lite --speed-root . --with=authn

# bare config-only skeleton, no switchable modules at all
go run ./go/saasctl new ../my-app-bare --speed-root . --with=""
```

`go/pki` is not a fourth switchable choice: whenever `authn` is
selected, the generated project wires `pki` silently as its signing-key
source. See [Modules](../modules/) for the concrete list of what a
consumer project actually imports.

## The four `saasctl` commands

| Command | What it does |
|---|---|
| `saasctl new [flags] <target-directory>` | Materializes the project skeleton into a new directory from `saasctl`'s embedded template tree, substituting the app's module path and the resolved speed-checkout path. Exit codes: 0 success/help, 2 usage error (bad flags, an invalid target name), 1 execution error (unresolvable speed root, a non-empty target, an I/O failure). |
| `saasctl upgrade --version vX.Y.Z [go.mod]` | Rewrites a consumer `go.mod`'s `github.com/vislake/speed/go/*` requires to one target version, in place, byte-for-byte preserving everything else (third-party requires, `replace` blocks, comments, formatting) — the lockstep-versioning rewrite a new release calls for. `--version` is required and validated against the release version grammar; a second run over an already-rewritten file is idempotent. |
| `saasctl db migrate [go.mod]` | Applies the SQL migrations of exactly the migration-shipping modules the project's `go.mod` requires (today: `authn`, `config`, `org`, `pki`, `rbac`, in alphabetical order) to the project's SQLite database, one transaction per module, recording every file in `schema_migrations` as it lands. Refuses a deployment mode other than standalone, and refuses an existing database file that carries no `schema_migrations` ledger rather than guessing. |
| `saasctl config print [go.mod]` | Renders how a generated project's bootstrap configuration resolves — the five `SPEED_*`/`PORT` environment variables, each line showing its value and provenance, with the two key rows redacted regardless of what the environment holds. |

## 3. Migrate the database and boot

`saasctl` itself is not installed anywhere yet, so its subcommands take
an explicit `go.mod` argument naming the target project when you are not
running from inside that project's own directory:

```sh
# still from inside the speed checkout
go run ./go/saasctl db migrate ../my-app/go.mod
go run ./go/saasctl config print ../my-app/go.mod

# then boot the generated app from its own directory
cd ../my-app
go run .
```

The generated app itself also applies its own migrations at startup
(`Kernel.Bootstrap`'s `Apply` step) — running `db migrate` first is the
operator-driven twin of that same step, useful when you want the schema
ready before the first boot rather than on it; a database already
migrated this way makes the startup `Apply` a no-op.

## Running Go commands in this repository

The repository root is a `go.work` workspace, not a single Go module —
a bare `go test ./...` from the root does not resolve as you would
expect in an ordinary module. Run module-scoped commands either from
inside the target module's own directory, or with the full import path
from the root:

```sh
# from the repository root
go build github.com/vislake/speed/go/authn/...
go vet github.com/vislake/speed/go/authn/...

# equivalently, from inside the module
cd go/authn && go build ./... && go vet ./...
```

## Next steps

- [Module index](../modules/) — the concrete list of Go modules and npm
  packages a consumer project can pull in, each linking to its own
  `AGENTS.md`/`README.md`.
- [For AI Agents](../ai-agents/) — what to read first, and the
  architecture rules that most often matter when an agent is doing the
  integrating.
- [Status](../status/) — where implementation genuinely stands today,
  module by module.
