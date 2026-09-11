#!/usr/bin/env python3
"""Host-composition fork gate: the shared host kernel lives in ONE place,
and no host re-grows it.

Two hosts compose HTTP servers from the same platform modules: the
reference app (examples/reference-app) and the project skeleton
`saasctl new` materializes from the embedded tree under
go/saasctl/internal/template/project. The host-neutral half of that
composition -- the application assembly engine (the configuration load,
the composition plan and the seven-stage drive, all in the platform
module go/app), authn's mount path, the serve timeouts, the pre-auth
allowlist set and the middleware chain (go/app/chain) -- lives in go/app,
imported by both hosts. (The liveness route set itself is observability's,
obs.MountLiveness; the mounted-route label seed is observability's
obs.RegisterMountedRoutes; the module-route mounting rule is
pkgcore.MountRoutes; this header lists what the scan below protects.)

Because the kernel is imported rather than copied, copy drift is
impossible by construction -- there are no two copies to compare. What
this gate checks is the no-fork half, in three layers:

  * no host tree may DECLARE any of the platform kernel's identifiers
    (HOST_COMPOSITION_IDENTIFIERS below) in its own code;
  * neither may re-grow the statements the kernel owns -- the serve loop
    and the literal paths (HOST_COMPOSITION_SENTINELS), each with its own
    named-file allowance for the one host file that legitimately carries
    it;
  * neither may re-issue a step of the assembly the engine owns
    (HOST_COMPOSITION_CALL_BANS): a host that builds its own kernel,
    opens its own database, applies its own migrations, composes its own
    mux, serves its own lifecycle, creates its own ComponentRegistry or
    drives the component stages itself has forked the shared composition
    no matter that it also imports the module. The fix is to select the
    component that owns the step in the composition configuration, or to
    register a genuinely host-specific behavior as a host component --
    never to re-issue the call here. The stage bans deliberately omit
    Stop, Start and Close: the hosts' composition files call those verbs
    on their own services today (a scheduler, a queue, a service), so
    banning them would fire on legitimate code; a hand-rolled drive is
    caught at its first call (Prepare) and at the registry's creation,
    which is the pair the bans build on.

Test files are deliberately exempt from the sentinel and call-ban scans:
tests build throwaway muxes, fake servers and their own mini-kernels on
purpose, and the discipline this protects is the production
composition's. The declaration scan still covers every file, tests
included -- a test re-declaring a kernel identifier is a name collision
waiting to become a fork.

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

# The five selection server.go templates `saasctl new` materializes into a
# generated project's cmd/server/server.go: each selection's own assembly
# file, which registers the host's own components, drives the assembly
# through the engine's Assemble, owns the process's signal handling and
# composes the host's HTTP face. Each is named in the allowances below for
# exactly the host-owned shapes it carries.
TEMPLATE_SELECTION_SERVERS = tuple(
    "go/saasctl/internal/template/project/selection/%s/server.go" % key
    for key in ("authn+org+rbac", "authn+rbac", "authn+org", "authn", "none")
)

# The platform kernel module the shared symbols now live in (named in the
# findings so a reader is sent to the import that fixes them).
KERNEL_MODULE = "github.com/vislake/speed/go/app"

# The platform kernel's exported surface: a host may not declare any of
# these in its own code (see the module docstring). The liveness route set
# (HealthzPath, MetricsPath, HealthzHandler, MountLiveness) and the
# mounted-route label seed (RegisterMountedRoutes) are deliberately absent:
# they are observability's, so a host reaches them through that package and
# this gate need not police the names. AuthnAPIPath and the serve lifecycle
# symbols are the kernel's (go/app/kernel.go); ServeUntilShutdown stays on
# the list although the platform no longer exports it -- a host
# re-declaring that name is re-growing the pre-engine serve loop this list
# exists to catch.
HOST_COMPOSITION_IDENTIFIERS = (
    "AuthnAPIPath",
    "ReadHeaderTimeout",
    "ShutdownTimeout",
    "PreAuthAllowlist",
    "ServeUntilShutdown",
)

# Expressions the platform kernel owns: their appearance in a non-test .go
# file inside either host tree means a host re-grew that piece. The path
# literals stay on this list because a host literal for any of them is
# drift whichever package owns the constant (obs.HealthzPath,
# obs.MetricsPath, and app.AuthnAPIPath respectively). Each entry is
# (sentinel, allowed), read as the call bans' allowed tuple is:
#
#   * BaseContext -- allowed in the reference app's application component
#     (examples/reference-app/internal/app/component.go) and in each
#     selection server.go template (TEMPLATE_SELECTION_SERVERS): both
#     compose and serve the host's own HTTP face, and the request base
#     context handed to a listener is that face's own contract, not a
#     re-grown copy of the engine's serve loop. Every other host file still
#     fires, and the ListenAndServe entry stays unconditionally banned (the
#     face calls net.Listen plus Serve, so no host file spells the engine's
#     listen call).
HOST_COMPOSITION_SENTINELS = (
    ("ListenAndServe", ()),
    (
        "BaseContext",
        ("examples/reference-app/internal/app/component.go",)
        + TEMPLATE_SELECTION_SERVERS,
    ),
    ('"/healthz"', ()),
    ('"/metrics"', ()),
    ('"/api/v1/authn"', ()),
)

# The composition surface of each host tree: the files where the host's own
# assembly lives (the generated project's cmd/server plus the selection's
# server.go; the reference app's cmd/server plus internal/app). An entry
# scoped "composition" is checked here only -- a host's own BUSINESS
# packages (internal/notes, internal/cases, ...) legitimately build their
# own routers with http.NewServeMux, and that is module-level code, not
# host assembly.
HOST_COMPOSITION_PATHS = (
    "go/saasctl/internal/template/project/cmd/server/",
    "go/saasctl/internal/template/project/selection/",
    "examples/reference-app/cmd/server/",
    "examples/reference-app/internal/app/",
)

# Assembly calls the application engine (go/app) owns: their appearance in
# a non-test .go file inside either host tree means a host re-issued a step
# of the shared assembly -- the option surface's steps and the component
# assembly's alike. Each entry is (label, regex, scope, allowed):
#
#   * label  -- the human-readable call the finding names;
#   * regex  -- what to look for. Most regexes guard the leading boundary so
#     a longer package name ending in the same letters cannot match; the
#     chain entry is the deliberate exception -- both hosts import the chain
#     package under the `speedchain` alias, so the pattern matches any
#     single selector ending in "chain" (the real spelling `chain.Chain(`,
#     the in-tree `speedchain.Chain(` and any other alias of the same
#     package), while the sanctioned entry point is `speedchain.Standard`
#     and never matches;
#   * scope  -- "tree" (any non-test .go file under the host tree) or
#     "composition" (only HOST_COMPOSITION_PATHS above);
#   * allowed -- repo-relative files where the call is the sanctioned,
#     host-owned shape, each named with its reason:
#       - jobs.NewStandaloneQueue and jobs.Wire: background execution is
#         host-owned by design, and the host reaches both through the
#         "queue.standalone" component's own assembly -- no host file
#         issues either call, so neither entry carries an allowance (a
#         host that re-issues one has forked the component).
#       - dbkit.Open in the reference app's host wiring
#         (examples/reference-app/internal/app/host_wiring.go): the
#         database connection IS the host's to open -- the write-capture
#         scope (dbkit.Options.AuditBus/AuditModels) is a construction
#         parameter of the connection, so the host's database component
#         owns the call. Any other host file still fires.
#       - signal.NotifyContext in the reference app's assembly core
#         (examples/reference-app/internal/app/server.go) and in each
#         selection server.go template (TEMPLATE_SELECTION_SERVERS): the
#         signal handling of the host's own serve belongs to the host whose
#         listener the process owns. Any other host file still fires.
#       - http.NewServeMux in the reference app's application component
#         (examples/reference-app/internal/app/component.go) and in each
#         selection server.go template (TEMPLATE_SELECTION_SERVERS): the mux
#         the application component's Init composes IS the host's own face
#         -- the platform liveness routes, the protected-face composition
#         and the SPA wrap hang off it there -- so those files own the call.
#         Any other composition-path file still fires.
#       - pkgcore.NewComponentRegistry: the reference app's assembly core
#         (examples/reference-app/internal/app/server.go) and each selection
#         server.go template (TEMPLATE_SELECTION_SERVERS) are the allowed
#         callers -- those files are where a host registers its own
#         components and drives the assembly through the engine's Assemble,
#         the shape this ban exists to force. Any other host file still
#         fires.
HOST_COMPOSITION_CALL_BANS = (
    ("pkgcore.NewKernel", r"(?<![A-Za-z0-9_.])pkgcore\.NewKernel\(", "tree", ()),
    (".Bootstrap(", r"\.Bootstrap\(", "tree", ()),
    (
        "pkgcore.NewComponentRegistry",
        r"(?<![A-Za-z0-9_.])pkgcore\.NewComponentRegistry\(",
        "tree",
        ("examples/reference-app/internal/app/server.go",)
        + TEMPLATE_SELECTION_SERVERS,
    ),
    ("a component-registry Prepare call", r"\.Prepare\(", "composition", ()),
    ("a component-registry Construct call", r"\.Construct\(", "composition", ()),
    ("a component-registry Init call", r"\.Init\(", "composition", ()),
    ("a component-registry Verify call", r"\.Verify\(ctx", "composition", ()),
    (
        "dbkit.Open",
        r"(?<![A-Za-z0-9_.])dbkit\.Open\(",
        "tree",
        ("examples/reference-app/internal/app/host_wiring.go",),
    ),
    ("dbkit.NewMigrationRegistry", r"(?<![A-Za-z0-9_.])dbkit\.NewMigrationRegistry\(", "tree", ()),
    (
        "http.NewServeMux",
        r"(?<![A-Za-z0-9_.])http\.NewServeMux\(",
        "composition",
        ("examples/reference-app/internal/app/component.go",)
        + TEMPLATE_SELECTION_SERVERS,
    ),
    (
        "jobs.NewStandaloneQueue",
        r"(?<![A-Za-z0-9_.])jobs\.NewStandaloneQueue\(",
        "tree",
        (),
    ),
    (
        "jobs.Wire",
        r"(?<![A-Za-z0-9_.])jobs\.Wire\(",
        "tree",
        (),
    ),
    (
        "signal.NotifyContext",
        r"(?<![A-Za-z0-9_.])signal\.NotifyContext\(",
        "tree",
        ("examples/reference-app/internal/app/server.go",) + TEMPLATE_SELECTION_SERVERS,
    ),
    ("chain.Chain", r"(?<![A-Za-z0-9_.])[A-Za-z0-9_]*chain\.Chain\(", "tree", ()),
    ("obs.Init", r"(?<![A-Za-z0-9_.])obs\.Init\(", "tree", ()),
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
    sentinel or engine-owned assembly call used, outside the platform
    module."""
    findings: list[str] = []
    patterns = [
        (identifier, declaration_pattern(identifier))
        for identifier in HOST_COMPOSITION_IDENTIFIERS
    ]
    call_bans = [
        (label, re.compile(rx), scope, allowed)
        for label, rx, scope, allowed in HOST_COMPOSITION_CALL_BANS
    ]
    for tree_rel in (TEMPLATE_TREE, APP_TREE):
        for path, rel in go_files(root, tree_rel):
            rel_str = rel.as_posix()
            in_composition = any(
                rel_str.startswith(prefix) for prefix in HOST_COMPOSITION_PATHS
            )
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
                for sentinel, allowed in HOST_COMPOSITION_SENTINELS:
                    if rel_str in allowed:
                        continue
                    if sentinel in line:
                        findings.append(
                            f"{rel_str}:{line_no}: uses {sentinel}, which "
                            "the shared host kernel owns -- the serve "
                            "lifecycle and the liveness/authn paths come "
                            f"from {KERNEL_MODULE}; re-growing them here "
                            "forks the kernel"
                        )
                for label, pattern, scope, allowed in call_bans:
                    if scope == "composition" and not in_composition:
                        continue
                    if rel_str in allowed:
                        continue
                    if pattern.search(line):
                        findings.append(
                            f"{rel_str}:{line_no}: calls {label}, a step "
                            "of the shared assembly the application engine "
                            f"drives ({KERNEL_MODULE}): select the component "
                            "that owns the step in the composition "
                            "configuration, or register the host's own "
                            "behavior as a host component and drive the "
                            "assembly through the engine's "
                            "RunAssembly/Assemble, instead of re-issuing "
                            "the call here"
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
