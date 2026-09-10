# tools/configrefgen — the platform configuration-reference generator

A Go tool module (its own `go.mod`/`go.sum`). Its go.work `use` entry
sits outside `go/`, in the consumer-module form — in the workspace,
never published — so the lockstep release coordinator neither tags nor
changeset-version it, and it carries no `go/` release convention
beyond its own matrix row.

It generates the repository's committed configuration reference from
the live configuration schema and the modules' own declarations —
never hand-written (root `CLAUDE.md`'s documentation discipline: "the
configuration reference is generated from the config schema"). The
reference documents the PLATFORM surface a speed-based application
receives from the modules it imports: the declared bootstrap keys, the
dynamic items and feature flags, and the loader mechanism a host drives
to resolve them. A host's own variables are that host's surface and
belong in that host's own docs.

## Files

| File | Role |
|---|---|
| `main.go` | The command: flags, repository-root discovery (the nearest ancestor carrying `go.work`), wiring of the two below |
| `host.go` | The schema-only host: composes the platform modules on a throwaway in-memory database and freezes the schema with `config.Module.Attach` |
| `document.go` | Assembly of the reference and all rendering (Markdown, JSON, site page) |
| `bootstrap.go` | The bootstrap layer's assembly rule: a key belongs to exactly one layer (a key declared on both fails the generator) |
| `outputs.go` | The write path and the `--check` drift gate |
| `config_example.go` | The loader-shaped target struct `config.example.yaml` is load-verified against |
| `configrefgen_test.go`, `main_test.go` | Unit suites: rendered-keys-vs-census correspondence, byte determinism, `run()` end-to-end over a throwaway root |

## Command contract

```
go run .            # regenerate the four artifacts
go run . --check    # drift gate: exits nonzero (printing a diff) on a stale or missing artifact
go test -race ./... # the unit suites
```

Run from this directory; the repository root is located automatically
as the nearest ancestor carrying `go.work` (or pass `--repo-root`).

## The four artifacts

Every output is deterministic and byte-identical across runs; the
repository root is the base for the first three:

- `docs/config-reference.md`
- `docs/config-reference.json`
- `config.example.json` — the JSON counterpart of `config.example.yaml`,
  DERIVED from it so the pair cannot disagree about a key or a value
- `docs/site/content.en/docs/user-guide/configuration.md` — the docs
  site's copy of the reference

## Data sources

- The dynamic layer: `config.Service.Describe()` over the frozen schema
  of the composed host — the modules' own `reg.Config` / `reg.Features`
  declarations, never a hand-kept list.
- The bootstrap layer: `reg.Bootstrap.Keys()` — the modules' own
  `pkgcore.BootstrapKey` declarations.
- `config.example.yaml` (hand-written source, repository root): loaded
  through the real `pkgcore/config` loader by the unit suite; the JSON
  twin is derived from it.

## Gates

- `docs-check.yml` runs `go run . --check` from this directory and
  fails when any committed artifact is stale. Its PATH SET names this
  directory (self-trigger on change), `go/config/**`, the declaring
  modules' `module.go` files, `config.example.yaml`/`.json` and the
  artifact consumers.
- `fast-check.yml` and `full-check.yml` each carry a
  `tools/configrefgen` matrix row (the reusable go-module-ci workflow:
  golangci-lint, `go vet`, `-race` unit tests, the coverage leg — a
  notice-and-pass for a module outside the gated set, this tool module
  is not a released one — plus the workspace-context and `GOWORK=off`
  standalone builds).
- Any change to the rendering, the composition or a source value must
  regenerate the artifacts in the same change: the byte-level `--check`
  is the gate, and hand-editing an artifact is a bug.

## Platform-surface discipline

- **No host variable name and no reference to any host application may
  appear in this module's sources or in its rendered artifacts.** The
  artifacts document the platform surface; a host's own variable names
  are documented where that host lives.
- **An environment-variable name the generator prints must come from
  `config.EnvName` (go/pkgcore/config), never a re-derived spelling** —
  one implementation, so the documented name can never drift from the
  name the loader actually reads.
- **Values in `config.example.yaml` are demonstration values** (`app.db`
  and friends), never a real host's names or paths.
- `tools/check_markdown_examples.py` scans every `AGENTS.md`: a fenced
  `go` block in this file that opens with its own `package` clause is
  really built (its `github.com/vislake/speed/go/...` imports are
  `replace`-directived onto the tree), anything else is syntax-checked.
  Keep examples to fragments that parse.
