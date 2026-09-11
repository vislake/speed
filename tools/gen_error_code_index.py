#!/usr/bin/env python3
"""Error-code index generator.

The index gives every error code its own documented entry, with the
triggering condition and what to do about it. go/pkgcore/apperr/apperr.go
holds only the encoding mechanism (the status-mapped builders); the
catalog of the codes built with it lives in the generated index.

What it does: walks every *.go file under --roots (default: go/ and
examples/, this repository's two source trees; excludes _test.go files
and generated *.gen.go / *_gen.go files, which carry no hand-written error
constructions of their own), and indexes every construction of an
*apperr.Error whose code argument is a string literal, in three shapes --

    ErrFoo = apperr.Invalid("module.some_code")        (named declaration)
    ErrFoo = &apperr.Error{Code: "module.some_code", ...}   (struct literal)
    ErrFoo = rateLimited("module.some_code")   (named declaration routed
                            through a module-local helper that builds the
                            error from its own string parameter -- the
                            HTTP-429-stamping rateLimited convention)

-- plus the same construction shapes used INLINE, without any
package-level declaration: a return, panic or assignment such as

    return nil, apperr.Internal("module.some_code")
    panic(apperr.Invalid("module.some_code"))

A code is indexed iff it is constructed in Go source with a literal code
argument -- named declaration or inline, builder call or struct literal;
the construction form is irrelevant, and the absence of a declaration
never hides a code (inline constructions are indexed too, not only
named declarations). The boundary this coverage claim lives inside is
enforced rather than assumed: a code built from a non-literal argument
(a variable, constant or concatenation) cannot be indexed by this
line-based tool -- a literal argument is a hard requirement -- and a
literal sitting on a different line than its builder call cannot either;
tools/check_error_code_index_coverage.py, the independent side this
output is measured against, turns red on both classes if they ever
land.

The struct-literal and helper shapes exist because none of apperr's six
builder statuses (400/401/403/404/409/500) is 429: a module stamps HTTP
429 either as the &apperr.Error literal or through a small helper like
rateLimited. A struct literal's status is read from the literal itself:
its Status field must be an integer literal or an http.Status* constant
(the closed net/http vocabulary in _HTTP_STATUS_CONSTANTS), and a Status
this tool cannot read -- a local constant, a computed expression, a
literal that does not close within its line bound -- aborts the run
naming the site, because a guessed status is the one output worse than
no output (sharing.resource_unavailable carries 502, a status outside
the builders' vocabulary, and an assumed 429 would contradict the code's
own documentation). A literal carrying no Status field at all keeps the
shape's documented 429 default -- the convention's purpose, since this
shape is how a module stamps the one status the builders do not
provide -- and the helper shape (rateLimited, which stamps 429 in its
own body) keeps 429 across its literal call sites.

For each construction the tool captures the Go identifier (when the
construction binds one), the apperr code string, the resulting HTTP
status, and the file:line. "Triggering condition" is harvested as the
code's own doc: for a named declaration, the contiguous "// ..." doc
comment immediately above the declaration (the same convention godoc
itself reads); for an inline construction, the comment directly above the
construction line, else the doc comment of the function whose body
contains it (nearest preceding named-function declaration -- for a panic
inside jobs' With* option functions, that doc states the exact rule the
code refuses). It also separately builds a code -> English message dict
from every locales/en-US.toml this repository ships (go/pkgcore/i18n's
own message-catalog convention: the TOML key IS the apperr code), so an
entryless code is reported as such rather than silently omitted. The
entryless class spans two shapes: a boot-time wiring refusal that never
reaches an end user, and a request-time refusal that reaches one only as
its structured code -- the catalog carries no copy for either, and any
end-user text for the second shape comes from the client's own fallback,
never from this table.

The result is one Markdown table (code / message / triggering condition /
module / HTTP status / source), grouped by module (the code's own
dot-prefix, e.g. "notification" from "notification.type_not_found" --
not the Go package's directory, since a handful of codes are declared in
one package but logically belong to another's vocabulary), sorted by
module then code, written to --out. The default --out is the
documentation site's own user-guide page
(docs/site/content.en/docs/user-guide/error-codes.md, served at
/docs/user-guide/error-codes/): the writer prepends a fixed Hugo front
matter block, and the page's prose explains the index for the reader --
error handling is a consumer-facing reference, so the human-readable
index ships on the site while the machine-readable twin stays in the
repository. One
row per code: when the same code is constructed more than once (a rare
deliberate re-export, one code built at several inline sites --
go/jobs/queue_standalone.go and go/jobs/queue/asynq/queue.go both refuse
with jobs.worker_count_zero -- or a code that has both a declaration and
inline uses), the duplicate sites are collapsed and a named
declaration's row wins over an inline site's.

Usage:
    python3 tools/gen_error_code_index.py [--roots go examples] [--out docs/site/content.en/docs/user-guide/error-codes.md] [--check]

--check exits nonzero (printing a diff) instead of writing, for the CI
wiring in docs-check.yml. A struct literal whose Status cannot be read
aborts either mode with exit 2, naming the construction site, the value
it could not resolve and the forms it accepts (an integer literal or an
http.Status* constant). The generator's own drift gate can only compare
the committed file against its own output, so it can never see a
construction form the tool itself does not index -- that blind spot is
closed by tools/check_error_code_index_coverage.py, which derives the
expected code set independently from the real tree and compares it
against this file (docs-check.yml runs it next to --check).
"""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys
import tomllib
from dataclasses import dataclass

