# tools/ — repo scripts

Plain, dependency-free Python scripts (standard library only, Python >= 3.11
for `tomllib`) that back the repository's cross-cutting disciplines and its release machinery: three discipline checkers, the dependency-license scanner with its committed manifest, one scaffold generator, the semgrep architecture-discipline ruleset under `tools/semgrep_rules/` with its planted-violation fixtures, and the lockstep release coordinator (a release tool, not a discipline checker — it follows the same convention, which is why it lives here). The checkers are the local-run counterparts of the CI discipline checks scheduled in `docs/internal/18-cicd.md` (the table rows for banning CJK outside `docs/internal/`, for requiring identical zh-CN/en-US message-key sets, and for making every tenant-scoped Repository run the tenancytest isolation suite, all marked there as self-written scripts); CI workflows mount them under `tools/`. Two further scripts are repo self-checks rather than 18-cicd discipline rows: `tools/check_toolchain.py` gates the tool versions the root `.mise.toml` pins — mirrors of the authoritative sources CI actually reads (Taskfile.yml header, `go.work`, `web/.nvmrc`, `web/package.json`, setup-go-env's `GOLANGCI_VERSION`) — proving the mirrors cannot drift, and pr-check's repo-checks job runs it; `tools/check_docs_site.py` validates the docs-site skeleton (required entry files, internal links, offline preview) and the docs-check pipeline runs it. The generator is the backend of the `task new:module` promised by `docs/internal/19-dev-workflow.md`. The release coordinator (`release/lockstep-release.py`) is the M0 deliverable for the roadmap's lockstep-release item (`docs/internal/02-repo-and-release.md`, `docs/internal/18-cicd.md`), an offline verification of the full one-version release plan, wrapped by the root Taskfile's `release:plan` task and mounted by `.github/workflows/release.yml`; its unittest suite and go.mod fixtures live beside it under `tools/release/`. Nothing here needs anything beyond `python3` except the semgrep ruleset, which needs a semgrep binary to run (see the ruleset section for the pinned local version and the CI shape), and the checkers print hit paths relative to their `--root`.

