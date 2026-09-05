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

## Current state: real content, real machinery

This round (Hugo migration) made the machinery decision
docs/internal/13-documentation-standards.md had deliberately deferred to
milestone M4, brought forward at the user's explicit request — the same
"landed ahead of schedule" pattern this repository's own
`storage`/`notification` module rounds already set as precedent. The
directory is no longer a hand-written HTML skeleton: it is a real
[Hugo](https://gohugo.io) project.

- **Theme: [hugo-book](https://github.com/alex-shpak/hugo-book)**, not
  [Docsy](https://www.docsy.dev), the other candidate the user named.
  The choice was verified against both themes' real, current setup
  documentation before deciding, not assumed:
  - Docsy requires a **recent extended Hugo build** (0.160.1+ per its
    own prerequisites page) *and* **Node.js/npm** — its own current
    docs say plainly "Docsy sources its Bootstrap and Font Awesome
    assets from npm, so you need Node.js" — plus a Dart Sass compiler
    on `PATH`. That reintroduces exactly the Node dependency this
    directory had deliberately avoided until now.
  - hugo-book needs only the Hugo binary itself (extended edition, for
    Sass — used only by the theme's own asset build, not by anything
    this site's content does) and Hugo v0.158+; its current release
    (v0.15.0, verified as the theme's latest at the time this decision
    was made) dropped even its earlier Sass dependency. No Node, no
    npm, no PostCSS.
  - Bilingual content is **Hugo's own multilingual mode**, not a theme
    feature — either theme could have served the i18n requirement
    equally well, since both themes' chrome sits on top of the same
    Hugo-core language machinery. The theme choice therefore turned
    entirely on the dependency-footprint question above, and hugo-book
    won it outright.
  - The theme is pinned as a git submodule at
    `docs/site/themes/hugo-book`, tag `v0.15.0`
    (commit `027be3349d20785efeb93f6773872a35497d7258` — see
    `.gitmodules` for the pin comment). Bump it deliberately; never via
    `git submodule update --remote`.
- **Bilingual (English + 中文) via Hugo's own multilingual mode**: `en`
  is the default/primary language (English-first, per
  docs/internal/13-documentation-standards.md's rule for this
  directory) with content under `content.en/`; `zh-cn` is the second
  language with content under `content.zh-cn/` — Hugo's
  translation-by-content-directory convention, matching the theme's own
  documented structure (its `exampleSite/hugo.toml` uses the identical
  `contentDir` shape). With `defaultContentLanguageInSubdir = false`
  (Hugo's own default), English pages render unprefixed and zh-cn pages
  render under an explicit `/zh-cn/` prefix — verified against a real
  `hugo --minify` build's `public/` output, not assumed. A working
  language switcher (the theme's own `book-languages` menu entry) is
  visible on every page — this was checked directly rather than
  assumed: the theme's `landing` layout suppresses the entire left
  sidebar (where the switcher lives), so this site deliberately does
  **not** use that layout for its home page, keeping the same page
  chrome — switcher included — on every page, home included.
- **`llms.txt` stays real and single**: `docs/site/static/llms.txt` is
  copied verbatim by Hugo to the built site's one root
  (`docs/site/public/llms.txt`), never split per language, per the
  `check_docs_site.py` gate below. Every internal link inside it was
  updated to the real URLs this Hugo configuration produces (verified
  against an actual build, not guessed) and every GitHub blob link (to
  `AGENTS.md`/`README.md` files this site does not reproduce) stays an
  absolute `github.com` URL, unchanged in kind from before.
- **The structural check now validates the built output**:
  `tools/check_docs_site.py` runs a real `hugo --minify` build (the
  docs-check pipeline installs the pinned Hugo binary first) and checks
  the same properties as before — required pages present, internal
  links resolve, the offline preview really serves — against
  `docs/site/public/` (gitignored, never committed), not against a
  Markdown source tree. It also checks that the build itself produces
  no warnings and that `llms.txt` lands exactly once.
- **Local preview**: `hugo server --minify --source docs/site` (or the
  Taskfile `docs:serve` task, which runs exactly that) — Hugo's own
  live-reload dev server, not a plain static-file server. Needs the
  pinned Hugo binary (`.mise.toml`'s `hugo` entry;
  `.github/actions/setup-hugo-env` mirrors the same pin in CI) and the
  theme submodule checked out (`git submodule update --init` if this
  clone predates the submodule).
- **Deferred to M4** (docs/internal/13-documentation-standards.md):
  per-version release directories (the site is versioned like the code
  it documents) and the complete M4 documentation set (a generated
  configuration reference, an error-code index, ADRs surfaced on the
  site).

## Language

English-first, per docs/internal/13-documentation-standards.md's
language table — `content.en/` is the primary, default-language
content. `content.zh-cn/` carries a genuine, technically precise
Chinese translation of every page (not a machine-translated stub),
matching the register of this repository's own Chinese prose in
`docs/internal/**`. Because both a localization directory and a
vendored theme submodule legitimately carry non-English or generated
content, the whole `docs/site/` subtree stays exempt from
`tools/scan_cjk.py` (`CARVED_SUBTREES` in that script — unconditional
for this directory, not conditioned on a particular content-directory
naming scheme, so this exemption survives any future Hugo
reconfiguration of where zh-cn content lives).

## Directory structure

```
docs/site/
  hugo.toml              Hugo configuration: theme, multilingual mode, params
  content.en/             English content (the default/primary language)
    _index.md             Home / landing page (site map)
    docs/
      _index.md           Documentation section landing page
      quickstart.md
      modules.md
      ai-agents.md
      about.md
      status.md
  content.zh-cn/          Chinese content, identical structure, real translations
  i18n/
    zh-cn.yaml            Project-level override for the theme's own UI strings
                           (the theme ships its Chinese translations under the
                           key "zh", not "zh-cn" -- see the file's own header)
  static/
    llms.txt              Copied verbatim to the built site's root
  themes/hugo-book/        Pinned git submodule (see .gitmodules)
  public/                 Hugo build output -- gitignored, never committed
```

## Preview locally

    hugo server --minify --source docs/site

then open the URL it prints (typically `http://localhost:1313/speed/`).
That is exactly what the Taskfile `docs:serve` task runs. Ctrl-C stops
it; pass `--port` to use a different one.

## Build

    hugo --minify --gc --source docs/site

Output lands in `docs/site/public/` (gitignored). This is exactly what
`.github/workflows/docs-site-deploy.yml` runs before uploading the
result to GitHub Pages, and what `tools/check_docs_site.py` runs before
validating it.

## Validate from the repository root

    python3 tools/check_docs_site.py

Runs a real Hugo build (pass `--skip-build` to reuse an existing
`docs/site/public/` instead) and validates the built output — see the
script's own docstring for the full list of properties it checks. It
also runs in CI on every PR that touches this site, the checker itself,
or the pinned Hugo install (docs-check pipeline,
`.github/workflows/docs-check.yml`).

## Publishing

`.github/workflows/docs-site-deploy.yml` deploys this site to GitHub
Pages on every push to `main` that touches `docs/site/**` (plus
`workflow_dispatch` for a manual re-run), using the official
`actions/configure-pages` + `actions/upload-pages-artifact` +
`actions/deploy-pages` flow. Unlike before the Hugo migration, a real
build step runs first (`hugo --minify --gc`) and the artifact path is
Hugo's output directory (`docs/site/public`), not `docs/site/` itself.

## What the pages cover

- Home (`content.<lang>/_index.md`) — landing page and site map.
- `docs/quickstart.md` — generating a starter project with
  `saasctl new`, the four `saasctl` commands, and how to run Go
  commands in this repository's workspace layout.
- `docs/modules.md` — the module index: every `go/` module and
  `web/packages/*` package (plus the reference-app web host), each
  linking to its own `AGENTS.md`/`README.md`.
- `docs/ai-agents.md` — an explicit AI-agent orientation: what to read
  first, the architecture rules that most often matter, and where the
  authoritative implementation status lives.
- `docs/about.md` — what speed is, how its documentation is
  distributed, and this round's machinery-decision rationale in full.
- `docs/status.md` — a coarse, verified-against-the-repository
  snapshot; points at the repository root CLAUDE.md's Repository Status
  section as the authoritative, current source of truth rather than
  duplicating its per-module detail.
- `static/llms.txt` — the llms.txt convention's entry point for
  crawlers and AI agents fetching this site directly, copied verbatim
  to the built site's root.