# Matches "IDENT = apperr.Builder("code")", optionally preceded by a
# standalone "var " keyword (a single declaration outside a var (...)
# block, e.g. examples/reference-app/internal/notes/handler.go's "var
# ErrTextRequired = apperr.Invalid(...)") -- the six status-mapped builders
# in go/pkgcore/apperr/apperr.go. A chained .WithParam(...)/.WithCause(...)
# on a continuation line does not affect this match: the code and builder
# are both already captured on the opening line. IDENT may be unexported
# (a package-private sentinel like notes' own errInternal) as well as
# exported.
#
# All three declaration patterns are anchored at column zero -- no leading
# whitespace -- because a top-level declaration is the only valid Go shape
# a line can have at column zero, while INDENTED assignments live in
# function bodies and var (...)-block entries alike. _scan_go_file decides
# between those two: an indented match counts only when the line sits
# inside a var block (it then probes the line with the leading whitespace
# stripped, so the column-zero patterns still apply). A plain
# reassignment inside a function -- "err = apperr.Invalid("module.code")"
# -- is never mistaken for a declaration; the inline scan indexes it on
# its own terms instead, since the code it builds is real.
_BUILDER_RE = re.compile(
    r'^(?:var\s+)?(?P<ident>[A-Za-z_]\w*)\s*=\s*apperr\.'
    r'(?P<builder>NotFound|Invalid|Conflict|Unauthorized|Forbidden|Internal)'
    r'\(\s*"(?P<code>[^"]+)"\s*\)'
)

# Matches the struct-literal shape: "IDENT = &apperr.Error{Code:
# "code", Status: http.StatusTooManyRequests}" (field order and whitespace
# tolerant, optional leading "var ", same as _BUILDER_RE above). This
# pattern anchors the declaration and captures the code; the literal's
# Status field is read from the literal body the match opens, by
# _struct_literal_status.
_STRUCT_RE = re.compile(
    r'^(?:var\s+)?(?P<ident>[A-Za-z_]\w*)\s*=\s*&apperr\.Error\{'
    r'\s*Code:\s*"(?P<code>[^"]+)"'
)

# Matches a named declaration routed through a module-local helper that
# builds an *apperr.Error from its own string parameter:
# "IDENT = rateLimited("code")" (optional leading "var ", inside or
# outside a var (...) block). Only a callee in the helper set discovered
# by discover_apperr_helpers counts -- a function declared in the
# scanned tree that constructs an error from one of its own parameters.
# Without that body evidence a bare "IDENT = fn("literal")" is some
# unrelated string-taking constructor (regexp.MustCompile("..."),
# net.ParseCIDR("..."), ...) and must never produce a phantom row.
_HELPER_DECL_RE = re.compile(
    r'^(?:var\s+)?(?P<ident>[A-Za-z_]\w*)\s*=\s*'
    r'(?P<callee>[A-Za-z_]\w*)\s*\(\s*"(?P<code>[^"]+)"'
)

# The unanchored inline twins: any occurrence, at any indentation, of an
# apperr builder call or an &apperr.Error literal with a string-literal
# first argument -- a return/panic/assignment construction of a code with
# no package-level declaration. The literal must sit on the same line as
# the builder name; a code literal on its own continuation line is the one
# boundary this line-based tool records rather than indexes (none exists
# in the tree; check_error_code_index_coverage.py's multi-line net turns
# red if one ever lands).
_INLINE_BUILDER_RE = re.compile(
    r'apperr\.(?P<builder>NotFound|Invalid|Conflict|Unauthorized|Forbidden|Internal)'
    r'\(\s*"(?P<code>[^"]+)"'
)
_INLINE_STRUCT_RE = re.compile(
    r'&apperr\.Error\{\s*Code:\s*"(?P<code>[^"]+)"'
)
# Inline helper calls: any bare-identifier call with a string-literal
# first argument; the callee must be in the helper set to count.
_INLINE_CALL_RE = re.compile(
    r'(?P<callee>[A-Za-z_]\w*)\s*\(\s*"(?P<code>[^"]+)"'
)

# A named function declaration line: "func Name(params" or "func (recv)
# Name(params", at column zero. Named functions cannot nest in Go, so a
# column-zero "func " line is always a top-level declaration; closures
# ("func(w, r) {" as an argument, say) are indented or mid-line and never
# start a line.
_FUNC_DECL_RE = re.compile(r'^func\s+(?:\([^)]*\)\s*)?(?P<name>[A-Za-z_]\w*)\s*\(')

# An apperr construction whose first argument is a bare identifier -- the
# body evidence that marks a function an apperr-constructing helper: only
# a function that builds an *apperr.Error from one of its own parameters
# (rateLimited's "func rateLimited(code string)" convention) can have its
# literal call sites' arguments indexed as codes. A function whose body
# constructs codes from literals only (a With* option function, an Open)
# is not a helper -- nothing about its arguments is a code.
_HELPER_EVIDENCE_RE = re.compile(
    r'apperr\.(?:NotFound|Invalid|Conflict|Unauthorized|Forbidden|Internal)'
    r'\s*\(\s*(?P<arg>[A-Za-z_]\w*)\s*[,)]'
    r'|&apperr\.Error\s*\{\s*Code:\s*(?P<struct_arg>[A-Za-z_]\w*)\s*[,}]'
)