| Script | Kind | Enforces / does | Exit codes |
|---|---|---|---|
| `scan_cjk.py` | Checker | Root `CLAUDE.md` Language Rule: English everywhere outside `docs/internal/` | 0 clean / 1 violations / 2 error |
| `check_i18n_keys.py` | Checker | Root `CLAUDE.md` internationalization rule: zh-CN and en-US key sets identical | 0 clean / 1 mismatch or parse error / 2 error |
| `check_repo_isolation.py` | Checker | Multi-tenant isolation discipline: every Repository type (a struct embedding `dbkit.Repository[T]`) is covered by `tenancytest.AssertIsolated` in its package's tests | 0 all covered / 1 uncovered repository / 2 error |
| `check_toolchain.py` | Checker | Root `.mise.toml` tool versions mirror their authoritative sources (Taskfile.yml header, `go.work`, `web/.nvmrc`, `web/package.json`, setup-go-env's `GOLANGCI_VERSION`) | 0 all mirrors match / 1 drift / 2 error |
| `check_docs_site.py` | Checker | `docs/site/` skeleton structure: required entry files present, internal links resolve inside the tree, offline preview serves (python3 stdlib HTTP server) | 0 clean / 1 violation / 2 error |
| `license_scan.py` | Checker | Dependency-license compliance: every direct third-party dependency of the implemented Go modules and web packages is adjudicated and within policy in `dependency-licenses.json`, re-derived from the live tree on every run | 0 clean / 1 violation / 2 usage error |
| `new_module.py` | Generator | Scaffolds the canonical stub of a new Go module under `go/<name>` and prints its registration checklist; never modifies shared repository files | 0 scaffolded / 2 refusal or validation error |
| `release/lockstep-release.py` | Release verifier | Verifies the lockstep one-version release plan offline — derives the publishable set at runtime (go.work `use` entries under `go/` + `web/packages/*`) and checks version form, no duplicate tag, go.work-to-tree completeness both ways, uniform npm versions, changesets fixed-group coverage (`web/.changeset/config.json`); `--self-test` runs its unittest suite; `--apply` is a hard-gated local-tag mode (real publishing is M4's job) | 0 consistent plan / 1 inconsistent plan or self-test failure / 2 usage / 3 `--apply` refused |

## scan_cjk.py — CJK scanner

Scans a repository tree for CJK (Han-script) characters and fails on any hit
outside the carved-out areas. This is the repo-wide generalization of the
module-level regression guard `go/ratelimit/language_test.go`, which uses
`go/parser` + `unicode.Is(unicode.Han, r)`; this script reproduces both
behaviors exactly.

Usage:

```
python3 tools/scan_cjk.py --root /path/to/repo
python3 tools/scan_cjk.py            # --root defaults to the current directory
```

Per-hit output is one grep-friendly header line plus the offending source
line:

```
examples/foo/bar.md:12: file text contains CJK character '<han>' (U+XXXX)
  <the offending source line>
```

### Per-extension rules

| File kind | What is checked |
|---|---|
| `.go` | Comments only — same semantics as `go/parser` with `parser.ParseComments`, implemented as a state-machine lexer: `//` and `/*` are only recognized in code context. String literals, rune literals, raw strings and their contents are never comment contexts, so CJK test data stays exempt (the intentional fixtures in `go/dbkit/encryption_test.go` and `examples/reference-app/internal/notes/handler_test.go` are exactly this case — a repo-wide scanner must apply the same string-literal exemption, never enumerate fixtures). Escaped quotes, `\` line continuations, and unterminated literals are handled per Go's lexical rules. |
| Every other text file (`.md`, `.txt`, `.html`, `.yml`, `.yaml`, `.toml`, `.json`, and any other file) | Full content, line by line. |
| Multi-line block comments in `.go` | Reported per physical line that carries CJK (the module-level Go test precedent instead attributes a whole comment to its start line — the comment sets are identical, this scanner just reports finer locations). |
| Binary / non-text files (any NUL byte in the first 8 KiB, or bytes that do not decode as strict UTF-8) | Skipped, never reported. |

Note the rule asymmetry is deliberate: Go source is the one place the repo
keeps a string-literal / test-data exemption; Markdown and every other text
file are prose with no such carve-out, so their whole content is checked.

### Carve-out matrix

A whole subtree is exempt when any of these applies; everything else in the
tree is scanned.

| Path | Why |
|---|---|
| `docs/internal/` | Chinese by rule — the Language Rule's own exception. |
| `docs/site/` | Public documentation site (per `docs/internal/13-documentation-standards.md`, English-first with zh-CN localization directories added by need) — localization legitimately carries CJK, so the whole subtree is exempt. |
| Any directory named `locales`, `locale`, `i18n` or `translations` | i18n resource directories legitimately carry CJK user-facing text (e.g. `.../notes/locales/zh-CN.toml`). The basename set is the `LOCALE_DIR_NAMES` constant at the top of the script — extend it when a new i18n directory convention appears, rather than loosening the scan. |
| `.git/`, `node_modules/`, `vendor/` | VCS metadata and vendored dependencies. |

### What counts as CJK

Han script, classified by the exact rune ranges Go's `unicode.Han` covers
(the `HAN_RANGES` table in the script transcribes the Go 1.26.1 toolchain's
`unicode/tables.go` `_Han` RangeTable and was verified code point for code
point against `unicode.Is(unicode.Han, r)` under that toolchain): CJK
radicals and Kangxi radicals; the Han members of the CJK Symbols and
Punctuation block — U+3005 and U+3007 are Han while U+3006 is not (Go's
entry is the stride-2 range `{0x3005, 0x3007, 2}`), U+3021–U+3029 and
U+3038–U+303B are Han; the ideograph blocks U+3400–U+4DBF and U+4E00–U+9FFF;
compatibility ideographs U+F900–U+FAD9; and the supplementary-plane
extension blocks. Full-width Latin, CJK punctuation and other
East-Asian-but-not-Han characters are not flagged — matching the
`unicode.Han` precedent exactly.

## check_i18n_keys.py — zh-CN / en-US key-set checker

Checks the internationalization discipline: every locale directory's
`zh-CN.toml` and `en-US.toml` must carry identical message-key sets, so new
text can never ship in only one language. Files use the go-i18n-style TOML
seen in `examples/reference-app/internal/notes/locales/`.

Usage:

```
python3 tools/check_i18n_keys.py --root /path/to/repo
python3 tools/check_i18n_keys.py            # --root defaults to the current directory
```

Discovery: a locale directory is any directory containing a file named
`zh-CN.toml` or `en-US.toml`; `--root` itself is a candidate and the tree
below it is walked (skipping `.git/`, `node_modules/` and `vendor/`). Both
pair members must exist in every locale directory.

### Message-id semantics

A message id is a key whose value is not a grouping table. Two kinds of
values make a message id: any non-table value (the plain single-form
message), and a table whose keys are all message keys — the CLDR plural
categories `zero`, `one`, `two`, `few`, `many`, `other`, the v1
`translation` synonym for `other`, and the metadata keys `description`,
`id`, `hash`, `leftdelim` and `rightdelim`, matched case-insensitively
(the set go-i18n reserves on a plural message). Such a table is ONE plural
message: its categories and metadata are forms of that message, not
separate ids, and the table's own key is the message id. A quoted header
such as `["notes.over_quota"]` with `one`/`other` entries and an inline
`"notes.over_quota" = { one = "...", other = "..." }` both declare the
single id `notes.over_quota`.

Quoted keys are TOML-native single keys, so a quoted dotted key such as
`"notes.text_required"` stays whole and is the message id, exactly as
handler code references it. A table with any key outside the message-key
set is a grouping table (`[errors]`, or an unquoted `[notes.over_quota]`
section carrying other tables); its message ids are its leaf keys,
compared leaf-wise, so a flat file and a section-grouped file declaring
the same messages match. go/pkgcore/i18n's AddModule is deliberately
stricter — it rejects grouping sections outright (ErrUnsupportedShape)
and requires the `<module>.` id prefix — so repository files follow the
flat contract; the script keeps the leaf-level grouping tolerance only so
its id semantics stay stable across both shapes. If one file defines the
same leaf id under two different paths (two sections carrying a key of
the same name), the pairing across languages would be ambiguous, and the
script reports that file as an error rather than comparing it.

Reported mismatches list the ids and the direction:

```
examples/reference-app/internal/notes/locales: zh-CN.toml and en-US.toml key sets differ
  only in en-US.toml (missing zh-CN translation):
    - notes.text_too_long
```

Any other `.toml` file in a locale directory is reported as an
informational note only: the repository's pair discipline is exactly
zh-CN/en-US, and additional languages should extend the script's pair table
rather than slip past the check silently.

## check_repo_isolation.py — tenant-isolation coverage checker

Checks the multi-tenant isolation testing discipline against the actual
Repository implementations. Root `CLAUDE.md` makes `dbkit.Repository[T]` the
mandatory base for tenant-data repositories (never a raw `*gorm.DB`), and
the tenancytest suite (`go/tenancy/tenancytest`) is the mandatory
isolation-assertion every module must run against each of its own
`dbkit.Repository[T]` usages. The script scans every Go module under
`--root` for struct types that anonymously embed the base — `type Repository
struct { *dbkit.Repository[Note] }`, value or pointer, in standalone or
grouped `type ( ... )` declarations — and checks that each type's package
tests call `tenancytest.AssertIsolated`.

Usage:

```
python3 tools/check_repo_isolation.py --root /path/to/repo
python3 tools/check_repo_isolation.py    # --root defaults to the current directory
```

Per-repository output is one grep-friendly line naming the declaring file
and line, the embed shape, and either the covering call
(`-- covered by tenancytest.AssertIsolated at <file>:<line>`) or the
failure. The exit code is 1 whenever any repository is uncovered.

### Coverage attribution (textual heuristics)

The script is a scanner, not a Go type checker; the attribution rules it
applies are chosen so that a compiling test file satisfies them
automatically:

- A package with exactly one Repository-embedding type is covered by any
  `AssertIsolated` call in its tests — with one repository, a compiling
  call can only be about it.
- A package with several embedding types is covered per type: a type is
  covered when a call's text mentions the type's own name or any
  identifier inside its embedding's type arguments (`dbkit.Repository[Note]`
  mentions `Note`). The `newRecord` argument naturally satisfies this —
  `func(tenant pkgcore.TenantID) *Note { ... }` — and an explicit
  instantiation (`AssertIsolated[Note](...)`) counts too. A call naming
  neither leaves the type reported, with the found calls listed so an
  author can see why.
- A package's tests are its `_test.go` files plus the `_test.go` files
  under its physically separated `integration_test/` directory (root
  `CLAUDE.md` testing rule), so integration-tier assertions count as
  coverage of the package they live under. Build tags are ignored: the
  check is textual.

### Deliberate exclusions

- Embeddings in `_test.go` files (test doubles, Example code such as
  `go/dbkit/example_test.go`'s demonstration) are never candidates — the
  discipline covers shipped repositories. They print as informational
  notes.
- The identity-data / platform-data half of the tenancytest pair
  (`AssertNotTenantScoped`) is reported as a note, never required: those
  models never embed `dbkit.Repository[T]` (its generic constraint
  requires `TenantScoped`, which identity and platform data must not
  implement), so nothing in production code statically marks them for
  enumeration.
- dbkit's own module cannot call tenancytest at all (that would be a
  module cycle: tenancy requires dbkit), and it has no embeddings outside
  its Example code — its own repository tests are the mechanism's proof.
- Documented scanner limitations: top-level declarations only, import
  aliases honoured but dot imports of dbkit not tracked, alias types of
  `dbkit.Repository` embedded under their own name not seen, and field
  boundaries hidden inside multi-line block comments not seen. None of
  these shapes occur in the repository today; the module docstring of the
  script is the authority when one appears.

## check_toolchain.py — .mise.toml drift gate

The root `.mise.toml` pins the developer toolchain (task, go, node, pnpm,
golangci-lint) for the local `mise install` that `task setup` runs. CI
cannot read `.mise.toml` directly -- actions/setup-go's go-version-file
resolves go.mod, go.work, go.sum or .go-version only, and setup-node reads
web/.nvmrc -- so every version the file pins is a MIRROR of an
authoritative source elsewhere in the repository, and this script fails
when a mirror drifts from its source. CI reads the sources; the gate is
what proves a local `mise install` cannot drift from them.

Usage:

```
python3 tools/check_toolchain.py --root /path/to/repo
python3 tools/check_toolchain.py        # --root defaults to the current directory
```

Per-tool output is one grep-friendly line; exit 1 whenever any mirror
differs from its source:

```
toolchain: ok          task: .mise.toml 3.53.1 == Taskfile.yml header comment (3.53.1)
toolchain: MISMATCH    node: .mise.toml pins 24 but web/.nvmrc says 23
```

The sources, one per tool: `task` from the Taskfile.yml header comment
(the one tool whose only pin lives there, scanned over the header's first
40 lines); `go` from go.work's `go` directive (the file actions/setup-go
actually reads -- setup-go-env's go-version-file input stays go.work
rather than being repointed at .mise.toml, which setup-go cannot parse);
`node` from web/.nvmrc; `pnpm` from web/package.json's packageManager
field; `golangci-lint` from GOLANGCI_VERSION in
.github/actions/setup-go-env/action.yml. Bump a source and its mirror
together -- the .mise.toml header comments name the source of every tool.

## check_docs_site.py — docs-site skeleton checker

The docs site (docs/site/) is a real, previewable skeleton -- static
HTML with no build step, no npm project and no network
(docs/site/README.md) -- whose full machinery (per-version release
directories, llms.txt at the public root, a build step/SSG) is a later
milestone per docs/internal/13-documentation-standards.md. This script
checks what can be checked about such a tree without any tooling: the
required entry files (index.html, README.md) exist at the site root;
every internal link and asset reference on every HTML page resolves
inside the site tree (fragments, external http(s)/mailto/tel/data URLs
and protocol-relative URLs are skipped; an absolute path escaping the
tree is a violation); and the offline preview really serves -- the
script starts the python3 stdlib HTTP server on an ephemeral port and
fetches the site root, expecting a 200.

Usage:

```
python3 tools/check_docs_site.py --root /path/to/repo
python3 tools/check_docs_site.py        # --root defaults to the current directory
```

Per-violation output is one grep-friendly line; exit 1 on any
violation, 2 when the site tree or the preview server cannot be
handled:

```
docs-site: violation    index.html: required entry file is missing
docs-site: violation    status.html: link 'aboutx.html' resolves to nothing
```

## new_module.py — Go module stub generator

Scaffolds the canonical stub of a future module, exactly the three files
every not-yet-implemented module under `go/` already carries (`go/sharing`,
`go/notification`, ...): `go.mod` (`module github.com/vislake/speed/go/<name>`
plus the stub convention's bare `go 1.23` — the `go 1.25.0` directive and
require/replace blocks appear when an implementation round adds the first
dependency), `doc.go` (one-line English package doc; the package name is
the module name with hyphens removed, per the `go/ai-gateway` ->
`aigateway` precedent), and `AGENTS.md` (the one-liner stub form pointing
at the design doc). It is the generator behind the `new:module` task
in the root `Taskfile.yml` (promised by `docs/internal/19-dev-workflow.md`);
the wiring contract the task implements is documented in the script's
`--help` epilog.

Usage:

```
python3 tools/new_module.py NAME --description '...' --design-doc docs/internal/XX-name.md
python3 tools/new_module.py NAME --description '...' --design-doc docs/internal/XX-name.md --dry-run
```

`--target-dir` defaults to the repository root, detected as the nearest
ancestor containing `go.work`; the scaffold always lands at
`<target>/go/<name>` and nothing is ever written outside it. After
scaffolding, a registration checklist prints (go.work `use` entry — which
is also the module's lockstep release registration, since the release
coordinator derives its tag list from go.work at runtime — CI matrix row,
roadmap and navigation rows) as reminders — the script never edits those
shared files itself, because a scaffolder that silently rewrites `go.work`
and CI matrices makes review diffs unreadable; the checklist is the
contract for the human or for the future Taskfile task.

Guardrails: an existing `go/<name>` is never overwritten (exit 2); the
name must be lowercase letters, digits and single hyphens starting with a
letter, with no underscores (go module names are hyphen-convention — the
repo's only underscore-named directories are the deliberate
`go/*/integration_test` test tiers, which are test packages rather than
module names) and must not be a Go keyword (`doc.go` carries
`package <name with hyphens removed>`, and `package type` cannot compile,
so a scaffolded stub must always build); `--description` must be a single
ASCII line (the Language Rule would flag anything else in `doc.go`);
`--category npm` is refused because no npm package template exists in the
repository yet — `docs/internal/19-dev-workflow.md` only names the future
`task new:npm-package`; `--dry-run` prints the plan without writing
anything.

## Semgrep architecture-discipline ruleset (tools/semgrep_rules/)

Six semgrep rules, one per architecture-discipline row of the
`docs/internal/18-cicd.md` discipline table. Each rule file carries its own
header: the discipline row it maps to, the exact shapes that fire, the
path allowlist (every allowlisted site is a deliberate, documented
mechanism, named and justified in the header), and the residual gaps the
rule deliberately does not close (code review owns those evasions).

| Rule file | Discipline row (18-cicd table / root CLAUDE.md) | What fires | Allowlist (paths.exclude) | Residual gap |
|---|---|---|---|---|
| `deployment-mode-branch.yml` | no `if mode == "standalone"` branching in business logic | mode-value comparisons (`==`/`!=`, both operand orders), `case` labels naming the mode values, `os.Getenv("SPEED_DEPLOYMENT_MODE")`; same-package literal-valued consts resolve before matching, so an alias const still fires | test files; `go/pkgcore/deployment_mode.go` + `registry.go`, `go/observability/init.go` (kernel wiring), the reference-app command entry (`cmd/server/server.go`, `main.go`) | an indirection neither text matching nor value propagation reaches -- a helper inside an allowlisted file, a cross-package alias constant, a runtime-computed decision |
| `gorm-automigrate-ban.yml` | no `AutoMigrate` (migrations are versioned SQL) | any `.AutoMigrate(...)` call in shipped code. STATUS: future guard -- zero real call sites exist today | test files | none stated (the ban is total) |
| `handwritten-tenant-id-filter.yml` | no hand-written `WHERE tenant_id = ?` | a `Where` / `Or` / `Not` / `Having` chain call whose FIRST argument is a string literal containing a `tenant_id = ?`-style clause | test files; `go/dbkit/**` (the scoping plugin builds the filter everyone else relies on); `go/jobs/store.go` (platform-data idempotency guard) | a clause assembled dynamically (fmt.Sprintf into the clause, a filter passed through a helper); only the first-argument literal form is matched |
| `non-constant-log-message.yml` | log messages are constant strings (structured logging) | a call to the observability logger's `Info`/`Warn`/`Error`/`Debug` whose first argument is not a string literal (fmt.Sprintf output, concatenation, a variable) | test files | a raw-string (backtick) literal message is flagged although constant (none exists today); anything logged outside the shared structured logger is a separate discipline |
| `raw-gorm-bypass.yml` | no `db.Table` / `db.Model` / `db.Raw` around the Repository | any call to the three bypass entry points on any receiver, in shipped code | test files; `go/dbkit/**` (dbkit owns the raw surface it provides); `go/jobs/store.go` (platform data whose dispatch query must scan every tenant) | a workaround through another `*gorm.DB` method (e.g. `Exec` with hand-written SQL) is not matched |
| `tenant-id-metric-label.yml` | `tenant_id` never becomes a Prometheus/OTel metric label | the metric-label NAME carriers: `metric.WithAttributes(...)` and `[]attribute.KeyValue{...}` literals containing the text `tenant_id`, the four `prometheus.New*Vec` constructors and `prometheus.Labels{...}` literals | test files; span attributes and `.WithLabelValues(...)` value passes deliberately do NOT fire (spans are the tenant dimension's sanctioned home; the NAME is the cardinality hazard, fixed at vector construction) | a `tenant_id` label past the first variadic option / first composite-literal element; a label name introduced through a constant indirection (`attribute.String(obs.TenantIDKey, ...)`) -- the runtime assertion test and review cover those |

The fixtures live under `tools/semgrep_rules/testdata/<rule>/`: each rule's
`positive.go` must fire on every pattern shape the rule declares, and
`negative.go` proves the flip side (the shapes that deliberately stay
clean, including allowlisted behavior). The rules are proven against those
fixtures and against the real tree before shipping. Fixture proofs run per
rule (`--config tools/semgrep_rules/<rule>.yml` against that rule's own
`testdata/` directory): a negative fixture may legitimately carry a shape
another rule fires on, so a whole-ruleset scan over fixtures is not the
proof shape (the real-tree scan excludes `testdata/` at the CLI level
anyway, so cross-rule fixture hits never reach CI).

Running locally (the docker image is the pinned local version; CI instead
pip-installs into a throwaway venv -- see below):

```
docker run --rm -v "$PWD:/repo:ro" -w /repo \
  returntocorp/semgrep:1.176.0 semgrep scan --config tools/semgrep_rules \
  --error --exclude tools/semgrep_rules go examples tools
```

Execution status, stated honestly:

- The real-tree scan passes today: 0 findings across `go/` `examples/`
  `tools/` (89 Go files, 6 rules), exit 0. Proven locally with the docker
  image above on the round's final state.
- Known parser limitation: semgrep always skips line 19 of
  `examples/reference-app/internal/notes/repository.go` (the embedded
  instantiated generic `*dbkit.Repository[Note]` raises a PartialParsing
  exception; roughly 3.4% of that file's lines are never analyzed).
  The skipped line is a struct-field declaration, which none of the six
  rules' shapes targets, so no rule is blind-sided today -- but a rule
  written later must know this file cannot be fully scanned.
- Version posture: the CI step's `pip install semgrep` is deliberately
  unpinned until the first green CI run pins it (the local proofs all ran
  the pinned `returntocorp/semgrep:1.176.0` image); the docker run prints
  a `safe.directory` warning because the host git path is unreachable
  inside the container -- benign, the scan completes.

## license_scan.py — dependency license compliance

Verifies `tools/dependency-licenses.json` against the live tree. The
manifest records the license adjudication for every direct third-party
dependency of the implemented Go modules (all direct requires in
`go/*/go.mod`) and npm packages (`web/packages/*/package.json`
dependencies + peerDependencies, versions resolved from
`web/pnpm-lock.yaml` -- what a frozen-lockfile CI install yields).
Workspace-internal packages carry no entry.

Policy (mirroring `docs/internal/20-quality-and-security.md`): strong
copyleft (GPL family, AGPL) fails outright; weak copyleft (MPL, LGPL)
fails unless the entry carries an `adr` field naming an existing `docs/`
file that records the adjudication (none exists); any unrecognized license
string fails closed with an adjudication message; the permissive set
(0BSD, Apache-2.0, BSD-2/3-Clause, CC0-1.0, ISC, MIT, Unlicense) passes.
Beyond the policy check, the scan re-derives the expected dependency set
from the tree and fails on any drift: a newly required dependency, an
orphan manifest entry, a version change, a `used_by` list that no longer
matches, or an npm dependency `package.json` declares but the lockfile
does not resolve (a real find -- the frozen-lockfile install would fail).

Usage:

```
python3 tools/license_scan.py            # check the repository (exit 0/1)
python3 tools/license_scan.py --selftest # planted-fixture suite under
                                         # tools/license_scan_testdata/
```

The manifest's license ids were identified from the license file each
release ships; the `evidence` field records where (go module cache paths
are deterministic: `$GOMODCACHE/<module>@<version>/<file>`).

Execution status, stated honestly: the planted-fixture suite
(`tools/license_scan_testdata/`, one directory per case with an
`expected_exit` file) passes 10/10, and the real-tree check passes ("42
manifest entries match the tree, all licenses within policy"), both
proven locally. Wired into the security pipeline's license job (selftest,
then the real check) in `.github/workflows/security.yml`.

When a dependency appears, a version changes, or a dependency goes away:
adjudicate the license (read the license file the release ships, record
it as `evidence`, set the SPDX id), add/update/remove the manifest entry
with its `used_by` list, and run `python3 tools/license_scan.py` until it
passes.

## lockstep-release.py — the lockstep release coordinator (M0)

The repository's release rule (`docs/internal/02-repo-and-release.md`,
`docs/internal/18-cicd.md`): every Go module and every npm package shares
ONE version number and releases together — one tag per module under
`go/`, `go/<module>/<version>` form — and the roadmap's M0 exit condition
is that a single command can publish all of them at one version. This
coordinator is that command's M0 deliverable, and the round is
deliberately offline and verification-only:

```
python3 tools/release/lockstep-release.py v1.2.0   # print the one-version plan, verify consistency
python3 tools/release/lockstep-release.py --self-test
python3 -m unittest discover -s tools/release      # the same suite
```

The default mode derives the publishable set at runtime — the go.work
`use` entries under `go/` for the Go half (a `use` entry IS a module's
release registration; the checklist that `new_module.py` prints says so),
`web/packages/*` for the npm half — and prints the full plan: every module
with the tag it would get, every package with the version the
`web/.changeset` fixed group would bump it to, closing with one aggregate
line. Exit 0 only when the plan is consistent: the version has
release-version form (`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`, the
leading `v` required — the same pattern lives in the version-validation
step of `.github/workflows/release.yml` and the two must stay in step), no
tag exists for it yet, go.work and the `go/` tree are complete in both
directions (a module missing from go.work, a `use` entry whose go.mod does
not exist, or a stray module under `go/` not registered all fail loudly),
the npm versions are uniform, and the changesets fixed group covers
exactly the packages that exist. `examples/reference-app` is deliberately
outside the publishable set — it is the repository's consumer module and
keeps its `replace` lines; nothing under `go/` outside `go/` itself is
published either.

Exit codes: 0 plan consistent; 1 plan inconsistent or self-test failure;
2 usage error (a version that is not of release form, unknown flags);
3 `--apply` refused. `--apply` requires `--allow-local-tag-creation` and
is refused otherwise: the gated mode creates LOCAL tags only (never
pushed), and exists to exercise the tag-creation half of the machinery
against a scratch git checkout. Real publishing — pushing tags, the
changesets bump and `npm publish`, artifacts, the GitHub Release — is
scheduled for the v1.0 release at M4, and `.github/workflows/release.yml`
wires no publish credential, so no mode of the coordinator (or of that
workflow) can publish for real.

The coordinator also ships the first-release replace-cleanup engine
(`first_release_replace_cleanup`, the rewrite of transitional
`replace ... => ../<module>` lines into real versions that the first
release performs) as pure functions exercised ONLY against the go.mod
fixtures under `tools/release/testdata/`. Never run the engine against a
live module go.mod: the tree keeps its transition state until M4.
`tools/release/AGENTS.md` is the authority for this directory — its
absolute prohibitions, and what must change when a module or package
joins the tree.

## Running in CI and locally

CI workflows mount the checkers directly, from the repository root, and
fail the build on a nonzero exit: `python3 tools/scan_cjk.py` and
`python3 tools/check_toolchain.py` and the semgrep ruleset step
(catalogued above) run in pr-check's repo-checks job (every pull
request, `.github/workflows/pr-check.yml`);
`python3 tools/check_i18n_keys.py` plus `python3 tools/check_docs_site.py`
run in the docs-check pipeline (`.github/workflows/docs-check.yml`),
whose pull_request path filter fires on PRs touching documentation or
i18n resources; and the license scanner (`python3 tools/license_scan.py`,
selftest first, then the real check) runs in the security pipeline's
license job (`.github/workflows/security.yml`).
`tools/check_repo_isolation.py` is wired into no workflow yet; its row
lands with a future CI round. Locally, run them from the
repository root — the default `--root` is the current directory, so plain
`python3 tools/scan_cjk.py` also works there. All output paths are relative
to `--root`. All scripts are plain executables with no third-party
dependencies and no module metadata of their own; they live here precisely
so a CI image never needs a Go toolchain or a package install to enforce
these disciplines. The generator is a developer-time tool: run it when a
roadmap item assigns a module a milestone, commit the three scaffolded
files with the design doc, and perform the printed registrations in the
same change.

The release coordinator is wired the same way: `.github/workflows/
release.yml` (a manual dispatch with a version number) validates the
version form and then runs `python3 tools/release/lockstep-release.py
"$VERSION"` followed by `--self-test`; locally, the root Taskfile's
`task release:plan VERSION=v1.2.0` runs the verification form, or run the
script directly from the repository root — the coordinator discovers the
publishable set from the tree it runs in, so it must run at the
repository root, exactly like the checkers' `--root` default. Its
`--self-test` suite needs nothing but the standard library and a `git`
binary; the sandbox proof inside it touches only scratch repositories and
asserts the live tree's tags are untouched.

The secret-leakage half of the security pipeline scans with
`.gitleaks.toml` at the repository root: it extends gitleaks' default
rule set (no rule added or disabled) with exactly one path-scoped
allowlist, for the go/observability redaction-test fixtures — secret-
shaped stand-in values are the point of that test code, and the file's
comment records the reasoning and the residual risk. Any further
allowlist entry must arrive with its own justification in that file.
