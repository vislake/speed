# pkg/core

The module mechanism: the descriptor a module registers, the registry that
holds registrations and instances, the queries modules reach each other
through, and the lifecycle driver.

## Where the authority is

- What this module is and why it is shaped this way:
  [`docs/design/modules/design-core.md`](../../docs/design/modules/design-core.md).
- The decisions behind it, one file per decision:
  [`docs/adr/`](../../docs/adr/) — its scope, module registration, module
  resources, capability tokens, dependency resolution, module enablement,
  the lifecycle stages, packaging.
- The API itself: the godoc in this package. Read `core.Module`'s field
  comments before adding a callback or a declaration seat.

This file states only what neither of those states: the boundaries a
change here must not cross, and how to run it.

## Boundaries

- **No third-party dependency, ever.** Every module imports this one, so a
  dependency added here lands in every host's dependency list. The
  standard library is the whole budget.
- **No knowledge of any capability, and no interpretation of any
  resource.** Both travel through the registry as opaque values; the
  module that defines a type is the one that reads it back. A `switch` on
  a concrete capability or resource type in this package is the shape to
  reject.
- **The descriptor's named fields are limited to what the registry itself
  interprets.** Outward metadata a mechanism elsewhere consumes is a
  resource, not a new field.
- **Registrations and the driver are single-goroutine.** Descriptors are
  written before `Run` and read-only after it; stages are strictly ordered
  and callbacks within a stage run one at a time.

## Shape of a module

A capability is an interface; a token is a typed nil pointer to it; a
provider declares it in `Provides` and a consumer in `Requires`. The
requirement is also what orders construction and shutdown.

```go
package greeting

import (
	"context"

	"github.com/vislake/speed/pkg/core"
)

// Greeter is a capability: a set of methods, named by an interface.
type Greeter interface {
	Greet(name string) string
}

type plain struct{}

func (plain) Greet(name string) string { return "hello, " + name }

// Provider delivers the capability.
func Provider() core.Module {
	return core.Module{
		Name:     "greeting",
		Provides: []core.Provision{{Token: (*Greeter)(nil)}},
		New: func(context.Context, *core.Registry) (any, error) {
			return plain{}, nil
		},
	}
}

// Consumer takes the capability up without naming an implementation of it.
func Consumer() core.Module {
	return core.Module{
		Name:     "app",
		Requires: []core.Requirement{{Token: (*Greeter)(nil)}},
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			greeter, err := core.Resolve[Greeter](reg)
			if err != nil {
				return nil, err
			}
			return greeter.Greet("world"), nil
		},
	}
}
```

## Running it

From the repository root, over every module in the workspace:

```
make build
make test
make lint
make check
```

`make check` is what CI runs. Tests here are the same-package unit suite
and need nothing installed beyond the Go toolchain.
