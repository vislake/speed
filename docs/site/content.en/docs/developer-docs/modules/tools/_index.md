---
title: Tools
weight: 6
description: "The group at the end of the module design section: saasctl, the consumer-facing CLI — the one deliverable that never composes into a running kernel, and why it ships anyway."
bookCollapseSection: true
---

# Tools

Every other group in this module-design section documents a Go module
or npm package your binary composes: it implements `the module contract`,
registers on the kernel's `Registry` and boots inside a `Kernel`. This
group documents the one deliverable that does none of that — **saasctl**,
the consumer-facing CLI. It implements no `the module contract`, ships no
tables, migrations, HTTP fragment or permissions, and a running product
never serves a route from it. It does not appear in the module
dependency graph at all — it sits above nothing and below nothing.

Its job is the project boundary. speed delivers libraries, and
saasctl is the tool that shapes the application a consumer actually
runs: materialising a starter project (`new`), rewriting the project's
speed requires onto one lockstep release version (`upgrade`), applying
the required modules' schema ahead of a boot (`db migrate`) and
previewing what a boot would run on (`config print`). Every action
happens at development time, against files and a database file, before
and around boots — never inside a running kernel. Its consumers are
the generated projects, which is why the reference app — the mandatory
first consumer of every *library* module — deliberately never wires
it; the design page in this group explains that composition in full.

| Page | What it gives you |
|---|---|
| [saasctl](/docs/developer-docs/modules/tools/saasctl/) | Why the consumer story ships as a CLI module inside the lockstep release — the template that is a working app by construction, the in-place `go.mod` rewrite, migrations driven by the require graph, the bootstrap twin — and why the reference app never wires the tool |

## Related reading

- The user guide documents the same tool from the operator's side:
  [the four commands' usage](/docs/user-guide/modules/tools/saasctl/)
  and the [tools group](/docs/user-guide/modules/tools/) it sits in.
- [Repository and release](/docs/developer-docs/repo-and-release/) —
  lockstep versioning, the context that makes `upgrade` a one-version
  rewrite, and the release coordinator whose version grammar the CLI
  mirrors.
- [Architecture](/docs/developer-docs/architecture/) — the modular
  monolith, the module graph saasctl stands outside, and the
  mandatory-first-consumer rule that shapes the two consumer stories
  this group's design page tells.
