#!/usr/bin/env python3
"""Repo-wide CJK scanner enforcing the repository Language Rule.

The rule: docs/internal/** is written in Chinese; every other file in the
repository is English, and CI fails on CJK characters found outside
docs/internal/ (i18n resources under locale directories, and the
docs/site/ public documentation site -- English-first, with zh-CN
localization directories added by need -- are the exceptions). This
script is the local-run discipline check; CI runs the same check.

Scan semantics, mirroring go/ratelimit/no_cjk_characters_test.go (the module-level
precedent):

* Go files (.go): comments only. A single-pass lexer extracts comment text
  while honouring string, rune and raw-string contexts, exactly as go/parser
  with parser.ParseComments does -- a "//" or "/*" inside an interpreted
  string, a raw string or a rune literal never starts a comment, and string
  literals / test data are never checked (see go/dbkit/encryption_test.go
  and examples/reference-app/internal/notes/handler_test.go for intentional
  CJK string fixtures that must stay exempt).
* Every other text file (.md, .txt, .html, .yml, .yaml, .toml, .json, and
  any other file that decodes as strict UTF-8 without NUL bytes): the full
  content is checked, line by line.
* Non-text files (any NUL byte in the first 8 KiB, or undecodable bytes)
  are skipped, never reported.

Carve-outs (a whole subtree is exempt):

  docs/internal/          Chinese by rule; never scanned
  docs/site/              public documentation site (English-first, zh-CN
                          by need); never scanned
  resource directories    i18n resource directories, judged by CONTENT
                          rather than by directory name: a directory that
                          directly holds a zh-CN.* or en-US.* file is a
                          resource directory (files there legitimately
                          carry CJK user-facing text, e.g.
                          .../notes/locales/zh-CN.toml) and its whole
                          subtree is exempt. This is the same discovery
                          rule tools/check_i18n_keys.py uses, so the two
                          tools agree on what a locale directory is; a
                          directory merely NAMED locales/locale/i18n/
                          translations that holds no such file is scanned
                          like any other source tree (go/pkgcore/i18n and
                          web/packages/i18n, source directories whose
                          catalogs live in sibling locales/ trees, are
                          scanned like every other package).
  vendor/, node_modules/, .git/   vendored dependencies and VCS metadata
  .idea/, .vscode/                IDE-local directories holding developer
                                  machine state (UI strings etc.); they are
                                  gitignored, never exist in CI

Go comments additionally exempt one fixture-in-comment shape: a godoc
Example function's "Output:" block -- the comment lines that follow a
"// Output:" marker inside a godoc Example (see
go/pkgcore/i18n/example_test.go, whose Example demonstrates zh-CN catalog
rendering and whose expected output is Chinese text). The Go toolchain
REQUIRES an Example's expected output to be spelled in comments -- the
godoc Output convention -- and CI compiles and runs every Example, so an
Example demonstrating zh-CN catalog rendering must carry Chinese output
text verbatim in comment syntax. That text is executed-and-verified
fixture data, the same class as the CJK string literals that stay exempt
(see the Scan semantics section), merely forced into comment syntax by
the toolchain; only the lines after an "Output:" marker within one
contiguous comment run are exempt, so a CJK comment anywhere else in the
same file is still a violation.

CJK means Han script, classified by the same rune ranges Go's unicode.Han
covers (unicode.Is(unicode.Han, r) in the no_cjk_characters_test.go precedent); the
table below transcribes the Go 1.26.1 toolchain's unicode/tables.go _Han
RangeTable (its stride-2 entry {0x3005, 0x3007, 2} becomes two singleton
ranges here) and was verified code point for code point against
unicode.Is(unicode.Han, r) under that toolchain.

Usage:
    python3 tools/scan_cjk.py [--root PATH]

--root defaults to the current directory; run from the repository root for
a whole-repo scan. Every hit prints, on one line, a grep-friendly
"<repo-relative path>:<line>:" prefix followed by the offending character
and its kind, with the offending source line on the next line.

Exit codes: 0 = no CJK found; 1 = at least one violation; 2 = bad usage or
an unrecoverable error (reported on stderr).

Standard library only; requires Python >= 3.11 (tomllib import is used by
check_i18n_keys.py, not here, but the scripts share a floor).
"""

from __future__ import annotations

import argparse
import bisect
import os
import sys

# File-name prefixes identifying user-facing translation resources: the
# zh-CN.* / en-US.* pair files every locale directory ships (the same
# discovery rule tools/check_i18n_keys.py applies). Resource-directory
# exemption is judged by CONTENT: a directory that directly holds a file
# with one of these prefixes is a resource directory (whole subtree
# exempt), whatever its name -- and a directory that holds no such file
# is scanned like any other source tree, whatever its name.
LOCALE_FILE_PREFIXES = ("zh-CN.", "en-US.")

