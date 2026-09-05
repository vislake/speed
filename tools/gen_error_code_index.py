#!/usr/bin/env python3
"""Error-code index generator.

Closes docs/internal/13-documentation-standards.md's must-have doc list
item (an error-code index) and its AI Agent requirements item 6 (every
error code gets its own documented entry, with the triggering condition
and what to do about it). No such index existed anywhere in the repository
before this
script: go/pkgcore/apperr/apperr.go only has the encoding mechanism (the
six status-mapped builders), never a catalog of the codes built with it.

What it does: walks every *.go file under --roots (default: go/ and
examples/, this repository's two source trees; excludes _test.go files
and generated *.gen.go / *_gen.go files, which carry no hand-written error
declarations of their own), and regex-matches two declaration shapes --

    ErrFoo = apperr.Invalid("module.some_code")
    ErrFoo = &apperr.Error{Code: "module.some_code", Status: http.StatusTooManyRequests}

(the second shape covers the handful of error vars built as a struct
literal because none of apperr's five builder functions map to HTTP 429 --
go/sharing's, go/integration's, go/org's and go/ai-gateway's own
ErrRateLimited, all following the identical documented convention) --
capturing the Go identifier, the apperr code string, the resulting HTTP
status, and the file:line. It then walks upward from the matched line
collecting the contiguous "// ..." doc comment immediately above the
declaration (the same convention godoc itself reads), and separately
builds a code -> English message dict from every locales/en-US.toml this
repository ships (go/pkgcore/i18n's own message-catalog convention: the
TOML key IS the apperr code), so a code with no locale entry --
a boot-time wiring refusal, say, never rendered to an end user -- is
reported as such rather than silently omitted.

The result is one Markdown table (code / message / triggering condition /
module / HTTP status / source), grouped by module (the code's own
dot-prefix, e.g. "notification" from "notification.type_not_found" --
not the Go package's directory, since a handful of codes are declared in
one package but logically belong to another's vocabulary), sorted by
module then code, written to --out (default: docs/error-codes.md).

Usage:
    python3 tools/gen_error_code_index.py [--roots go examples] [--out docs/error-codes.md] [--check]

--check exits nonzero (printing a diff) instead of writing, for a future
CI wiring the same way tools/check_i18n_keys.py's own header records this
generator has none yet (docs-check.yml's DELIBERATELY NOT WIRED list is
the honest place to record that, mirroring how this repository already
treats every other self-written check with no live CI hookup).
"""

from __future__ import annotations

import argparse
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
_BUILDER_RE = re.compile(
    r'^\s*(?:var\s+)?(?P<ident>[A-Za-z_]\w*)\s*=\s*apperr\.'
    r'(?P<builder>NotFound|Invalid|Conflict|Unauthorized|Forbidden|Internal)'
    r'\(\s*"(?P<code>[^"]+)"\s*\)'
)

# Matches the struct-literal 429 shape: "IDENT = &apperr.Error{Code:
# "code", Status: http.StatusTooManyRequests}" (field order and whitespace
# tolerant, optional leading "var ", same as _BUILDER_RE above; Status is
# assumed StatusTooManyRequests since that is the one status none of the
# five builders provide, and every real call site in this repository uses
# this shape for exactly that reason).
_STRUCT_RE = re.compile(
    r'^\s*(?:var\s+)?(?P<ident>[A-Za-z_]\w*)\s*=\s*&apperr\.Error\{'
    r'\s*Code:\s*"(?P<code>[^"]+)"'
)

_BUILDER_STATUS = {
    "NotFound": 404,
    "Invalid": 400,
    "Conflict": 409,
    "Unauthorized": 401,
    "Forbidden": 403,
    "Internal": 500,
}

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


def _scan_go_file(path: pathlib.Path, rel: str) -> list[ErrorEntry]:
    text = path.read_text(encoding="utf-8", errors="replace")
    lines = text.splitlines()
    entries: list[ErrorEntry] = []
    for i, line in enumerate(lines):
        m = _BUILDER_RE.match(line)
        if m:
            entries.append(
                ErrorEntry(
                    ident=m.group("ident"),
                    code=m.group("code"),
                    status=_BUILDER_STATUS[m.group("builder")],
                    source=f"{rel}:{i + 1}",
                    doc=_leading_comment(lines, i),
                )
            )
            continue
        m = _STRUCT_RE.match(line)
        if m:
            entries.append(
                ErrorEntry(
                    ident=m.group("ident"),
                    code=m.group("code"),
                    status=429,
                    source=f"{rel}:{i + 1}",
                    doc=_leading_comment(lines, i),
                )
            )
    return entries