_BUILDER_STATUS = {
    "NotFound": 404,
    "Invalid": 400,
    "Conflict": 409,
    "Unauthorized": 401,
    "Forbidden": 403,
    "Internal": 500,
}

# The status the helper shape carries: rateLimited stamps HTTP 429 in its
# own body (the convention), and every literal call site of a discovered
# helper inherits it. HTTP 429 is the status none of the six builders
# provides, which is why the helper exists at all.
_HELPER_STATUS = 429

# The status a struct literal with no Status field at all keeps: 429, the
# shape's documented default (the struct-literal shape is how a module
# stamps the one status the builders do not provide). A literal that does
# carry a Status field renders it -- the field is read, never assumed.
_DEFAULT_STRUCT_STATUS = 429

# net/http's status constants: the closed vocabulary a struct literal's
# Status field may name (the full set from net/http/status.go, not only
# the constants in use, so a literal naming any standard status resolves
# without a tool change). A Status value outside this table and the
# integer-literal form is refused by _struct_literal_status, never
# guessed.
_HTTP_STATUS_CONSTANTS = {
    "StatusContinue": 100,
    "StatusSwitchingProtocols": 101,
    "StatusProcessing": 102,
    "StatusEarlyHints": 103,
    "StatusOK": 200,
    "StatusCreated": 201,
    "StatusAccepted": 202,
    "StatusNonAuthoritativeInfo": 203,
    "StatusNoContent": 204,
    "StatusResetContent": 205,
    "StatusPartialContent": 206,
    "StatusMultiStatus": 207,
    "StatusAlreadyReported": 208,
    "StatusIMUsed": 226,
    "StatusMultipleChoices": 300,
    "StatusMovedPermanently": 301,
    "StatusFound": 302,
    "StatusSeeOther": 303,
    "StatusNotModified": 304,
    "StatusUseProxy": 305,
    "StatusTemporaryRedirect": 307,
    "StatusPermanentRedirect": 308,
    "StatusBadRequest": 400,
    "StatusUnauthorized": 401,
    "StatusPaymentRequired": 402,
    "StatusForbidden": 403,
    "StatusNotFound": 404,
    "StatusMethodNotAllowed": 405,
    "StatusNotAcceptable": 406,
    "StatusProxyAuthRequired": 407,
    "StatusRequestTimeout": 408,
    "StatusConflict": 409,
    "StatusGone": 410,
    "StatusLengthRequired": 411,
    "StatusPreconditionFailed": 412,
    "StatusRequestEntityTooLarge": 413,
    "StatusRequestURITooLong": 414,
    "StatusUnsupportedMediaType": 415,
    "StatusRequestedRangeNotSatisfiable": 416,
    "StatusExpectationFailed": 417,
    "StatusTeapot": 418,
    "StatusMisdirectedRequest": 421,
    "StatusUnprocessableEntity": 422,
    "StatusLocked": 423,
    "StatusFailedDependency": 424,
    "StatusTooEarly": 425,
    "StatusUpgradeRequired": 426,
    "StatusPreconditionRequired": 428,
    "StatusTooManyRequests": 429,
    "StatusRequestHeaderFieldsTooLarge": 431,
    "StatusUnavailableForLegalReasons": 451,
    "StatusInternalServerError": 500,
    "StatusNotImplemented": 501,
    "StatusBadGateway": 502,
    "StatusServiceUnavailable": 503,
    "StatusGatewayTimeout": 504,
    "StatusHTTPVersionNotSupported": 505,
    "StatusVariantAlsoNegotiates": 506,
    "StatusInsufficientStorage": 507,
    "StatusLoopDetected": 508,
    "StatusNotExtended": 510,
    "StatusNetworkAuthenticationRequired": 511,
}

# The Status field inside a struct literal's body: the value runs to the
# next comma, closing brace or line end, so a trailing comment or a
# following field is never swallowed.
_STRUCT_STATUS_FIELD_RE = re.compile(r'\bStatus:\s*(?P<value>[^,}\n]+)')

# How many lines a struct literal's body may span before its Status can no
# longer be read (see _struct_literal_body): generous against gofmt's
# wrapping, bounded so a malformed tree cannot walk to EOF.
_STRUCT_LITERAL_MAX_LINES = 20

_EXCLUDED_SUFFIXES = ("_test.go",)
_EXCLUDED_GLOBS = ("*.gen.go", "*_gen.go")


@dataclass
class ErrorEntry:
    ident: str
    code: str
    status: int
    source: str
    doc: str = ""
    message: str = ""
    # "declared" = the code is bound to a package-level named variable
    # (directly, as a struct literal, or through an apperr-constructing
    # helper); "inline" = the code is constructed without any declaration.
    # A declaration's row wins over an inline site's row for the same
    # code, because the declaration carries the code's own doc comment.
    kind: str = "declared"

    @property
    def module(self) -> str:
        return self.code.split(".", 1)[0] if "." in self.code else self.code


