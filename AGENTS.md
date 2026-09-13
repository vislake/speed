# AGENTS.md

Orientation for any AI coding tool that found its way here by convention.

This repository is a configuration-driven modular application framework,
delivered as Go libraries: the modules live under `pkg/`, hosts that
assemble them under `examples/`, the design under `docs/`.

**`CLAUDE.md` is the full guide** — where things are, which language to
write in, and the boundaries a change must not cross. Read it before
editing anything. A module's own `AGENTS.md` carries the boundaries
specific to that module.

## Commands

Everything runs from the repository root. `make help` prints this list
from the Makefile itself:

```
make help        make build       make test        make test-race
make modules     make fmt         make lint        make tidy
make check       make tidy-check  make tools-test
```

`make check` is the one CI runs.
