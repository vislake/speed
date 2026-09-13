# speed

A configuration-driven modular application framework, delivered as Go
libraries.

speed gives a Go program a modular skeleton and a set of general-purpose
modules. It does not take over the entry point: a host writes its own
`main`, imports the modules it wants, and drives startup and shutdown.
What the program is — an HTTP service, a gRPC service, a background
process with no inbound entry point at all — is the host's decision, not
the framework's.

A module declares what it provides, what it requires and what
configuration it accepts. Which modules exist in a binary is settled by
what is imported; which of them run is settled by each module's own
reading of the configuration. There is no central list of modules to keep
in step.

## Getting started

Build, test and check everything in the workspace:

```
make check
```

Run the smallest host that assembles a real run, and see the inputs the
assembled modules declared:

```
go -C examples/minimal-host run . --help
```

Run it for real, against the config file its tests use:

```
MINIHOST_CONFIG=file:testdata/local.yaml go -C examples/minimal-host run .
```

It starts, greets, and waits; Ctrl-C stops and closes every module in
reverse dependency order.

`make help` lists the other entry points. The toolchain versions the
checks need are pinned in `.mise.toml`.

## Layout

| Path | What it is |
|---|---|
| `pkg/` | The modules. One directory per release unit, each with its own `go.mod`. |
| `examples/` | Hosts that assemble modules into a running program. |
| `docs/` | The design documents and decision records. |
| `tools/` | Repository self-checks. |
| `go.work` | The module roster every command derives its module set from. |

## Documentation

The design lives in [`docs/design`](docs/design): the whole picture in
`architecture.md`, one document per module under `modules/`. The
reasoning behind each decision, with the alternatives and the price, is
in [`docs/adr`](docs/adr). Terms mean what
[`docs/glossary.md`](docs/glossary.md) says they mean. Those three are
written in Chinese; everything else in the repository is English.

Each module carries its own `AGENTS.md` with the boundaries a change
inside it must respect. `CLAUDE.md` orients an AI coding agent in the
repository as a whole.