def _is_excluded(path: pathlib.Path) -> bool:
    if any(path.name.endswith(suf) for suf in _EXCLUDED_SUFFIXES):
        return True
    return any(path.match(pat) for pat in _EXCLUDED_GLOBS)


def _leading_comment(lines: list[str], decl_index: int) -> str:
    """Collects the contiguous "// ..." block directly above lines[decl_index],
    stopping at the first non-comment (or blank) line -- the same rule
    godoc itself uses to associate a doc comment with the declaration right
    below it. Returns the joined text with the "// " prefix stripped, or ""
    if the declaration has no immediately preceding comment.
    """
    collected: list[str] = []
    i = decl_index - 1
    while i >= 0:
        stripped = lines[i].strip()
        if stripped.startswith("//"):
            collected.append(stripped[2:].strip())
            i -= 1
            continue
        break
    collected.reverse()
    return " ".join(collected)


def _code_part(line: str) -> str:
    """Returns the part of line before its trailing "// ..." comment, if any.
    Codes are dotted module keys and never contain "//", and the code
    argument always precedes any chained .WithParam(...) value that could
    contain one, so cutting at the first "//" can never amputate a code
    literal while it does keep a trailing comment's incidental text out of
    the inline patterns."""
    if "//" in line:
        return line.split("//", 1)[0]
    return line


def _is_code_line(line: str) -> bool:
    stripped = line.strip()
    return bool(stripped) and not stripped.startswith("//")


def _enclosing_func_doc(lines: list[str], index: int) -> str:
    """Returns the doc comment of the nearest named function declaration
    above lines[index] -- the function whose body contains the line -- or
    "" when no function precedes it (the construction then sits at package
    scope). Named functions cannot nest in Go and doc comment lines start
    with "//", so the nearest preceding line whose stripped form starts
    with "func " is the containing function's declaration, and its doc is
    the contiguous comment block directly above it."""
    for j in range(index - 1, -1, -1):
        stripped = lines[j].strip()
        if stripped.startswith("func "):
            return _leading_comment(lines, j)
    return ""


def _param_identifiers(lines: list[str], decl_line: int) -> set[str]:
    """Returns the identifiers appearing in a function's parameter list:
    the candidates for "this function builds an error from one of its own
    parameters". The signature is accumulated from the declaration line
    until the line that opens the body (gofmt may wrap a long parameter
    list); the parameter group is the paren group whose closing paren
    precedes the body brace, matched backward so a method receiver's own
    paren group is excluded from the scan start (its identifiers may join
    the set -- harmless over-approximation, since the evidence test
    additionally requires that identifier to be the first argument of an
    apperr construction)."""
    sig_parts: list[str] = []
    for j in range(decl_line, len(lines)):
        line = lines[j]
        sig_parts.append(line)
        if "{" in line:
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


def _discover_helpers_in_file(lines: list[str]) -> set[str]:
    """Returns the names of every function declared in this file that
    constructs an *apperr.Error from one of its own parameters -- the
    helper functions through which a module routes a code it cannot
    express as a direct builder or struct-literal declaration (today: the
    rateLimited convention stamping HTTP 429, in go/org/errors.go and
    go/notification/errors.go). A construction with a literal or
    concatenated argument is not evidence: only a construction whose first
    argument is a bare identifier named in the function's own parameter
    list makes the function's literal call sites' arguments codes. Named
    functions cannot nest, so the functions of one file appear in
    declaration order and the body of the function declared at line i is
    everything below it down to the next function declaration (or EOF);
    gofmt indents every body line, so nothing can be misattributed across
    that boundary. Comment lines are excluded from the evidence test -- a
    doc comment quoting an apperr construction is not a construction."""
    decls: list[tuple[str, int]] = []
    for i, line in enumerate(lines):
        if not _is_code_line(line):
            continue
        stripped = line.strip()
        if not stripped.startswith("func "):
            continue
        m = _FUNC_DECL_RE.match(stripped)
        if m:
            decls.append((m.group("name"), i))
    helpers: set[str] = set()
    for k, (name, decl_line) in enumerate(decls):
        params = _param_identifiers(lines, decl_line)
        if not params:
            continue
        body_end = decls[k + 1][1] if k + 1 < len(decls) else len(lines)
        body = lines[decl_line + 1 : body_end]
        for line in body:
            if not _is_code_line(line):
                continue
            for evidence in _HELPER_EVIDENCE_RE.finditer(line):
                arg = evidence.group("arg") or evidence.group("struct_arg")
                if arg in params:
                    helpers.add(name)
                    break
            if name in helpers:
                break
    return helpers


def discover_apperr_helpers(roots: list[pathlib.Path], repo_root: pathlib.Path) -> set[str]:
    """Returns the names of every function declared in the scanned tree
    that constructs an *apperr.Error from one of its own parameters --
    the helper functions through which a module routes a code it cannot
    express as a direct builder or struct-literal declaration (today: the
    rateLimited convention stamping HTTP 429, in go/org/errors.go and
    go/notification/errors.go). The parameter-derived body evidence is
    what keeps the helper shapes honest: only a callee discovered here
    lets the scanner accept "IDENT = rateLimited("code")" or an inline
    rateLimited("code"), while unrelated string-taking constructors
    (regexp.MustCompile("..."), net.ParseCIDR("..."), ...) -- and any
    function that merely builds literal codes of its own -- never do.
    """
    helpers: set[str] = set()
    for root in roots:
        for path in sorted(root.rglob("*.go")):
            if _is_excluded(path):
                continue
            text = path.read_text(encoding="utf-8", errors="replace")
            helpers |= _discover_helpers_in_file(text.splitlines())
    return helpers


