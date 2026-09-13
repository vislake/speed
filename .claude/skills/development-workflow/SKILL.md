---
name: development-workflow
description: Development workflow standards — how a task travels from statement to merged commit: branch discipline, the reference sweep that splitting and moving demand, staged implementation, the bug-fix test rule, the warning rule, verification and fast-forward merging. Also defines the coder role, a long-running coordinator session that delivers every task through a workflow instead of editing code itself. Load before starting, finishing or merging any development task.
triggers:
  - starting a development task
  - planning a change
  - creating a branch
  - splitting or moving files
  - renaming a symbol across the repository
  - fixing a bug
  - finishing a task
  - verifying a change
  - merging a branch
  - running as a coordinator session
  - running unattended
  - dispatching a workflow
  - reporting task progress
  - a CI failure
globs:
  - "**/*"
---

# Development Workflow

The authoritative standard for **how a change travels** — from a stated task to a
merged commit. It does not say how to write code, how to word a commit or how to
write a document; each of those has its own standard, and this one points at them
rather than restating them.

**The premise that overrides everything else**: a task is finished when the work
is verified, merged and reported truthfully — not when the code is written.
Every rule below follows from that premise, and the premise outranks convenience
at every step.

---

## 1. Scope

| Question | Standard |
|---|---|
| How does a change travel from task to merge? | [Task to Merge](references/task-to-merge.md) |
| How is the code itself written? | The project's coding standards |
| How is a commit message worded? | The project's commit convention |
| How is a document written? | The project's documentation standard |

Where this standard and a project's own rule disagree, the project's rule wins;
this one carries what holds regardless of language, stack and repository layout.

## 2. Configuration

Workflow settings are declared in `CLAUDE.md` under a fixed heading, with
lowercase keys:

```markdown
## Development

- plans: .claude/plans
```

| Key | Default | Meaning |
|---|---|---|
| `plans` | `.claude/plans` | Where plan documents live |

**This standard configures only what it introduces.** A plan document is its own
invention, so it declares where those live. Everything else a workflow needs is
already a fact of the repository or of the project, and restating it here would
only create a second copy to keep in sync:

- **The target branch comes from the repository.** Read it from `origin/HEAD`, or
  from whichever of `main` / `master` the repository actually has.
- **The test and lint commands come from the project** — its own command
  documentation, then its task runner (`Taskfile.yml`, `Makefile`,
  `package.json` scripts). If neither answers, **ask the user**. Never invent a
  command, and never report a check as passing because no command was found to
  run.
- **Policy numbers come from the team** — how long a branch may live, how many
  tasks run at once. A project that fixes one records it as its own rule.

The `plans` directory sits **in the main checkout** and is **excluded from
version control, not committed** — add it to `.git/info/exclude`. Progress notes
are process records; git history and the code are the durable ones. Being
untracked, a plan document does not travel with a branch or a working tree, so
there is exactly one of it, at one path, for everyone to read and write.

## 3. Roles

A session runs in exactly one of two shapes. The shape holds for the whole
session; it is not chosen per task.

- **Direct contributor** — plans, edits, tests and merges the work itself.
- **Coder** — a long-running coordinator that never edits product code. It
  receives tasks, delivers each through a workflow, monitors progress and CI, and
  reports. **[The Coder Role](references/coder-role.md)** is mandatory reading
  before running as one.

[Task to Merge](references/task-to-merge.md) applies to both, in full. The Coder
Role adds to it and never replaces it.

**A session is a direct contributor by default.** It becomes a coder only when
the user says so explicitly, and stays one until the user says otherwise.

- **DO NOT promote yourself.** A task being large, parallelizable or long-running
  is not a reason to become a coder — a direct contributor handed a large task
  splits it and does it.

**A coder is attended by default.** The user can put one in **unattended mode**,
where an unclear point is decided by the coder, or settled with the session that
sent the task, and never put to the user ([The Coder
Role](references/coder-role.md), section 9). Like the role itself, only the user
turns it on and only the user turns it off — and it relaxes no rule about what
gets delivered.

## 4. The Standards

| Standard | Content |
|---|---|
| [Task to Merge](references/task-to-merge.md) | The universal standard: branch discipline, the reference sweep that splitting and moving demand, implementation, tests, warnings and verification, merging. Applies to every role and every task. |
| [The Coder Role](references/coder-role.md) | What the coder role adds on top: how tasks arrive, the workflow that delivers each one, the plan document, the heartbeat, the CI watch, and unattended mode. Read before running as a coder. |

`templates/` holds the starting points: `plan.md` for a plan document,
`heartbeat-report.md` for the coder's periodic report.
`scripts/check_plan.py` checks a plan document against `plan.md`'s structure —
that one thing, and nothing else about how the work was done.
