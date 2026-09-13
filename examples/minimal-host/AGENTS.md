# examples/minimal-host

The smallest host that assembles a run: a `main` that imports the modules
it wants and drives the lifecycle. It is a module of the workspace like
any other, and it is the working proof that the mechanism assembles
something — what its `main` does not need to contain is as much the point
as what it does.

Everything in the directory except `main.go` and `host.go` is an ordinary
module: two implementations of one capability, and an application module
that takes the capability up without naming either.

## Where the authority is

- The mechanism it exercises:
  [`docs/design/modules/design-core.md`](../../docs/design/modules/design-core.md)
  and
  [`docs/design/modules/design-config.md`](../../docs/design/modules/design-config.md).
- The decisions behind the shape of a host:
  [`docs/adr/`](../../docs/adr/) — the assembly bootstrap above all.
- What this host itself demonstrates: the package comment in `main.go`
  lists the cases, each with a test in `e2e_test.go`.

## Boundaries

- **The host contributes its identity as a resource on a module it
  registers**, not as an argument of the driver. A change that grows the
  driver's signature to carry host input is the shape to reject.
- **An example stays an example.** Nothing here is imported by a released
  module, and nothing here is a place to put code the modules should
  carry: if a host would have to write it again, it belongs in a module.
- **Keep the demonstrated cases and their tests together.** A case the
  package comment claims and `e2e_test.go` does not cover is a lie the
  next reader has no way to catch.

## Running it

From the repository root, over every module in the workspace:

```
make build
make test
make lint
make check
```

`make check` is what CI runs. The host's own behaviour is covered by
`e2e_test.go`, which builds the binary and runs it; it needs nothing
installed beyond the Go toolchain.