# Directories never scanned regardless of content: VCS metadata, vendored
# dependencies, and IDE-local directories that live only on developer
# machines (GoLand writes Chinese UI strings into .idea/, for instance).
NON_SCANNED_DIR_NAMES = frozenset({".git", ".idea", ".vscode", "node_modules", "vendor"})

# docs/internal/** and the docs/site/** subtree are exempt wholesale. The
# entries are repo-relative directory paths whose whole subtree is skipped;
# their common ancestor ("docs") itself is still descended into so the
# pruning happens at the right level.
CARVED_SUBTREES = frozenset({"docs/internal", "docs/site"})

# Han-script rune ranges, byte-for-byte what Go's unicode.Han classifies
# (go/ratelimit/no_cjk_characters_test.go calls unicode.Is(unicode.Han, r)),
# transcribed from the Go 1.26.1 toolchain's unicode/tables.go _Han table
# and verified code point for code point against unicode.Is: CJK radicals
# and Kangxi radicals; the Han members of the CJK Symbols and Punctuation
# block -- U+3005 and U+3007 are singletons (Go's entry is the stride-2
# range {0x3005, 0x3007, 2}, so U+3006 is *not* Han), U+3021-U+3029 and
# U+3038-U+303B are contiguous; the ideograph blocks U+3400-U+4DBF and
# U+4E00-U+9FFF; compatibility ideographs U+F900-U+FAD9; and the
# supplementary-plane extension blocks.
HAN_RANGES = (
    (0x2E80, 0x2E99),
    (0x2E9B, 0x2EF3),
    (0x2F00, 0x2FD5),
    (0x3005, 0x3005),
    (0x3007, 0x3007),
    (0x3021, 0x3029),
    (0x3038, 0x303B),
    (0x3400, 0x4DBF),
    (0x4E00, 0x9FFF),
    (0xF900, 0xFA6D),
    (0xFA70, 0xFAD9),
    (0x16FE2, 0x16FE3),
    (0x16FF0, 0x16FF1),
    (0x20000, 0x2A6DF),
    (0x2A700, 0x2B739),
    (0x2B740, 0x2B81D),
    (0x2B820, 0x2CEA1),
    (0x2CEB0, 0x2EBE0),
    (0x2F800, 0x2FA1D),
    (0x30000, 0x3134A),
    (0x31350, 0x323AF),
)


def first_han_char(s: str) -> str | None:
    """Return the first Han-script character in s, or None."""
    for ch in s:
        o = ord(ch)
        if o < HAN_RANGES[0][0]:
            continue  # ASCII and friends; ranges are sorted ascending
        for lo, hi in HAN_RANGES:
            if o < lo:
                break
            if o <= hi:
                return ch
    return None


def _skip_go_literal(text: str, start: int) -> int:
    """Return the index just past the literal opened by text[start].

    start must point at a quote character. '\\' escapes the next character,
    so a quote after a backslash never closes the literal (Go has no
    line-continuation escape inside interpreted strings -- only raw strings
    span lines; if a file tries one anyway the file is invalid and the
    literal simply runs to EOF, which the scanner tolerates). A raw string
    (backtick) has no escapes and ends at the next backtick. Unterminated
    literals run to EOF; such a file would not compile, but the scanner must
    not crash on it either.
    """
    quote = text[start]
    n = len(text)
    i = start + 1
    if quote == "`":
        while i < n and text[i] != "`":
            i += 1
        return i + 1 if i < n else n
    while i < n:
        ch = text[i]
        if ch == "\\":
            i += 2
        elif ch == quote:
            return i + 1
        else:
            i += 1
    return n


def go_comment_spans(text: str) -> list[tuple[int, int, str]]:
    """Return (content_start, content_end, kind) for every Go comment.

    The content spans exclude the "//" and "/*", "*/" markers themselves
    (they are ASCII and irrelevant to the CJK check, but excluding them
    keeps the reported offending text clean). Comments are recognised only
    in code context: while inside an interpreted string, a rune literal or a
    raw string, no comment can begin -- this is the state-machine equivalent
    of go/parser's ParseComments and the reason string literals full of CJK
    test data stay exempt. Kind is "line" or "block".
    """
    spans: list[tuple[int, int, str]] = []
    n = len(text)
    i = 0
    while i < n:
        ch = text[i]
        if ch == "/" and i + 1 < n:
            nxt = text[i + 1]
            if nxt == "/":  # line comment: content ends at the newline
                j = i + 2
                while j < n and text[j] != "\n":
                    j += 1
                spans.append((i + 2, j, "line"))
                i = j
            elif nxt == "*":  # block comment: content ends at "*/"
                j = text.find("*/", i + 2)
                if j == -1:
                    spans.append((i + 2, n, "block"))
                    i = n
                else:
                    spans.append((i + 2, j, "block"))
                    i = j + 2
            else:
                i += 1
        elif ch in "\"'`":
            i = _skip_go_literal(text, i)
        else:
            i += 1
    return spans


