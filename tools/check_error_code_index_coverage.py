#!/usr/bin/env python3
"""Independent error-code index coverage checker.

The blind-spot mirror of tools/gen_error_code_index.py's own --check.
That gate compares the committed docs/error-codes.md against the
generator's output -- both sides derive from the same scanner, so a
construction form the scanner does not index is invisible to it: this
check exists so an extractor blind spot shows as a real diff. It derives
the set of codes the index SHOULD carry independently from the real Go
tree and compares it against the rows the committed index actually has
(the same shape as tools/semgrep_fixture_check.py's
check_fixture_env_liveness, which compares a tool's artifact against the
real tree rather than against another artifact of the same tool).

The domain is deliberately identical to the generator's -- every
*apperr.Error constructed in hand-written Go source under --roots
(default: go/ and examples/; _test.go and generated *.gen.go / *_gen.go
files excluded) with a string-literal code argument, declared or inline:

    ErrFoo = apperr.Invalid("module.code")          (named declaration)
    panic(apperr.Invalid("module.code"))            (inline: builder)
    return nil, apperr.Internal("module.code").     (inline, chained
        WithCause(err))                                  continuation)
    ErrFoo = &apperr.Error{Code: "module.code", ...}     (struct literal)
    ErrFoo = rateLimited("module.code")   (declaration or inline call
                                routed through a module-local helper that
                                builds the error from its own string
                                parameter -- the 429-stamping rateLimited
                                convention)

-- but the scan is its own implementation, deliberately broader at the
margin than the generator's line-based one, so a form the generator
cannot see turns into a missing-code finding rather than silent
agreement:

  * the builder-call and code-literal patterns span lines (a bounded
    window of whitespace between the opening parenthesis and the
    literal), so a code on its own continuation line is found here even
    though gen_error_code_index.py indexes only same-line literals
    (none exists in the tree today; if one ever lands, this check goes
    red and the generator must be extended rather than the code
    re-spelled);
  * comment text is masked out by a small lexer (full-line "//" lines,
    trailing "//" comments and "/* ... */" blocks), so a doc comment
    quoting a construction can never fabricate a code on this side;
  * the unindexable class is reported rather than silently absorbed,
    because the index's "every code built in Go source with a literal
    argument" claim only stays checkable while that class stays empty.
    Two shapes are refused: a builder call whose code argument is not a
    string literal anywhere outside an apperr-constructing helper's own
    parameterized body (a package-level var built from a concatenation,
    say -- no index can carry its code), and a call to an
    apperr-constructing helper with a non-literal argument
    (rateLimited(someVar)). A helper -- a function that builds an
    *apperr.Error from one of its own parameters -- is the one
    sanctioned channel for a non-literal construction, because its codes
    all come from the helper's literal call sites and are indexed
    there; anything else building a code from a variable would be
    invisible to both this check and the index.

When the two sides disagree the check fails, printing every code the
tree constructs that the committed index has no row for (with each
construction site) and every unindexable construction (with its site),
so the printed list is directly comparable against an independent probe
of the tree. Exit codes follow this directory's convention: 0 clean /
1 findings / 2 usage or environment error. Nothing is written; the
generator alone writes docs/error-codes.md.

Usage:
    python3 tools/check_error_code_index_coverage.py
    python3 tools/check_error_code_index_coverage.py --roots go examples --index docs/error-codes.md
"""

from __future__ import annotations

import argparse
import pathlib
import re
import sys

_BUILDERS = ("NotFound", "Invalid", "Conflict", "Unauthorized", "Forbidden", "Internal")
_BUILDER_ALT = "|".join(_BUILDERS)

# The same file scope the generator walks: hand-written Go source under
# the roots, no tests, no generated code.
_EXCLUDED_SUFFIXES = ("_test.go",)
_EXCLUDED_GLOBS = ("*.gen.go", "*_gen.go")

