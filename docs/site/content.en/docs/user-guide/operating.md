---
title: Operating a generated project
weight: 4
description: The day-to-day of a saasctl-generated project — the two deployment modes it can run in, and the upgrade, db migrate and config print commands that maintain it.
---

# Operating a generated project

Once a [starter project](../quickstart/) is generated and booting, three
operations recur: applying the schema ahead of a boot, moving the
project onto a new speed release, and understanding what it would
boot on. All three go through `saasctl`, run from a speed checkout as
`go run ./go/saasctl <command>`; each command takes the target
project's `go.mod` — `[go.mod]` defaults to `./go.mod` inside the
project itself.

## Deployment modes in brief

`APP_DEPLOYMENT_MODE` declares which of the two deployment modes a
generated project runs in; the default is `standalone`.

- **Standalone** — one process, one SQLite file, every infrastructure
  seam (event bus, KV store, mailer, object store) on its in-process
  implementation. Zero external dependencies; what local development
  and small single-machine installs run.
- **Distributed** — the same binary, run as multiple replicas. The
  seams that must be shared across replicas compose real
  implementations from the environment: `APP_REDIS_ADDR` wires a real
  Redis-backed event bus and KV store, the `APP_S3_*` group an
  S3-compatible object store, the `APP_SMTP_*` pair a real mailer, and
  `APP_SMS_GATEWAY_URL` the authn SMS transport.

The mode constrains, it never selects: with none of those variables
set, `APP_DEPLOYMENT_MODE=distributed` fails startup with
`ErrCapabilityUnsatisfied`, naming the seam still on an in-process
implementation. The project's own database speaks SQLite in both
modes.

## `db migrate` — apply the schema ahead of a boot

The generated app applies its own migrations at every startup
(`the assembly`'s `Apply`, idempotent through the
`schema_migrations` ledger). `saasctl db migrate` is the operator-driven
twin of that step, for when you want the schema in place before any
process runs — a prepared-schema first boot, a scripted provisioning
step, CI.

```sh
saasctl db migrate                 # migrates ./go.mod's project
saasctl db migrate ../my-app/go.mod
```

It applies the SQL migrations of exactly the migration-shipping modules
the project's `go.mod` requires — `authn`, `config`, `org`, `pki` and
`rbac`, in alphabetical order — one transaction per module, every file
recorded in `schema_migrations` as it lands. The full
`authn+org+rbac` selection applies 33 files (`authn` 11, `config` 1,
`org` 9, `pki` 9, `rbac` 3), the counts moving with the modules' own
migration sets. A re-run reports the database up to date,
and a boot against the migrated file no-ops its startup `Apply` —
CLI-then-boot and boot-only agree.

Refusals name their reason: a `go.mod` with no speed requires is
probably not a generated project, and an existing database file
without a `schema_migrations` ledger is refused rather than guessed
at. The default database path, `app.db`, resolves next to the
`[go.mod]` argument — the directory the app is documented to run from;
an explicit `APP_DB_PATH` always wins, a relative one anchored the same
way. Run the command to completion before booting any replica: the
shared SQLite file does not take kindly to concurrent writers.

## `config print` — what will this app boot on, and why

```sh
saasctl config print ../my-app/go.mod
```

`config print` renders how the generated project's bootstrap
configuration resolves: the deployment mode, the port, the effective
SQLite path, the five key materials, and the optional infrastructure
variables — one row per variable the generated `cmd/server/config.go`
parses, showing the resolved value and its provenance (the variable
that carried it, or the default it fell back to). The SQLite path row
shows the effective file, so a relative path anchored to the project
directory is reported as the file the app would actually open. Secret
rows — the five key materials, the S3 secret key, the SMTP password,
the SMS gateway URL — render `[redacted]` whatever the environment
holds. Malformed input is refused exactly when the app itself would
refuse to boot. Run it before changing secrets or debugging a boot
that will not start.

## `upgrade` — move the project onto one release version

speed releases are lockstep: every Go module and npm package shares one
version number, and only same-version combinations are supported. When
a release lands, one command rewrites the whole compatibility surface
of a consumer project:

```sh
saasctl upgrade --version v1.0.0 ../my-app/go.mod
```

The rewrite touches only the version token of each
`github.com/vislake/speed/go/*` require line — third-party requires and
their `// indirect` markers, `replace` blocks, comments and formatting
survive byte for byte, and the set of modules rewritten is derived from
the file's own requires. `--version` is required and validated against
the release-version grammar; a second run over an already-rewritten
file is a no-op. The command rewrites Go `go.mod` files only.

The quickstart's present-state note applies here: no usable release
version exists to move to yet, so a generated `go.mod`'s `replace`
directives keep the local checkout authoritative for its build.

## Next steps

- [Reference app walkthrough](../walkthrough-reference-app/) — the fully
  wired, demo-seeded composition, driven over real HTTP.
- [Error code index](../error-codes/) — the codes a running service can
  answer with when something refuses.
- [Module index](/docs/user-guide/modules/) — every Go module and npm package a
  project can require, with its own `AGENTS.md`.
- [Quickstart](../quickstart/) — generating the project this page operates.

## Source

- [saasctl AGENTS.md](https://github.com/vislake/speed/blob/main/go/saasctl/AGENTS.md) —
  the authoritative contract for `upgrade`, `db migrate` and `config
  print`, including exit codes and refusal shapes.
