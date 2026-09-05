#!/usr/bin/env python3
"""check_markdown_examples.py -- compile/parse-check fenced ```go blocks in
markdown prose (AGENTS.md, package READMEs, ADRs).

Root CLAUDE.md's Documentation section states the gap this script closes:
"Godoc Example functions are compiled and run by CI inside each module's
unit suite... Examples embedded in markdown prose (AGENTS.md, READMEs, ADRs)
have no compile harness." docs-check.yml's own header carried the matching
entry in its DELIBERATELY NOT WIRED list ("a dedicated harness for
prose-embedded documentation examples... what does not exist is a harness
for examples embedded in docs prose and AGENTS.md files") until this script
and its docs-check.yml wiring landed.

WHAT IS CHECKED, and why a blanket "every fenced go block must go build"
rule is not honest here: a real survey of this repository's markdown corpus
(see the "Survey" note below) found the overwhelming majority of fenced
```go blocks are deliberately partial illustrative fragments -- a bare
method signature, a struct literal, a handful of statements with the
surrounding function/imports left to the reader's imagination -- not
complete, buildable Go files. Forcing every one of those into a padded,
noisy "complete program" just to satisfy a linter would make the docs
worse, not better. So this script draws the same line the corpus itself
draws, by whether a block's own first non-comment line is a `package`
clause:

  * A COMPLETE block (own `package` clause) is a claim that this is real,
    working code. It is compiled for real: written into a throwaway Go
    module, `replace`-directived onto whichever github.com/vislake/speed/
    go/<module> packages its own import list names (inferred from the
    import paths themselves -- no guessing beyond that), then `go build`
    and `go vet` are run against it with GOWORK=off (so it never inherits
    the repo's own go.work) and the default GOPROXY (a `replace` directive
    always wins over the proxy regardless of GOPROXY, so the repo's own
    vislake/speed modules can never silently drift onto a stale published
    version -- the replace lines alone give that guarantee; GOPROXY=off
    would buy nothing for it while blocking every legitimate third-party
    transitive dependency `go mod tidy` needs to resolve on a cold module
    cache). A genuine compile failure here means the example is lying
    about what the API looks like -- a real documentation bug -- and is
    reported as a violation.

  * A FRAGMENT block (no `package` clause) cannot be honestly compiled
    without inventing context its author never wrote. Reinventing that
    context (guessing imports, receivers, surrounding types) would let the
    checker "pass" fragments that are actually wrong in ways a real compile
    would catch, which is worse than not checking at all. Instead, a
    fragment is syntax-checked: gofmt -e (a zero-additional-code front end
    onto go/parser -- gofmt's own implementation is exactly
    `parser.ParseFile` plus `go/printer`; shelling out to it gets the real
    compiler-grade parser this task calls for without this repository
    maintaining a second, hand-rolled Go binary next to it) is run against
    the fragment wrapped several different ways in turn -- as a complete
    top-level declaration list (legal even for a bare function *signature*
    with no body, which Go's grammar permits), as the body of a throwaway
    function, as the body of a throwaway interface, as the body of a
    throwaway struct, and as the body of a throwaway const/var block --
    stopping at the first wrapping that parses. A fragment that fails to
    parse under EVERY wrapping is reported as a violation: a syntactically
    broken example (stray tokens, an unbalanced brace, a bare `...`
    elision placeholder standing where Go actually requires an expression
    or a real elided-and-marked-as-such comment) is worse than a merely
    partial one, and is exactly the class of documentation bug a reader
    cutting and pasting the snippet would hit immediately. This is a
    deliberate policy choice, not a default: see "Escape hatch" below for
    why it is a hard failure rather than a warning, and how an author
    overrides it when the fragment is genuinely, unavoidably not
    wrappable.

Survey (repository state as of this script's introduction; re-run
--survey to recheck): 53 in-scope files were found under the corpus
described below, 8 of which contain at least one ```go block, for 20
blocks total -- 2 "complete" (own `package` clause) and 18 "fragment".
Every fragment in the corpus parses under one of the four wrappings once
two markdown bugs the survey turned up were fixed (a bare `...` elision
standing where Go syntax requires a real token, in go/dbkit/AGENTS.md and
go/admin/AGENTS.md -- see this script's companion commit for the fix).
docs/internal/** also contains ```go blocks (9 files, 26 blocks) but is
out of scope by design: that directory is Chinese-language internal
design discussion (root CLAUDE.md's Language Rule), its own audience and
its own rules, and its code blocks are illustrative API sketches for
still-being-designed shapes -- not the "AGENTS.md, READMEs, ADRs" gap
CLAUDE.md's Documentation section and docs-check.yml's own header name.
Confirmed by inspection, not assumed: several of its blocks mix Chinese
prose comments into the Go and sketch signatures for mechanisms predating
their real implementation, which is a different, and legitimately
unchecked, kind of example than a shipped module's own AGENTS.md makes.

CORPUS -- exactly:
  * every **/AGENTS.md
  * every go/*/README.md and web/packages/*/README.md package README, plus
    the root README.md
  * every docs/adr/*.md
  * NOT docs/internal/** (see above)
  * NOT any path under .git/, node_modules/ or vendor/

ESCAPE HATCH: a fragment that is genuinely, unavoidably unwrappable (a
snippet illustrating invalid-on-purpose code, say) can be marked with an
HTML comment on the line immediately before the opening fence:

    <!-- markdown-example: no-parse-check -->
    ```go
    ... whatever the doc needs to show ...
    ```

This is a hard, git-diff-visible, per-block opt-out a reviewer sees in the
same pull request that adds it -- the same shape as this repository's
other adjudication escapes (dependency-licenses.json's "adr" field for a
weak-copyleft exception, semgrep's planted-fixture allowlist): recorded
and reviewed, never silent. As of this script's introduction nothing in
the corpus uses it -- every real violation the survey found was fixed in
the markdown instead.

Exit codes (matching tools/check_docs_site.py's convention): 0 clean;
1 a block failed its check (a complete block did not build/vet clean, or
a fragment parsed under no wrapping); 2 infrastructure error (go or gofmt
missing, a referenced go/<module> import has no such directory, --root is
not a repository, or a build harness step could not even run).

Usage:
  python3 tools/check_markdown_examples.py             # check the repo
  python3 tools/check_markdown_examples.py --root PATH
  python3 tools/check_markdown_examples.py --survey    # print the corpus
                                                        # census only, no
                                                        # compiling/parsing
  python3 tools/check_markdown_examples.py --keep-temp # leave the
                                                        # throwaway build
                                                        # directories on
                                                        # disk (debugging)

What is NOT checked: a fragment's imports, types and call targets are
never resolved (go/parser does not type-check), so a fragment that parses
but calls a method that does not exist is not caught -- only a complete
block's real `go build` catches that class of bug, by construction, since
only a complete block carries enough context to build at all.
"""

