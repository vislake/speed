# speed

**The platform layer of a modern SaaS, ready to assemble.**

Every SaaS ends up building the same hard, security-critical
underbelly: authentication, multi-tenant isolation, roles and
permissions, organizations, subscription billing and metering, an AI
gateway, background jobs, file storage, notifications, audit and
compliance, an admin console. speed ships that layer as Go modules and
npm packages you pull into your own product — you pick the
capabilities you need, compose them into your own service, and own
the result.

## What you get

Instead of months of platform work, you start with what your product
actually is:

- **Identity and access, done safely** — password, phone, social and
  enterprise sign-in; sessions and refresh rotation; MFA and step-up;
  role-based permissions that are deny-by-default and
  tenant-scoped. No security code you have to get right yourself.
- **Multi-tenant isolation you can't accidentally break** — tenant
  scoping is enforced by the data layer, not by convention.
- **The rest of the platform stack** — organizations and members,
  plans and credits, usage metering, an AI gateway, background jobs,
  object storage, notifications with consent-aware delivery,
  public-share links, audit and data-retention compliance, an
  operations console.
- **A frontend that matches** — React packages for sign-in, accounts,
  tenant switching, design tokens and i18n, plus a typed client
  generated from the same API contracts your backend implements.

And because the modules compile **into your binary**, nothing is
hidden behind a service boundary: every piece is your code to read,
extend, and own — not a vendor API you're locked into.

## How it works

speed is a **modular monolith distributed as libraries**. You `go get`
the modules your product needs and `npm install` the frontend
packages; a small CLI generates a starter project; everything runs as
one deployable service. Run it as a single process with a SQLite file,
or the same code in a distributed mode with PostgreSQL and Redis — the
deployment shape is a configuration choice, not a fork.

Everything is **contract-first**: each module's HTTP API is an
OpenAPI specification, and both the backend handlers and the
frontend's typed calls are generated from it — what the docs describe
and what the service answers can never drift apart.

## Getting started

The [documentation site](https://speed.vislake.com/) carries the full
story: [user guides](https://speed.vislake.com/docs/user-guide/) walk
each capability with runnable examples and a complete API reference,
and the [quickstart](https://speed.vislake.com/docs/user-guide/quickstart/)
takes a starter project from clone to running.

Nothing is published to a package registry yet — the current way to
try speed is a local checkout, materializing a starter project with
`saasctl new` from the repository. The first release lands on the
registry.

## Project layout

```
speed/
  go/          21 Go modules — the platform layer
  web/         12 npm packages — the frontend layer
  examples/    reference-app: a complete product built on speed
  contracts/   the merged OpenAPI contract of the platform API
  docs/        the documentation site, design docs and decision records
```

For developing speed itself — conventions, architecture discipline,
repository layout and where everything lives — see
[CLAUDE.md](CLAUDE.md) (for AI coding agents) and the coding-standard
handbooks under `.claude/skills/`.
