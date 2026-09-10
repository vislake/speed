#!/usr/bin/env python3
"""Host-core parity gate: the shared host kernel exists once, in two
byte-identical copies.

Two hosts compose HTTP servers from the same platform modules: the
reference app (examples/reference-app) and the project skeleton
`saasctl new` materializes from the embedded tree under
go/saasctl/internal/template/project. Their host-neutral half -- the
liveness endpoints (paths, handlers, mount rule), the four pre-auth
allowlist entries, the mounted-route label seed and the
serve/graceful-shutdown lifecycle -- lives in ONE file, package hostcore,
carried byte-identically by both hosts:

  canonical:  go/saasctl/internal/template/project/internal/hostcore/hostcore.go
              (a template .go file, so it opens with the build-ignore
              marker line `saasctl new` strips at materialization)
  app copy:   examples/reference-app/internal/hostcore/hostcore.go
              (a real, compiling file -- no marker)

The canonical copy lives in the tooling because the tooling is what SHIPS
it: every generated project materializes it verbatim, and the reference
app carries the same bytes as the repository's mandatory first consumer.
This gate is what keeps one file from becoming two hand-maintained
variants, and it checks three things:

  * Content parity -- the app copy must be exactly the canonical file
    with its build-ignore marker line and the blank line after it
    removed; the canonical copy must carry the marker, the app copy must
    not. A difference in either direction fails with a diff.
  * No forked declarations -- neither host may declare any of the shared
    kernel's identifiers (HostCoreIdentifiers below) outside its own copy
    of the file: a host that grows its own HealthzPath or MountRoute has
    forked the kernel no matter what the copies say, and the fix is to
    change the shared file in both places or to keep the new symbol
    host-specific under a host-specific name.
  * No forked statements -- the sentinel expressions the kernel owns
    (ListenAndServe, BaseContext, the three literal paths on a
    non-test .go file) must not reappear in either host's own code:
    re-growing the serve loop or a hand-rolled path table is the drift
    this gate exists to stop. Test files are deliberately exempt from
    this one: tests build throwaway muxes and fake servers on purpose,
    and the discipline it protects is the production composition's.

To change shared host behavior, edit the canonical file and copy it over
the app copy in the same change (both paths above); the gate's failure
output names both files. To make one host behave differently, the symbol
does not belong in the kernel -- move it back into that host.

Scan scope: all .go files under the two host trees, plus the two copies
themselves; .claude/, node_modules/ and vendor/ are skipped.

Usage:
    python3 tools/check_host_core_parity.py [--root DIR]

Exit codes: 0 = one shared kernel, two identical copies, no forked
symbols; 1 = at least one finding; 2 = usage/infrastructure error (a
missing copy, unreadable root).
Standard library only, Python >= 3.11.
"""

from __future__ import annotations

import argparse
import difflib
import pathlib
import re
import sys

# The canonical copy (inside the saasctl template tree) and the reference
# app's copy of the same file, relative to the repository root.
CANONICAL_REL_PATH = (
    "go/saasctl/internal/template/project/internal/hostcore/hostcore.go"
)
APP_COPY_REL_PATH = "examples/reference-app/internal/hostcore/hostcore.go"

# The two host trees the no-fork scans cover.
TEMPLATE_TREE = "go/saasctl/internal/template/project"
APP_TREE = "examples/reference-app"

# The build-ignore marker every template .go file opens with (the line
# `saasctl new` strips at materialization) and the blank line that follows
# it.
BUILD_IGNORE_LINE = "//go:build ignore"

# Package hostcore's exported surface: a host may not declare any of these
# outside its own copy of the shared file (see the module docstring).
HOSTCORE_IDENTIFIERS = (
    "HealthzPath",
    "MetricsPath",
    "AuthnAPIPath",
    "ReadHeaderTimeout",
    "ShutdownTimeout",
    "HealthzHandler",
    "MetricsHandler",
    "MountLiveness",
    "PreAuthAllowlist",
    "MountRoute",
    "RegisterMountedRoutes",
    "ServeUntilShutdown",
)

# Expressions the shared kernel owns: their appearance in a non-test .go
# file outside the two copies means a host re-grew that piece.
HOSTCORE_SENTINELS = (
    "ListenAndServe",
    "BaseContext",
    '"/healthz"',
    '"/metrics"',
    '"/api/v1/authn"',
)

# Directory names the scans never descend into.
SKIP_DIRS = {".git", ".claude", "node_modules", "vendor", "__pycache__"}

# Diff lines printed for a copy mismatch.
MAX_DIFF_LINES = 16


def strip_line_comments(line: str) -> str:
    """Drop a `//` comment from one line. String literals are not tracked
    (the same convention the sibling checkers document); the identifiers
    and sentinels this gate scans for never live inside a literal in
    these files except the sentinel path literals themselves, which are
    matched as literals on purpose."""
    idx = line.find("//")
    return line[:idx] if idx >= 0 else line


def declaration_pattern(identifier: str) -> re.Pattern[str]:
    """Matches a declaration of identifier in Go source: a func (method
    receivers included), a const/var, or a const/var block entry."""
    return re.compile(
        r"^\s*(?:"
        rf"func\s+(?:\([^)]*\)\s*)?{identifier}\b"
        rf"|(?:const|var)\s+{identifier}\b"
        rf"|{identifier}\s*=(?!=)"
        r")"
    )


