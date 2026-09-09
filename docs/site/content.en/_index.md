---
title: speed
---

# speed

A modular monolith distributed as libraries: independently released Go
modules and npm packages that business projects pull in via `go get` /
`npm install` and compile into one binary. Pick the modules your SaaS
needs — identity, tenancy, notifications, billing, AI, compliance —
and compose them through a kernel assembly, instead of building each
layer yourself.

This site has two main sections.

## Build your SaaS on speed

For teams **using** speed's modules in their own product: the quick
start, domain-by-domain tutorials, and a complete per-module reference
with runnable examples.

- [User guides](/docs/user-guide/) — start here.
- [Quickstart](/docs/quickstart/) — a starter project in five minutes.
- [Error code index](/docs/user-guide/error-codes/) — the complete
  list of codes a speed-based API can answer with.

## Develop speed itself

For **developers working on speed**: the architecture, the design
principles behind it, and a per-module design deep dive that explains
*why* each module is shaped the way it is.

- [Developer docs](/docs/developer-docs/) — start here; the section
  covers the architecture (the modular monolith, the module dependency
  direction, the deployment mode / implementation composition axes),
  the design principles, and the per-module design deep dives.

## For AI agents

Reading this site as a coding agent? Start at
[For AI Agents](/docs/ai-agents/) for what to read first, and grab
[/llms.txt](/llms.txt) at this site's root for the machine-readable
index of every page. The repository's own
[root `AGENTS.md`](https://github.com/vislake/speed/blob/main/AGENTS.md)
remains the authoritative orientation for repository work.

This site is built with [Hugo](https://gohugo.io) and the
[hugo-book](https://github.com/alex-shpak/hugo-book) theme. A language
switcher (English / 中文) sits in the header of every page.