from __future__ import annotations

import argparse
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

# ---------------------------------------------------------------------------
# Corpus discovery
# ---------------------------------------------------------------------------

_EXCLUDED_DIR_NAMES = {".git", "node_modules", "vendor"}

_SKIP_MARKER = "<!-- markdown-example: no-parse-check -->"

_FENCE_OPEN = re.compile(r"^```go\s*$")
_FENCE_CLOSE = re.compile(r"^```\s*$")


def _in_scope(rel: str) -> bool:
    """Whether rel (posix-style, relative to --root) is part of the corpus.

    Exactly: any AGENTS.md, any go/*/README.md or web/packages/*/README.md
    or the root README.md, any docs/adr/*.md. Explicitly NOT
    docs/internal/** -- see this module's docstring "Survey" section for
    why.
    """
    if rel.startswith("docs/internal/"):
        return False
    base = os.path.basename(rel)
    if base == "AGENTS.md":
        return True
    if base == "README.md" and (
        rel.startswith("go/") or rel.startswith("web/packages/") or rel == "README.md"
    ):
        return True
    if rel.startswith("docs/adr/") and rel.endswith(".md"):
        return True
    return False


def discover_markdown_files(root: Path) -> list[str]:
    """Return in-scope markdown file paths, relative to root, posix-style."""
    found = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in _EXCLUDED_DIR_NAMES]
        for fn in filenames:
            if not fn.endswith(".md"):
                continue
            full = Path(dirpath) / fn
            rel = full.relative_to(root).as_posix()
            if _in_scope(rel):
                found.append(rel)
    return sorted(found)


