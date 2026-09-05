---
title: About
weight: 5
---

# About speed

speed is the shared foundation of a multi-tenant SaaS product family. It
is **not an application**: it is a set of independently released Go
modules and npm packages, versioned in lockstep, that a business project
pulls in and compiles into a single binary. Modules call each other
in-process — there is no service discovery and no Kubernetes-shaped
infrastructure.

## The shape

| Aspect | Design |
|---|---|
| Dependencies | Strictly bottom-up: the foundation modules (`pkgcore`, `dbkit`, `observability`, `ratelimit`, `tenancy`) underpin everything above them. |
| Deployment mode and composition | Two orthogonal axes. Every infrastructure dependency is an interface with several implementations (an in-process one, plus PostgreSQL, Redis, S3 and other backed ones). The **deployment mode** does not select an implementation, it only constrains one: each implementation declares its capabilities, and assembly fails when the composition cannot run in the declared mode. A single-process deployment may talk to real external services. Business code never branches on the mode. |
| Multi-tenancy | A shared database with `tenant_id` isolation, guarded by a GORM plugin, a mandatory generic repository base, and PostgreSQL row-level security in the distributed mode. |
| Versioning | Lockstep: all modules and packages share one version and release together; only same-version combinations are supported. |

## How documentation is distributed

Each module carries its own `AGENTS.md` inside the module — a quick
orientation for an AI coding tool: responsibility boundary, public API,
typical usage, and explicit dos-and-don'ts — plus a plain `README.md`,
so documentation ships with the code and stays current with the version
a consumer actually pulls. (A per-module `docs/usage.md` is the
longer-term plan recorded in
[docs/internal/13](https://github.com/vislake/speed/blob/main/docs/internal/13-documentation-standards.md);
as of this writing exactly one module, `go/notification`, has created
one —
[go/notification/docs/usage.md](https://github.com/vislake/speed/blob/main/go/notification/docs/usage.md),
written as a template for the rest, its own header says as much — while
every other Go module and every npm package still relies on just its
`AGENTS.md` plus its `README.md`.) This site is the *central* reference
across modules — see [Modules](../modules/) for the full index — planned
to be versioned per release, with the module-level docs pointing at it
for the overview.

## This site today versus the plan

**The machinery decision is made**: this site is built with
[Hugo](https://gohugo.io) and the
[hugo-book](https://github.com/alex-shpak/hugo-book) theme, bringing
forward the M4 static-site-generator decision that
[docs/internal/13-documentation-standards.md](https://github.com/vislake/speed/blob/main/docs/internal/13-documentation-standards.md)
had deliberately deferred — the same "landed ahead of schedule" pattern
this repository's own `storage`/`notification` module rounds set as
precedent. hugo-book was chosen over the other candidate, Docsy: Docsy
pulls in Hugo Modules plus a Node/PostCSS asset pipeline (it sources its
Bootstrap and Font Awesome assets from npm, and needs a Dart Sass
compiler on `PATH`, per its own current setup docs), reintroducing
exactly the Node dependency this directory had deliberately avoided
until now, while hugo-book has no such dependency — its current release
(v0.15.0) dropped even its earlier Sass dependency — and needs only the
Hugo binary itself. Bilingual (English / 中文) content is **Hugo's own
multilingual mode**, not a theme feature: either theme candidate could
have served the i18n requirement equally well, since Docsy's Bootstrap
chrome and hugo-book's plain chrome both sit on top of the same
Hugo-core language machinery.

Real now: content in both languages under `content.en/` and
`content.zh-cn/` (Hugo's translation-by-content-directory convention,
matching this theme's own documented structure), a working language
switcher in every page's header, `hugo --minify` producing the site
under `public/` (gitignored, never committed), and a real `llms.txt` at
the built site's root, updated to the URLs this Hugo configuration
actually produces. The structural check keeping the built output honest
in CI is `tools/check_docs_site.py` (the docs-check pipeline) — it now
builds the site and checks the same properties (required pages present,
internal links resolve, the site serves) against `public/`, not against
a hand-written HTML source tree.

Deferred to a later milestone (M4): per-version release directories
(this site versioned like the code it documents) and the complete
documentation set (an error-code index, ADRs surfaced on the site, a
generated configuration reference).
