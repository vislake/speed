#!/usr/bin/env python3
"""check_markdown_examples.py -- compile/parse-check fenced ```go blocks in
markdown prose (AGENTS.md, package READMEs).

Godoc Example functions compile inside each module's own unit suite;
examples embedded in markdown prose have no compile harness of their own,
and this script is the check that closes that gap.

WHAT IS CHECKED, and why a blanket "every fenced go block must go build"
rule is not honest here: the overwhelming majority of fenced ```go blocks
in the markdown corpus are deliberately partial illustrative fragments --
a bare method signature, a struct literal, a handful of statements with
the surrounding function/imports left to the reader's imagination -- not
complete, buildable Go files. Forcing every one of those into a padded,
noisy "complete program" just to satisfy a linter would make the docs
worse, not better. So this script draws the same line the corpus itself
draws, by whether a block's own first non-comment line is a `package`
clause:

  * A COMPLETE block (own `package` clause) is a claim that this is real,
    working code. It is compiled for real: written into a throwaway Go
    module, `replace`-directived onto whichever workspace modules its own
    import list names (inferred from the import paths themselves -- no
    guessing beyond that), then `go build`
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

Survey: run with --survey to re-print the corpus census, and the census
of the ```go blocks this gate leaves alone. The first answers the
honest-check question above: the corpus's fenced ```go blocks are
overwhelmingly partial illustrative fragments, which is why the
package-clause distinction exists. The second keeps the scope decision
below checkable rather than assumed.

CORPUS -- exactly:
  * the repository root's own AGENTS.md and README.md
  * every AGENTS.md and README.md inside a module the go.work workspace
    lists
  * NOT prose outside the workspace (see below)
  * NOT any path under .git/, node_modules/ or vendor/

The workspace is the scope, and go.work is where it is read from, for
two reasons. One is truthfulness: `make check` runs this gate, so
whatever it reaches is a tree this repository builds and checks, and the
root CLAUDE.md says which trees those are. A gate reaching past the
workspace would make that statement false, quietly, in a file every
agent reads first. The other is cost: compiling a complete block
resolves the dependency graph of every module it imports, so a block in
a tree outside the workspace would pull that tree's whole dependency
graph into every CI run, on a cold module cache, before the first
in-workspace example compiles. Scoping by the roster also means no list
of paths is maintained here: a module joining the workspace brings its
prose into the corpus with it.

Design and decision documents are out of scope by the same rule, and
would be even if they lived inside a module directory: their code blocks
sketch shapes under design rather than document a shipped API, so a
sketch that does not parse is normal there and must not fail this check.

ESCAPE HATCH: a fragment that is genuinely, unavoidably unwrappable (a
snippet illustrating invalid-on-purpose code, say) can be marked with an
HTML comment on the line immediately before the opening fence:

    <!-- markdown-example: no-parse-check -->
    ```go
    ... whatever the doc needs to show ...
    ```

This is a hard, git-diff-visible, per-block opt-out a reviewer sees in the
same pull request that adds it: recorded and reviewed, never silent.

Exit codes: 0 clean; 1 a block failed its check (a complete block did not
build/vet clean, a complete block imported a module the workspace does
not list, or a fragment parsed under no wrapping); 2 infrastructure error
(go or gofmt missing, the go.work roster unreadable or listing no module,
--root is not a directory, or a build harness step could not even run).

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
import json
import os
import posixpath
import re
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

# ---------------------------------------------------------------------------
# Corpus discovery
# ---------------------------------------------------------------------------

_EXCLUDED_DIR_NAMES = {".git", "node_modules", "vendor"}

# Directories never descended into that must be matched by exact
# repo-relative path rather than basename: .claude/worktrees/ holds this
# repository's git worktrees -- complete checkouts that carry their own
# AGENTS.md/README trees, gitignored local machine state that never
# exists in CI. Matched exactly, so a directory merely named worktrees/
# is walked and .claude/skills/** stays walked like any other source
# tree (a skill's own SKILL.md is not part of this corpus; an AGENTS.md
# anywhere is, by the scope rule above).
NON_SCANNED_DIR_PATHS = frozenset({".claude/worktrees"})

_SKIP_MARKER = "<!-- markdown-example: no-parse-check -->"

# The prose a module ships. A module's other markdown (a changelog, say)
# makes no claim about the API and carries no compile obligation.
_CORPUS_FILENAMES = frozenset({"AGENTS.md", "README.md"})

_FENCE_OPEN = re.compile(r"^```go\s*$")
_FENCE_CLOSE = re.compile(r"^```\s*$")


@dataclass(frozen=True)
class Workspace:
    """What go.work says: the go directive and the module directories."""

    go_directive: str
    module_dirs: frozenset[str]


class WorkspaceError(Exception):
    """go.work could not be read, or lists no module."""


def _normalize_module_dir(disk_path: str) -> str:
    """A use directive's path as a posix path relative to the root."""
    return posixpath.normpath(disk_path.replace(os.sep, "/")).lstrip("/")


