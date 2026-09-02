#!/usr/bin/env python3
"""Tenant-isolation test coverage checker for the repository's Repositories.

docs/internal/18-cicd.md's architecture-discipline table carries the row
"every Repository must run the isolation test suite", marked there as a
self-written script ("self-written" distinguishes it from the rows that
reuse ready-made Go tooling): the enforcing script scans Repository
implementations and compares them against a test-coverage inventory. This
script is that check's local-run counterpart (the row is marked
self-written like the CJK and i18n-key rows that tools/scan_cjk.py and
tools/check_i18n_keys.py already implement).

What is checked. Root CLAUDE.md's multi-tenant isolation rules require every
tenant-data / link-data repository to embed the mandatory generic base
`dbkit.Repository[T]` instead of holding a raw connection, and the tenancytest
suite (go/tenancy/tenancytest, whose doc comment names the same obligation --
"the mandatory isolation-assertion test suite every other module is required
to run against its own dbkit.Repository[T] usage") requires each such usage
to be exercised by the tenancytest.AssertIsolated assertion. Concretely the
script scans every Go module under --root and, for every top-level struct
type that anonymously embeds the dbkit Repository generic (a field of the
form `dbkit.Repository[T]` or `*dbkit.Repository[T]`, with no field name,
i.e. `type Repository struct { *dbkit.Repository[Note] }`), checks that the
type's own package tests call tenancytest.AssertIsolated. A repository with
no such call in its package is reported with its declaration location; exit
code is 1 when anything is uncovered, 0 when everything is covered.

Coverage attribution heuristics (documented here because the script is a
textual scanner, not a Go type checker):

  * A package with exactly one Repository-embedding type is covered by any
    tenancytest.AssertIsolated call in its tests -- with only one
    repository, a compiling call can only be about it.
  * A package with several embedding types is covered per type: a type is
    covered when some AssertIsolated call's text mentions the type's own
    name or any identifier inside its embedding's type arguments
    (dbkit.Repository[Note] mentions Note). Real-world call sites satisfy
    this naturally -- the newRecord argument of tenancytest.AssertIsolated
    is written "func(tenant pkgcore.TenantID) *Note { ... }" -- but a call
    that names neither is reported as not covering the type, with the calls
    found listed so the author can see why.
  * Coverage tests are the package's _test.go files in the package
    directory plus the _test.go files under its physically separated
    integration_test/ directory (root CLAUDE.md testing rule: integration
    tests live in their own directory so a plain `go test` never touches
    them). Build tags are ignored: the check is textual, so tagged-in and
    tagged-out files are both counted.

What is deliberately NOT required, and why:

  * Types declared in _test.go files (test doubles, Example-code types
    like go/dbkit/example_test.go's embedding demonstration) are never
    candidates -- the discipline covers shipped repository
    implementations, not fixtures. They are listed as informational notes
    when seen.
  * The identity-data / platform-data half of the tenancytest pair
    (AssertNotTenantScoped) is reported but not required: those models
    never touch dbkit.Repository[T] (its generic constraint requires
    TenantScoped, which identity and platform data must not implement), so
    nothing in production code statically marks them and they cannot be
    enumerated. Observed AssertNotTenantScoped calls are printed as
    informational notes.
  * dbkit's own module cannot call tenancytest at all (tenancytest lives
    in go/tenancy, which requires dbkit -- a dbkit -> tenancy require would
    be a module cycle), and its own repository_test.go / tenant_scope_test.go
    are the mechanism's proof; the script only tracks *embeddings*, which
    dbkit itself has none of outside its Example code.

Scanner limitations (assumed gofmt'd code, as the repo enforces): top-level
type declarations only -- a standalone declaration starts in column 0, and
grouped declarations inside a `type ( ... )` block are handled per entry;
import aliases are honoured but dot imports of the dbkit package are not
tracked (an embedding written unqualified because of `import . ".../dbkit"`
is not seen); a type alias of dbkit.Repository embedded under its own name
is not seen (the alias is not an embedding of the base itself); in a grouped
entry declaring several names for one struct (A, B struct { ... }) only the
first name is attributed; comments are skipped wholesale, so a field
boundary hidden inside a multi-line block comment is not seen. None of
these shapes occur in the repository today; the tenancytest doc comment
above each of these choices is the authority when one appears.

Usage:
    python3 tools/check_repo_isolation.py [--root PATH]

--root defaults to the current directory; run from the repository root for a
whole-repo scan. Every finding prints a grep-friendly
"<repo-relative path>:<line>: ..." line plus a summary. Paths are relative
to --root.

Exit codes: 0 = every Repository-embedding type is covered; 1 = at least one
uncovered repository; 2 = bad usage or an unrecoverable error (reported on
stderr). Standard library only; requires Python >= 3.11 (shared floor with
the other tools/ scripts).
"""

