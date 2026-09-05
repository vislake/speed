# docs/site — the speed documentation site

This directory holds the documentation site for speed: the modular
monolith distributed as libraries that business projects pull in via
`go get` / `npm install`. The site is the versioned, central reference
for consuming teams; each module additionally ships its own `AGENTS.md`
and `README.md` inside the module, so documentation travels with the
code (see docs/internal/13-documentation-standards.md for the full
split — a per-module `docs/usage.md` is that doc's longer-term plan,
created so far by exactly one module, `go/notification`, as a template
for the rest).

## Current state: real content, machinery still deferred

- **Real today**: static HTML pages with one stylesheet, previewable
  offline with zero external dependencies — no build step, no npm
  project, no network. `tools/check_docs_site.py` keeps the skeleton
  honest in CI (the docs-check pipeline): the required entry files
  exist, every internal link on a page resolves, and the offline preview
  really serves (the check spawns the python3 stdlib HTTP server on an
  ephemeral port and fetches `/`). The page set now covers a real
  quickstart, a full module index (all 21 `go/` modules and all 11
  `web/packages/*` plus the reference-app web host, each linking to its
  own `AGENTS.md`/`README.md` in the repository), and an explicit
  AI-agent orientation page.
- **`llms.txt` is now real** (`docs/site/llms.txt`, at the site root):
  the repository is genuinely public on GitHub and published to GitHub
  Pages (`.github/workflows/docs-site-deploy.yml`), which is exactly the
  condition docs/internal/13-documentation-standards.md names for
  `llms.txt` to belong at a site root rather than the repo root. It is
  not covered by `tools/check_docs_site.py` (which only scans `*.html`
  pages for `href`/`src`) — its links were verified by hand against the
  live repository tree when it was written; re-verify by hand if you
  edit it.
- **Deferred to M4** (docs/internal/13-documentation-standards.md):
  per-version release directories (`v1.2/...` — the site is versioned
  like the code it documents), the complete M4 documentation set
  (config reference generated from schema, an error-code index, ADRs),
  and the machinery decision (a build step / static site generator).
  This hand-written-HTML skeleton deliberately does not anticipate that
  choice.

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

## Publishing

`.github/workflows/docs-site-deploy.yml` deploys this directory to GitHub
Pages on every push to `main` that touches `docs/site/**` (plus
`workflow_dispatch` for a manual re-run), using the official
`actions/configure-pages` + `actions/upload-pages-artifact` +
`actions/deploy-pages` flow with the artifact path set to `docs/site` —
no build step runs; the directory is uploaded exactly as committed.

## What the pages cover

- `index.html` — landing page and site map.
- `quickstart.html` — generating a starter project with `saasctl new`,
  the four `saasctl` commands, and how to run Go commands in this
  repository's workspace layout.
- `modules.html` — the module index: every `go/` module and
  `web/packages/*` package (plus the reference-app web host), each
  linking to its own `AGENTS.md`/`README.md` in the repository.
- `ai-agents.html` — an explicit AI-agent orientation: what to read
  first, the architecture rules that most often matter, and where the
  authoritative implementation status lives.
- `about.html` — what speed is, how its documentation is distributed.
- `status.html` — a coarse, verified-against-the-repository snapshot;
  points at the repository root CLAUDE.md's Repository Status section
  as the authoritative, current source of truth rather than duplicating
  its per-module detail.
- `llms.txt` — the llms.txt convention's entry point for crawlers and
  AI agents fetching this site directly (see above).