def read_workspace(root: Path) -> Workspace:
    """Read the module roster, letting the go toolchain parse it.

    go.work is the roster, and `go work edit -json` is the parser here for
    the same reason it is the parser in the Makefile: a use directive has
    two legal spellings -- a parenthesised block and a bare `use ./dir`
    line -- and a matcher written here would have to know both or come up
    empty on the one it does not know, without saying so.

    A roster that lists no module is an error rather than an empty corpus.
    Everything this gate checks belongs to a module, so an unreadable or
    empty roster would otherwise report a clean tree it never looked at.
    """
    if shutil.which("go") is None:
        raise WorkspaceError("'go' must be on PATH to read the go.work roster")
    gowork = root / "go.work"
    proc = subprocess.run(
        ["go", "work", "edit", "-json", str(gowork)],
        capture_output=True,
        text=True,
        timeout=60,
    )
    if proc.returncode != 0:
        detail = (proc.stderr + proc.stdout).strip()
        raise WorkspaceError(f"could not read {gowork}: {detail}")
    try:
        document = json.loads(proc.stdout)
    except json.JSONDecodeError as exc:
        raise WorkspaceError(f"could not parse the roster of {gowork}: {exc}") from exc
    module_dirs = frozenset(
        _normalize_module_dir(entry["DiskPath"])
        for entry in (document.get("Use") or [])
        if entry.get("DiskPath")
    )
    if not module_dirs:
        raise WorkspaceError(
            f"{gowork} lists no module, so this gate would check nothing and "
            "still report success"
        )
    return Workspace(str(document.get("Go") or "1.25"), module_dirs)


def _in_scope(rel: str, module_dirs: frozenset[str]) -> bool:
    """Whether rel (posix-style, relative to --root) is part of the corpus.

    Exactly: the root's own AGENTS.md and README.md, plus either file
    inside a module the workspace lists. See this module's docstring
    "CORPUS" section for why the workspace is the scope.
    """
    base = os.path.basename(rel)
    if base not in _CORPUS_FILENAMES:
        return False
    if rel == base:
        return True
    return any(
        directory == "." or rel.startswith(directory + "/")
        for directory in module_dirs
    )


def discover_markdown_files(root: Path, module_dirs: frozenset[str]) -> list[str]:
    """Return in-scope markdown file paths, relative to root, posix-style."""
    found = []
    for dirpath, dirnames, filenames in os.walk(root):
        rel_dir = Path(dirpath).relative_to(root)
        dirnames[:] = [
            d for d in dirnames
            if d not in _EXCLUDED_DIR_NAMES
            and (rel_dir / d).as_posix() not in NON_SCANNED_DIR_PATHS
        ]
        for fn in filenames:
            if not fn.endswith(".md"):
                continue
            full = Path(dirpath) / fn
            rel = full.relative_to(root).as_posix()
            if _in_scope(rel, module_dirs):
                found.append(rel)
    return sorted(found)


def discover_out_of_scope_go_blocks(
    root: Path, module_dirs: frozenset[str]
) -> dict[str, int]:
    """Census only: how many ```go blocks sit in markdown this gate does
    not check, counted per top-level directory -- reported by --survey so
    the scope decision stays checkable rather than assumed. Never fed into
    the pass/fail check."""
    counts: dict[str, int] = {}
    for dirpath, dirnames, filenames in os.walk(root):
        rel_dir = Path(dirpath).relative_to(root)
        dirnames[:] = [
            d for d in dirnames
            if d not in _EXCLUDED_DIR_NAMES
            and (rel_dir / d).as_posix() not in NON_SCANNED_DIR_PATHS
        ]
        for fn in filenames:
            if not fn.endswith(".md"):
                continue
            full = Path(dirpath) / fn
            rel = full.relative_to(root).as_posix()
            if _in_scope(rel, module_dirs):
                continue
            try:
                text = full.read_text(encoding="utf-8")
            except OSError:
                continue
            n = len(extract_go_blocks(text))
            if n:
                tree = rel.split("/")[0] if "/" in rel else "."
                counts[tree] = counts.get(tree, 0) + n
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