from __future__ import annotations

import argparse
import os
import sys
from dataclasses import dataclass, field

# Directories never entered regardless of content.
PRUNED_DIR_NAMES = frozenset({".git", "node_modules", "vendor"})

# Test files (and whole integration_test/ trees) are never candidates: the
# discipline covers shipped repository implementations, not test doubles or
# Example code (go/dbkit/example_test.go embeds dbkit.Repository purely to
# demonstrate the convention).
TEST_FILE_SUFFIX = "_test.go"
TEST_DIR_NAME = "integration_test"

# The import paths that make a qualifier meaningful. Matching is exact -- in
# this repository every module imports these two packages by their full
# monorepo path. The module-level constants are the extension point if a
# consumer repo with a different org prefix wants to reuse the script.
DBKIT_PKG_PATH = "github.com/vislake/speed/go/dbkit"
TENANCYTEST_PKG_PATH = "github.com/vislake/speed/go/tenancy/tenancytest"

# Assertion call names, from go/tenancy/tenancytest's public API
# (assert_isolated.go / assert_not_tenant_scoped.go).
ASSERT_ISOLATED = "AssertIsolated"
ASSERT_NOT_TENANT_SCOPED = "AssertNotTenantScoped"

# Delimiters that raise / lower the nesting depth of a token stream. Go
# type syntax never mismatches bracket kinds across levels in valid code
# and the repo is assumed gofmt'd, so a single depth counter is enough.
_OPEN = frozenset("([{")
_CLOSE = frozenset(")]}")


@dataclass
class Token:
    kind: str  # "ident" | "sym" | "str" | "newline"
    text: str
    line: int  # 1-based
    pos: int  # 0-based offset into the source text


def tokenize(text: str) -> list[Token]:
    """Go-lite lexer: identifiers, symbols, string contents, newlines.

    Comments (line and block) are skipped as trivia; newlines inside block
    comments are swallowed with the comment. That is accurate for gofmt'd
    code (a field boundary can never hide inside a comment there) and keeps
    the offset bookkeeping exact; the exotic layouts this misses are the
    documented limitations above. String and rune literals ("" and '' with
    backslash escapes, raw `` without) and their contents are emitted as
    single "str" tokens carrying the decoded content -- import paths and
    struct tags are read from these. Unterminated literals and comments run
    to EOF rather than crash, mirroring tools/scan_cjk.py's lexer.
    Non-ASCII bytes pass through as individual "sym" tokens (they cannot
    appear in this repo's code outside comments, which are skipped).
    """
    tokens: list[Token] = []
    i = 0
    n = len(text)
    line = 1
    while i < n:
        c = text[i]
        if c == "\n":
            tokens.append(Token("newline", "\n", line, i))
            i += 1
            line += 1
        elif c in " \t\r\f\v":
            i += 1
        elif c == "/" and i + 1 < n and text[i + 1] == "/":
            j = text.find("\n", i)
            i = n if j == -1 else j  # the newline itself is emitted next
        elif c == "/" and i + 1 < n and text[i + 1] == "*":
            end = text.find("*/", i + 2)
            seg = text[i + 2 : n if end == -1 else end]
            line += seg.count("\n")
            i = n if end == -1 else end + 2
        elif c in "\"'`":
            quote = c
            j = i + 1
            content_parts: list[str] = []
            while j < n:
                ch = text[j]
                if quote == "`":
                    if ch == "`":
                        break
                    content_parts.append(ch)
                    j += 1
                elif ch == "\\":
                    if j + 1 < n:
                        content_parts.append(text[j + 1])
                        j += 2
                    else:
                        content_parts.append(ch)
                        j += 1
                elif ch == quote:
                    break
                else:
                    content_parts.append(ch)
                    j += 1
            content = "".join(content_parts)
            # Backslash escapes inside interpreted literals are rare in
            # import paths and tags; undo the two common ones so reported
            # text stays readable. Exotic escapes stay as-is (harmless for
            # a checker).
            content = content.replace("\\\"", "\"").replace("\\\\", "\\")
            tokens.append(Token("str", content, line, i))
            line += content.count("\n") if quote == "`" else 0
            i = j + 1 if j < n else n
        elif c.isalpha() or c == "_":
            j = i
            while j < n and (text[j].isalnum() or text[j] == "_"):
                j += 1
            tokens.append(Token("ident", text[i:j], line, i))
            i = j
        elif c.isdigit():
            j = i
            while j < n and (text[j].isalnum() or text[j] in "._"):
                j += 1
            tokens.append(Token("sym", text[i:j], line, i))
            i = j
        else:
            tokens.append(Token("sym", c, line, i))
            i += 1
    return tokens


