---
title: Status
weight: 8
description: "Where implementation genuinely stands: a coarse snapshot, with the repository root CLAUDE.md's Repository Status section as the authoritative, always-current statement."
aliases: ["/docs/status"]
---

# Implementation status

The authoritative, always-current statement of what is real lives in
the repository root [CLAUDE.md](https://github.com/vislake/speed/blob/main/CLAUDE.md)'s
*Repository Status* section, which this page deliberately does not
reproduce — a duplicated per-module census would rot the moment the
next module round lands. What follows is a coarse snapshot verified
against the live repository when this page was last written; treat it
as an orientation, not a source.

This page belongs to the developer docs column; it succeeds the site's
former top-level status page, which remains in place at `/docs/status/`
for now, and carries an alias for that address. The old page's
CI-pipeline table lives on now in
[Contributing](/docs/developer-docs/contributing/), which says what
runs when.

> [!NOTE]
> **Milestone numbers no longer describe the current state.** The
> roadmap's milestones
> ([docs/internal/15-roadmap.md](https://github.com/vislake/speed/blob/main/docs/internal/15-roadmap.md))
> still record when each module was originally scheduled, but several
> modules shipped well ahead of that window — read the roadmap for the
> schedule, not for where implementation stands.

## What is real today

| Area | State |
|---|---|
| Go modules | Every module in the root `go.work` has real, tested implementation — build, vet, lint and race-tested unit suites pass — with a per-module CI matrix row in `fast-check.yml`, and Docker-backed integration tiers against real PostgreSQL, Redis and RustFS in `full-check.yml`. |
| Web packages | The `web/` workspace's `@speed/*` packages, plus the reference app's web host as an unversioned external member, are implemented and tested; lint, strict typecheck, tests and build run clean per package in CI. |
| API contract | The spec-first loop covers the platform modules' OpenAPI fragments: `api-contract.yml` regenerates every backend interface and the frontend SDK from the merged document, consistency-gates the committed artifacts and rebuilds the reference app, so a spec change nobody implemented cannot compile. The reference app's own fragments ride its separate app-owned generation flow. |
| Deployment modes | Standalone is the default and the day-to-day development mode. Distributed deployment is genuinely proven, not just composed in-process: integration tests boot two real replicas over real Redis, RustFS and SMTP, and the daily scaffold-verify job materializes and boots a generated project in both modes. |
| Database dialects | SQLite runs on every pull request; PostgreSQL runs on `full-ci`-labeled pull requests and every push to `main`, through the integration tiers. |

## What is still ahead

- Browser automation over the server-served page — the `e2e` pipeline
  remains a deliberately gated stub.
- The first tagged release with real publishing: nothing is published
  to any registry yet; the roadmap's release milestone covers it.
- The `nightly` pipeline's regression jobs (flaky-test detection,
  benchmark comparisons) remain gated stubs.

## Reading the authoritative status yourself

The root CLAUDE.md's *Repository Status* section states, per module,
exactly what genuinely runs and passes in CI today — and it is written
to be *checked*, not trusted: verify a claim against the workflow
files and the module's own tests before relying on it, the same way
this page's facts were gathered. It is not part of this site; open it
alongside this page when you need current, module-by-module detail.
See also [For AI Agents](/docs/ai-agents/) for why this matters
especially for an agent working in the repository.

## Source

- [Root CLAUDE.md](https://github.com/vislake/speed/blob/main/CLAUDE.md)
- [docs/internal/15-roadmap.md](https://github.com/vislake/speed/blob/main/docs/internal/15-roadmap.md)
