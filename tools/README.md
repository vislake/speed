# tools/ — repo scripts

Plain, dependency-free Python scripts (standard library only, Python >= 3.11
for `tomllib`) that back the repository's cross-cutting disciplines: three
discipline checkers and one scaffold generator. The checkers are the
local-run counterparts of the CI discipline checks scheduled in
`docs/internal/18-cicd.md` (the table rows for banning CJK outside
`docs/internal/`, for requiring identical zh-CN/en-US message-key sets, and
for making every tenant-scoped Repository run the tenancytest isolation
suite, all marked there as self-written scripts); CI workflows mount them
under `tools/`. The generator is the backend of the `task new:module`
promised by `docs/internal/19-dev-workflow.md`. Nothing here needs anything
beyond `python3`, and the checkers print hit paths relative to their
`--root`.

| Script | Kind | Enforces / does | Exit codes |
|---|---|---|---|
| `scan_cjk.py` | Checker | Root `CLAUDE.md` Language Rule: English everywhere outside `docs/internal/` | 0 clean / 1 violations / 2 error |
| `check_i18n_keys.py` | Checker | Root `CLAUDE.md` internationalization rule: zh-CN and en-US key sets identical | 0 clean / 1 mismatch or parse error / 2 error |
| `check_repo_isolation.py` | Checker | Multi-tenant isolation discipline: every Repository type (a struct embedding `dbkit.Repository[T]`) is covered by `tenancytest.AssertIsolated` in its package's tests | 0 all covered / 1 uncovered repository / 2 error |
| `new_module.py` | Generator | Scaffolds the canonical stub of a new Go module under `go/<name>` and prints its registration checklist; never modifies shared repository files | 0 scaffolded / 2 refusal or validation error |

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
| `docs/site/` | Future localization tree (per `CLAUDE.md`); reserved. |
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
scaffolding, a registration checklist prints (go.work `use` entry, CI
matrix row, lockstep release tag list, roadmap and navigation rows) as
reminders — the script never edits those shared files itself, because a
scaffolder that silently rewrites `go.work` and CI matrices makes review
diffs unreadable; the checklist is the contract for the human or for the
future Taskfile task.

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

## Running in CI and locally

CI workflows mount the checkers directly, from the repository root, and
fail the build on a nonzero exit: `python3 tools/scan_cjk.py` runs in
pr-check's repo-checks job (every pull request,
`.github/workflows/pr-check.yml`), and `python3 tools/check_i18n_keys.py`
runs in the docs-check pipeline (`.github/workflows/docs-check.yml`),
whose pull_request path filter fires on PRs touching documentation or
i18n resources. `tools/check_repo_isolation.py` is wired into no workflow
yet; its row lands with a future CI round. Locally, run them from the
repository root — the default `--root` is the current directory, so plain
`python3 tools/scan_cjk.py` also works there. All output paths are relative
to `--root`. All scripts are plain executables with no third-party
dependencies and no module metadata of their own; they live here precisely
so a CI image never needs a Go toolchain or a package install to enforce
these disciplines. The generator is a developer-time tool: run it when a
roadmap item assigns a module a milestone, commit the three scaffolded
files with the design doc, and perform the printed registrations in the
same change.
