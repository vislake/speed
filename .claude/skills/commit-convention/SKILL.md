---
name: commit-convention
description: speed Commit Message Convention — Conventional Commits format, scope list and repository-specific rules for every git commit
triggers:
  - creating a commit
  - writing a commit message
  - git commit
  - committing changes
globs:
  - "**/*"
---

# speed Commit Message Convention

Follows [Conventional Commits](https://www.conventionalcommits.org/), enforced by commitlint and the pre-commit hook.

**Language**: commit messages are written in **English**, consistent with the repository-wide language rule — everything except `docs/internal/` is English, because this code is delivered to business projects and read by tooling and non-Chinese-speaking collaborators.

## Format

```
<type>(<scope>): <description>

[optional body]

[optional footer(s)]
```

- **Header** is required and must be a single line.
- **Keep the header short — aim for 72 characters or less.** This is a guideline, not a hard limit: a header that needs a few more characters to stay clear is better than a truncated one. Do not sacrifice meaning for the count.
- **Scope is required.** This monorepo holds Go modules (all importable except `saasctl`, a CLI) and npm packages (all importable except `create-saas-app`, a CLI); without a scope there is no way to tell what a commit touched.
- **Description** uses the imperative mood, lowercase, no trailing period: "add subdomain tenant resolution", not "added ..." or "adds ...".
- **Body** explains *why*, not *what* — the diff already shows what changed.
- **Footer** carries breaking-change notices (`BREAKING CHANGE:`) and issue references (`Closes #123`).

## Types

| Type | When to use |
|---|---|
| `feat` | New functionality |
| `fix` | Bug fix |
| `docs` | Documentation only (including `docs/internal/**`, ADRs, module `AGENTS.md`) |
| `style` | Formatting only, no logic change |
| `refactor` | Restructuring that is neither a feature nor a fix |
| `perf` | Performance improvement |
| `test` | Adding or updating tests |
| `chore` | Build, tooling, dependencies, CI |
| `i18n` | Adding or updating zh-CN / en-US resources |
| `api` | OpenAPI spec change, together with the regenerated artifacts |

## Scopes

### Backend Go modules (`go/<scope>/`)

`pkgcore`, `dbkit`, `observability`, `tenancy`, `ratelimit`, `config`, `jobs`, `storage`, `notification`, `authn`, `rbac`, `org`, `metering`, `billing`, `ai-gateway`, `sharing`, `integration`, `compliance`, `admin`, `saasctl`

### Frontend npm packages (`web/packages/<scope>/`)

`tokens`, `i18n`, `ui-kit`, `api-client`, `api-sdk`, `auth-core`, `auth-ui`, `tenancy-ui`, `billing-core`, `billing-ui`, `notification-core`, `notification-ui`, `layout-kit`, `product-shell`, `admin-shell`, `create-saas-app`

### Cross-cutting

`openapi` (spec and generation pipeline), `deps`, `ci`, `compose`, `templates` (CLI skeletons), `reference-app`, `release`, `adr`

Repository-level:
- `internal` — design documents under `docs/internal/`
- `repo` — repository-level configuration, guidance and standards (`CLAUDE.md`, `.claude/skills/`, `.gitignore`, Taskfile)
- `site` — the public documentation site under `docs/site/`

## Examples

```
feat(tenancy): resolve tenant from subdomain

fix(billing): prevent credit over-deduction under concurrent debits

fix(jobs): rebuild tenant context before handler execution

api(billing): add trial_end to the subscription response

i18n(auth-ui): add en-US strings for the login page

test(metering): cover outbox recovery after process crash

chore(deps): bump gorm.io/driver/postgres to v1.6.0

docs(adr): record the decision to use spec-first over code-first

refactor(rbac): extract policy cache to avoid full reload per request

feat(storage)!: return a struct from the presign endpoint

BREAKING CHANGE: SignUpload now returns UploadTicket instead of a
plain string; callers must read the .URL field. Migration notes in
docs/upgrade/v1-to-v2.md
```

## Rules

1. **One logical change per commit.** Do not mix unrelated work.
2. **Each commit compiles and passes tests on its own** — CI runs per commit on pull requests.
3. **No generic messages.** "fix bug", "update code", "misc" are rejected.
4. **Breaking changes** get `BREAKING CHANGE:` in the footer and a `!` after the scope.
   This repository ships libraries consumed via `go get` / `npm install`, so **any change to an exported signature is breaking**, even when the internal behaviour is unchanged.
5. **Lockstep versioning.** Never tag a single module by hand. Releases tag every module with the same version through the release pipeline — see `docs/internal/02-repo-and-release.md`.
6. **API changes commit the spec and the implementation together.** Editing `api/openapi.yaml` means committing the regenerated backend interface and frontend sdk in the same commit; CI regenerates and diffs. Use the `api` type.
7. **New user-facing text is bilingual in the same commit** — zh-CN and en-US both, CI checks the key sets match.
8. **Every bug fix carries a test that reproduces the bug** (failing before the fix, passing after). If a test genuinely cannot be added, say so in the body with the reason and the follow-up plan.
9. **Rebase before merging; fast-forward merges only.** Branch protection on `main` rejects merge commits, keeping history linear.
10. **Test helper directories are not scopes.** When touching `testutil` or `testdata`, use the domain scope they serve, e.g. `test(billing): ...`.
