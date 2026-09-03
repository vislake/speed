# saasctl

saasctl is speed's consumer CLI — the Go half of the product's consumer story (`docs/internal/02-repo-and-release.md` is the design authority). speed ships as independently released modules, not an application; saasctl is the tool that shapes the application a consumer actually runs. It manages the boundary where a project meets the speed modules it pulls in: materializing the project skeleton (`new`, the one command this build wires), and — in later milestones — rewriting a generated project's dependency lines when a new speed release lands (`upgrade`), and maintaining a generated project's database and dynamic configuration (`db migrate`, `config print`). A consumer's code is theirs; the boundary is this CLI's.

The command surface is deliberately small and deliberately stdlib: dispatch is `flag` subcommand handling in `main.go`, no cobra or any third-party CLI dependency. Everything saasctl says is developer tooling, so help text, errors and generated-code comments are English everywhere.

| Concern | Where |
|---|---|
| Command dispatch, root usage text, the planned-command refusal | `main.go` |
| The `new` command: usage/exit-code contract, `--with` parsing and closure validation, speed-root discovery, materialization | `internal/new/new.go` |
| The embedded project skeleton tree and its invariants | `internal/template/` |
| The release-version regex + validator (twin of lockstep-release.py's `VERSION_PATTERN`) | `internal/version/` |
| Package doc comment | `doc.go` |

## Status: what this build wires

This build wires `new` only. `saasctl upgrade`, `saasctl db migrate` and `saasctl config print` are named on the usage surface and refused with a clear not-implemented message and exit code 1 until their milestones land. In this build saasctl needs a speed checkout on disk at run time (see Speed-root resolution) — nothing is published, so every materialization points at a checkout. `web/package.json` rewriting and the frontend-scaffold tool (`create-saas-app`) belong to a separate round.

## The embedded template (`internal/template/`)

The project `saasctl new` materializes is not synthesized in code: it is an embedded tree of real files (`project/` under `internal/template/`) that are a working app by construction — they were produced during development by materializing, `go mod tidy`-ing and building them against a real speed checkout, then converting the paths back to tokens for embedding. The tree mirrors the reference app's `cmd/server` shape with every demo-specific piece removed: no notes module, no demo tenants, grants, membership store or subject resolver, no seeded data. A generated project is an honest skeleton whose host seams — authn's `MembershipReader`, org's `SubjectResolver`, the config resolver — are left unwired and fail closed per each module's contract, each named by a doc comment in the generated files as the owner's first task.

Three conventions hold the template together, each test-pinned on the template side and enforced again from `internal/new`'s side (the two packages cannot import each other's tests, so both sides assert their half of the shared ground truth):

