# AGENTS.md

Quick orientation for any AI coding tool working in this repository. Read this
first if you found your way here by convention rather than by being told —
it is a couple of minutes of reading, not a replacement for the deeper
references it points to below.

## What this is

speed is a **modular monolith distributed as libraries**: independently
released Go modules and npm packages that business projects `go get` /
`npm install` and compile into their own binary. It is not an application you
run, and it is not a repo you fork — a consuming project pulls these packages
in and calls them in-process. Everything here therefore behaves like a
public-API library: an exported signature change propagates to every
downstream project, and a dependency added here lands in someone else's
`go.sum` or bundle.

## Top-level shape

| Path | What lives there |
|---|---|
| `go/` | The Go modules (`go/pkgcore`, `go/dbkit`, `go/authn`, …), one per row of `go.work`'s `use` block. Each is its own release unit with its own `go.mod`. |
| `web/` | The pnpm workspace: the `@speed/*` npm packages (`web/packages/*`) plus its own root config. A separate workspace root from the repo root by design — see `web/README.md`. |
| `examples/reference-app` | A demo dental-SaaS app that is the **mandatory first consumer** of every module (see Reference App below); its web half rides in `web/`'s pnpm workspace as an external member. |
| `docs/` | `docs/internal/**` (Chinese-language design rationale and rejected alternatives), `docs/adr/` (decision records), `docs/site/` (the public docs site source). |
| `build/` | Generated, committed build artifacts — currently the merged OpenAPI document (`build/openapi/`) that drives the frontend SDK generation. |
| `tools/` | Repo-maintenance scripts: CI checks (`scan_cjk.py`, `check_i18n_keys.py`, `check_toolchain.py`, …), the semgrep architecture-discipline rules, the release coordinator. |

## The one fact to internalize before writing code

Module dependencies flow **strictly bottom-up**. As of this writing (verified
against root `CLAUDE.md`'s "Module dependency direction" section):

```
pkgcore -> dbkit / observability / ratelimit -> tenancy -> config / jobs -> storage / notification / pki
        -> authn / rbac / org / metering -> billing / ai-gateway / sharing / integration
        -> compliance -> admin
```

This is a coarse ordering, not the full edge list — `docs/internal/01-architecture.md`'s
mermaid graph is the authority for exactly which module imports which (e.g.
`admin`, at the top, fans in directly on most modules below it rather than
depending on `compliance` alone; `pki` sits above `jobs`/`config` but has no
import edge to or from `authn`, which reaches it only through a structurally-typed
seam at assembly time).

Every module — `go/<name>` and `web/packages/<name>` alike — ships its **own**
`AGENTS.md` carrying discipline specific to that module (its file layout, its
own known limitations, its own testing setup). **Read the target module's
`AGENTS.md` before editing anything in it.** This root file deliberately does
not restate any of that.

## Rules that will burn you fastest if ignored

The full list — enforced by code review and, where noted, by CI — lives in
root `CLAUDE.md`'s "Architecture Discipline" section. The five most likely to
bite on a first change:

- **Never hand-write tenant filtering.** A repository for tenant-owned data
  must embed `dbkit.Repository[T]` — never hold a raw `*gorm.DB` and write
  `WHERE tenant_id = ?` yourself, and never accept a caller-supplied
  `tenant_id` at the API layer (it comes from the access-token claims only).
- **Never cross a module boundary with a concrete import.** Depend on IDs
  plus domain events for cross-module relations (`rbac` must never import
  `authn`, `org` must never import `authn.User`), and on `pkgcore`'s
  interfaces (`KVStore`, `EventBus`, `Mailer`, `ObjectStore`) rather than a
  concrete backend, for infrastructure.
- **API changes are spec-first, non-negotiable order:** edit the module's
  `api/openapi.yaml` → `task api:gen` → fix the resulting compile errors →
  implement → update the frontend, committed together. The frontend may only
  call generated `@speed/api-sdk` hooks; hand-written `fetch`/`axios` is
  permitted solely inside `@speed/api-client`.
- **Deployment-mode differences belong exclusively to kernel wiring.** Never
  branch on `if mode == "standalone"` (or similar) in business logic.
- **Every bug fix ships with a test that reproduces the bug** (failing before
  the fix, passing after). If one genuinely cannot be added, say so
  explicitly, along with why and the follow-up plan, before calling the fix
  done.

## Language

- `docs/internal/**` is written in Chinese (internal design discussion).
- Everything else — code comments, godoc/TSDoc, `AGENTS.md` files, READMEs,
  this file, commit messages — is English.
- User-facing product text is bilingual (zh-CN + en-US) and lives only in
  i18n resources, never hardcoded in code.

CI (`tools/scan_cjk.py`) fails on CJK characters found outside
`docs/internal/` (i18n resources and `docs/site/` localization directories
excepted).

## Commands

Task runner is Taskfile, defined in `docs/internal/19-dev-workflow.md`:

```
task setup        # install toolchain, fetch deps, initialize the database
task dev          # run backend + frontend in standalone deployment mode, hot reload
task test         # test the affected modules
task lint         # lint everything
task api:gen      # merge specs, generate backend interfaces and frontend sdk
```

`task dev` must work in standalone mode (single process, SQLite, no
`docker compose` required). Note: the `task` CLI binary is not guaranteed to
be installed in every environment — if it isn't on `PATH`, run the commands
it wraps directly instead (`go test ./...`, `go vet ./...`,
`golangci-lint run ./...`, `pnpm -r <script>` from `web/`). The repo root is
a `go.work` workspace, not a Go module, so Go commands must be run from
inside the target module's own directory (or with the full import path from
the root) — a bare `./...` at the repo root does not resolve as expected.

## Where to go next, in order of depth

1. **This file** — orientation, load-bearing rules, where everything else is.
2. **The target module's own `go/<name>/AGENTS.md` or `web/packages/<name>/AGENTS.md`**
   — module-specific discipline, file layout, known limitations, testing setup.
3. **Root `CLAUDE.md`** — the exhaustive architecture, full discipline list,
   and a dense per-module "Repository Status" census of what is actually
   implemented and CI-enforced today. Deliberately **not duplicated here**:
   that census changes with nearly every round of work, and a second copy in
   this file would only drift out of sync with it — read `CLAUDE.md` for the
   current, authoritative answer to "is module X real yet."
4. **`docs/internal/**`** — the design rationale behind the rules above,
   including alternatives that were tried and rejected. Chinese-language;
   start at `docs/internal/00-overview.md` for the navigation table.
5. **`.claude/skills/**`** — coding-standard handbooks (`backend-coding-standards`,
   `frontend-coding-standards`, `commit-convention`) with templates and
   checklists. Written for Claude Code but plain markdown, readable by any
   tool.

## Reference App

`examples/reference-app` is the **mandatory first consumer** of every
module — a module API it does not actually exercise is not considered done.
It is a multi-tenant dental SaaS deliberately chosen to touch every corner of
a general SaaS: organizations, pay-per-use credits, long-running AI jobs,
media handling, external sharing, sensitive-data compliance, third-party
integration. When adding or changing a module's public surface, check whether
the reference app needs a matching change before considering the work done.