def _column_is_zero(text: str, tok: Token) -> bool:
    """True when tok starts in column 0 of its line (gofmt'd top level)."""
    start = text.rfind("\n", 0, tok.pos)
    return tok.pos - start - 1 == 0  # start == -1 -> tok.pos == 0


def _matching_delim(tokens: list[Token], open_i: int) -> int:
    """Index of the delimiter closing tokens[open_i] (or len(tokens)).

    One depth counter for all three bracket kinds (see _OPEN/_CLOSE); an
    unbalanced stream (an invalid file) simply runs to EOF.
    """
    depth = 0
    i = open_i
    n = len(tokens)
    while i < n:
        t = tokens[i]
        if t.kind == "sym":
            if t.text in _OPEN:
                depth += 1
            elif t.text in _CLOSE:
                depth -= 1
                if depth == 0:
                    return i
        i += 1
    return n


def parse_imports(tokens: list[Token], file_text: str) -> dict[str, str]:
    """Return {qualifier: import path} for the file's imports.

    Top-level import declarations only (col-0 'import' per gofmt). Handles
    parenthesed blocks, single lines, aliases, dot imports and blank
    imports. The default qualifier is the last path segment, which is the
    Go package name for every module path used here; a dot import is keyed
    by "." and a blank import by "_".
    """
    quals: dict[str, str] = {}
    i = 0
    n = len(tokens)
    while i < n:
        tok = tokens[i]
        if (tok.kind == "ident" and tok.text == "import"
                and _column_is_zero(file_text, tok)):
            j = i + 1
            if j < n and tokens[j].kind == "sym" and tokens[j].text == "(":
                close_paren = _matching_delim(tokens, j)
                region = tokens[j + 1:close_paren]
                for start, end in _split_top_level(region):
                    path, alias = _entry_path_and_alias(region[start:end])
                    if path:
                        quals[alias] = path
                i = close_paren + 1 if close_paren < n else n
            else:
                j = i + 1
                while j < n and tokens[j].kind != "newline":
                    j += 1
                path, alias = _entry_path_and_alias(tokens[i + 1:j])
                if path:
                    quals[alias] = path
                i = j
        else:
            i += 1
    return quals