class Hit:
    """One violation: a CJK character at a physical source line."""

    __slots__ = ("line", "char", "kind", "line_text")

    def __init__(self, line: int, char: str, kind: str, line_text: str):
        self.line = line  # 1-based physical line number
        self.char = char  # the offending character
        self.kind = kind  # "line comment" | "block comment" | "file text"
        self.line_text = line_text  # the offending source line, rstripped


def _line_starts(text: str) -> list[int]:
    starts = [0]
    starts.extend(i + 1 for i, ch in enumerate(text) if ch == "\n")
    return starts


def scan_text(text: str, kind_label: str) -> list[Hit]:
    """Full-content scan: every physical line is checked whole."""
    hits: list[Hit] = []
    start = 0
    line_no = 0
    while True:
        nl = text.find("\n", start)
        line = text[start : nl if nl != -1 else len(text)]
        if nl == -1 and start >= len(text) and not line:
            break
        line_no += 1
        ch = first_han_char(line)
        if ch is not None:
            hits.append(Hit(line_no, ch, kind_label, line.rstrip("\r")))
        if nl == -1:
            break
        start = nl + 1
    return hits


def _example_output_lines(
    text: str,
    spans: list[tuple[int, int, str]],
    starts: list[int],
) -> set[int]:
    """1-based line numbers of the godoc Example "Output:" block lines.

    The godoc Output convention requires an Example function's expected
    output to be spelled as comment lines following a "// Output:" marker
    within the same contiguous comment run, and CI compiles and runs every
    Example -- so an Example demonstrating zh-CN catalog rendering must
    carry Chinese output verbatim in comment syntax. Those lines are
    executed-and-verified fixture text, the same class as the CJK string
    literals the scanner leaves alone (see the module docstring's
    carve-outs section), so they are exempt from the comment scan -- and
    ONLY they are: the marker's own line, comment lines before it, and any
    CJK comment elsewhere in the file stay violations.

    The run ends where the comments end: a non-line comment, or a line
    comment whose content does not start on the very next line (a blank
    line between two examples). The marker itself is a comment content
    whose stripped text is exactly "Output:" (the content spans exclude
    the "//" markers, per go_comment_spans).
    """
    output_lines: set[int] = set()
    in_output = False
    prev_end: int | None = None
    for content_start, content_end, kind in spans:
        if kind != "line":
            in_output = False
            prev_end = None
            continue
        # Two line comments belong to one contiguous run when the text
        # between them is exactly the previous comment's own terminating
        # newline, the next line's indentation and its "//" prefix -- i.e.
        # no blank line between the two. Content spans exclude the "//"
        # markers, so the check reads the raw text rather than comparing
        # offsets.
        contiguous = (
            prev_end is not None
            and text[prev_end:content_start].count("\n") == 1
            and text[content_start - 2:content_start] == "//"
        )
        prev_end = content_end
        if not in_output:
            if text[content_start:content_end].strip() == "Output:":
                in_output = True
            continue
        if not contiguous:
            in_output = False
            continue
        # This span's content is expected-output text: mark its physical
        # line. The span's first character falls on the line after the
        # previous span (contiguity above), which is the line index bisect
        # derives; a line comment never spans lines, so one line suffices.
        idx = bisect.bisect_right(starts, content_start) - 1
        output_lines.add(idx + 1)
    return output_lines


def scan_go(text: str) -> list[Hit]:
    """Comment-only scan of Go source, per the repo's Language Rule."""
    hits: list[Hit] = []
    if not text:
        return hits
    starts = _line_starts(text)
    ends = starts[1:] + [len(text)]
    spans = go_comment_spans(text)
    output_lines = _example_output_lines(text, spans, starts)
    seen_lines: set[int] = set()
    for content_start, content_end, kind in spans:
        if content_start >= content_end:
            continue
        # Attribute the comment's content to physical lines. A block comment
        # spanning several lines reports each line that carries CJK.
        idx = bisect.bisect_right(starts, content_start) - 1
        pos = content_start
        while pos < content_end and idx < len(starts):
            line_start = starts[idx]
            line_end = ends[idx]
            seg_start = max(pos, line_start)
            seg_end = min(content_end, line_end)
            if seg_start < seg_end:
                ch = first_han_char(text[seg_start:seg_end])
                if (
                    ch is not None
                    and idx not in seen_lines
                    and idx + 1 not in output_lines
                ):
                    seen_lines.add(idx)
                    hits.append(
                        Hit(
                            idx + 1,
                            ch,
                            f"{kind} comment",
                            text[line_start:line_end].rstrip("\r\n"),
                        )
                    )
            pos = line_end
            idx += 1
    return hits