def discover_docs_internal_go_blocks(root: Path) -> dict[str, int]:
    """Census only: which docs/internal/**.md files carry ```go blocks, and
    how many -- reported by --survey so the out-of-scope decision stays
    checkable rather than assumed. Never fed into the pass/fail check."""
    counts: dict[str, int] = {}
    internal_root = root / "docs" / "internal"
    if not internal_root.is_dir():
        return counts
    for dirpath, dirnames, filenames in os.walk(internal_root):
        dirnames[:] = [d for d in dirnames if d not in _EXCLUDED_DIR_NAMES]
        for fn in filenames:
            if not fn.endswith(".md"):
                continue
            full = Path(dirpath) / fn
            rel = full.relative_to(root).as_posix()
            try:
                text = full.read_text(encoding="utf-8")
            except OSError:
                continue
            n = len(extract_go_blocks(text))
            if n:
                counts[rel] = n
    return counts


# ---------------------------------------------------------------------------
# Fenced-block extraction
# ---------------------------------------------------------------------------


class GoBlock:
    __slots__ = ("start_line", "end_line", "body", "skip")

    def __init__(self, start_line: int, end_line: int, body: str, skip: bool):
        self.start_line = start_line  # 1-indexed line of the opening fence
        self.end_line = end_line  # 1-indexed line of the closing fence
        self.body = body
        self.skip = skip  # the no-parse-check escape hatch was used


def extract_go_blocks(text: str) -> list[GoBlock]:
    lines = text.splitlines()
    blocks: list[GoBlock] = []
    i = 0
    while i < len(lines):
        if _FENCE_OPEN.match(lines[i]):
            start = i
            j = i + 1
            while j < len(lines) and not _FENCE_CLOSE.match(lines[j]):
                j += 1
            body = "\n".join(lines[start + 1 : j])
            skip = _preceded_by_skip_marker(lines, start)
            blocks.append(GoBlock(start + 1, min(j, len(lines) - 1) + 1, body, skip))
            i = j + 1
        else:
            i += 1
    return blocks


def _preceded_by_skip_marker(lines: list[str], fence_index: int) -> bool:
    k = fence_index - 1
    # Tolerate one blank line between the marker and the fence, but the
    # marker itself must be the nearest non-blank line -- an escape hatch
    # buried paragraphs above the block it claims to cover would not be
    # the git-diff-visible, per-block thing this is meant to be.
    while k >= 0 and lines[k].strip() == "":
        k -= 1
    return k >= 0 and lines[k].strip() == _SKIP_MARKER


def classify(body: str) -> str:
    """'complete' if the block's own first non-comment, non-blank line is a
    `package` clause; 'fragment' otherwise (including an empty block)."""
    for line in body.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("//"):
            continue
        return "complete" if stripped.startswith("package ") else "fragment"
    return "fragment"


# ---------------------------------------------------------------------------
# Complete-block check: a real throwaway-module `go build` + `go vet`
# ---------------------------------------------------------------------------

_SPEED_IMPORT_RE = re.compile(r'"github\.com/vislake/speed/([^"]+)"')


def _module_dirs_for_block(body: str) -> set[str]:
    """Infer which github.com/vislake/speed/<module_dir> this block's own
    import list needs, from the import paths alone -- e.g. an import of
    ".../go/pkgcore/i18n" needs the go/pkgcore module (i18n is a
    subpackage, not a separate module); ".../examples/reference-app/..."
    needs the examples/reference-app module."""
    dirs: set[str] = set()
    for m in _SPEED_IMPORT_RE.finditer(body):
        segments = m.group(1).split("/")
        if not segments:
            continue
        if segments[0] == "go" and len(segments) >= 2:
            dirs.add(f"go/{segments[1]}")
        elif segments[0] == "examples" and len(segments) >= 2 and segments[1] == "reference-app":
            dirs.add("examples/reference-app")
    return dirs


def _repo_go_directive(root: Path) -> str:
    goword = root / "go.work"
    try:
        text = goword.read_text(encoding="utf-8")
    except OSError:
        return "1.25"
    m = re.search(r"^go\s+(\S+)", text, re.MULTILINE)
    return m.group(1) if m else "1.25"