def _split_top_level(region: list[Token]) -> list[tuple[int, int]]:
    """Split a token region at its depth-0 newline / ';' boundaries."""
    spans: list[tuple[int, int]] = []
    start = 0
    depth = 0
    k = 0
    n = len(region)
    while k <= n:
        at_end = k == n
        t = region[k] if not at_end else None
        if not at_end and t.kind == "sym":
            if t.text in _OPEN:
                depth += 1
            elif t.text in _CLOSE:
                depth = max(0, depth - 1)
        if at_end or ((t.kind == "newline" or (t.kind == "sym"
                                               and t.text == ";"))
                      and depth == 0):
            if k > start:
                spans.append((start, k))
            start = k + 1
        k += 1
    return spans


def _entry_path_and_alias(entry: list[Token]) -> tuple[str, str]:
    """Return (import path, qualifier) for one import spec's tokens."""
    if not entry:
        return "", ""
    first = entry[0]
    if first.kind == "ident":
        alias = first.text
    elif first.kind == "sym" and first.text in (".", "_"):
        alias = first.text
    else:
        alias = ""
    path = ""
    for t in entry:
        if t.kind == "str":
            path = t.text
    if not path:
        return "", ""
    if not alias:
        alias = path.rstrip("/").split("/")[-1]
    return path, alias


@dataclass
class Candidate:
    """One Repository-embedding type declaration (a shipped repository)."""

    type_name: str
    models: set[str]  # identifiers in the embedded Repository's type args
    embed_text: str  # e.g. "*dbkit.Repository[Note]" (for messages)
    file_path: str  # repo-relative
    decl_line: int


@dataclass
class CallSite:
    """One tenancytest assertion call in a package's tests."""

    name: str  # AssertIsolated | AssertNotTenantScoped
    file_path: str  # repo-relative
    line: int
    idents: set[str] = field(default_factory=set)  # identifiers in the call
    arg1_text: str = ""  # the repository argument, for diagnostics


def find_candidates(file_path: str, tokens: list[Token], file_text: str,
                    quals: dict[str, str]) -> list[Candidate]:
    """Return the Repository-embedding candidates of one source file.

    A fast path skips files that do not import the dbkit package at all;
    the caller may rely on [] for them.
    """
    dbkit_quals = {q for q, p in quals.items() if p == DBKIT_PKG_PATH}
    if not dbkit_quals:
        return []
    candidates: list[Candidate] = []
    n = len(tokens)
    i = 0
    while i < n:
        tok = tokens[i]
        if not (tok.kind == "ident" and tok.text == "type"
                and _column_is_zero(file_text, tok)):
            i += 1
            continue
        nxt = tokens[i + 1] if i + 1 < n else None
        if nxt is None:
            break
        if nxt.kind == "ident":
            # Standalone declaration: name, optional type parameters, then
            # the spec, all between col-0 tokens.
            j = i + 2
            while j < n and not _column_is_zero(file_text, tokens[j]):
                if (tokens[j].kind == "ident" and tokens[j].text == "struct"):
                    break
                j += 1
            if j < n and tokens[j].kind == "ident" \
                    and tokens[j].text == "struct":
                open_i = j + 1
                if open_i < n and tokens[open_i].kind == "sym" \
                        and tokens[open_i].text == "{":
                    close_i = _matching_delim(tokens, open_i)
                    candidates.extend(_embeddings_in_body(
                        tokens[open_i + 1:close_i], dbkit_quals, nxt.text,
                        file_path, tok.line))
                    i = close_i + 1 if close_i < n else n
                    continue
            i += 1
        elif nxt.kind == "sym" and nxt.text == "(":
            # Grouped declaration: type ( A struct { ... }; B = C ). Each
            # entry is scanned for its own struct spec; a multi-name entry
            # (A, B struct { ... }) attributes the struct to the first name
            # (documented limitation).
            close_paren = _matching_delim(tokens, i + 1)
            region = tokens[i + 2:close_paren]
            for entry_start, entry_end in _split_top_level(region):
                name_tok = region[entry_start]
                if name_tok.kind != "ident":
                    continue
                struct_rel = _entry_struct_index(
                    region[entry_start + 1:entry_end]
                )
                if struct_rel is None:
                    continue
                abs_struct = i + 2 + entry_start + 1 + struct_rel
                open_i = abs_struct + 1
                if open_i < n and tokens[open_i].kind == "sym" \
                        and tokens[open_i].text == "{":
                    close_i = _matching_delim(tokens, open_i)
                    candidates.extend(_embeddings_in_body(
                        tokens[open_i + 1:close_i], dbkit_quals,
                        name_tok.text, file_path, name_tok.line))
            i = close_paren + 1 if close_paren < n else n
            continue
        else:
            i += 1
    return candidates


