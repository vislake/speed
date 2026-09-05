---
title: Status
weight: 6
---

# Implementation status

The authoritative, always-current statement lives in the repository
root [CLAUDE.md](https://github.com/vislake/speed/blob/main/CLAUDE.md)'s
*Repository Status* section, which this page deliberately does not
reproduce — a duplicated per-module census would rot the moment the
next module round lands. What follows is a coarse snapshot verified
against the live repository at the time this page was last written;
treat it as an orientation, not a source.

> [!NOTE]
> **Naming the status by milestone number stopped being the useful
> framing here** — implementation moved well past the original M0
> milestone this site once tracked. Most of the planned module graph is
> landed; the roadmap's milestone numbers
> ([docs/internal/15-roadmap.md](https://github.com/vislake/speed/blob/main/docs/internal/15-roadmap.md))
> still record which round a module was originally scheduled for, but
> several modules shipped well ahead of that window.

## What is real today

| Area | State |
|---|---|
| Go modules | **21 of 21** modules listed in the root `go.work` have real, tested implementations — `go build`, `go vet`, `golangci-lint run ./...` and `go test -race` all pass. See [Modules](../modules/) for the list. |
| Go CI coverage | All **21** have a CI matrix row in `fast-check.yml` (every pull request, plus every push to `main`) — `golangci-lint`, `go vet`, race-tested unit tests, a workspace-context build and a `GOWORK=off` standalone build, per module, `go/admin` included. |
| npm packages | **11** `@speed/*` packages under `web/packages/`, plus the reference app's own web host as an unversioned twelfth workspace member, are implemented, tested, and lint/typecheck/build clean. |
| API contract | Six OpenAPI fragments (notes, org, authn, storage, notification, sharing) drive the spec-first generation loop; three of them (notes, authn, notification) feed the merged document that also generates the `@speed/api-sdk` frontend. `admin` and `pki` have their own fragments too but are not yet wired into the `api-contract` pipeline. |

## CI pipelines

| Pipeline | Trigger | Covers |
|---|---|---|
| `fast-check.yml` | Every pull request, plus every push to `main` | Lint, vet and race-tested unit tests for the 21-module Go matrix; lint/typecheck/test/build for the 11 npm packages plus the reference-app web host; repository-wide checks (CJK-outside-`docs/internal` scan, a workspace-wide build, a `go.work` drift gate, the architecture-discipline semgrep rules). |
| `full-check.yml` | PRs labeled `full-ci`, plus every push to `main` | The same module matrix plus the Docker-backed PostgreSQL/Redis/MinIO integration tiers, and the reference app's own composed-HTTP flow tests. |
| `docs-check.yml` | PRs touching docs or i18n resources | i18n key-set parity (zh-CN vs en-US) and this site's own structural check (`tools/check_docs_site.py`, run against a real `hugo build` output). |
| `api-contract.yml` | PRs touching the API-contract toolchain | Regenerates backend interfaces (and, for the merged fragments, the frontend SDK) from the OpenAPI specs and rebuilds the reference app, so a spec change nobody implemented cannot compile. |
| `security.yml` | Every PR plus a daily schedule | Dependency audit, secret scan, CodeQL, license check. |
| `release.yml` | Manual dispatch only | Verifies a lockstep, one-version release plan entirely offline. No publish credential is wired yet — real publishing is a later (v1.0) milestone. |
| `docs-site-deploy.yml` | Push to `main` touching `docs/site/**`, plus manual dispatch | Builds this site with `hugo --minify` and deploys `public/` to GitHub Pages. |

`e2e.yml`, `nightly.yml` and `scaffold-verify.yml` exist as deliberately
gated stubs, not yet triggering on pull requests.

## What's still ahead

- Nothing has ever booted in the **distributed deployment mode**
  itself — every module is proven standalone, with the
  distributed-mode infrastructure (Redis Streams, etc.) so far
  exercised only inside a standalone topology.
- Owner-facing share-link management (`sharing`) ships service-level
  only, with no HTTP surface yet.
- `compliance` has landed retention sweeps, right-to-erasure and export
  delivery; what remains is narrower — immutable database-level
  enforcement, an optional hash chain, formatted report export,
  partitioned archival.
- `admin`'s round 2 — role management, a usage dashboard, per-tenant
  enforcement — has not landed yet.
- The browser-page and end-to-end test legs, and the v1.0 release
  itself (real package publishing), are still ahead.

## Reading the authoritative status yourself

Root `CLAUDE.md`'s *Repository Status* section states, per module,
exactly what genuinely runs and passes in CI today — and it is written
to be *checked*, not trusted: verify a claim against the workflow files
and the module's own tests before relying on it, the same way this
page's own facts were gathered. It is not part of this site; open it
alongside this page when you need current, module-by-module detail. See
also [For AI Agents](../ai-agents/) for why this matters especially for
an agent working in the repository.
