# pkg/config

The configuration module: it collects what every other module declares it
needs, reads the primary source, applies the environment and command-line
layers over it, and hands each module its own values back.

The root package carries the declarations, the manifest and the layers,
and defines the `Source` and `Format` extension points. Concrete
transports and formats are subpackages: `source/file`, `format/json`,
`format/yaml`.

## Where the authority is

- What this module is and why it is shaped this way:
  [`docs/design/modules/design-config.md`](../../docs/design/modules/design-config.md).
- The decisions behind it: [`docs/adr/`](../../docs/adr/) — its scope, the
  source extension point, the lifecycle position, the assembly bootstrap.
- The API itself: the godoc in this package and in each subpackage.

This file states only what neither of those states: the boundaries a
change here must not cross, and how to run it.

## Boundaries

- **A third-party dependency belongs to the subpackage that needs it, never
  to the root package.** The split exists so a host that configures itself
  in JSON carries no YAML parser. Adding an import to the root package
  spends that budget for every host at once.
- **A transport or a format reaches the assembly by being imported**, as a
  resource of a module registered in `init`. Nothing about it is named in
  the root package.

```go
import _ "github.com/vislake/speed/pkg/config/format/yaml"
```

- **This module depends on `pkg/core` and on nothing else in the
  repository.** It is constructed before every other module, so anything
  it required would have to exist before configuration does.
- **No value is interpreted on a module's behalf.** The declaring module
  gets its own values back and decides what they mean.

## Running it

From the repository root, over every module in the workspace:

```
make build
make test
make lint
make check
```

`make check` is what CI runs. The subpackages are covered by the same
commands; nothing needs to be installed beyond the Go toolchain.