def _entry_struct_index(entry: list[Token]) -> int | None:
    """Return the entry-relative index of its depth-0 'struct', or None.

    Depth counts the entry's own brackets, so a 'struct' hiding inside the
    type parameters of a field-less spec is skipped and only the spec-level
    one (immediately followed by '{') matches.
    """
    depth = 0
    k = 0
    n = len(entry)
    while k < n:
        t = entry[k]
        if t.kind == "sym":
            if t.text in _OPEN:
                depth += 1
            elif t.text in _CLOSE:
                depth = max(0, depth - 1)
        if (depth == 0 and t.kind == "ident" and t.text == "struct"
                and k + 1 < n and entry[k + 1].kind == "sym"
                and entry[k + 1].text == "{"):
            return k
        k += 1
    return None


def _embeddings_in_body(body: list[Token], quals: set[str], type_name: str,
                        file_path: str, decl_line: int) -> list[Candidate]:
    """Scan one struct body for anonymous dbkit.Repository[...] fields.

    A field starts after a depth-0 newline or ';' (or at the body's start).
    An anonymous embedding begins the field: optional '*', then a dbkit
    qualifier, '.', 'Repository' and its '[' type arguments. Anything else
    (a field name first, a named type, deeper nesting) ends the field start
    and the scan continues after the boundary. Type arguments are read to
    their matching ']' (nested brackets counted) and every identifier
    inside is remembered as a model mention for coverage matching.
    """
    found: list[Candidate] = []
    n = len(body)
    depth = 0
    field_start = True
    b = 0
    while b < n:
        t = body[b]
        if t.kind == "newline" or (t.kind == "sym" and t.text == ";"):
            if depth == 0:
                field_start = True
            b += 1
            continue
        if t.kind == "sym":
            if t.text == "*":
                b += 1  # a pointer marker keeps an embedding anonymous
                continue
            if t.text in _OPEN:
                depth += 1
            elif t.text in _CLOSE:
                depth = max(0, depth - 1)
            field_start = False
            b += 1
            continue
        if t.kind != "ident":
            b += 1
            continue
        if not (field_start and depth == 0 and t.text in quals):
            field_start = False
            b += 1
            continue
        # Lookahead for '.' 'Repository' '['.
        if not (b + 3 < n
                and body[b + 1].kind == "sym" and body[b + 1].text == "."
                and body[b + 2].kind == "ident"
                and body[b + 2].text == "Repository"
                and body[b + 3].kind == "sym"
                and body[b + 3].text == "["):
            field_start = False
            b += 1
            continue
        # Read the type arguments to their matching ']'.
        models: set[str] = set()
        k = b + 4
        arg_depth = 1
        while k < n and arg_depth:
            tk = body[k]
            if tk.kind == "ident":
                models.add(tk.text)
            elif tk.kind == "sym":
                if tk.text == "[":
                    arg_depth += 1
                elif tk.text == "]":
                    arg_depth -= 1
            k += 1
        if arg_depth:  # unbalanced: the file does not compile; give up
            break
        embed_text = "".join(
            tok.text for tok in body[b:k] if tok.kind != "newline"
        )
        found.append(Candidate(
            type_name=type_name,
            models=models,
            embed_text=embed_text,
            file_path=file_path,
            decl_line=decl_line,
        ))
        b = k  # continue after the closing ']'
        field_start = True  # a struct tag or a boundary may follow
    return found


