---
title: speed
---

# speed

A modular monolith distributed as libraries: independently released Go
modules and npm packages that business projects pull in via `go get` /
`npm install` and compile into one binary.

{{<button href="/docs/quickstart/">}}Quickstart{{</button>}}
{{<button href="/docs/modules/">}}Modules{{</button>}}

## On this site

- [Quickstart](docs/quickstart/) — generate a starter project with
  `saasctl new`, and the four `saasctl` commands.
- [Modules](docs/modules/) — the concrete list of Go modules and npm
  packages a consumer project can pull in, each linking to its own docs.
- [For AI Agents](docs/ai-agents/) — what to read first, and the
  architecture rules that most often matter when an AI coding agent is
  doing the integrating.
- [About speed](docs/about/) — what the project is, how the pieces fit,
  and how its documentation is distributed.
- [Status](docs/status/) — where implementation genuinely stands today.

This site is built with [Hugo](https://gohugo.io) and the
[hugo-book](https://github.com/alex-shpak/hugo-book) theme — see
[About](docs/about/) for the machinery decision and what is still
deferred to a later milestone. A machine-readable
[llms.txt](/speed/llms.txt) lives at this site's root, and a language switcher
(English / 中文) sits in the header of every page.