# A replace directive pointing at a path inside this repository.
_LOCAL_REPLACE_RE = re.compile(
    r'^replace\s+(github\.com/vislake/speed/\S+)\s+=>\s+(\.\.?/\S+)', re.MULTILINE
)


def _module_dirs_for_block(
    body: str, module_dirs: frozenset[str]
) -> tuple[set[str], set[str]]:
    """Split this block's own github.com/vislake/speed/... imports into the
    workspace modules that provide them, and the import paths no workspace
    module does.

    An import resolves to the longest workspace module directory that
    prefixes it: ".../pkg/config/source/file" resolves to pkg/config,
    source/file being a subpackage rather than a module of its own. No
    list of module names lives here -- go.work is the list, and an import
    it cannot account for is returned as unresolved rather than guessed
    at."""
    needed: set[str] = set()
    unresolved: set[str] = set()
    for match in _SPEED_IMPORT_RE.finditer(body):
        import_path = match.group(1)
        candidates = [
            directory for directory in module_dirs
            if directory != "."
            and (import_path == directory or import_path.startswith(directory + "/"))
        ]
        if candidates:
            needed.add(max(candidates, key=len))
        else:
            unresolved.add(import_path)
    return needed, unresolved


def _local_replacements(root: Path, dirs: set[str]) -> dict[str, Path]:
    """Map every repository module the given modules resolve locally onto
    its directory, following each module's own replace directives
    transitively.

    A replace directive in a dependency is ignored by consumers, so a
    throwaway module that requires pkg/config alone would try to resolve
    pkg/config's own unreleased dependencies from the proxy -- and either
    fail, or silently validate the example against a published snapshot
    instead of this checkout. Re-stating those replacements here is what
    makes the example's compile an assertion about the working tree."""
    found: dict[str, Path] = {}
    pending = list(dirs)
    while pending:
        d = pending.pop()
        if d in found:
            continue
        modfile = root / d / "go.mod"
        if not modfile.is_file():
            continue
        found[d] = (root / d).resolve()
        try:
            text = modfile.read_text(encoding="utf-8")
        except OSError:
            continue
        for m in _LOCAL_REPLACE_RE.finditer(text):
            target = (root / d / m.group(2)).resolve()
            try:
                rel = target.relative_to(root.resolve()).as_posix()
            except ValueError:
                continue
            pending.append(rel)
    return found


def check_complete_block(
    root: Path, rel: str, block: GoBlock, workspace: Workspace, keep_temp: bool
) -> list[str]:
    """Build and vet a complete block for real. Returns violation strings
    (empty means clean)."""
    module_dirs, unresolved = _module_dirs_for_block(block.body, workspace.module_dirs)
    if unresolved:
        named = ", ".join(
            f"github.com/vislake/speed/{path}" for path in sorted(unresolved)
        )
        return [
            f"{rel}:{block.start_line}: imports {named}, which go.work does not "
            "list -- this gate builds an example against the workspace, and a "
            "module outside it has no local source to build against"
        ]
    for d in module_dirs:
        modfile = root / d / "go.mod"
        if not modfile.is_file():
            return [
                f"{rel}:{block.start_line}: imports github.com/vislake/speed/{d}/..., "
                f"but {d}/go.mod does not exist under --root -- go.work lists a "
                "module directory that does not hold a module"
            ]

    go_directive = workspace.go_directive
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
        for d, path in sorted(_local_replacements(root, module_dirs).items()):
            replace_lines.append(f"replace github.com/vislake/speed/{d} => {path}")
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
        help="print the corpus census (files, block counts, complete/fragment "
        "split, and the blocks left out of scope) and exit 0 without compiling "
        "or parsing anything",
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

    try:
        workspace = read_workspace(root)
    except WorkspaceError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    files = discover_markdown_files(root, workspace.module_dirs)

    if args.survey:
        out_of_scope = discover_out_of_scope_go_blocks(root, workspace.module_dirs)
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
        print(f"workspace modules: {' '.join(sorted(workspace.module_dirs))}")
        print(f"in-scope markdown files: {len(files)}")
        print(f"in-scope files with >=1 go block: {files_with_blocks}")
        print(f"total go blocks: {total_blocks}  complete: {total_complete}  fragment: {total_fragment}")
        print(f"go blocks outside the corpus: {sum(out_of_scope.values())}")
        for tree, n in sorted(out_of_scope.items()):
            print(f"  {tree}: {n} blocks")
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
                violations += check_complete_block(
                    root, rel, block, workspace, args.keep_temp
                )
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