def find_calls(tokens: list[Token], quals: dict[str, str], file_path: str,
               dot_tenancytest: bool) -> list[CallSite]:
    """Return the tenancytest assertion calls of one test file."""
    calls: list[CallSite] = []
    tt_quals = {q for q, p in quals.items() if p == TENANCYTEST_PKG_PATH}
    n = len(tokens)
    i = 0
    while i < n:
        t = tokens[i]
        if t.kind != "ident" or t.text not in (ASSERT_ISOLATED,
                                               ASSERT_NOT_TENANT_SCOPED):
            i += 1
            continue
        name = t.text
        prev = tokens[i - 1] if i >= 1 else None
        prev2 = tokens[i - 2] if i >= 2 else None
        qualified = (
            prev is not None and prev.kind == "sym" and prev.text == "."
            and prev2 is not None and prev2.kind == "ident"
            and prev2.text in tt_quals
        )
        # A bare name is a tenancytest call only under a dot import of
        # tenancytest, and only when it is not a method/field selector.
        bare = dot_tenancytest and not (
            prev is not None and (prev.kind == "ident"
                                  or (prev.kind == "sym" and prev.text == "."))
        )
        if not (qualified or bare):
            i += 1
            continue
        # Optional explicit instantiation '[' ... ']' before the call '('.
        j = i + 1
        instantiation: set[str] = set()
        if j < n and tokens[j].kind == "sym" and tokens[j].text == "[":
            k = j + 1
            depth = 1
            while k < n and depth:
                tk = tokens[k]
                if tk.kind == "ident":
                    instantiation.add(tk.text)
                elif tk.kind == "sym":
                    if tk.text == "[":
                        depth += 1
                    elif tk.text == "]":
                        depth -= 1
                k += 1
            if depth:
                i += 1  # unbalanced; treat as not a call
                continue
            j = k
        if not (j < n and tokens[j].kind == "sym"
                and tokens[j].text == "("):
            i += 1  # a mention, not a call
            continue
        # Capture the call region up to its matching ')' and record every
        # identifier inside (type or model mentions) plus the repository
        # argument's text for diagnostics.
        region: list[Token] = []
        idents: set[str] = set(instantiation)
        comma_at: list[int] = []  # region offsets of depth-0 commas
        depth = 1
        k = j + 1
        while k < n and depth:
            tk = tokens[k]
            if tk.kind == "ident":
                idents.add(tk.text)
            elif tk.kind == "sym":
                if tk.text == "(":
                    depth += 1
                elif tk.text == ")":
                    depth -= 1
                    if depth == 0:
                        break
                elif tk.text == "," and depth == 1:
                    comma_at.append(len(region))
            region.append(tk)
            k += 1
        arg1_text = ""
        if len(comma_at) >= 2:
            seg = region[comma_at[0] + 1:comma_at[1]]
            arg1_text = "".join(
                tok.text for tok in seg if tok.kind != "newline"
            ).strip()
        elif len(comma_at) == 1:
            seg = region[comma_at[0] + 1:]
            arg1_text = "".join(
                tok.text for tok in seg if tok.kind != "newline"
            ).strip()
        calls.append(CallSite(
            name=name,
            file_path=file_path,
            line=t.line,
            idents=idents,
            arg1_text=arg1_text,
        ))
        i = k if k < n else n
    return calls


def package_key_of_test_file(test_rel: str) -> str:
    """Directory whose package a _test.go file tests.

    Files under an integration_test/ subtree test the package that subtree
    lives in (the physically separated integration tier per root
    CLAUDE.md's testing rule); every other test file tests its own
    directory's package.
    """
    parts = test_rel.split("/")
    if TEST_DIR_NAME in parts:
        return "/".join(parts[:parts.index(TEST_DIR_NAME)]) or "."
    return "/".join(parts[:-1]) or "."


def module_roots(root: str) -> list[str]:
    """Repo-relative paths of the Go module roots under root.

    A module root is a directory containing go.mod; its subtree is not
    descended into further, which keeps nested modules (none exist in this
    repo) out of a parent module's scan.
    """
    found: list[str] = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = sorted(d for d in dirnames if d not in PRUNED_DIR_NAMES)
        if "go.mod" in filenames:
            found.append(os.path.relpath(dirpath, root))
            dirnames[:] = []
    return sorted(found)


