---
title: Tools
weight: 6
description: "The developer-facing group: saasctl, the consumer CLI that materialises a starter project, rewrites its module requires onto one release version, applies the modules' SQL migrations and previews its bootstrap configuration."
bookCollapseSection: true
---

# Tools

Every other group in this module reference documents a module your
binary composes: you `go get` it, register it through `pkgcore` and
boot it inside a `Kernel`. This group documents the one entry that is
not a module: **saasctl**, speed's consumer-facing CLI — the tool that
shapes the application a consumer actually runs. Consumers own their
code; saasctl manages the boundary where a project meets the speed
modules it pulls in:

- `new` materialises a bootable starter project from an embedded
  template tree into a new directory;
- `upgrade` rewrites a project's `github.com/vislake/speed/go/*`
  requires onto one lockstep release version;
- `db migrate` applies the SQL migrations of the migration-shipping
  modules a project requires to its SQLite database;
- `config print` renders how a generated project's bootstrap
  configuration resolves, secret rows `[redacted]`.

Nothing assembles saasctl into a running service. It implements no
`the module contract`, never registers into a `Registry`, and consumer code
never imports it; the reference app deliberately does not wire it —
its consumers are the generated projects themselves. Where the module
pages answer "I am integrating module X into my binary", this page
answers "I am starting or maintaining a speed-based project": the tool
acts on the project boundary at development time, creating a
directory, editing a `go.mod`, applying schema to a database file,
before and around boots, never inside them.

## When you reach for it

- **From nothing** — `new` is the standard entry point for a new
  project; the [Quickstart](/docs/user-guide/quickstart/) walks one
  generation, migration and boot end to end.
- **Before a boot** — `db migrate` puts the schema in place ahead of
  any process, and `config print` shows what a boot would run on
  before you start it.
- **At a release** — `upgrade` moves the whole compatibility surface
  of a project onto the one version a release ships.
- **Around the project** — the [walkthrough](/docs/user-guide/walkthrough-reference-app/)
  and [operating](/docs/user-guide/operating/) pages cover the
  assembled shape of a running product and the daily commands; the
  page in this group is the complete reference underneath both.

## Pages in this group

| Page | What it gives you | Typical first use |
|---|---|---|
| [saasctl](/docs/user-guide/modules/tools/saasctl/) | All four commands — usage, flags, validation and exit codes — plus the shape of the project `new` generates and the tool's boundary rules | Generating the project the Quickstart then migrates and boots |

The page follows the group convention: what the tool is for (including
what it deliberately does **not** do), when to use it, the full command
surface, the generated project's anatomy, and boundaries and notes.
Its "Source" section links `go/saasctl`'s own `AGENTS.md`, which
remains the authoritative, always-current contract; this site page is
a condensation.

The dependency direction this reference is ordered by does not apply
here — saasctl sits outside the module graph, above nothing and below
nothing. It is developer tooling, not platform surface: there are no
tables, migrations, HTTP fragments or permissions of its own, and no
`/api/v1/*` routes a running product ever serves from it.