def check_complete_block(
    root: Path, rel: str, block: GoBlock, keep_temp: bool
) -> list[str]:
    """Build and vet a complete block for real. Returns violation strings
    (empty means clean)."""
    module_dirs = _module_dirs_for_block(block.body)
    for d in module_dirs:
        modfile = root / d / "go.mod"
        if not modfile.is_file():
            return [
                f"{rel}:{block.start_line}: imports github.com/vislake/speed/{d}/..., "
                f"but {d}/go.mod does not exist under --root -- the example names a "
                "module that is not part of this repository"
            ]

    go_directive = _repo_go_directive(root)
    package_match = re.search(r"^\s*package\s+(\w+)", block.body, re.MULTILINE)
    if not package_match:
        return [f"{rel}:{block.start_line}: classified complete but has no package clause (internal error)"]

    tmpdir = Path(tempfile.mkdtemp(prefix="mdexample-"))
    try:
        require_lines = []
        replace_lines = []
        for d in sorted(module_dirs):
            import_path = f"github.com/vislake/speed/{d}"
            require_lines.append(f"\t{import_path} v0.0.0-00010101000000-000000000000")
            replace_lines.append(f"replace {import_path} => {(root / d).resolve()}")
        go_mod = ["module speedmdcheck.local/example", "", f"go {go_directive}", ""]
        if require_lines:
            go_mod += ["require (", *require_lines, ")", ""]
        go_mod += replace_lines
        (tmpdir / "go.mod").write_text("\n".join(go_mod) + "\n", encoding="utf-8")
        (tmpdir / "example.go").write_text(block.body + "\n", encoding="utf-8")

        env = dict(os.environ)
        env["GOWORK"] = "off"
        # Deliberately NOT GOPROXY=off: the replace directives above already
        # guarantee the repo's own vislake/speed modules resolve to this
        # working tree regardless of GOPROXY (replace always wins over the
        # proxy), so GOPROXY=off buys nothing for that goal -- it only blocks
        # `go mod tidy` from fetching real third-party transitive
        # dependencies (e.g. go/pkgcore pulls in BurntSushi/toml,
        # nicksnyder/go-i18n, golang.org/x/text, go-redis, minio-go) on a
        # cold module cache, which would misreport an infrastructure/network
        # problem as a documentation bug.
        env["GOFLAGS"] = "-mod=mod"

        violations = []
        steps = [
            ("go mod tidy", ["go", "mod", "tidy"]),
            ("go build ./...", ["go", "build", "./..."]),
            ("go vet ./...", ["go", "vet", "./..."]),
        ]
        for label, argv in steps:
            proc = subprocess.run(
                argv, cwd=tmpdir, env=env, capture_output=True, text=True, timeout=180
            )
            if proc.returncode != 0:
                detail = (proc.stdout + proc.stderr).strip()
                violations.append(
                    f"{rel}:{block.start_line}: {label} failed for this complete example "
                    f"(module(s) {sorted(module_dirs) or ['<none>']}):\n{detail}"
                )
                break
        return violations
    finally:
        if keep_temp:
            print(f"  (kept: {tmpdir})", file=sys.stderr)
        else:
            shutil.rmtree(tmpdir, ignore_errors=True)


# ---------------------------------------------------------------------------
# Fragment check: gofmt -e against several throwaway wrappings
# ---------------------------------------------------------------------------

# Each wrapping is tried in turn; the first one gofmt -e accepts wins. A
# bare function *signature* with no body is legal top-level Go (the grammar
# permits FunctionDecl without a FunctionBody, the shape used for
# assembly-implemented functions) which is why "top-level" alone already
# covers the common "here is the shape of several methods" sketch, without
# needing an interface wrapping for that case.
_WRAPPINGS = [
    ("top-level", "package p\n{body}\n"),
    ("func-body", "package p\nfunc _() {{\n{body}\n}}\n"),
    ("interface-body", "package p\ntype I interface {{\n{body}\n}}\n"),
    ("struct-body", "package p\ntype T struct {{\n{body}\n}}\n"),
    ("const-block", "package p\nconst (\n{body}\n)\n"),
    ("var-block", "package p\nvar (\n{body}\n)\n"),
]