# A bounded-window literal-argument builder call. \s{0,120} spans a code
# literal sitting up to a few lines below its builder -- the multi-line
# form this checker sees and the generator (single-line) does not.
_BUILDER_LITERAL_RE = re.compile(
    r'apperr\.(?:%s)\s*\(\s{0,120}"([^"]+)"' % _BUILDER_ALT,
    re.DOTALL,
)
# A builder call regardless of its argument, for the non-literal class.
_BUILDER_CALL_RE = re.compile(
    r'apperr\.(?:%s)\s*\(' % _BUILDER_ALT,
    re.DOTALL,
)
# A &apperr.Error literal carrying a Code field, bounded to the literal's
# own braces.
_STRUCT_LITERAL_RE = re.compile(
    r'&apperr\.Error\s*\{[^}]{0,400}?Code:\s*"([^"]+)"',
    re.DOTALL,
)
# Any bare-identifier call with a string-literal first argument; only a
# callee discovered as an apperr-constructing helper counts.
_CALL_LITERAL_RE = re.compile(
    r'(?P<callee>[A-Za-z_]\w*)\s*\(\s{0,120}"(?P<code>[^"]+)"',
    re.DOTALL,
)
# Any bare-identifier call at all, to spot helper calls whose first
# argument is not a string literal (the unindexable class).
_CALL_RE = re.compile(r'(?P<callee>[A-Za-z_]\w*)\s*\(', re.DOTALL)
# The body evidence marking a function an apperr-constructing helper: an
# apperr construction whose first argument is a bare identifier -- only a
# function that builds an *apperr.Error from one of its own parameters
# (rateLimited's "func rateLimited(code string)" convention) can have its
# literal call sites' arguments indexed as codes. A function whose body
# constructs codes from literals only (a With* option function, an Open)
# is not a helper -- nothing about its arguments is a code.
_CONSTRUCTION_RE = re.compile(
    r'apperr\.(?:%s)\s*\(\s*([A-Za-z_]\w*)\s*[,)]|'
    r'&apperr\.Error\s*\{\s*Code:\s*([A-Za-z_]\w*)\s*[,}]' % _BUILDER_ALT
)
# Named function declaration line (column zero; named functions cannot
# nest in Go).
_FUNC_DECL_RE = re.compile(r'^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)\s*\(')


def _is_excluded(path: pathlib.Path) -> bool:
    if any(path.name.endswith(suf) for suf in _EXCLUDED_SUFFIXES):
        return True
    return any(path.match(pat) for pat in _EXCLUDED_GLOBS)


def mask_comments(text: str) -> str:
    """Returns text with every comment region replaced by spaces (newlines
    preserved, so offsets still map to original line numbers).

    A small per-line lexer tracks string literals (double-quoted with
    escapes, and backtick raw strings) so "//" and "/*" inside a string
    are never mistaken for comment starts, and comment regions -- full
    or trailing "//" comments and "/* ... */" blocks, which may span
    lines -- are blanked. A doc comment quoting apperr.Invalid("x") must
    never fabricate a code on this side of the check."""
    out = []
    i = 0
    n = len(text)
    in_block = False
    while i < n:
        ch = text[i]
        if in_block:
            if text.startswith("*/", i):
                in_block = False
                out.append("  ")
                i += 2
                continue
            out.append(" " if ch != "\n" else "\n")
            i += 1
            continue
        if text.startswith("//", i):
            # Trailing or full-line comment: blank to end of line.
            while i < n and text[i] != "\n":
                out.append(" ")
                i += 1
            continue
        if text.startswith("/*", i):
            in_block = True
            out.append("  ")
            i += 2
            continue
        if ch == '"':
            # Double-quoted string with backslash escapes.
            out.append(ch)
            i += 1
            while i < n:
                out.append(text[i])
                if text[i] == "\\" and i + 1 < n:
                    out.append(text[i + 1])
                    i += 2
                    continue
                if text[i] == '"':
                    i += 1
                    break
                i += 1
            continue
        if ch == "`":
            out.append(ch)
            i += 1
            while i < n and text[i] != "`":
                out.append(text[i])
                i += 1
            if i < n:
                out.append(text[i])
                i += 1
            continue
        out.append(ch)
        i += 1
    return "".join(out)


def _line_of(text: str, offset: int) -> int:
    return text.count("\n", 0, offset) + 1


def _functions_of(lines: list[str]) -> list[tuple[str, int]]:
    """The named functions of one file: (name, line index) in declaration
    order. Named functions cannot nest, so consecutive declarations bound
    each function's body extent (the doc comments of the next function sit
    between them at column zero and carry no code)."""
    decls: list[tuple[str, int]] = []
    for i, line in enumerate(lines):
        stripped = line.strip()
        if not stripped.startswith("func "):
            continue
        m = _FUNC_DECL_RE.match(stripped)
        if m:
            decls.append((m.group(1), i))
    return decls