def collect_entries(roots: list[pathlib.Path], repo_root: pathlib.Path) -> list[ErrorEntry]:
    entries: list[ErrorEntry] = []
    for root in roots:
        for path in sorted(root.rglob("*.go")):
            if _is_excluded(path):
                continue
            rel = str(path.relative_to(repo_root))
            entries.extend(_scan_go_file(path, rel))
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


def render_markdown(entries: list[ErrorEntry]) -> str:
    lines = [
        "# Error code index",
        "",
        "Generated by `tools/gen_error_code_index.py` -- do not hand-edit.",
        "Regenerate with:",
        "",
        "```",
        "python3 tools/gen_error_code_index.py",
        "```",
        "",
        "Every code is a `*apperr.Error` built in Go source (`apperr.Invalid`,"
        " `apperr.NotFound`, and so on, or the equivalent `&apperr.Error{...}`"
        " struct literal for the one HTTP status -- 429 -- none of apperr's"
        " five builder functions cover). \"Message\" is the code's own"
        " `en-US.toml` catalog entry when one exists (a code with none is"
        " never rendered to an end user -- typically a boot-time wiring"
        " refusal); \"Triggering condition\" is the Go declaration's own doc"
        " comment, verbatim.",
        "",
    ]
    by_module: dict[str, list[ErrorEntry]] = {}
    for e in entries:
        by_module.setdefault(e.module, []).append(e)

    for module in sorted(by_module):
        lines.append(f"## {module}")
        lines.append("")
        lines.append("| Code | Status | Message | Triggering condition | Source |")
        lines.append("|---|---|---|---|---|")
        seen_codes: set[str] = set()
        for e in sorted(by_module[module], key=lambda e: (e.code, e.source)):
            if e.code in seen_codes:
                # The same code declared more than once (a rare, deliberate
                # re-export, or two vars sharing one code) -- keep the
                # first occurrence's row only, so the index stays one row
                # per code rather than duplicating it.
                continue
            seen_codes.add(e.code)
            message = e.message.replace("|", "\\|").replace("\n", " ") or "_(no locale message -- not user-facing)_"
            doc = e.doc.replace("|", "\\|") or "_(undocumented)_"
            lines.append(f"| `{e.code}` | {e.status} | {message} | {doc} | `{e.source}` |")
        lines.append("")

    lines.append(f"{len(entries)} declaration(s), {sum(len(v) for v in by_module.values())} across {len(by_module)} module(s) (before de-duplicating a code declared more than once).")
    lines.append("")
    return "\n".join(lines)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--roots", nargs="+", default=["go", "examples"], help="Directories to scan for *.go files (relative to the repo root).")
    parser.add_argument("--out", default="docs/error-codes.md", help="Output Markdown file (relative to the repo root).")
    parser.add_argument("--check", action="store_true", help="Exit nonzero if --out is not already up to date, instead of writing it.")
    args = parser.parse_args()

    repo_root = pathlib.Path(__file__).resolve().parent.parent
    roots = [repo_root / r for r in args.roots]

    entries = collect_entries(roots, repo_root)
    messages = collect_messages(roots)
    for e in entries:
        e.message = messages.get(e.code, "")

    rendered = render_markdown(entries)
    out_path = repo_root / args.out

    if args.check:
        current = out_path.read_text(encoding="utf-8") if out_path.exists() else ""
        if current != rendered:
            print(f"{args.out} is out of date; run: python3 tools/gen_error_code_index.py", file=sys.stderr)
            return 1
        print(f"{args.out} is up to date ({len(entries)} declarations).")
        return 0

    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(rendered, encoding="utf-8")
    print(f"wrote {args.out} ({len(entries)} declarations across {len(set(e.module for e in entries))} modules)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