- **Compile containment (A2).** Every embedded `.go` file starts with the line `//go:build ignore`, so the template tree never compiles, vets or lints as part of this module; `new` strips exactly that one line (plus its blank line) at materialization. The generated project's compile-correctness is proven by real builds of materialized apps, never by compiling the assets in place. A template edit that forgets the marker fails in two places: the embed-tree walk in `embed_test.go` and `StripBuildIgnore`'s error path when `new` materializes.
- **Selection (A5).** Only two files differ between selections: `selection/<key>/go.mod.txt` and `selection/<key>/server.go` (the go.mod require set with replace directives, and the import, module-construction and middleware set). The go.mod document carries the inert `.txt` name through the embed for a mechanical reason — go:embed's directory scan refuses to descend into a subdirectory containing a file named `go.mod`, reading such a directory as a nested module root, so a `go.mod` named as such would silently vanish from the embedded tree — and `new` renames it to `go.mod` at materialization. `<key>` is `SelectionKey`'s canonical rendering — sorted module names joined with `+`, or `none` for the empty set. The v0.1 switchable universe is `{authn, rbac, org}`: the three implemented modules that participate in the minimal combination. The required five (pkgcore, dbkit, tenancy, config, observability) have no off option. Every legal selection ships an embedded directory; an extra directory is dead weight the template tests refuse.
- **Tokens (A4).** Two tokens appear in template files and nowhere else: `__APP_NAME__` (the module path, derived at `new` time from the target directory's base name) and `__SPEED_ROOT__` (the resolved speed checkout path). Materialization substitutes them; tests assert no token survives in a materialized tree, so a template edit introducing a third token fails until the substitution learns it.

The five legal selections are the minimal combination `{authn, rbac, org}` plus its four closures. Each selection's `server.go` composes its modules in a fixed order that is also the migration-registration order (`[]pkgcore.Module{...}`); `config.go` (shared) parses the full uniform env surface of a generated project — `SPEED_CONFIG_KEY`, `SPEED_ORG_INDEX_KEY`, `SPEED_DB_PATH`, `SPEED_DEPLOYMENT_MODE`, `PORT` — so a consumer's bootstrap contract never changes with the selection.

## The `new` contract

`saasctl new [flags] <target-directory>` materializes the skeleton into a new directory and prints the written files plus next steps. The exit-code contract, pinned by tests:

- **0** — success, and `-h`/`--help` (help is printed, nothing is written).
- **2** — usage errors: bad flags, a wrong argument count, a target whose base name cannot serve as a module path (the name becomes the go.mod module path; the grammar is stricter than `go mod init`'s own — no leading dot or dash, no space, no trailing dot, no `.` or `..` — and every accepted name is one `go mod init` accepts, probed against go 1.25), or an invalid `--with` value.
- **1** — execution errors: an unresolvable speed root, an existing non-empty target, an I/O failure.

`--with` is positive selection with downward-closure validation: selecting `rbac` or `org` without `authn` is an error naming `authn` as implied (their routes need an authenticating layer), and unknown names are rejected listing the valid set. There is no `--without`: closing `authn` is expressed by not listing it, and dependents cannot be selected without it — the v0.1 correction note to `docs/internal/11-cross-cutting.md`'s module-switch section.

An existing non-empty target is refused before anything is written (only an existing *empty* directory is accepted and filled); on a write failure everything the run created is removed again. The written file list is fixed and relative, never derived from a walk of the target, so a hostile or stale target tree cannot smuggle paths into the write.

## Speed-root resolution

The go.mod `replace` directives point at a speed checkout — the transition-state shape a consumer go.mod carries until the M4 first-release cleanup (requires at `v0.0.0-00010101000000-000000000000`, `replace ... => <speed-root>/go/<m>` for every workspace module in the graph, full indirect require block, no go.sum: the first consumer-side `go mod tidy` builds it, normal for a fresh Go project). The checkout path resolves in three tiers: `--speed-root` flag, then the `SPEED_ROOT` environment variable, then a walk up the working directory's ancestors for a `go.work` whose `use` entries include `go/pkgcore` (the pattern of `tools/release/lockstep-release.py`'s `find_repo_root`, sharpened so an unrelated go.work up the tree cannot be mistaken for a speed checkout). A resolved path is validated to actually hold that go.work before anything is written.

## Testing

The module's unit suite runs entirely offline (no testcontainers, no network — the materialization tests fabricate a fake speed checkout as a temp directory holding a `go.work`, and compare materialized bytes against the embedded assets with tokens substituted, which is also the go.mod golden check). Run from the module directory: `go build ./...`, `go vet ./...`, `golangci-lint run ./...`, `go test ./... -race`. Layout follows the repository convention: `new_test.go` covers `new.go`, `embed_test.go` covers the template package's invariants, and `version_test.go` plus `example_test.go` (the module's runnable godoc example) cover the version validator.

The real proof — materializing each legal selection to a temp directory and running a genuine `go mod tidy` (network) and `go build` there — is the development proof behind the committed template goldens (the go.mod files embedded in `internal/template/project/selection/` are the tidy-pruned results of exactly that run, converted back to tokens), and it doubles as this module's mandatory-first-consumer evidence: saasctl's consumers are generated projects, not the reference app, and the generated projects really compile. A formal, repeatable end-to-end consumer proof (new → tidy → build → boot → smoke) lands with the B4 milestone and is run as that block's gate.

## Known limitations and deferred work

- `upgrade`, `db migrate` and `config print` are not implemented in this build (see Status). The planned-command refusal naming them on the usage surface is the one place this file's claims are testable today.
- `db migrate`'s module universe is the migration-shipping root modules present in the project go.mod's speed requires — subpackage modules such as `go/dbkit/audit` stay deliberately outside the CLI universe: audit migrations apply through the app's own startup `Apply` when the app wires `audit.New` (the reference-app pattern), and startup Apply is idempotent via `schema_migrations`, so CLI-then-boot and boot-only agree.
- saasctl ships no integration tier; pr-full's integration matrix is untouched. PostgreSQL legs of anything saasctl drives are proven by the owning modules' own integration tiers.
- Deferred with authority (see `docs/internal/02-repo-and-release.md` and the roadmap): `create-saas-app` and the web-side template (frontend-scaffold round); `saasctl openapi generate` (app-owned API-fragment flow, `docs/internal/21-api-contract.md`); dynamic-config print of `configs`-table values with tenant scopes and schema-driven redaction; a switchable universe beyond `{authn, rbac, org}`; the scaffold-verify workflow's M4 dual-mode boot gate; version discovery for `upgrade` without `--version` (nothing publishes before M4).
