---
title: About
weight: 5
---

# About speed

speed is a modular monolith distributed as libraries: independently
released Go modules and npm packages that a business project pulls in
with `go get` / `npm install` and compiles into one binary. It is not
an application you run and not a repository you fork — you assemble
the modules your product needs through one assembly, and own the result.

| Decision | What it means in practice |
|---|---|
| Shape: modular monolith | The modules compile into your binary and call each other in-process — no service discovery, no network hops between them. |
| Distribution: as libraries | Every exported signature change propagates to every delivered project, so public API is frozen unless a breaking change is intentional; every dependency added lands in someone else's `go.sum` or bundle; implementation details live under `internal/`. |
| Deployment mode and composition | Two orthogonal axes. Every infrastructure dependency is an interface with several implementations (an in-process one, plus PostgreSQL, Redis, S3 and other backed ones). The **deployment mode** does not select an implementation, it only constrains one: each implementation declares its capabilities, and assembly fails when the composition cannot run in the declared mode. A single-process deployment may talk to real external services. Business code never branches on the mode. |
| Multi-tenancy | A shared database with `tenant_id` isolation, guarded by a GORM plugin, a mandatory generic repository base, and PostgreSQL row-level security in the distributed mode. |
| Versioning | Lockstep: all modules and packages share one version and release together; only same-version combinations are supported. |

## How the documentation is distributed

Each module ships its own `AGENTS.md` inside the module — orientation
for anyone (human or AI tool) working with it: responsibility and
boundary, public API, wiring requirements, known limitations — so
documentation travels with the code and stays current with the version
a consumer actually pulls.

This site is the central reference across modules. It has two main
sections: the [user guides](/docs/user-guide/) (build your SaaS on
speed) and the [developer docs](/docs/developer-docs/) (develop speed
itself), each with a per-module reading. Module pages link the
module's own `AGENTS.md` for the authoritative description.

[/llms.txt](/llms.txt) at the site root carries the machine-readable
index of everything for agents and crawlers.