def scan_module(root: str, module_rel: str) -> tuple[list[Candidate],
                                                     list[CallSite],
                                                     list[str]]:
    """Scan one module. Returns (candidates, calls, notes)."""
    module_root = os.path.join(root, module_rel)
    parsed: list[tuple[str, list[Token], str, dict[str, str], bool]] = []
    notes: list[str] = []
    for dirpath, dirnames, filenames in os.walk(module_root):
        dirnames[:] = sorted(d for d in dirnames if d not in PRUNED_DIR_NAMES)
        for filename in sorted(filenames):
            if not filename.endswith(".go"):
                continue
            full = os.path.join(dirpath, filename)
            rel = os.path.relpath(full, root)
            try:
                with open(full, "rb") as fh:
                    data = fh.read()
                file_text = data.decode("utf-8")
            except (OSError, UnicodeDecodeError) as exc:
                notes.append(f"{rel}: cannot read (skipped): {exc}")
                continue
            tokens = tokenize(file_text)
            quals = parse_imports(tokens, file_text)
            dot_tt = any(q == "." and p == TENANCYTEST_PKG_PATH
                         for q, p in quals.items())
            parsed.append((rel, tokens, file_text, quals, dot_tt))
    candidates: list[Candidate] = []
    calls: list[CallSite] = []
    for rel, tokens, file_text, quals, dot_tt in parsed:
        if rel.endswith(TEST_FILE_SUFFIX):
            # Embedding shapes inside _test.go files are notes, not
            # candidates: test doubles and Example code are fixtures.
            for cand in find_candidates(rel, tokens, file_text, quals):
                notes.append(
                    f"{rel}:{cand.decl_line}: type {cand.type_name} embeds "
                    f"{cand.embed_text} in a _test.go file -- test doubles "
                    "and Example code are not shipped repository "
                    "implementations, so this embedding is not a candidate "
                    "(not checked)"
                )
            calls.extend(find_calls(tokens, quals, rel, dot_tt))
        else:
            candidates.extend(find_candidates(rel, tokens, file_text, quals))
    return candidates, calls, notes


def pair_candidates_with_calls(
    candidates: list[Candidate], calls: list[CallSite]
) -> list[tuple[Candidate, CallSite | None]]:
    """Pair each candidate with the call that covers it (None = uncovered).

    See the module docstring for the attribution rules: single-candidate
    packages are covered by any AssertIsolated call; multi-candidate
    packages need a call mentioning the type or one of its embedded model
    identifiers.
    """
    by_pkg: dict[str, list[Candidate]] = {}
    for cand in candidates:
        by_pkg.setdefault(os.path.dirname(cand.file_path), []).append(cand)
    calls_by_pkg: dict[str, list[CallSite]] = {}
    for call in calls:
        if call.name == ASSERT_ISOLATED:
            calls_by_pkg.setdefault(package_key_of_test_file(call.file_path),
                                    []).append(call)
    paired: list[tuple[Candidate, CallSite | None]] = []
    for pkg, pkg_candidates in by_pkg.items():
        pkg_calls = calls_by_pkg.get(pkg, [])
        for cand in pkg_candidates:
            cover: CallSite | None = None
            if len(pkg_candidates) == 1 and pkg_calls:
                cover = pkg_calls[0]
            else:
                want = {cand.type_name} | cand.models
                for call in pkg_calls:
                    if want & call.idents:
                        cover = call
                        break
            paired.append((cand, cover))
    return paired


