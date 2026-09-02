# tools/ — repo-discipline checkers

Plain, dependency-free Python scripts (standard library only, Python >= 3.11
for `tomllib`) that enforce two of the repository's cross-cutting
disciplines locally. They are the local-run counterparts of the CI
discipline checks scheduled in `docs/internal/18-cicd.md` (the table rows
for banning CJK outside `docs/internal/` and for requiring identical
zh-CN/en-US message-key sets, both marked there as self-written scripts);
CI workflows mount them under `tools/`. Neither script needs anything beyond
`python3`, and both print hit paths relative to their `--root`.

| Script | Enforces | Exit codes |
|---|---|---|
| `scan_cjk.py` | Root `CLAUDE.md` Language Rule: English everywhere outside `docs/internal/` | 0 clean / 1 violations / 2 error |
| `check_i18n_keys.py` | Root `CLAUDE.md` internationalization rule: zh-CN and en-US key sets identical | 0 clean / 1 mismatch or parse error / 2 error |

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

A message id is the leaf key of each key/value pair whose value is not a
table. This matches how the repository itself names keys: a quoted dotted
key such as `"notes.text_required"` is a single TOML key and is the message
id, exactly as handler code references it. Section headers such as `[errors]`
are organizational grouping and do not prefix the id — a flat file and a
section-grouped file with the same leaf keys match. If one file defines the
same leaf id under two different paths (two sections carrying a key of the
same name), the pairing across languages would be ambiguous, and the script
reports that file as an error rather than comparing it.

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

## Running in CI and locally

CI workflows mount these scripts directly (`python3 tools/scan_cjk.py
--root "$REPO"` and `python3 tools/check_i18n_keys.py --root "$REPO"`) and
fail the build on a nonzero exit. Locally, run them from the repository
root — the default `--root` is the current directory, so plain
`python3 tools/scan_cjk.py` also works there. All output paths are relative
to `--root`. Both scripts are plain executables with no third-party
dependencies and no module metadata of their own; they live here precisely
so a CI image never needs a Go toolchain or a package install to enforce
these two rules.