class UnparseableStructLiteral(Exception):
    """Raised when an &apperr.Error struct literal's Status field cannot be
    read: the literal does not close within _STRUCT_LITERAL_MAX_LINES
    lines, or its Status names something this tool cannot resolve. main
    prints the message (which names the construction site) and exits 2 --
    a refused run, never a rendered guess, because the index's whole claim
    is that each status is what the source says."""


def _struct_literal_body(lines: list[str], line_index: int, open_col: int) -> str | None:
    """Returns the text between a struct literal's opening brace (at
    lines[line_index][open_col]) and its matching closing brace, or None
    when the brace does not close within _STRUCT_LITERAL_MAX_LINES lines.

    Braces inside double-quoted values are data, not structure, and a
    "//" sequence outside such a value starts a comment skipped to the
    end of its line, so neither can end the scan early. The line bound
    turns the one shape this scanner cannot read (a literal longer than
    the bound) into a report by the caller instead of a silent default."""
    depth = 0
    body: list[str] = []
    in_string = False
    last = min(line_index + _STRUCT_LITERAL_MAX_LINES, len(lines))
    for i in range(line_index, last):
        line = lines[i]
        j = open_col if i == line_index else 0
        while j < len(line):
            ch = line[j]
            if in_string:
                body.append(ch)
                if ch == "\\" and j + 1 < len(line):
                    body.append(line[j + 1])
                    j += 2
                    continue
                if ch == '"':
                    in_string = False
                j += 1
                continue
            if ch == '"':
                in_string = True
                body.append(ch)
                j += 1
                continue
            if line.startswith("//", j):
                break
            if ch == "{":
                depth += 1
                if depth > 1:
                    body.append(ch)
            elif ch == "}":
                depth -= 1
                if depth == 0:
                    return "".join(body)
                body.append(ch)
            else:
                body.append(ch)
            j += 1
    return None


def _struct_literal_status(lines: list[str], line_index: int, open_col: int, site: str) -> int:
    """The HTTP status of the &apperr.Error struct literal whose opening
    brace sits at lines[line_index][open_col]: the literal's own Status
    field -- an integer literal or an http.Status* constant -- or the
    documented _DEFAULT_STRUCT_STATUS when the literal carries no Status
    field at all. Any other Status value is refused with
    UnparseableStructLiteral: reading the status from the literal is the
    point; a guessed status is a rendered falsehood."""
    body = _struct_literal_body(lines, line_index, open_col)
    if body is None:
        raise UnparseableStructLiteral(
            f"{site}: the struct literal does not close within "
            f"{_STRUCT_LITERAL_MAX_LINES} lines, so its Status cannot be read"
        )
    field = _STRUCT_STATUS_FIELD_RE.search(body)
    if field is None:
        return _DEFAULT_STRUCT_STATUS
    value = field.group("value").strip()
    if value.isdigit():
        return int(value)
    # The source names net/http's constant qualified ("http.StatusBadGateway");
    # only the standard package's own spelling resolves -- a bare or
    # otherwise-qualified identifier is not this table's vocabulary.
    if value.startswith("http.") and value[len("http."):] in _HTTP_STATUS_CONSTANTS:
        return _HTTP_STATUS_CONSTANTS[value[len("http."):]]
    raise UnparseableStructLiteral(
        f"{site}: cannot read the struct literal's Status value {value!r}; "
        f"use an integer literal or an http.Status* constant"
    )


def _declaration_entry(match, kind: str, lines: list[str], line_index: int, rel: str) -> ErrorEntry:
    """Builds the entry for a package-level declaration matched by one of
    the three anchored declaration patterns."""
    site = f"{rel}:{line_index + 1}"
    if kind == "builder":
        status = _BUILDER_STATUS[match.group("builder")]
    elif kind == "struct":
        # The literal's status is read from the literal, not assumed. The
        # anchored pattern matches the line's first construction, so the
        # line's first &apperr.Error is the matched one, and its opening
        # brace is the "{" right after it.
        line = lines[line_index]
        open_col = line.index("{", line.index("&apperr.Error"))
        status = _struct_literal_status(lines, line_index, open_col, site)
    else:  # helper
        status = _HELPER_STATUS
    return ErrorEntry(
        ident=match.group("ident"),
        code=match.group("code"),
        status=status,
        source=site,
        doc=_leading_comment(lines, line_index),
        kind="declared",
    )


def _match_declaration_line(probe: str, helpers: set[str]) -> tuple[re.Match, str] | None:
    """Return (match, kind) for one (possibly probed) declaration line.

    kind is "builder", "struct" or "helper", the pattern that fired -- the
    three patterns differ in how the HTTP status is derived, so the caller
    must know which one matched. The helper pattern fires only when its
    callee is a discovered apperr-constructing helper.
    """
    m = _BUILDER_RE.match(probe)
    if m:
        return m, "builder"
    m = _STRUCT_RE.match(probe)
    if m:
        return m, "struct"
    m = _HELPER_DECL_RE.match(probe)
    if m and m.group("callee") in helpers:
        return m, "helper"
    return None