def run(root: str) -> int:
    roots = module_roots(root)
    if not roots:
        print(f"no Go modules (directories with go.mod) found under "
              f"{os.path.abspath(root)}; nothing to check")
        return 0
    all_candidates: list[Candidate] = []
    all_calls: list[CallSite] = []
    notes: list[str] = []
    for module_rel in roots:
        cands, calls, mod_notes = scan_module(root, module_rel)
        all_candidates.extend(cands)
        all_calls.extend(calls)
        notes.extend(mod_notes)
    notes.sort()
    paired = pair_candidates_with_calls(all_candidates, all_calls)
    uncovered = [cand for cand, cover in paired if cover is None]
    n_assert_not = sum(1 for c in all_calls
                       if c.name == ASSERT_NOT_TENANT_SCOPED)
    n_assert_iso = sum(1 for c in all_calls if c.name == ASSERT_ISOLATED)

    for cand, cover in sorted(paired, key=lambda p: p[0].file_path):
        if cover is not None:
            print(f"{cand.file_path}:{cand.decl_line}: repository type "
                  f"{cand.type_name} (embeds {cand.embed_text}) -- covered "
                  f"by tenancytest.{ASSERT_ISOLATED} at {cover.file_path}:"
                  f"{cover.line}")
        else:
            print(f"{cand.file_path}:{cand.decl_line}: repository type "
                  f"{cand.type_name} (embeds {cand.embed_text}) is NOT "
                  "covered: no tenancytest.AssertIsolated call in its "
                  "package's tests (root CLAUDE.md multi-tenant isolation "
                  "rule: every tenant-data repository must run the "
                  "tenancytest suite; see tenancytest's doc comment)")
            pkg_calls = [c for c in all_calls
                         if c.name == ASSERT_ISOLATED
                         and package_key_of_test_file(c.file_path)
                         == os.path.dirname(cand.file_path)]
            if pkg_calls:
                print(f"  ({len(pkg_calls)} AssertIsolated call(s) exist in "
                      "the package's tests but none of them names "
                      f"{cand.type_name} or model "
                      f"{sorted(cand.models) or '?'} -- if the repository "
                      "is genuinely covered, name the type or its model in "
                      "the call so this checker can match it)")
    for note in notes:
        print(f"note: {note}")
    if n_assert_not:
        sample = next(c for c in all_calls
                      if c.name == ASSERT_NOT_TENANT_SCOPED)
        print(f"note: {n_assert_not} tenancytest."
              f"{ASSERT_NOT_TENANT_SCOPED} call(s) observed (e.g. "
              f"{sample.file_path}:{sample.line}) -- the identity-data / "
              "platform-data half of the tenancytest pair is reported but "
              "not required: those models never embed dbkit.Repository[T], "
              "so they cannot be enumerated statically")
    n_candidates = len(all_candidates)
    if uncovered:
        print(f"FAILED: {len(uncovered)} of {n_candidates} tenant-scoped "
              "Repository type(s) (types embedding dbkit.Repository[T]) "
              f"across {len(roots)} Go module(s) lack a "
              f"tenancytest.{ASSERT_ISOLATED} call in their package's tests; "
              "every tenant-scoped repository must run the tenancytest "
              "isolation suite (root CLAUDE.md, tenancytest, "
              "docs/internal/04-data-and-tenancy.md)")
        return 1
    print(f"OK: all {n_candidates} tenant-scoped Repository type(s) across "
          f"{len(roots)} Go module(s) are covered by "
          f"tenancytest.{ASSERT_ISOLATED} ({n_assert_iso} assertion "
          "call(s) found)")
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Check that every Repository type in the repository -- a type "
            "embedding dbkit.Repository[T] -- is covered by the mandatory "
            "tenancytest.AssertIsolated assertion in its package's tests, "
            "per the docs/internal/18-cicd.md discipline row for repository "
            "isolation tests. Repository types declared in _test.go files "
            "(test doubles, Example code) are not candidates; "
            "identity/platform data cannot be enumerated statically and is "
            "reported as notes only."
        )
    )
    parser.add_argument(
        "--root",
        default=".",
        help="repository root to scan (default: current directory); report "
        "paths are printed relative to it",
    )
    args = parser.parse_args(argv)
    root = os.path.abspath(args.root)
    if not os.path.isdir(root):
        print(f"error: --root is not a directory: {args.root}",
              file=sys.stderr)
        return 2
    return run(root)


if __name__ == "__main__":
    sys.exit(main())
