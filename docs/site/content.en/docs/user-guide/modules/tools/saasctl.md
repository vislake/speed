---
title: saasctl
weight: 1
description: "The consumer CLI: new, upgrade, db migrate and config print — each command's usage, flags, validation and exit codes, plus the shape of the project new generates."
---

# saasctl

saasctl is speed's consumer-facing CLI — the tool that shapes the
application a consumer actually runs. speed ships as libraries, not an
application, and saasctl manages the boundary where a project meets the
modules it pulls in: materialising the project skeleton (`new`),
rewriting its speed-module requires onto one lockstep version
(`upgrade`), applying the modules' SQL migrations to its database
(`db migrate`) and showing how its bootstrap configuration resolves
(`config print`).

Unlike every module in this reference, nothing assembles saasctl into a
binary: it ships no component and implements no module, consumer code never imports
it, and the reference app deliberately does not wire it — its consumers
are the generated projects. It acts on the project boundary at
development time, never inside a running service. No usable release is
published yet, so it runs from a checkout
(`go run ./go/saasctl <command>` at the repository root). All four
commands share an exit-code contract — **0** success and help, **2**
usage errors, **1** execution errors — and take an optional `[go.mod]`
argument, `./go.mod` by default; a `go.mod` with no speed requires is
refused by name.

## When to use it

Run `new` to start a project from nothing, `db migrate` when the schema
must exist before any process runs (a prepared first boot, provisioning,
CI), `config print` when a boot will not start or key material is about
to rotate, and `upgrade` when a release has landed. The
[Quickstart](/docs/user-guide/quickstart/) walks one project through
the first two end to end.

## `new` — materialise a starter project

```
saasctl new [flags] <target-directory>
```

Materialises the skeleton from an embedded template tree into the
target directory, which must not already exist (an existing empty
directory is accepted and filled). Its base name becomes the go.mod
module path. Before anything is created, both must pass the go
command's own validators — the base name `module.CheckImportPath`, the
produced go.mod document `modfile.Parse`: its `replace` directives
embed the speed-root path verbatim, so a checkout path the go.mod
grammar rejects would ship a skeleton no `go mod tidy` accepts. A
failed run removes what it created.

`--speed-root` names the checkout the generated `go.mod` points at:
the flag, else the `SPEED_ROOT` variable, else an ancestor `go.work`
listing `go/pkgcore`. `--with` is comma-separated positive selection.
Five modules (`pkgcore`, `dbkit`, `tenancy`, `config`,
`observability`) are always present; the switchable universe is
`{authn, rbac, org}`, the default the full set. Selection closes
downward — `rbac` or `org` without `authn` is refused naming `authn`
as implied, unknown names are rejected listing the valid set, and
there is no `--without`. `go/pki` is never a choice: it rides along
silently with `authn` as its signing-key source.

```sh
go run ./go/saasctl new --speed-root . ../my-app            # default: authn,org,rbac
go run ./go/saasctl new --speed-root . --with=authn,org ../my-app-lite
go run ./go/saasctl new --speed-root . --with="" ../my-app-bare   # bare config-only skeleton
```

## `upgrade` — move a project onto one release version

```
saasctl upgrade --version vX.Y.Z [go.mod]
```

Rewrites the version token of every speed require line the file
carries, nothing else: third-party requires and their `// indirect`
markers, `replace` blocks, comments and formatting survive byte for
byte, since the parsed syntax tree is edited and reprinted through
`golang.org/x/mod/modfile`, the go command's own parser. `--version` is
required (no usable release is published yet and version discovery is
not implemented — the operator supplies the target version) and
validated against the release-version grammar. The
result is re-parsed from bytes and structurally self-checked before
anything is written back; the check also refuses a `replace`/`exclude`
that defeats the rewritten requires — a clean report would lie about
the version the project builds. A second run over an already-rewritten
file reports nothing changed and exits 0. Go `go.mod` files only.

## `db migrate` — apply the schema ahead of a boot

```
saasctl db migrate [go.mod]
```

Applies the SQL migrations of exactly the migration-shipping modules
the project's `go.mod` requires — `authn`, `config`, `org`, `pki` and
`rbac`, in alphabetical order — one transaction per module, every file
recorded in `schema_migrations` as it lands; `pki`'s tables migrate
only when the go.mod genuinely requires `go/pki`. The full
`authn+org+rbac` selection applies 34 files, the per-module counts
moving with the modules' own migration trees. A re-run applies only
what is unrecorded and reports the database up to date; booting against
the migrated file no-ops the startup `Apply` the app would otherwise
perform — the twin of this command.

The default database path — the fixed literal `app.db` — resolves next
to the `[go.mod]` argument, the directory the app is documented to run
from; an explicit `APP_DB_PATH` wins, a relative one anchored the same
way. Refusals name their reason: no speed requires, a file without a
`schema_migrations` ledger, a path that is not a regular file. Both
deployment modes are accepted — the project's database speaks SQLite
under either; the mode changes components, never the dialect. Run to
completion before booting any replica: the shared SQLite file does not
take kindly to concurrent writers.

