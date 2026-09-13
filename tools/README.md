# tools

Repository self-checks. Each script is a plain executable with a `--root`
option (default: the current directory) and prints the paths it reports
relative to that root. They are stdlib-only Python, so `python3` is the
whole dependency list — except `check_markdown_examples.py`, which shells
out to the Go toolchain because compiling an example is its job.

Python 3.11 or newer is required: `check_toolchain.py` parses TOML with
`tomllib`. The root `.mise.toml` pins the version `mise install` provides.

## Running them

A script named `check_*.py` is a gate: it takes no arguments, it reports
against the current directory, and `make repo-check` runs every one of
them, so a gate added here is wired by its name alone. `scan_cjk.py`
carries another prefix because it is run on demand rather than by
`make check`.

Each script also runs on its own:

```
python3 tools/check_toolchain.py
python3 tools/check_markdown_examples.py
python3 tools/scan_cjk.py
```

Their own test suites run together, from the repository root:

```
make tools-test
```

## The scripts

`check_toolchain.py` — fails when a version pinned in the root
`.mise.toml` no longer mirrors the authoritative source that a tool
actually reads. Today that is one pin, `go` against `go.work`'s `go`
directive; the file's own header says which pins have no mirror and are
therefore not checked. Exit codes: 0 clean, 1 drift, 2 infrastructure
error (a source missing or unparsable, a mirrored tool absent from
`.mise.toml`).

`check_markdown_examples.py` — compiles or parse-checks the fenced Go code
blocks in markdown prose, which no other harness covers (godoc `Example`
functions compile inside their module's own test binary; prose does not).
A block that carries its own `package` clause is claiming to be real
code and is built and vetted for real, in a throwaway module wired by
`replace` to the checkout it documents. A block without one is a
fragment, and is syntax-checked under several wrappings instead of being
forced into a padded full program. The script's own docstring carries the
scope rules and the escape hatch for a fragment that genuinely cannot
parse.

`scan_cjk.py` — reports CJK characters outside the subtrees it carves
out, which is how the language rule stated in the root `CLAUDE.md` gets
checked. The two lists are read together: a tree `CLAUDE.md` declares
Chinese is only exempt once it is carved out here. Go files are checked
in their comments only, through a lexer that honours string and rune
context; other text files are checked whole. The script's docstring
carries the carve-outs.