def _inline_entries(
    lines: list[str], line: str, helpers: set[str], rel: str, line_index: int
) -> list[ErrorEntry]:
    """Indexes the inline constructions on one code line: apperr builder
    calls, &apperr.Error literals, and apperr-constructing helper calls,
    each with a string-literal first argument. The same line can carry
    several (finditer over each shape). No doc is harvested here -- the
    caller supplies it, since the harvest looks up the lines array above
    the construction. `line` may be the line's code part (a trailing
    comment already cut), which is a prefix of lines[line_index], so the
    struct-literal scan's offsets still address the real line -- and it
    re-reads the real line, so a literal opening before the cut can close
    on a later line even when a comment trails its first line."""
    site = f"{rel}:{line_index + 1}"
    entries: list[ErrorEntry] = []
    for m in _INLINE_BUILDER_RE.finditer(line):
        entries.append(
            ErrorEntry(
                ident="",
                code=m.group("code"),
                status=_BUILDER_STATUS[m.group("builder")],
                source=site,
                kind="inline",
            )
        )
    for m in _INLINE_STRUCT_RE.finditer(line):
        open_col = m.start() + m.group(0).index("{")
        entries.append(
            ErrorEntry(
                ident="",
                code=m.group("code"),
                status=_struct_literal_status(lines, line_index, open_col, site),
                source=site,
                kind="inline",
            )
        )
    for m in _INLINE_CALL_RE.finditer(line):
        if m.group("callee") in helpers:
            entries.append(
                ErrorEntry(
                    ident="",
                    code=m.group("code"),
                    status=_HELPER_STATUS,
                    source=site,
                    kind="inline",
                )
            )
    return entries


def _scan_go_file(path: pathlib.Path, rel: str, helpers: set[str]) -> list[ErrorEntry]:
    text = path.read_text(encoding="utf-8", errors="replace")
    lines = text.splitlines()
    entries: list[ErrorEntry] = []
    in_var_block = False
    for i, line in enumerate(lines):
        stripped = line.strip()
        if not stripped:
            continue
        if in_var_block:
            if stripped == ")":
                in_var_block = False
                continue
            if stripped.startswith("//"):
                # A comment inside a var (...)-block: doc comment for an
                # entry below, never an entry itself.
                continue
            # A var (...)-block entry is indented; its declaration shape
            # is the column-zero patterns applied to the stripped line.
            # Every var-block line is a declaration, so no inline scan
            # applies here.
            matched = _match_declaration_line(line.lstrip(), helpers)
            if matched is None:
                continue
            m, kind = matched
            entries.append(_declaration_entry(m, kind, lines, i, rel))
            continue
        if stripped.startswith("//"):
            continue
        if stripped.startswith("var ") and stripped.endswith("("):
            in_var_block = True
            continue
        # Outside a var block only a column-zero line can be a
        # top-level declaration (gofmt'd code; function-body statements
        # are indented), which is exactly what the column-zero-anchored
        # patterns require -- an indented plain assignment
        # ("err = apperr.Invalid(...)" in a function) can never match
        # here, so it cannot masquerade as a declaration.
        matched = _match_declaration_line(line, helpers)
        if matched is not None:
            m, kind = matched
            entries.append(_declaration_entry(m, kind, lines, i, rel))
            continue
        # Any other code line may still construct codes inline -- a
        # panic, a return, a short-variable declaration, a call argument.
        # Comment text is already excluded above; a trailing "//"
        # comment is cut off so its incidental text cannot match.
        code_part = _code_part(line)
        for e in _inline_entries(lines, code_part, helpers, rel, i):
            if not e.doc:
                e.doc = _leading_comment(lines, i) or _enclosing_func_doc(lines, i)
            entries.append(e)
    return entries


def collect_entries(
    roots: list[pathlib.Path], repo_root: pathlib.Path, helpers: set[str]
) -> list[ErrorEntry]:
    entries: list[ErrorEntry] = []
    for root in roots:
        for path in sorted(root.rglob("*.go")):
            if _is_excluded(path):
                continue
            rel = str(path.relative_to(repo_root))
            entries.extend(_scan_go_file(path, rel, helpers))
    return entries


def _plural_english(value) -> str:
    """Renders a go-i18n plural-form TOML table (the {one = "...", other =
    "..."} shape tools/check_i18n_keys.py's own header documents) down to
    one representative English string: "other" is the CLDR catch-all form
    and therefore the most representative single string for an index meant
    for skimming, not the plural message's rendering engine."""
    if isinstance(value, dict):
        for key in ("other", "one", "translation"):
            for k, v in value.items():
                if k.lower() == key and isinstance(v, str):
                    return v
        # Fall back to the first string value found, if any.
        for v in value.values():
            if isinstance(v, str):
                return v
        return ""
    return str(value)


