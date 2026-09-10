#!/usr/bin/env python3
"""Host-composition fork gate: the shared host kernel lives in ONE place,
and no host re-grows it.

Two hosts compose HTTP servers from the same platform modules: the
reference app (examples/reference-app) and the project skeleton
`saasctl new` materializes from the embedded tree under
go/saasctl/internal/template/project. The host-neutral half of that
composition -- authn's mount path, the serve timeouts, the pre-auth
allowlist set, the mounted-route label seed and the serve/graceful-
shutdown lifecycle -- lives in the platform module go/app, imported by
both hosts (the liveness route set itself is observability's,
obs.MountLiveness; the module-route mounting rule is pkgcore.MountRoutes;
the middleware chain is go/app/chain; this header lists what the scan
below protects).

Because the kernel is imported rather than copied, copy drift is
impossible by construction -- there are no two copies to compare. What
this gate checks is the no-fork half: NEITHER host tree may declare any
of the platform kernel's identifiers (HOST_COMPOSITION_IDENTIFIERS below)
in its own code, and neither may re-grow the statements the kernel owns
(the serve loop and the literal paths -- HOST_COMPOSITION_SENTINELS). A
host that declares its own ServeUntilShutdown or its own "/healthz"
literal has forked the shared composition no matter that it also imports
the module: the fix is to call the platform's function, or to keep a
genuinely host-specific symbol under a host-specific name. Test files are
deliberately exempt from the sentinel scan: tests build throwaway muxes
and fake servers on purpose, and the discipline this protects is the
production composition's.

Scan scope: all .go files under the two host trees; .claude/,
node_modules/ and vendor/ are skipped.

Usage:
    python3 tools/check_host_composition.py [--root DIR]

Exit codes: 0 = no host re-declares the shared composition; 1 = at least
one finding; 2 = usage/infrastructure error (unreadable root).
Standard library only, Python >= 3.11.
"""

from __future__ import annotations

import argparse
import pathlib
import re
import sys

# The two host trees the no-fork scans cover.
TEMPLATE_TREE = "go/saasctl/internal/template/project"
APP_TREE = "examples/reference-app"

# The platform kernel module the shared symbols now live in (named in the
# findings so a reader is sent to the import that fixes them).
KERNEL_MODULE = "github.com/vislake/speed/go/app"

# The platform kernel's exported surface: a host may not declare any of
# these in its own code (see the module docstring). The liveness route set
# (HealthzPath, MetricsPath, HealthzHandler, MountLiveness) is deliberately
# absent: it is observability's, so a host reaches it through that package
# and this gate need not police the names. AuthnAPIPath and the serve
# lifecycle symbols are the kernel's (go/app/kernel.go).
HOST_COMPOSITION_IDENTIFIERS = (
    "AuthnAPIPath",
    "ReadHeaderTimeout",
    "ShutdownTimeout",
    "PreAuthAllowlist",
    "RegisterMountedRoutes",
    "ServeUntilShutdown",
)

# Expressions the platform kernel owns: their appearance in a non-test .go
# file inside either host tree means a host re-grew that piece. The path
# literals stay on this list because a host literal for any of them is
# drift whichever package owns the constant (obs.HealthzPath,
# obs.MetricsPath, and app.AuthnAPIPath respectively).
HOST_COMPOSITION_SENTINELS = (
    "ListenAndServe",
    "BaseContext",
    '"/healthz"',
    '"/metrics"',
    '"/api/v1/authn"',
)

# Directory names the scans never descend into.
SKIP_DIRS = {".git", ".claude", "node_modules", "vendor", "__pycache__"}


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


def scan(root: pathlib.Path) -> list[str]:
    """The no-fork rules: no shared identifier declared, and no kernel
    sentinel used, outside the platform module."""
    findings: list[str] = []
    patterns = [
        (identifier, declaration_pattern(identifier))
        for identifier in HOST_COMPOSITION_IDENTIFIERS
    ]
    for tree_rel in (TEMPLATE_TREE, APP_TREE):
        for path, rel in go_files(root, tree_rel):
            rel_str = rel.as_posix()
            text = path.read_text(encoding="utf-8")
            lines = [strip_line_comments(line) for line in text.splitlines()]
            for line_no, line in enumerate(lines, start=1):
                for identifier, pattern in patterns:
                    if pattern.match(line):
                        findings.append(
                            f"{rel_str}:{line_no}: declares {identifier}, "
                            "which belongs to the shared host kernel "
                            f"({KERNEL_MODULE}); call the platform's "
                            "function, or keep this symbol host-specific "
                            "under a host-specific name"
                        )
                if is_test_file(rel):
                    continue
                for sentinel in HOST_COMPOSITION_SENTINELS:
                    if sentinel in line:
                        findings.append(
                            f"{rel_str}:{line_no}: uses {sentinel}, which "
                            "the shared host kernel owns -- the serve "
                            "lifecycle and the liveness/authn paths come "
                            f"from {KERNEL_MODULE}; re-growing them here "
                            "forks the kernel"
                        )
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
        print(f"check_host_composition: --root is not a directory: {root}",
              file=sys.stderr)
        return 2
    try:
        findings = scan(root)
    except OSError as exc:
        print(f"check_host_composition: {exc}", file=sys.stderr)
        return 2
    for finding in findings:
        print(finding)
    if findings:
        print(
            "check_host_composition: %d finding(s)" % len(findings),
            file=sys.stderr,
        )
        return 1
    print("check_host_composition: no host re-declares the shared composition")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