def check_fragment(gofmt_bin: str, body: str) -> tuple[bool, str, str]:
    """Returns (ok, strategy_name_or_empty, first_error_text_or_empty)."""
    first_error = ""
    first_strategy = ""
    for name, template in _WRAPPINGS:
        src = template.format(body=body)
        proc = subprocess.run(
            [gofmt_bin, "-e"], input=src, capture_output=True, text=True, timeout=30
        )
        if proc.returncode == 0:
            return True, name, ""
        if not first_error:
            first_error = proc.stderr.strip() or proc.stdout.strip()
            first_strategy = name
    return False, "", f"(first attempted: {first_strategy})\n{first_error}"


# ---------------------------------------------------------------------------
# Driver
# ---------------------------------------------------------------------------


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Compile complete (own `package` clause) ```go blocks in markdown "
            "prose for real, and syntax-check fragment blocks under several "
            "throwaway wrappings. See this file's module docstring for the "
            "full design and what is/isn't checked."
        )
    )
    parser.add_argument(
        "--root", default=".", help="repository root to check (default: current directory)"
    )
    parser.add_argument(
        "--survey",
        action="store_true",
        help="print the corpus census (files, block counts, complete/fragment split, "
        "the docs/internal/** out-of-scope count) and exit 0 without compiling or "
        "parsing anything",
    )
    parser.add_argument(
        "--keep-temp",
        action="store_true",
        help="leave each complete-block's throwaway build directory on disk instead "
        "of deleting it (debugging)",
    )
    args = parser.parse_args(argv)

    root = Path(args.root).resolve()
    if not root.is_dir():
        print(f"error: --root is not a directory: {args.root}", file=sys.stderr)
        return 2

    files = discover_markdown_files(root)

    if args.survey:
        internal = discover_docs_internal_go_blocks(root)
        total_blocks = 0
        total_complete = 0
        total_fragment = 0
        files_with_blocks = 0
        for rel in files:
            text = (root / rel).read_text(encoding="utf-8")
            blocks = extract_go_blocks(text)
            if not blocks:
                continue
            files_with_blocks += 1
            for b in blocks:
                total_blocks += 1
                if classify(b.body) == "complete":
                    total_complete += 1
                else:
                    total_fragment += 1
        print(f"in-scope markdown files: {len(files)}")
        print(f"in-scope files with >=1 go block: {files_with_blocks}")
        print(f"total go blocks: {total_blocks}  complete: {total_complete}  fragment: {total_fragment}")
        print(f"docs/internal/** files with go blocks (out of scope): {len(internal)}")
        for rel, n in sorted(internal.items()):
            print(f"  {rel}: {n} blocks")
        return 0

    go_bin = shutil.which("go")
    gofmt_bin = shutil.which("gofmt")
    if go_bin is None or gofmt_bin is None:
        print("error: both 'go' and 'gofmt' must be on PATH", file=sys.stderr)
        return 2

    violations: list[str] = []
    checked_complete = 0
    checked_fragment = 0
    skipped = 0

    for rel in files:
        full = root / rel
        try:
            text = full.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError) as exc:
            violations.append(f"{rel}: could not read file as UTF-8 text ({exc})")
            continue
        blocks = extract_go_blocks(text)
        for block in blocks:
            if block.skip:
                skipped += 1
                print(f"markdown-examples: skip (escape hatch)  {rel}:{block.start_line}")
                continue
            kind = classify(block.body)
            if kind == "complete":
                checked_complete += 1
                violations += check_complete_block(root, rel, block, args.keep_temp)
            else:
                checked_fragment += 1
                ok, strategy, err = check_fragment(gofmt_bin, block.body)
                if not ok:
                    violations.append(
                        f"{rel}:{block.start_line}: fragment does not parse under any "
                        f"wrapping (top-level / func body / interface body / struct body "
                        f"/ const block / var block) -- a syntactically broken example "
                        f"{err}"
                    )

    print(
        f"markdown-examples: checked {checked_complete} complete block(s) (real go build "
        f"+ go vet) and {checked_fragment} fragment(s) (gofmt -e parse check), "
        f"{skipped} skipped via the no-parse-check escape hatch"
    )
    for v in violations:
        print(f"markdown-examples: violation    {v}")
    if violations:
        print(
            f"error: {len(violations)} markdown Go example(s) failed -- fix the "
            "markdown content (never pad a fragment into a full program just to "
            "satisfy this check; see this script's module docstring for the escape "
            "hatch if a block is genuinely unwrappable)",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
