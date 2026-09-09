# tools/ — repo scripts

Plain, dependency-free Python scripts (standard library only, Python >= 3.11
for `tomllib`) that back the repository's cross-cutting disciplines and its release machinery: three discipline checkers, a fourth Go-toolchain-requiring markdown-example checker, the dependency-license scanner with its committed manifest, one scaffold generator, the semgrep architecture-discipline ruleset under `tools/semgrep_rules/` with its planted-violation fixtures, and the lockstep release coordinator (a release tool, not a discipline checker — it follows the same convention, which is why it lives here). The checkers are the local-run counterparts of the CI discipline checks scheduled in `docs/internal/18-cicd.md` (the table rows for banning CJK outside `docs/internal/`, for requiring identical zh-CN/en-US message-key sets, and for making every tenant-scoped Repository run the tenancytest isolation suite, all marked there as self-written scripts); CI workflows mount all three under `tools/` — `scan_cjk.py` and `check_repo_isolation.py` in fast-check's repo-checks job, `check_i18n_keys.py` in the docs-check pipeline. Two further scripts are repo self-checks rather than 18-cicd discipline rows: `tools/check_toolchain.py` gates the tool versions the root `.mise.toml` pins — mirrors of the authoritative sources CI actually reads (Taskfile.yml header, `go.work`, `web/.nvmrc`, `web/package.json`, setup-go-env's `GOLANGCI_VERSION`) — proving the mirrors cannot drift, and fast-check's repo-checks job runs it; `tools/check_docs_site.py` validates the docs-site skeleton (required entry files, internal links, offline preview) and the docs-check pipeline runs it. `tools/check_api_fragments.py` is the same shape for `.github/workflows/api-contract.yml`'s backend-fragment enumeration, whose legs -- the trigger path filters, the thirteen oapi-codegen regeneration steps, the redocly join input list -- were kept consistent only by comments in that file until the fragment set grew from six to thirteen with no gate going red; its manifest `tools/api_fragments.json` is now the single machine-readable source of truth, and the api-contract pipeline runs the gate (and its planted-drift suite `tools/test_check_api_fragments.py`) as its first steps on every trigger. `tools/check_markdown_examples.py` closes root CLAUDE.md's own recorded gap ("Examples embedded in markdown prose... have no compile harness"): every fenced ```go block in AGENTS.md/README/ADR prose is either really `go build`+`go vet`'d (a block with its own `package` clause) or `gofmt -e` syntax-checked under several throwaway wrappings (a bare fragment, the corpus majority) — the one script here that genuinely needs a Go toolchain, not just `python3` (see its own section below for why, and for the design tradeoff that keeps a partial snippet legitimate rather than forcing every example into a padded full program). The generator is the backend of the `task new:module` promised by `docs/internal/19-dev-workflow.md`. The release coordinator (`release/lockstep-release.py`) is the M0 deliverable for the roadmap's lockstep-release item (`docs/internal/02-repo-and-release.md`, `docs/internal/18-cicd.md`), an offline verification of the full one-version release plan, wrapped by the root Taskfile's `release:plan` task and mounted by `.github/workflows/release.yml`; its unittest suite and go.mod fixtures live beside it under `tools/release/`. Nothing here needs anything beyond `python3` except the semgrep ruleset (needs a semgrep binary) and `check_markdown_examples.py` (needs `go`/`gofmt` on PATH) — see their own sections for the pinned local versions and the CI shape — and the checkers print hit paths relative to their `--root`.

| Script | Kind | Enforces / does | Exit codes |
|---|---|---|---|
| `scan_cjk.py` | Checker | Root `CLAUDE.md` Language Rule: English everywhere outside `docs/internal/` | 0 clean / 1 violations / 2 error |
| `check_i18n_keys.py` | Checker | Root `CLAUDE.md` internationalization rule: zh-CN and en-US key sets identical | 0 clean / 1 mismatch or parse error / 2 error |
| `check_repo_isolation.py` | Checker | Multi-tenant isolation discipline: every Repository type (a struct embedding `dbkit.Repository[T]`) is covered by `tenancytest.AssertIsolated` in its package's tests, or by the equivalent `Test<TypeName>_AssertIsolated` suite | 0 all covered / 1 uncovered repository / 2 error |
| `check_toolchain.py` | Checker | Root `.mise.toml` tool versions mirror their authoritative sources (Taskfile.yml header, `go.work`, `web/.nvmrc`, `web/package.json`, setup-go-env's `GOLANGCI_VERSION`) | 0 all mirrors match / 1 drift / 2 error |
| `check_docs_site.py` | Checker | `docs/site/` skeleton structure: required entry files present, internal links resolve inside the tree, offline preview serves (python3 stdlib HTTP server) | 0 clean / 1 violation / 2 error |
| `check_api_fragments.py` | Checker | `.github/workflows/api-contract.yml`'s backend-fragment enumeration self-consistent: `tools/api_fragments.json` is the single source of truth, and the tree, the pull_request/push trigger path filters, the oapi-codegen regeneration steps and the redocly join input list must all agree with it (a fragment added to one leg only, or to the tree without the manifest, goes red) | 0 clean / 1 drift / 2 error |
| `check_spec_request_tenant_id.py` | Checker | 18-cicd discipline row "the API layer must not accept an externally supplied tenant_id": every backend OpenAPI fragment's requests (requestBody schema properties and parameters) are scanned for a `tenant_id` declaration, with authn's four pre-auth tenant-naming request schemas the one recorded exception (its spec header: a request for that tenant's first access token, never a grant); response schemas are not requests and prose that merely mentions tenant_id is not a property line | 0 clean / 1 finding / 2 error |
| `check_migration_cross_module_fks.py` | Checker | 18-cicd discipline row "no cross-module database foreign keys": a `REFERENCES` target in a module's migration files must belong to that module, measured by where the target is CREATEd across every `migrations/` tree under go/ and examples/reference-app. STATUS: future guard -- the tree ships no REFERENCES clause at all today; the planted fixtures are the teeth | 0 clean / 1 finding / 2 error |
| `check_coverage_baseline.py` | Checker | docs/internal/20-quality-and-security.md's coverage rule for every released module (the 21 go/ modules with a go.mod plus examples/reference-app): the measured unit-suite statement coverage -- generated files (.gen.go) excluded from the census -- must clear the 80% floor (the 2026-09 product decision) and must not sit more than the documented tolerance below its committed `tools/coverage-baselines.json` row (the earlier foundation-modules no-decline rule, kept); `--update` records new baselines by real measurement (a deliberate decline must ride the same change that records it, with the reason in the commit, and a sub-floor measurement refuses to be recorded -- the floor is not re-baselineable); `--selfcheck` keeps the baseline file itself complete and current (every row a live gated module, every gated module with a row) so a stale row cannot silently exempt a module | 0 clean / 1 floor or decline breach, stale/missing row, or toolchain moved / 2 usage error |
| `check_markdown_examples.py` | Checker | Root `CLAUDE.md` Documentation section: every fenced ```go block in AGENTS.md/README/ADR prose really compiles (a complete block) or at least parses under some throwaway wrapping (a fragment) | 0 clean / 1 a block fails its check / 2 error |
| `license_scan.py` | Checker | Dependency-license compliance: every direct third-party dependency of the implemented Go modules and web packages is adjudicated and within policy in `dependency-licenses.json`, re-derived from the live tree on every run | 0 clean / 1 violation / 2 usage error |
| `check_error_code_index_coverage.py` | Checker | Error-code index completeness: every code constructed in Go source (declared or inline, literal argument only) has a row in `docs/error-codes.md`, plus the unindexable classes (non-literal code arguments outside an apperr-constructing helper's body; helper calls with non-literal arguments) — the independent side of `gen_error_code_index.py`'s own drift gate, able to go red on an extractor blind spot the gate cannot see | 0 clean / 1 finding / 2 usage error |
| `affected_go_modules.py` | Scope helper | Computes which Go modules a set of changes affects -- changed modules plus their downstream dependents, the dependency edges derived from each module go.mod's require lines (never a hand-maintained table) -- the computation behind the Taskfile `test` task's diff-aware scoping | 0 scope printed (one module dir per line, or the token ALL) / 1 git plumbing failed / 2 usage error |
| `new_module.py` | Generator | Scaffolds the canonical stub of a new speed Go module under `go/<name>` (go.mod + doc.go + AGENTS.md) with `--category go`, or the canonical `@speed/<name>` package skeleton under `web/packages/<name>` (package.json, the tsconfig pair, a doc-comment index.ts plus its wiring test, README and AGENTS.md) with `--category npm`, and prints the category's registration checklist; never modifies shared repository files | 0 scaffolded / 2 refusal or validation error |
| `release/lockstep-release.py` | Release verifier | Verifies the lockstep one-version release plan offline — derives the publishable set at runtime (go.work `use` entries under `go/` + `web/packages/*`) and checks version form, no duplicate tag, go.work-to-tree completeness both ways, uniform npm versions, changesets fixed-group coverage (`web/.changeset/config.json`); `--self-test` runs its unittest suite; `--apply` is a hard-gated local-tag mode (real publishing is M4's job) | 0 consistent plan / 1 inconsistent plan or self-test failure / 2 usage / 3 `--apply` refused |

## scan_cjk.py — CJK scanner

Scans a repository tree for CJK (Han-script) characters and fails on any hit
outside the carved-out areas. This is the repo-wide generalization of the
module-level regression guard `go/ratelimit/no_cjk_characters_test.go`, which uses
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
| Any directory that directly holds a `zh-CN.*` / `en-US.*` file | i18n resource directory, judged by CONTENT (the same discovery rule `check_i18n_keys.py` applies): files there — the pair members and anything beside them — legitimately carry CJK user-facing text (e.g. `.../notes/locales/zh-CN.toml`, `.../src/locales/zh-CN.json`). A directory merely NAMED `locales`/`locale`/`i18n`/`translations` that holds no such file is scanned like any other source tree, so source packages like `go/pkgcore/i18n` and `web/packages/i18n` are not silently exempt. |
| `.git/`, `node_modules/`, `vendor/` | VCS metadata and vendored dependencies. |

One further carve-out is per-line rather than per-subtree: the comment
lines inside a godoc Example's `Output:` block (after a `// Output:`
marker within the same contiguous comment run). The Go toolchain requires
an Example's expected output to be spelled in comments, and CI compiles
and runs every Example, so an Example demonstrating zh-CN catalog
rendering (go/pkgcore/i18n/example_test.go) carries Chinese expected
output in comment syntax — executed-and-verified fixture text, the same
class as the CJK string literals that stay exempt. A CJK comment anywhere
else in the same file is still a violation.

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
tests call `tenancytest.AssertIsolated`, or run the equivalent
store-level suite named `Test<TypeName>_AssertIsolated` (the coverage
attribution section below spells the rule out). fast-check's repo-checks
job mounts it on every PR and every push to main.

Usage:

```
python3 tools/check_repo_isolation.py --root /path/to/repo
python3 tools/check_repo_isolation.py    # --root defaults to the current directory
```

Per-repository output is one grep-friendly line naming the declaring file
and line, the embed shape, and either the covering call
(`-- covered by tenancytest.AssertIsolated at <file>:<line>`, or `--
covered by the equivalent isolation suite ...`) or the failure. The exit
code is 1 whenever any repository is uncovered.

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
- A type whose package tests run a function named exactly
  `Test<TypeName>_AssertIsolated` is covered by that suite. This is the
  equivalent-suite rule: `tenancytest.AssertIsolated` reflects T's
  exported `ID` field and queries the `id` column, so a Repository
  embedding whose record deliberately deviates from dbkit's ID convention
  — a Create-only embedding over a differently-keyed primary key, e.g.
  the reference app's `SimulationStore` over smilesim's `job_id`-keyed
  `simulationRecord` — cannot run the mandatory suite at all and ships a
  store-level isolation suite under that canonical name instead (the
  suite `examples/reference-app/internal/smilesim/simulation_store_test.go`
  names `TestSimulationStore_AssertIsolated`).
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
- Whole `internal/testutil` trees are never scanned: root `CLAUDE.md`'s
  Testing rule makes `internal/testutil` the repo-mandated home of shared
  test helpers, and the test doubles there (go/compliance/internal/
  testutil's `FakeRepository`, say) embed `dbkit.Repository[T]` so OTHER
  packages' tests can exercise the real generic base — the fakes are
  fixtures, not shipped repositories, and no `AssertIsolated` call
  belongs beside them.
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

## check_api_fragments.py — api-contract fragment-enumeration drift gate

`.github/workflows/api-contract.yml` names the backend-fragment universe
in several coherent places at once: the trigger path filters (the
`pull_request` and `push` `paths` blocks carry one `<dir>/**` row per
fragment), the oapi-codegen regeneration steps (one `cd <dir>` + pinned
oapi-codegen run per fragment, each followed by its porcelain
artifact-vs-spec gate), and the redocly `join` input list (the fragments
merged into `contracts/speed.yaml`, whose input order is
load-bearing: the merged document is committed and diff-gated, and the
order is what join renders). The fragment set grew from six to thirteen
over successive rounds with the sites' consistency held only by comments
in that file ("keep the two in lockstep") -- an enumeration that can
drift silently, and one that did. `tools/api_fragments.json` is the
single machine-readable source of truth for the fragment list, and this
gate fails (exit 1) when the live tree, the manifest and any leg of the
workflow disagree about a fragment.

Manifest schema: one `fragments` array; each entry is `{"name":
"<fragment id>", "dir": "<repo-relative api directory, ending in
/api>", "merge_rank": <int, present only for fragments that join the
redocly merge>}`. `merge_rank` is the join order (rank order == join
order); its
absence marks a fragment outside the merge (regenerated and
compile-checked, but feeding neither the merge nor the frontend SDK).

Checked invariants (each goes red with an actionable message):

* a registered fragment whose `openapi.yaml` or `oapi-codegen.yaml` is
  missing from the tree;
* an `api/` directory on disk carrying `openapi.yaml` +
  `oapi-codegen.yaml` that the manifest does not register (the tree-scan
  signature of a backend fragment; the scan skips `.git/`, `.claude/`
  (nested worktree checkouts), `node_modules/`, `vendor/` and
  `__pycache__/`);
* a registered fragment missing from the `pull_request` or the `push`
  path filter, or a fragment-shaped path-filter row (`.../api/**`)
  naming an unregistered directory;
* a registered fragment with no oapi-codegen regeneration step, or a
  regeneration step regenerating an unregistered directory;
* the redocly join input list disagreeing with the manifest's merged
  fragments in membership or order;
* the manifest or the gate itself missing from either trigger path
  filter (a change to either must re-run this job).

Usage:

```
python3 tools/check_api_fragments.py --root /path/to/repo
python3 tools/check_api_fragments.py      # --root defaults to the current directory
python3 tools/test_check_api_fragments.py # the planted-drift suite
```

The companion suite (`tools/test_check_api_fragments.py`) reproduces
every drift class on a small fixture repository -- a fragment added to
the tree without the manifest, a manifest entry missing from a leg, an
unregistered fragment in a leg, a join list dropped/swapped/extended --
plus a final case running the real check against this repository. The
api-contract pipeline runs the suite and then the gate as its first
steps on every trigger (`.github/workflows/api-contract.yml`, the
selftest-first order the security pipeline's license job uses), so a
change to the workflow that forgets the manifest, or to the manifest
that forgets a leg, fails there. Deliberately not read: Taskfile.yml's
`api:gen`/`api:merge` task legs enumerate the same universe for the
local regeneration command, and keeping them in step with the manifest
stays a code-review concern (the workflow steps' "the same command as
Taskfile's api:gen task" comments) -- this gate guards the workflow. A
fragment added to the tree that touches no trigger path at all does not
run this job (a path filter cannot name a directory it does not know);
the moment its author touches any trigger path -- the Taskfile `api:gen`
leg, the workflow itself, the manifest -- the gate runs and catches an
omission.

## check_markdown_examples.py — markdown Go-example compiler/parser

Closes the gap root CLAUDE.md's Documentation section names plainly:
"Godoc Example functions are compiled and run by CI inside each module's
unit suite... Examples embedded in markdown prose (AGENTS.md, READMEs,
ADRs) have no compile harness." A real survey of this repository's own
corpus (every `AGENTS.md`, every `go/*/README.md` and
`web/packages/*/README.md`, the root `README.md`, `docs/adr/*.md` --
`docs/internal/**` is deliberately out of scope, see the script's own
module docstring) found 20 fenced ```go blocks across 8 files, 18 of
them deliberately partial illustrative fragments and only 2 complete,
self-contained files (their own `package` clause). That split is not
incidental — most of a good AGENTS.md's code blocks are "here is the
shape of this call", not "here is a whole program" — so the script draws
its check exactly on that line rather than forcing every fragment into a
padded, noisy full program just to satisfy a linter:

* A **complete** block (starts with its own `package` clause) is a claim
  of real, working code. It is really compiled: written into a throwaway
  Go module whose own imports are scanned for
  `github.com/vislake/speed/go/<module>` (or `.../examples/reference-app`)
  paths, `replace`-directived onto that module's real directory under
  `--root`, then `go build ./...` and `go vet ./...` are run against it
  with `GOWORK=off` (so it never inherits the repo's own go.work) and the
  default `GOPROXY` — the `replace` directive alone guarantees this
  repo's own modules resolve to the working tree under review rather than
  a stale published version regardless of `GOPROXY`, so `GOPROXY=off`
  would buy nothing for that goal while blocking real third-party
  transitive dependencies from resolving on a cold module cache. A real
  compile failure here is a genuine documentation bug.
* A **fragment** block (no `package` clause) cannot be honestly compiled
  without inventing context its author never wrote, so it is instead
  syntax-checked with `gofmt -e` (a zero-additional-code front end onto
  `go/parser` — gofmt's own implementation is exactly `parser.ParseFile`
  plus `go/printer`) under several throwaway wrappings in turn: as a
  top-level declaration list (legal even for a bare function *signature*
  with no body — Go's grammar permits that), as a function body, as an
  interface body, as a struct body, as a const block, as a var block.
  The first wrapping that parses wins; a fragment that parses under NONE
  of them is a hard violation, not a warning — a syntactically broken
  snippet (a stray `...` elision placeholder standing where Go requires a
  real token, an unbalanced brace) is exactly the bug a reader
  cutting-and-pasting the snippet would hit immediately, and this
  repository's own first run of the script found two real instances of
  precisely that shape (go/dbkit/AGENTS.md, go/admin/AGENTS.md — both
  fixed in the same commit that introduced this script, never papered
  over by loosening the check).

A fragment that is genuinely, unavoidably unwrappable can be marked with
an HTML comment on the line immediately before its fence —
`<!-- markdown-example: no-parse-check -->` — a hard, git-diff-visible,
per-block opt-out a reviewer sees in the same pull request that adds it,
mirroring this repository's other adjudication escapes (`license_scan.py`'s
`adr` field, semgrep's planted-fixture allowlist). Nothing in the corpus
uses it today.

Usage:

```
python3 tools/check_markdown_examples.py --root /path/to/repo
python3 tools/check_markdown_examples.py        # --root defaults to the current directory
python3 tools/check_markdown_examples.py --survey     # corpus census only, no compiling/parsing
python3 tools/check_markdown_examples.py --keep-temp  # leave throwaway build dirs on disk
```

Needs `go` and `gofmt` on PATH (the one script under `tools/` with that
requirement) but no network beyond whatever `go mod tidy` needs to
resolve a complete block's ordinary third-party imports from the local
module cache, and no Docker. Runs in the docs-check pipeline
(`.github/workflows/docs-check.yml`) after a pinned Go install.

## gen_error_code_index.py — error-code index generator

Closes docs/internal/13-documentation-standards.md's must-have doc list
item (an error-code index) -- go/pkgcore/apperr/apperr.go only has the
encoding mechanism (the six status-mapped builders), never a catalog of
the codes built with it, and no such catalog existed anywhere in the
repository before this script.

It walks every `*.go` file under `--roots` (default: `go` and `examples`,
excluding `_test.go` and generated `*.gen.go`/`*_gen.go` files) and
indexes every construction of an `*apperr.Error` whose code argument is
a string literal, in three shapes: the named declaration
`ErrFoo = apperr.Invalid("module.code")`; the struct-literal
`ErrFoo = &apperr.Error{Code: "module.code", ...}` (the shape
go/sharing's, go/integration's, go/org's and go/ai-gateway's own
`ErrRateLimited` use, since none of apperr's six builder statuses is 429);
and a declaration routed through a module-local helper that builds the
error from its own string parameter (`ErrFoo = rateLimited("module.code")`,
go/org's and go/notification's 429-stamping convention). The same three
shapes are indexed INLINE, with no package-level declaration at all: a
`panic(apperr.Invalid("module.code"))`, a
`return nil, apperr.Internal("module.code")` or an assignment carries its
code into the index exactly as a declaration does -- a code is indexed iff
it is constructed in Go source with a literal code argument; the absence
of a declaration never hides a code (this closed the blind spot where 25
inline codes -- jobs' option-time panics, dbkit's plugin/connect refusals,
the reference app's request-shape refusals -- were silently invisible).

For each construction the tool collects: the Go identifier (when the
construction binds one), the code string, the resulting HTTP status (429
assumed for the struct-literal and helper shapes, since none of the six
builder statuses -- 400/401/403/404/409/500 -- is 429), the file:line,
the "triggering condition" -- for a declaration, the contiguous doc
comment immediately above it; for an inline construction, the comment
directly above the construction line, else its enclosing function's doc
comment -- and the code's own `en-US.toml` catalog entry when one exists
(go/pkgcore/i18n's own message-catalog convention: the TOML key *is* the
apperr code) -- a code with none is reported as such rather than silently
omitted. The entryless class spans two shapes: boot-time wiring refusals
that never reach an end user, and request-time refusals that reach one
only as their structured code, whose client-side text (if any) comes from
the client's own fallback, never from this table.

The result is one Markdown file, one row per code, one table per module
(grouped by the code's own dot-prefix, e.g. `notification` from
`notification.type_not_found`), sorted by module then code; a code
constructed more than once collapses to one row, a named declaration's
row winning over an inline site's.

Usage:

```
python3 tools/gen_error_code_index.py                          # writes docs/error-codes.md
python3 tools/gen_error_code_index.py --check                  # exit 1 if docs/error-codes.md is stale
python3 tools/gen_error_code_index.py --roots go examples --out docs/error-codes.md
```

Cross-checked, at generation time, against the 34-code reachable-error
enumeration `examples/reference-app/web/src/codes-alignment.test.ts`
hand-maintains for its four frontend surfaces: every one of that
enumeration's backend (non-`client.*`) codes appears in the generated
index.

The generator's own `--check` compares the committed index against its
own output, so a construction form it does not index is invisible to that
gate; the blind spot is closed by
`tools/check_error_code_index_coverage.py` (next section), which derives
the expected code set independently from the real tree and turns red on
any code the index has no row for. The two checkers run side by side in
the docs-check pipeline.

## check_error_code_index_coverage.py — error-code index coverage checker

The independent-side counterpart of the generator's `--check`: it scans
the real Go tree with its own implementation of the same coverage domain
(comment text masked out by a small lexer, code literals matched across a
bounded multi-line window so a continuation-line literal the generator
cannot see turns red, helper discovery by body evidence: a function that
builds an `*apperr.Error` from one of its own parameters) and compares the
codes it finds against the rows of the committed `docs/error-codes.md` --
the semgrep_fixture_check shape of comparing a tool's artifact against
the real tree, so an extractor blind spot shows as a real diff instead of
silent agreement between the file and the generator. It additionally
reports the unindexable classes that would otherwise break the index's
"every code built with a literal argument" claim silently: builder calls
whose code argument is not a string literal outside an apperr-constructing
helper's own parameterized body, and calls to such a helper with a
non-literal argument. The check fails (exit 1) printing every missing
code with its construction sites, or every unindexable construction, so
its output is directly comparable against an independent probe of the
tree. When the tree predates a code's indexing this check goes red first
and the generator extension (or an index regeneration) makes it green
again.

Usage:

```
python3 tools/check_error_code_index_coverage.py                # exit 1 on any missing/unindexable code
python3 tools/check_error_code_index_coverage.py --roots go examples --index docs/error-codes.md
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

Seven semgrep rules, one per architecture-discipline row of the
`docs/internal/18-cicd.md` discipline table. Each rule file carries its own
header: the discipline row it maps to, the exact shapes that fire, the
path allowlist (every allowlisted site is a deliberate, documented
mechanism, named and justified in the header), and the residual gaps the
rule deliberately does not close (code review owns those evasions).

| Rule file | Discipline row (18-cicd table / root CLAUDE.md) | What fires | Allowlist (paths.exclude) | Residual gap |
|---|---|---|---|---|
| `deployment-mode-branch.yml` | no `if mode == "standalone"` branching in business logic | mode-value comparisons (`==`/`!=`, both operand orders), `case` labels naming the mode values, `os.Getenv("APP_DEPLOYMENT_MODE")`; same-package literal-valued consts resolve before matching, so an alias const still fires | test files; `go/pkgcore/deployment_mode.go` (the mode constants, parser and `RequiredCapabilities()` dispatch -- the one library-internal mode consultation left by the deployment-composition retrofit); the reference-app assembly (`internal/app/server.go`, where `ConfigFromEnv` reads `APP_DEPLOYMENT_MODE` and `BuildServer` passes it to Bootstrap); `go/authn/module.go` (the newOptions refusal of a distributed composition that wired no explicit `WithSMSSender` -- the module-owned SMSSender seam's assembly-time check, counterpart of the pkgcore Kernel's own seam resolution, wiring-time only per `WithDeploymentMode`'s doc); the saasctl generated-project command entries (the embedded `cmd/server/config.go` and each authn-containing selection's `server.go`, consumer projects materialized outside this tree). The former registry.go / observability init.go / main.go / saasctl `db migrate.go` entries were inert since the retrofit removed their mode switches (migrate's standalone-only gate went in a later round once its premise was shown false); they now fire on sight instead of being exempted | an indirection neither text matching nor value propagation reaches -- a helper inside an allowlisted file, a cross-package alias constant, a runtime-computed decision; branching on a `Capability` (`caps.Has(MultiReplicaSafe)`-shaped) is deliberately not covered -- the capability vocabulary arrived with the retrofit and no discipline row governs it yet |
| `gorm-automigrate-ban.yml` | no `AutoMigrate` (migrations are versioned SQL) | any `.AutoMigrate(...)` call in shipped code. STATUS: future guard -- zero real call sites exist today | test files | none stated (the ban is total) |
| `handwritten-tenant-id-filter.yml` | no hand-written `WHERE tenant_id = ?` | a `Where` / `Or` / `Not` / `Having` chain call whose FIRST argument is a string literal containing a `tenant_id = ?`-style clause | test files; `go/dbkit/**` (the scoping plugin builds the filter everyone else relies on); `go/jobs/store.go` (platform-data idempotency guard); `go/config/store.go` (the configs-table accessor's exact `(scope, tenant_id, key)` lookups on platform data -- the model implements no `TenantScoped`, so no injected filter exists for this table, and ScopeSystem rows carry an empty tenant_id) | a clause assembled dynamically (fmt.Sprintf into the clause, a filter passed through a helper); only the first-argument literal form is matched |
| `non-constant-log-message.yml` | log messages are constant strings (structured logging) | a call to the observability logger's `Info`/`Warn`/`Error`/`Debug` whose first argument is not a string literal (fmt.Sprintf output, concatenation, a variable) | test files | a raw-string (backtick) literal message is flagged although constant (none exists today); anything logged outside the shared structured logger is a separate discipline |
| `raw-gorm-bypass.yml` | no `db.Table` / `db.Model` / `db.Raw` around the Repository | any call to the three bypass entry points on any receiver, in shipped code | test files; `go/dbkit/**` (dbkit owns the raw surface it provides); `go/jobs/store.go` (platform data whose dispatch query must scan every tenant); `go/notification/repository.go` (the inbox UnreadCount's one tenant-scoped COUNT query -- gorm's Count finisher can only anchor to a table through a Model destination, and dbkit's tenant-scoping plugin is designed around exactly that shape, injecting `WHERE tenant_id` from the Model's TenantScoped type, so the call is what makes the count tenant-scoped, not a bypass); `go/authn/internal/testutil/testutil.go` (a test fixture that cannot be named `*_test.go` itself, since authn's own external test files import it); `go/authn/identity.go` (identity-domain rows dbkit.Repository[T] cannot serve -- its constraint requires TenantScoped, which identity data deliberately does not implement -- with one flagged call, DeleteUnlessLastLoginMethod's serializing touch-lock, a value-preserving single-column conditional UPDATE gorm can only anchor to a model); `go/pki/internal/testutil/migration_dedupe.go` (the migration-0008 duplicate-ledger regression's dual-package fixture -- shared by the module's SQLite unit leg and its `integration_test/` PostgreSQL leg, so it cannot be named `*_test.go`; its flagged calls probe the revocation ledger, each dialect's index catalog and the `schema_migrations` ledger -- registry/platform rows, never tenant data) | a workaround through another `*gorm.DB` method (e.g. `Exec` with hand-written SQL) is not matched |
| `snake-case-log-attribute-key.yml` | log attribute keys must be snake_case literals (the 18-cicd "log field names" row) | a key-value pair of the structured logger's `Info`/`Warn`/`Error`/`Debug` whose KEY is a double-quoted literal that is not snake_case (any uppercase letter, hyphen, space or other character outside `[a-z0-9_.]`; dotted config-style keys stay snake per segment); the value position is excluded from the literal requirement so a literal in value position cannot bind as a key | test files; `http.Error(w, ...)` (a net/http stdlib helper whose `(writer, message, status)` shape collides with the logger-call syntax) | a key passed as an identifier (a key-name constant), a dynamically built key, or a `slog.Attr` literal constructed before the call -- the redaction layer still masks by key name at the sink, and review owns the evasion |
| `tenant-id-metric-label.yml` | `tenant_id` never becomes a Prometheus/OTel metric label | the metric-label NAME carriers: `metric.WithAttributes(...)` and `[]attribute.KeyValue{...}` literals containing the text `tenant_id`, the four `prometheus.New*Vec` constructors and `prometheus.Labels{...}` literals | test files; span attributes and `.WithLabelValues(...)` value passes deliberately do NOT fire (spans are the tenant dimension's sanctioned home; the NAME is the cardinality hazard, fixed at vector construction) | a `tenant_id` label past the first variadic option / first composite-literal element; a label name introduced through a constant indirection (`attribute.String(obs.TenantIDKey, ...)`) -- the runtime assertion test and review cover those |

The fixtures live under `tools/semgrep_rules/testdata/<rule>/`: each rule's
`positive.go` must fire on every pattern shape the rule declares, and
`negative.go` proves the flip side (the shapes that deliberately stay
clean, including allowlisted behavior). The rules are proven against those
fixtures and against the real tree before shipping. Fixture proofs run per
rule (`--config tools/semgrep_rules/<rule>.yml` against that rule's own
`testdata/` directory): a negative fixture may legitimately carry a shape
another rule fires on, so a whole-ruleset scan over fixtures is not the
proof shape.

The same per-rule expectations are re-checked on every pull request by
`tools/semgrep_fixture_check.py`, invoked at the end of the semgrep step
in fast-check's repo-checks job: each rule must fire at least once on its
own `positive.go` and stay clean on its own `negative.go` (the mirror of
how the no-literal-text rule's own unit tests keep that rule honest).
The self-check exists because the real-tree scan alone cannot detect a
semgrep upgrade that silently stopped matching a rule -- the tree would
stay clean and nothing would go red.

The first two expectations compare the rule to its own fixture, so a
rename that aged the rule and the fixture TOGETHER keeps them green
while the real tree moved on -- the teeth prove "every rule fires on its
own fixture", not "the fixture still looks like real code" (the
deployment-mode env rename from `SPEED_DEPLOYMENT_MODE` to
`APP_DEPLOYMENT_MODE` aged that rule and its positive fixture exactly
this way). A third, reverse expectation closes the blind spot without
any semgrep run: every environment-variable name literal a rule's
positive fixture exercises (a direct `os.Getenv`/`os.LookupEnv` string
argument, or a same-file literal const passed to one) must still be read
by a non-test `.go` file under `go/` `examples/` `tools/` (the rules'
own `testdata/` subtree excluded, the same exclusion shape the real-tree
scan applies) -- a fixture naming an env variable real code no longer
reads has aged away from the tree and proves nothing.

Running locally (the docker image is the pinned local version; CI instead
pip-installs into a throwaway venv -- see below):

```
docker run --rm -v "$PWD:/repo:ro" -w /repo \
  returntocorp/semgrep:1.176.0 semgrep scan --config tools/semgrep_rules \
  --error --exclude tools/semgrep_rules go examples tools
```

The fixture self-check runs the same way, per rule, through a shim that
maps the `semgrep` binary to the pinned image:

```
python3 tools/semgrep_fixture_check.py /path/to/semgrep-docker-shim
```

Execution status, stated honestly:

- The real-tree scan passes today: 0 findings across `go/` `examples/`
  `tools/` (89 Go files, 7 rules), exit 0. Proven locally with the docker
  image above on the round's final state.
- The fixture self-check passes today: all seven rules fire on their
  planted `positive.go` fixtures and stay clean on their own
  `negative.go`. Proven locally with the same pinned image (through a
  throwaway shim in /tmp, nothing installed on the host).
- Known parser limitation: semgrep always skips line 19 of
  `examples/reference-app/internal/notes/repository.go` (the embedded
  instantiated generic `*dbkit.Repository[Note]` raises a PartialParsing
  exception; roughly 3.4% of that file's lines are never analyzed).
  The skipped line is a struct-field declaration, which none of the seven
  rules' shapes targets, so no rule is blind-sided today -- but a rule
  written later must know this file cannot be fully scanned.
- Version posture: the CI step installs `semgrep==1.176.0`, the version
  the local proofs ran against (the pinned `returntocorp/semgrep:1.176.0`
  image). It was deliberately unpinned until the first green CI run; that
  condition has been met, and leaving it floating made the ruleset the
  only merge-gating tool in a repository that pins every other version.
  The pin is not mirrored in `.mise.toml` or `check_toolchain.py` -- that
  is new gate wiring for a future CI round, not a drift fix. The docker
  run prints a `safe.directory` warning because the host git path is
  unreachable inside the container -- benign, the scan completes.

## license_scan.py — dependency license compliance

Verifies `tools/dependency-licenses.json` against the live tree. The
manifest records the license adjudication for every direct third-party
dependency of the implemented Go modules (all direct requires in
`go/*/go.mod`) and npm packages (`web/packages/*/package.json`
dependencies + peerDependencies, versions resolved from
`web/pnpm-lock.yaml` -- what a frozen-lockfile CI install yields).
Workspace-internal packages carry no entry.
`examples/reference-app/go.mod` is deliberately out of scope: the
reference app is a consumer example, not a delivered library, so its own
dependencies are not part of the repository's dependency-delivery list
(its direct third-party requires are all permissive today). Transitive
coverage and the reference app's own deps are the M4 release-prep
expansion of this manifest.

Policy (mirroring `docs/internal/20-quality-and-security.md`): strong
copyleft (GPL family, AGPL) fails outright; weak copyleft (MPL, LGPL)
fails unless the entry carries an `adr` field naming an existing `docs/`
file that records the adjudication (`github.com/hashicorp/vault/api`'s
entry is the one case today, adjudicated by
`docs/adr/0003-accept-mpl2-for-pki-signer-vault.md`); any unrecognized
license string fails closed with an adjudication message; the permissive
set (0BSD, Apache-2.0, BSD-2/3-Clause, CC0-1.0, ISC, MIT, Unlicense)
passes.
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
`expected_exit` file) passes 10/10, and the real-tree check passes ("55
manifest entries match the tree, all licenses within policy" -- 46 go +
9 npm), both proven locally. Wired into the security pipeline's license
job (selftest, then the real check) in `.github/workflows/security.yml`.

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
release-version form (`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`, the
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
fail the build on a nonzero exit: `python3 tools/scan_cjk.py`,
`python3 tools/check_toolchain.py`, `python3 tools/check_repo_isolation.py`
and the semgrep ruleset step (catalogued above) run in fast-check's
repo-checks job (every pull request and every push to main,
`.github/workflows/fast-check.yml`);
`python3 tools/check_i18n_keys.py` plus `python3 tools/check_docs_site.py`
plus `python3 tools/check_markdown_examples.py` (after a pinned Go
install, `./.github/actions/setup-go-env`) run in the docs-check pipeline
(`.github/workflows/docs-check.yml`), whose pull_request path filter fires
on PRs touching documentation, i18n resources, or the two modules
(`go/dbkit`, `go/ratelimit`) a markdown example currently claims to
build against; and the license scanner (`python3 tools/license_scan.py`,
selftest first, then the real check) runs in the security pipeline's
license job (`.github/workflows/security.yml`); and the
fragment-enumeration gate (suite first, then the real check:
`python3 tools/test_check_api_fragments.py` and
`python3 tools/check_api_fragments.py`) runs as the first steps of the
api-contract job (`.github/workflows/api-contract.yml`), whose path
filter also names the manifest and the gate itself so a change to
either re-runs the checks that consume it.
`tools/gen_error_code_index.py --check` runs in the docs-check pipeline
(`.github/workflows/docs-check.yml`) as a plain-python3 step alongside
the i18n key-set parity checker -- no setup step -- failing the job when
docs/error-codes.md is not what the generator renders from the current
tree. It landed there once the committed index had drifted 271 codes
behind the 359 the tool rendered (later rounds added codes with no
regeneration), retiring the row docs-check.yml's own DELIBERATELY NOT
WIRED list used to carry.
Locally, run them from the repository root — the default `--root` is the
current directory, so plain `python3 tools/scan_cjk.py` also works there. All output paths are relative
to `--root`. `license_scan.py` is the exception: it takes no `--root` at
all (passing one is a usage error, exit 2) and always resolves the
repository root from its own location under `tools/`, so it can be invoked
by absolute path from any working directory. All scripts here are plain
executables with no third-party Python dependencies and no module metadata
of their own; nearly all of them need nothing beyond `python3`, so a CI
image never needs a Go toolchain or a package install just to enforce
these disciplines. `check_markdown_examples.py` is the one exception --
compiling a markdown code example is, unavoidably, a Go-toolchain
operation, so its docs-check step is the one place in this pipeline that
installs one (`.github/actions/setup-go-env`, no golangci-lint). The
generator is a developer-time tool: run it when a
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
