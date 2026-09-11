---
title: Assembly layer
weight: 5
description: "The one module above every other group — go/app: the loader, the seven-stage component drive, the fixed middleware chain and the no-import seam bridges every host's boot composes through."
bookCollapseSection: true
---

# Assembly layer

This group holds one module: `go/app`, the **application assembly
layer** — the only module above every other group in the graph, and the
only one with no business domain. It owns the structure every host's
boot shares (the configuration load, the composition plan, the
seven-stage component drive, the shutdown sequence and the HTTP
helpers), while the host keeps the policy (which components compose,
which values configure them, its routes and its listener). It is also
the module every host's `cmd/server` imports — see
[the app page](./app/) for wiring, the middleware chain, the seam
bridges and the known limitations.
