# CLAUDE.md

This file orients an agent working in this repository: where things are,
which language to write in, how to run the checks, and which boundaries a
change must not cross. It is not a record of development status, and it
restates nothing another file already carries:

- The module roster is whatever `go.work` lists. That file is the
  membership authority, and the Makefile derives every module loop from
  it — no prose census here or anywhere else.
- What the architecture is, and why: `docs/design` and `docs/adr`. A
  design question is answered there, never from this file.
- What a module's own contract is: that module's `AGENTS.md`, which
  travels with the code.
- What the checks are: the root `Makefile`. `make help` lists the entry
  points; CI calls one of them.

## Language Rule (read this first)

- **`docs/internal/**`, `docs/design/**`, `docs/adr/**` and `docs/glossary.md` are written in Chinese.** They hold design documents, decision records and the shared glossary.
- **Everything else is English**: code comments, godoc/TSDoc, module docs, per-module `AGENTS.md` files, package READMEs, the root `README.md`, `.claude/skills/**`, and commit messages.
- User-facing product text is bilingual (zh-CN + en-US) and lives in i18n resources, never in code.

## Documentation

- design: docs/design
- adr: docs/adr
- glossary: docs/glossary.md
- language: zh-CN

Requirements documents are not used. Load the `document-standards` skill before creating or editing anything under these paths.

## Commands

Everything runs from the repository root through `make`, and the Makefile
is the only place those commands are written down. `make help` lists the
entry points, one line each; `make check` is the one CI calls. The
toolchain they need comes from `mise install`, which reads `.mise.toml`.

## Where things are

| Path | What it is |
|---|---|
| `go.work` | The module roster. Adding a module here registers it everywhere. |
| `pkg/` | The implementation. One directory per module, each with its own `go.mod` and `AGENTS.md`. |
| `examples/` | Hosts that assemble the modules into a running program. |
| `docs/design`, `docs/adr`, `docs/glossary.md` | The design authority. Chinese, per the language rule above. |
| `tools/` | Repository self-checks. `tools/README.md` says what each one does. |
| `Makefile`, `.mise.toml`, `.golangci.yml`, `.github/workflows/` | The engineering machinery: entry points, toolchain pins, lint configuration, CI. |
| `.claude/skills/` | How the work is done. Each handbook's own description says what it covers; the ones that apply here are at the bottom of this file. |
| `go/`, `web/`, `examples/reference-app`, `docs/internal/`, `docs/site/` | An earlier implementation and its documentation. No command in this repository builds or checks them. |

## Boundaries

- **New code goes under `pkg/`; a program that assembles modules goes
  under `examples/`.** A module directory owns its `go.mod`, its
  `AGENTS.md` and its tests.
- **Read the design before changing a public signature.** The design
  document is where a shape is decided; code that contradicts it is the
  thing to fix, in one direction or the other, before the change lands.
- **A module's own `AGENTS.md` carries boundaries this file does not.**
  Read it before editing inside that module.
- **Every bug fix ships with a test that reproduces the bug** — failing
  before the fix, passing after. If one genuinely cannot be written, say
  so explicitly and say what the follow-up is.
- **Warnings are first-class issues.** Compiler, lint, deprecation and
  race-detector warnings are fixed, not silenced; anything deferred is
  called out rather than left to be discovered.
- **Commit messages are English, in Conventional Commits form, scoped by
  module** — `fix(config): reject a manifest with two mounts on one path`.
- **Rebase onto the target branch and fast-forward.** History stays
  linear.

## Where the rest lives

| Location | Content |
|---|---|
| `docs/design` | What the architecture is: the whole picture in `architecture.md`, one document per module under `modules/`. |
| `docs/adr` | Why it is that way: one decision per file, alternatives included. |
| `docs/glossary.md` | The shared vocabulary. A term used in a design document means what this file says it means. |
| `<module>/AGENTS.md` | The module's own contract: its boundaries and how to run it. |
| `.claude/skills/` | For work under `pkg/` and `examples/`: `document-standards` before writing under the documentation paths, `development-workflow` for how a task travels from statement to merge, `commit-convention` for the commit message. |