def collect_messages(roots: list[pathlib.Path]) -> dict[str, str]:
    """Builds a code -> English message dict from every locales/en-US.toml
    this repository ships. The TOML key IS the apperr code
    (go/pkgcore/i18n's own catalog convention), so this is a direct lookup,
    not a heuristic match."""
    messages: dict[str, str] = {}
    for root in roots:
        for path in sorted(root.rglob("locales/en-US.toml")):
            try:
                with path.open("rb") as f:
                    data = tomllib.load(f)
            except (tomllib.TOMLDecodeError, OSError):
                continue
            for key, value in data.items():
                if key not in messages:
                    messages[key] = _plural_english(value)
    return messages


def code_counts(entries: list[ErrorEntry]) -> tuple[int, int, int]:
    """Return (construction sites, unique codes, duplicates removed).

    A construction site is one indexed construction: a named declaration
    or an inline builder/struct/helper call. The table renders one row per
    code and collapses a code constructed more than once (a rare,
    deliberate re-export, one code built at several inline sites -- the
    two jobs queue implementations refusing with the same code -- or a
    code that has both a declaration and inline uses) into its winning
    row -- so the real numbers are the unique-code count and the number of
    duplicate sites the rendering removed, not the raw site count. A
    code's module is its own dot-prefix, so a code can never span two
    modules and the per-module collapse equals the global one.
    """
    n_sites = len(entries)
    n_codes = len({e.code for e in entries})
    return n_sites, n_codes, n_sites - n_codes


def winning_rows(entries: list[ErrorEntry]) -> list[ErrorEntry]:
    """One row per code, the same collapse render_markdown applies: a
    named declaration's row wins over an inline site's row for the same
    code (the declaration carries the code's own doc comment), and
    among equal kinds the earliest source wins. The machine-readable
    output and the Markdown table must agree row for row, so both are
    derived from this one collapse."""
    by_module: dict[str, list[ErrorEntry]] = {}
    for e in entries:
        by_module.setdefault(e.module, []).append(e)
    winners: list[ErrorEntry] = []
    for module in by_module:
        seen_codes: set[str] = set()
        ranked = sorted(
            by_module[module],
            key=lambda e: (e.code, 0 if e.kind == "declared" else 1, e.source),
        )
        for e in ranked:
            if e.code in seen_codes:
                continue
            seen_codes.add(e.code)
            winners.append(e)
    return winners


def render_machine_json(entries: list[ErrorEntry]) -> str:
    """The machine-readable twin of the Markdown table, one row per
    code with the same collapse (winning_rows). Each row carries the
    code's construction site as file and line separately (the Markdown
    Source column's "file:line" split), the bound identifier when the
    construction binds one, and the kind. This is the artifact the
    reference-app shell's codes-alignment suite reads back to verify
    its hand-maintained server-code enumeration against the extracted
    census (examples/reference-app/web/src/codes-alignment.test.ts)."""
    rows = [
        {
            "code": e.code,
            "module": e.module,
            "status": e.status,
            "file": e.source.rsplit(":", 1)[0],
            "line": int(e.source.rsplit(":", 1)[1]),
            "ident": e.ident,
            "kind": e.kind,
        }
        for e in sorted(winning_rows(entries), key=lambda e: e.code)
    ]
    return json.dumps(
        {
            "generated_by": "tools/gen_error_code_index.py",
            "rows": rows,
        },
        indent=2,
        sort_keys=True,
    ) + "\n"


# Hugo front matter prepended at write time (only when writing the
# default site-page target; the exact bytes are part of the committed
# artifact the --check drift gate compares). weight 99 keeps the page
# last in its section's menu regardless of the content pages around it.
_SITE_PAGE_PREFIX = (
    "---\n"
    'title: "Error code index"\n'
    'description: "Every error code a speed-based API can answer with: HTTP status, locale message, triggering condition and source, grouped by module."\n'
    "weight: 99\n"
    "---\n"
)


