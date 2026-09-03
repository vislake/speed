# docs/site — the speed documentation site

This directory holds the documentation site for speed: the modular
monolith distributed as libraries that business projects pull in via
`go get` / `npm install`. The site is the versioned, central reference
for consuming teams; each module additionally ships its own `docs/`
inside the module, so documentation travels with the code (see
docs/internal/13-documentation-standards.md for the full split).

## Current state: real skeleton, machinery deferred

- **Real today (M0)**: static HTML pages with one stylesheet, previewable
  offline with zero external dependencies — no build step, no npm
  project, no network. `tools/check_docs_site.py` keeps the skeleton
  honest in CI (the docs-check pipeline): the required entry files
  exist, every internal link on a page resolves, and the offline preview
  really serves (the check spawns the python3 stdlib HTTP server on an
  ephemeral port and fetches `/`).
- **Deferred to M4** (docs/internal/13-documentation-standards.md):
  per-version release directories (`v1.2/...` — the site is versioned
  like the code it documents), `llms.txt` at the site root once the site
  goes public, the complete M4 documentation set, and the machinery
  decision (a build step / static site generator). This hand-written-HTML
  skeleton deliberately does not anticipate that choice.

## Language

English-first, per docs/internal/13-documentation-standards.md's
language table: the site is written in English, with zh-CN versions
added by need under localization directories. Because localization may
legitimately carry CJK, the whole `docs/site/` subtree is exempt from
tools/scan_cjk.py; until localization directories actually exist, the
English-only discipline here is a review rule, not a scanner rule.

## Preview locally

    python3 -m http.server 8000 -d docs/site

then open http://localhost:8000/. That is exactly what the Taskfile
`docs:serve` task runs — no task CLI needed, the command above is the
whole preview. The M4 machinery round will change these instructions.

## Validate from the repository root

    python3 tools/check_docs_site.py

The check also runs in CI on every PR that touches this site or the
checker itself (docs-check pipeline, `.github/workflows/docs-check.yml`).

## What the skeleton pages cover

- `index.html` — landing page.
- `about.html` — what speed is, how its documentation is distributed.
- `status.html` — coarse milestone status; points at the repository
  root CLAUDE.md's Repository Status section as the authoritative,
  current source of truth.