## `config print` — preview what a boot would use

```
saasctl config print [go.mod]
```

Renders how a generated project's bootstrap configuration resolves,
one row per variable of the env surface the generated
`cmd/server/config.go` resolves: the deployment mode, `PORT`,
`APP_DB_PATH`, the six key materials in the loader's derived spellings
(`APP_CONFIG__CIPHER_KEY`, `APP_ORG__INVITATION_EMAIL_INDEX_KEY`,
`APP_AUTHN__BLIND_INDEX_KEY`, `APP_AUTHN__PII_CIPHER_KEY`,
`APP_PKI__LOCAL_KEY_CIPHER_KEY`, `APP_NOTIFICATION__CONTACT_INDEX_KEY`),
the `APP_REDIS_ADDR`/`APP_S3_*`/`APP_SMTP_*` infrastructure groups and
`APP_OTLP_ENDPOINT`. Each
row shows the resolved value and its provenance — the variable that
carried it, or the default it fell back to. The SQLite path row reports
the effective file, the raw value staying visible when a relative path
resolves differently. Secret rows render `[redacted]` whatever the
environment holds: the six key materials, the S3 secret key, the SMTP
password and the SMS gateway URL. A partially set
`APP_S3_*`/`APP_SMTP_*` group and malformed input are refused exactly
as the app's own bootstrap would refuse them. The command writes
nothing.

## What `new` generates

The template is a working app by construction — produced by
materialising, tidying and building it against a real speed checkout,
then converting the paths back to tokens — and mirrors the reference
app's `cmd/server` shape with every demo-specific piece removed (no
notes module, no demo tenants, grants, membership store or seeded
data). The host modules (authn's `MembershipReader`, org's
`SubjectResolver`, the config resolver) stay unwired and fail closed,
each named by a doc comment as the owner's first task. Selections
differ in two files only — the go.mod require set and `server.go`'s
wiring; `config.go` is shared verbatim — and two tokens,
`__APP_NAME__` (the module path) and `__SPEED_ROOT__` (the checkout
path), are substituted at materialisation. The host-neutral
composition itself (the assembly engine — the configuration load, the
composition plan and the eight-stage component drive — plus authn's
mount path, the serve timeouts, the pre-auth allowlist set and the fixed
middleware chain; the liveness endpoints are `go/observability`'s and
the route-mount rule `pkgcore`'s) lives once, in the platform module
[`github.com/vislake/speed/go/app`](/docs/user-guide/modules/app/), imported by every generated project
and the reference app alike — each host's own `server.go` registers its
components on a `pkgcore.ComponentRegistry`, drives them through the
engine's `Assemble` and serves through its own listener. A repository
gate keeps either host from re-declaring it or re-growing the assembly
steps the engine owns.

The boot defaults: standalone mode, SQLite `app.db`, port 8080.
`/healthz` and `/api/v1/config/public` answer 200 and registering answers
201, but a password sign-in cannot succeed on the skeleton as shipped:
there is no membership store, and authn's nil host-injected
`MembershipReader` fails closed — both a wrong and a correct password
answer 401 `authn.invalid_credentials`. Until the next release lands, a
generated project carries the transition state — version strings
overridden by `replace` directives to the checkout (zero
pseudo-versions and the few real versions mixed), no `go.sum` until
the first consumer-side `go mod tidy` — and `upgrade` is the mechanism
that release will be consumed through.

## An example session

```sh
# From inside a speed checkout: materialise the full default selection.
go run ./go/saasctl new --speed-root . ../my-app
cd ../my-app

# Once a release exists, from inside the project:
saasctl upgrade --version v1.0.0     # rewrite ./go.mod's speed requires
saasctl db migrate                   # prepare app.db beside ./go.mod
saasctl config print                 # preview what this boot would use
go run ./cmd/server                  # boot; startup Apply no-ops
```

## Boundaries and notes

- The commands are safe to re-run, each second run doubling as its own
  check: `db migrate` on an up-to-date database and `upgrade` on an
  already-rewritten file report nothing to do and exit 0, and `config
  print` writes nothing at all.
- `db migrate`'s universe is the migration-shipping *root* modules in
  the go.mod's requires; subpackage migrations such as
  `go/dbkit/audit`'s stay out deliberately, applying through the app's
  own startup `Apply` when wired — the two paths agree.

## Related reading

- [Quickstart](/docs/user-guide/quickstart/) — one project's
  generation, migration and boot, end to end.
- [Operating a generated project](/docs/user-guide/operating/) — the
  daily commands and the two deployment modes in brief.
- [Module reference](/docs/user-guide/modules/) — the modules a
  generated project wires, each with its own page.

## Source

- [saasctl AGENTS.md](https://github.com/vislake/speed/blob/main/go/saasctl/AGENTS.md) —
  the authoritative contract for all four commands: flags, exit codes,
  validation and refusal shapes.