def render_markdown(entries: list[ErrorEntry]) -> str:
    lines = [
        "# Error code index",
        "",
        "The structured error code an API answers with is the contract a",
        "client handles -- never a prose message. This page lists every",
        "code a speed-based service can answer with, one row per code: the",
        "HTTP status, the message the code's own locale entry carries (a",
        "client renders its own copy of the text, never this table's), and",
        "the Go source that constructs it. Codes follow the",
        "`module.snake_case` convention and are grouped below by module.",
        "",
        "How the index is built: every code is an `*apperr.Error`",
        "constructed in Go source with a",
        " literal code argument -- `apperr.Invalid`, `apperr.NotFound`, and",
        " so on; the equivalent `&apperr.Error{...}` struct literal; a",
        " declaration routed through a module-local helper that builds the",
        " error from its own string parameter (the HTTP-429-stamping",
        " `rateLimited` convention); or an",
        " inline construction with no package-level declaration at all -- a",
        " `panic(apperr.Invalid(\"module.code\"))` or a",
        " `return nil, apperr.Internal(\"module.code\")` carries its code",
        " into this index exactly as a named declaration does. (The one",
        " boundary: the code argument must be a string literal on the",
        " builder's own line -- a variable-built or line-wrapped code is",
        " refused by `tools/check_error_code_index_coverage.py`, which",
        " measures this index against the tree independently.)",
        " \"Message\" is the code's own `en-US.toml` catalog entry when one",
        " exists. An entryless code is one the catalog carries no copy for:",
        " a boot-time wiring refusal never reaches an end user, while a",
        " request-time refusal with no entry still reaches the client as its",
        " structured code -- any text for it comes from the client's own",
        " fallback, never from this table. \"Triggering condition\" is",
        " the doc comment of the code's declaration when it has one, and",
        " for an inline construction the comment above the construction or",
        " its enclosing function's doc comment, verbatim.",
        "",
    ]
    # One row per code, the same collapse the machine-readable twin
    # derives from (winning_rows): a named declaration's row wins over
    # an inline site's row for the same code (the declaration carries
    # the code's own doc comment), and among equal kinds the earliest
    # source wins. The table and docs/error-codes.json must agree row
    # for row, so both are built from the one collapse.
    by_module: dict[str, list[ErrorEntry]] = {}
    for e in winning_rows(entries):
        by_module.setdefault(e.module, []).append(e)

    for module in sorted(by_module):
        lines.append(f"## {module}")
        lines.append("")
        lines.append("| Code | Status | Message | Triggering condition | Source |")
        lines.append("|---|---|---|---|---|")
        for e in by_module[module]:
            # The entryless marker must not assert a class: the header
            # paragraph above defines the two entryless shapes (a boot-time
            # wiring refusal that never reaches an end user, and a
            # request-time refusal that reaches one only as its structured
            # code, whose text -- if any -- comes from the client's own
            # fallback). A row-level "not user-facing" gloss would assert a
            # single class where the header defines two, contradicting it
            # on the request-time rows, so the cell states only the shared
            # fact.
            message = e.message.replace("|", "\\|").replace("\n", " ") or "_(no locale message -- the catalog carries no copy)_"
            if e.doc:
                doc = e.doc.replace("|", "\\|")
            elif e.kind == "inline":
                doc = "_(inline construction, no doc comment nearby)_"
            else:
                doc = "_(undocumented)_"
            lines.append(f"| `{e.code}` | {e.status} | {message} | {doc} | `{e.source}` |")
        lines.append("")

    n_sites, n_codes, n_dups = code_counts(entries)
    lines.append(
        f"<!-- Generated by tools/gen_error_code_index.py. Do not hand-edit; "
        f"regenerate with python3 tools/gen_error_code_index.py, whose drift "
        f"gate docs-check.yml runs. Index stats: {n_sites} construction "
        f"site(s) -- named declarations and inline apperr calls alike -- "
        f"collapsed to {n_codes} code(s) across {len(by_module)} module(s) "
        f"({n_dups} duplicate site(s) removed). The machine-readable twin "
        f"docs/error-codes.json feeds the reference app's codes-alignment "
        f"suite. -->"
    )
    lines.append("")
    return "\n".join(lines)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--roots", nargs="+", default=["go", "examples"], help="Directories to scan for *.go files (relative to the repo root).")
    parser.add_argument("--out", default="docs/site/content.en/docs/user-guide/error-codes.md", help="Output Markdown file (relative to the repo root); the default is the documentation site's user-guide page, written with a fixed Hugo front matter block prepended.")
    parser.add_argument("--machine-out", default="docs/error-codes.json", help="Output machine-readable JSON file, the row-for-row twin of the Markdown table (relative to the repo root).")
    parser.add_argument("--check", action="store_true", help="Exit nonzero if the outputs are not already up to date, instead of writing them.")
    args = parser.parse_args()

    repo_root = pathlib.Path(__file__).resolve().parent.parent
    roots = [repo_root / r for r in args.roots]

    helpers = discover_apperr_helpers(roots, repo_root)
    try:
        entries = collect_entries(roots, repo_root, helpers)
    except UnparseableStructLiteral as exc:
        # A refused run, never a rendered guess (see the exception's own
        # doc comment): exit 2, the directory's usage/environment code.
        print(f"error: {exc}", file=sys.stderr)
        return 2
    messages = collect_messages(roots)
    for e in entries:
        e.message = messages.get(e.code, "")

    rendered = render_markdown(entries)
    rendered_json = render_machine_json(entries)
    out_path = repo_root / args.out
    machine_path = repo_root / args.machine_out
    # The default target is a Hugo content page, so the committed file
    # carries the fixed front matter block; a custom --out target gets
    # the same treatment, which keeps --check and the write path one
    # comparison.
    page = _SITE_PAGE_PREFIX + rendered

    n_sites, n_codes, n_dups = code_counts(entries)

    if args.check:
        current = out_path.read_text(encoding="utf-8") if out_path.exists() else ""
        current_json = machine_path.read_text(encoding="utf-8") if machine_path.exists() else ""
        if current != page or current_json != rendered_json:
            print(
                f"{args.out} (or {args.machine_out}) is out of date; run: "
                "python3 tools/gen_error_code_index.py",
                file=sys.stderr,
            )
            return 1
        print(
            f"{args.out} and {args.machine_out} are up to date "
            f"({n_sites} construction sites, {n_codes} codes, "
            f"{n_dups} duplicate site(s) removed)."
        )
        return 0

    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(page, encoding="utf-8")
    machine_path.write_text(rendered_json, encoding="utf-8")
    print(
        f"wrote {args.out} and {args.machine_out} "
        f"({n_sites} construction sites, {n_codes} codes, "
        f"{n_dups} duplicate site(s) removed, across "
        f"{len(set(e.module for e in entries))} modules)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