def go_files(root: pathlib.Path, tree_rel: str):
    base = root / tree_rel
    if not base.is_dir():
        return
    for path in sorted(base.rglob("*.go")):
        rel = path.relative_to(root)
        if any(part in SKIP_DIRS for part in rel.parts):
            continue
        yield path, rel


def is_test_file(rel: pathlib.Path) -> bool:
    return rel.name.endswith("_test.go")


def check_copies(root: pathlib.Path, findings: list[str]) -> bool:
    """Verify the two copies exist and are identical modulo the marker.
    Returns True when the comparison ran (both files present)."""
    canonical_path = root / CANONICAL_REL_PATH
    app_path = root / APP_COPY_REL_PATH
    for path, rel in ((canonical_path, CANONICAL_REL_PATH), (app_path, APP_COPY_REL_PATH)):
        if not path.is_file():
            findings.append(
                f"{rel}: the shared host kernel's copy is missing -- the "
                "package hostcore is one file carried byte-identically by "
                "both hosts; restore this copy before anything else"
            )
    if not (canonical_path.is_file() and app_path.is_file()):
        return False

    canonical = canonical_path.read_text(encoding="utf-8")
    app = app_path.read_text(encoding="utf-8")

    prefix = BUILD_IGNORE_LINE + "\n\n"
    if not canonical.startswith(prefix):
        findings.append(
            f"{CANONICAL_REL_PATH}: a template .go file must open with "
            f"{BUILD_IGNORE_LINE!r} and a blank line (materialization "
            "strips exactly that); the marker is missing"
        )
    if app.startswith(BUILD_IGNORE_LINE + "\n"):
        findings.append(
            f"{APP_COPY_REL_PATH}: a real, compiling file must not carry "
            f"the template marker {BUILD_IGNORE_LINE!r}"
        )
    stripped = canonical[len(prefix):] if canonical.startswith(prefix) else canonical
    if stripped == app:
        return True
    diff = list(
        difflib.unified_diff(
            stripped.splitlines(),
            app.splitlines(),
            fromfile=CANONICAL_REL_PATH + " (marker stripped)",
            tofile=APP_COPY_REL_PATH,
            lineterm="",
        )
    )
    shown = "\n      ".join(diff[:MAX_DIFF_LINES])
    if len(diff) > MAX_DIFF_LINES:
        shown += "\n      ... (diff truncated)"
    findings.append(
        "the two hostcore copies have drifted apart; edit the canonical "
        f"file ({CANONICAL_REL_PATH}) and copy it over {APP_COPY_REL_PATH} "
        "(its build-ignore marker line and the following blank line "
        f"removed) in the same change. Diff (canonical -> app):\n      {shown}"
    )
    return True


def scan_no_forks(root: pathlib.Path, findings: list[str]) -> None:
    """The no-fork rules: no shared identifier declared, and no kernel
    sentinel used, outside the two copies."""
    copies = {CANONICAL_REL_PATH, APP_COPY_REL_PATH}
    patterns = [
        (identifier, declaration_pattern(identifier))
        for identifier in HOSTCORE_IDENTIFIERS
    ]
    for tree_rel in (TEMPLATE_TREE, APP_TREE):
        for path, rel in go_files(root, tree_rel):
            rel_str = rel.as_posix()
            if rel_str in copies:
                continue
            text = path.read_text(encoding="utf-8")
            lines = [strip_line_comments(line) for line in text.splitlines()]
            for line_no, line in enumerate(lines, start=1):
                for identifier, pattern in patterns:
                    if pattern.match(line):
                        findings.append(
                            f"{rel_str}:{line_no}: declares {identifier}, "
                            "which belongs to the shared host kernel "
                            f"({CANONICAL_REL_PATH} / {APP_COPY_REL_PATH}); "
                            "change the shared file in both places, or keep "
                            "this symbol host-specific under a "
                            "host-specific name"
                        )
                if is_test_file(rel):
                    continue
                for sentinel in HOSTCORE_SENTINELS:
                    if sentinel in line:
                        findings.append(
                            f"{rel_str}:{line_no}: uses {sentinel}, which "
                            "the shared host kernel owns -- the serve "
                            "lifecycle and the liveness/authn paths come "
                            f"from {CANONICAL_REL_PATH.split('/')[-1]}'s "
                            "package (internal/hostcore); re-growing them "
                            "here forks the kernel"
                        )


def scan(root: pathlib.Path) -> list[str]:
    findings: list[str] = []
    check_copies(root, findings)
    scan_no_forks(root, findings)
    return findings


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--root",
        default=None,
        help="repository root (default: the tools/ directory's parent)",
    )
    args = parser.parse_args(argv)
    root = (
        pathlib.Path(args.root).resolve()
        if args.root
        else pathlib.Path(__file__).resolve().parent.parent
    )
    if not root.is_dir():
        print(f"check_host_core_parity: --root is not a directory: {root}",
              file=sys.stderr)
        return 2
    try:
        findings = scan(root)
    except OSError as exc:
        print(f"check_host_core_parity: {exc}", file=sys.stderr)
        return 2
    for finding in findings:
        print(finding)
    if findings:
        print(
            "check_host_core_parity: %d finding(s)" % len(findings),
            file=sys.stderr,
        )
        return 1
    print("check_host_core_parity: one hostcore, two identical copies, no forks")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
