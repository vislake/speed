---
title: Assembly layer — design
weight: 5
description: "Design of go/app — the module discipline's one recorded exception: why a module with no domain is warranted, how its three packages split by dependency cost, and why the loader, the fixed chain order and the seven-stage drive are shaped the way they are."
bookCollapseSection: true
---

# Assembly layer — design

This group holds one module: `go/app`, the **application assembly
layer** — the one module above every other group, with no business
domain of its own, and the subject of the [app design
page](./app/): why a domain-less module is warranted, how the three
packages divide by measured dependency cost, why the middleware order
is fixed and what the two-host composition gate enforces.