def _pruned_dirnames(dirnames: list[str], rel_dir: str) -> list[str]:
    """Return the subdirectories worth descending into."""
    kept: list[str] = []
    for name in dirnames:
        if name in NON_SCANNED_DIR_NAMES:
            continue
        rel = name if rel_dir == "." else f"{rel_dir}/{name}"
        if rel in CARVED_SUBTREES:
            continue
        kept.append(name)
    return kept


def _is_resource_dir(filenames: list[str]) -> bool:
    """Whether this directory is a locale resource directory, by content.

    A directory that directly holds a zh-CN.* or en-US.* file is a
    resource directory (the discovery rule tools/check_i18n_keys.py
    applies); its whole subtree -- the pair files and anything beside them
    -- legitimately carries CJK user-facing text and is exempt.
    """
    return any(fn.startswith(LOCALE_FILE_PREFIXES) for fn in filenames)


def _print_hit(rel_path: str, hit: Hit) -> None:
    print(
        f"{rel_path}:{hit.line}: {hit.kind} contains CJK character "
        f"'{hit.char}' (U+{ord(hit.char):04X})"
    )
    print(f"  {hit.line_text}")


def scan_root(root: str) -> int:
    """Walk root and report every violation. Returns the exit code."""
    hits: list[Hit] = []
    hit_files: set[str] = set()
    n_go = 0
    n_text = 0
    n_skipped = 0
    for dirpath, dirnames, filenames in os.walk(root):
        rel_dir = os.path.relpath(dirpath, root)
        if _is_resource_dir(filenames):
            # A locale resource directory: its own files are the zh-CN /
            # en-US pair members (or content beside them), and everything
            # under it is resource content too -- never descended into.
            dirnames[:] = []
            continue
        dirnames[:] = sorted(_pruned_dirnames(dirnames, rel_dir))
        for filename in sorted(filenames):
            if filename == ".git":  # worktree pointer file, not a checkout
                continue
            full_path = os.path.join(dirpath, filename)
            rel_path = os.path.relpath(full_path, root)
            try:
                with open(full_path, "rb") as fh:
                    data = fh.read()
            except OSError as exc:
                print(f"{rel_path}: cannot read: {exc}", file=sys.stderr)
                return 2
            if not data:
                n_skipped += 1
                continue
            if b"\x00" in data[:8192]:
                n_skipped += 1
                continue  # binary
            try:
                text = data.decode("utf-8")
            except UnicodeDecodeError:
                n_skipped += 1
                continue  # binary or non-UTF-8; CJK would decode cleanly
            if filename.endswith(".go"):
                n_go += 1
                file_hits = scan_go(text)
            else:
                n_text += 1
                file_hits = scan_text(text, "file text")
            for hit in file_hits:
                _print_hit(rel_path, hit)
            if file_hits:
                hits.extend(file_hits)
                hit_files.add(rel_path)

    if hits:
        print(
            f"{len(hits)} CJK violation(s) across {len(hit_files)} file(s); "
            f"root CLAUDE.md's Language Rule requires English outside "
            f"docs/internal/ -- paraphrase in English, or cite the "
            f"docs/internal/ file path instead of quoting Chinese"
        )
        return 1
    print(
        f"OK: no CJK characters found (scanned {n_go + n_text} files: "
        f"{n_go} Go files comment-only, {n_text} other text files; "
        f"{n_skipped} binary/empty skipped)"
    )
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Scan a repository tree for CJK characters outside the carved-out "
            "docs/internal/, docs/site/ and locale resource directories "
            "(a directory holding a zh-CN.* or en-US.* file -- judged by "
            "content, never by directory name), enforcing root CLAUDE.md's "
            "Language Rule (English outside docs/internal/). Go files are "
            "checked comment-only -- except godoc Example Output: blocks, "
            "which carry expected-output fixture text in comment syntax --; "
            "every other UTF-8 text file is checked in full."
        )
    )
    parser.add_argument(
        "--root",
        default=".",
        help="repository root to scan (default: current directory); "
        "hit paths are printed relative to it",
    )
    args = parser.parse_args(argv)
    root = os.path.abspath(args.root)
    if not os.path.isdir(root):
        print(f"error: --root is not a directory: {args.root}", file=sys.stderr)
        return 2
    return scan_root(root)


if __name__ == "__main__":
    sys.exit(main())