def _scan_file(rel: str, text: str, helpers: set[str]) -> tuple[list[tuple[str, str]], list[tuple[str, str]]]:
    """Returns (codes, unindexable) for one file: every literal-argument
    code constructed here, and every non-literal-argument builder
    construction outside an apperr-constructing helper's own body."""
    lines = text.splitlines()
    decls = _functions_of(lines)
    # Helper bodies: line ranges (inclusive decl line .. exclusive next
    # decl line) of discovered helpers, inside which the parameterized
    # apperr.Invalid(code) construction is sanctioned (its codes are
    # indexed at the helper's literal call sites instead).
    helper_ranges: list[tuple[int, int]] = []
    for k, (name, decl_line) in enumerate(decls):
        if name not in helpers:
            continue
        body_end = decls[k + 1][1] if k + 1 < len(decls) else len(lines)
        helper_ranges.append((decl_line, body_end))

    masked = mask_comments(text)
    codes: list[tuple[str, str]] = []
    for m in _BUILDER_LITERAL_RE.finditer(masked):
        codes.append((m.group(1), f"{rel}:{_line_of(masked, m.start())}"))
    for m in _STRUCT_LITERAL_RE.finditer(masked):
        codes.append((m.group(1), f"{rel}:{_line_of(masked, m.start())}"))
    for m in _CALL_LITERAL_RE.finditer(masked):
        if m.group("callee") in helpers:
            codes.append((m.group("code"), f"{rel}:{_line_of(masked, m.start())}"))

    def _first_arg(match: re.Match, limit: int = 120) -> str | None:
        window = masked[match.end() : match.end() + limit]
        first = re.search(r"\S", window)
        return first.group(0) if first else None

    def _line_snippet(lineno: int) -> str:
        return text.splitlines()[lineno - 1].strip()[:100]

    unindexable: list[tuple[str, str]] = []
    for m in _BUILDER_CALL_RE.finditer(masked):
        lineno = _line_of(masked, m.start())
        if any(lo <= lineno - 1 <= hi for lo, hi in helper_ranges):
            # A parameterized construction inside an apperr-constructing
            # helper's own body (rateLimited's apperr.Invalid(code)): its
            # codes are indexed at the helper's literal call sites.
            continue
        if _first_arg(m) != '"':
            # A builder call whose code argument is not a string literal
            # outside any helper body -- a package-level declaration
            # building a code from a variable or concatenation, say: a
            # code the index can never carry. The literal-argument form on
            # the same line was already captured above.
            unindexable.append((_line_snippet(lineno), f"{rel}:{lineno}"))
    for m in _CALL_RE.finditer(masked):
        if m.group("callee") not in helpers:
            continue
        lineno = _line_of(masked, m.start())
        if lines[lineno - 1].strip().startswith("func "):
            # The helper's own declaration line ("func rateLimited(code
            # string)") is not a call.
            continue
        if _first_arg(m) == '"':
            continue  # the literal-argument form was already captured
        # A call to an apperr-constructing helper with a non-literal code
        # argument (rateLimited(someVar)): the code it builds is
        # unindexable -- the helper's literal call sites are the one
        # sanctioned channel.
        unindexable.append((_line_snippet(lineno), f"{rel}:{lineno}"))
    return codes, unindexable


def _param_identifiers(lines: list[str], decl_line: int) -> set[str]:
    """The identifiers appearing in a function's parameter list (see
    gen_error_code_index.py's twin for the full rationale): the
    candidates for "this function builds an error from one of its own
    parameters". The signature is accumulated until the line opening the
    body; the parameter group is the paren group whose closing paren
    precedes the body brace, matched backward so a method receiver's own
    paren group is not mistaken for it."""
    sig_parts: list[str] = []
    for j in range(decl_line, len(lines)):
        sig_parts.append(lines[j])
        if "{" in lines[j]:
            break
    sig = " ".join(sig_parts)
    brace = sig.find("{")
    sig = sig[:brace] if brace != -1 else sig
    close = sig.rfind(")")
    if close == -1:
        return set()
    depth = 0
    open_paren: int | None = None
    for i in range(close, -1, -1):
        if sig[i] == ")":
            depth += 1
        elif sig[i] == "(":
            depth -= 1
            if depth == 0:
                open_paren = i
                break
    if open_paren is None:
        return set()
    return set(re.findall(r"[A-Za-z_]\w*", sig[open_paren + 1 : close]))


def _discover_helpers_in_lines(lines: list[str], masked_lines: list[str]) -> set[str]:
    """The functions of one file that construct an *apperr.Error from one
    of their own parameters -- whose literal call sites' arguments are
    codes (rateLimited and friends). Comment text is masked before both
    the signature scan and the evidence test, so a doc comment quoting a
    construction is never body evidence. Named functions cannot nest, so
    consecutive declarations bound each function's body."""
    decls = _functions_of(lines)
    helpers: set[str] = set()
    for k, (name, decl_line) in enumerate(decls):
        params = _param_identifiers(masked_lines, decl_line)
        if not params:
            continue
        body_end = decls[k + 1][1] if k + 1 < len(decls) else len(lines)
        for line in masked_lines[decl_line + 1 : body_end]:
            for evidence in _CONSTRUCTION_RE.finditer(line):
                arg = evidence.group(1) or evidence.group(2)
                if arg in params:
                    helpers.add(name)
                    break
            if name in helpers:
                break
    return helpers


def discover_helpers(root_paths: list[pathlib.Path]) -> set[str]:
    """Every function in the scanned tree that constructs an *apperr.Error
    from one of its own parameters -- the apperr-constructing helpers
    whose literal call sites carry codes (rateLimited and friends)."""
    helpers: set[str] = set()
    for root in root_paths:
        for path in sorted(root.rglob("*.go")):
            if _is_excluded(path):
                continue
            text = path.read_text(encoding="utf-8", errors="replace")
            lines = text.splitlines()
            masked_lines = mask_comments(text).splitlines()
            helpers |= _discover_helpers_in_lines(lines, masked_lines)
    return helpers


def index_codes(index_path: pathlib.Path) -> set[str]:
    text = index_path.read_text(encoding="utf-8", errors="replace")
    return set(re.findall(r"^\| `([^`]+)` \|", text, re.M))


def collect_findings(
    root_paths: list[pathlib.Path],
    repo_root: pathlib.Path,
    index_path: pathlib.Path,
) -> tuple[dict[str, list[str]], list[str]]:
    """Returns (missing, unindexable): every code constructed in the
    scanned tree that the committed index has no row for, keyed to all of
    its construction sites (repo-relative "file:line" strings), and every
    non-literal-argument builder construction outside an
    apperr-constructing helper's own body, as "snippet  file:line"."""
    helpers = discover_helpers(root_paths)
    constructed: dict[str, list[str]] = {}
    unindexable: list[str] = []
    for root in root_paths:
        for path in sorted(root.rglob("*.go")):
            if _is_excluded(path):
                continue
            try:
                rel = str(path.relative_to(repo_root))
            except ValueError:
                rel = str(path.relative_to(root))
            text = path.read_text(encoding="utf-8", errors="replace")
            codes, bad = _scan_file(rel, text, helpers)
            for code, site in codes:
                constructed.setdefault(code, []).append(site)
            unindexable.extend(bad)

    indexed = index_codes(index_path)
    missing = {code: constructed[code] for code in sorted(set(constructed) - indexed)}
    return missing, sorted(set(unindexable))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--roots", nargs="+", default=["go", "examples"], help="Directories to scan for *.go files (relative to the repo root).")
    parser.add_argument("--index", default="docs/error-codes.md", help="The committed error-code index to check (relative to the repo root).")
    args = parser.parse_args()

    repo_root = pathlib.Path(__file__).resolve().parent.parent
    for r in args.roots:
        if not (repo_root / r).is_dir():
            print(f"error: --roots directory not found: {r}", file=sys.stderr)
            return 2
    index_path = repo_root / args.index
    if not index_path.is_file():
        print(f"error: index file not found: {args.index}", file=sys.stderr)
        return 2

    root_paths = [repo_root / r for r in args.roots]
    missing, unindexable = collect_findings(root_paths, repo_root, index_path)

    n_sites = sum(len(v) for v in missing.values())
    findings = 0
    if missing:
        findings = 1
        print(f"codes constructed in Go source with NO row in {args.index} ({len(missing)}):")
        for code, sites in missing.items():
            print(f"  {code}")
            for site in sites:
                print(f"    constructed at {site}")
    else:
        print(f"every code constructed in Go source has a row in {args.index}.")

    if unindexable:
        findings = 1
        print(f"apperr builder calls whose code argument is not a string literal -- "
              f"unindexable by design; make the code a literal or route it through "
              f"a declaration ({len(unindexable)}):")
        for site in unindexable:
            print(f"  {site}")

    if findings:
        print("error: error-code index coverage check failed", file=sys.stderr)
        return 1
    print("error-code index coverage check passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
