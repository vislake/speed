#!/usr/bin/env python3
"""Scaffold the canonical stub of a new speed Go module.

docs/internal/19-dev-workflow.md's module-generator section promises
`task new:module` so that adding a module never means hand-repeating the
same skeleton (the doc lists the eight things a new module needs --
go.mod, directory skeleton, AGENTS.md, design doc, migration directory, test
skeleton, CI matrix registration, release-script registration). This script
is the generator behind that task; the Taskfile task itself is wired in a
later round, and --help documents the intended wiring (see the epilog).

What it scaffolds is exactly the canonical stub the not-yet-implemented
modules under go/ already carry (go/sharing, go/notification, go/storage,
...): three files, nothing more:

  go/<name>/go.mod     "module github.com/vislake/speed/go/<name>" plus the
                       stub convention's bare "go 1.23" directive. Real
                       modules with dependencies carry "go 1.25.0" and
                       require/replace blocks instead -- those lines appear
                       when an implementation round adds the first
                       dependency, not in the stub.
  go/<name>/doc.go     The one-line English package doc form every module
                       uses ("// Package sharing provides public share links
                       with expiry and access tracking."). The Go package
                       name is the module name with hyphens removed,
                       matching the repo's go/ai-gateway -> package
                       aigateway precedent. The package doc sentence is the
                       --description argument and must be ASCII (the root
                       CLAUDE.md Language Rule would flag anything else).
  go/<name>/AGENTS.md  The one-liner stub form exactly as written in the
                       existing stubs: "# <name>\n\nNot yet implemented. See
                       docs/internal/XX-*.md for the design." The design-doc
                       pointer is the --design-doc argument; it does not
                       need to exist yet, but the script warns when it does
                       not, because AGENTS.md already points at it.

Refusals and guardrails:

  * An existing directory or file at go/<name> is never overwritten -- the
    run fails instead (exit 2).
  * The module name must be lowercase letters, digits and single hyphens,
    start with a letter, contain no underscore (root CLAUDE.md module
    naming: directory-safe, no underscores anywhere in the tree), and not
    be "." or ".." (validated before any filesystem access).
  * Nothing is ever written outside --target-dir: the scaffold only ever
    creates <target-dir>/go/<name>/{go.mod,doc.go,AGENTS.md}.
  * --category npm is refused with an explanation: docs/internal/
    19-dev-workflow.md names a future "task new:npm-package" alongside the
    Go generator, but no web/ workspace or npm package template exists in
    the repository yet, so there is nothing canonical to scaffold.

After scaffolding, the script prints a registration checklist (go.work use
entry, CI matrix row, lockstep release tag list, roadmap/design-doc rows)
as actionable reminders. It never modifies any of those shared repository
files itself -- that is deliberate: a scaffolder that silently edits go.work
and CI matrices makes review diffs impossible to read, so the checklist is
the contract with the human (or with the future Taskfile task, which can
perform the mechanical registrations on top of this script).

Usage:
    python3 tools/new_module.py NAME --description '...' \
        --design-doc docs/internal/XX-name.md
    python3 tools/new_module.py NAME --description '...' \
        --design-doc docs/internal/XX-name.md --dry-run
    python3 tools/new_module.py NAME --description '...' \
        --design-doc docs/internal/XX-name.md --target-dir /tmp/sandbox

NAME is the module directory name (go/<name>). --target-dir defaults to the
repository root, detected by walking up from the current directory to the
first directory containing go.work (--target-dir overrides the detection,
which is how the script is exercised from a sandbox).

Exit codes: 0 = scaffold created (or --dry-run plan printed); 1 = unused;
2 = bad usage, validation failure, or an I/O error (reported on stderr).

Standard library only; requires Python >= 3.11 (shared floor with the
other tools/ scripts).
"""

from __future__ import annotations

import argparse
import os
import re
import sys

# Module path prefix shared by every Go module in the monorepo
# (go.mod files under go/ all read "module github.com/vislake/speed/go/X").
MODULE_PATH_PREFIX = "github.com/vislake/speed/go"

# The go directive of the canonical stub, byte for byte what every
# not-yet-implemented module's go.mod carries ("go 1.23"). Modules with
# real dependencies use "go 1.25.0" instead; the directive is bumped by the
# implementation round that adds the first dependency, not by the stub.
GO_VERSION_LINE = "go 1.23"

# Where stubs live relative to the repo root (docs/internal/02-repo-and-release.md).
GO_DIR_NAME = "go"

# Marker file that identifies the repository root during --target-dir
# detection (the root is a go.work workspace, not a module).
REPO_MARKER_FILE = "go.work"

# docs/internal/ design docs are numbered "NN-name.md"; the AGENTS.md stub
# line points at one.
DESIGN_DOC_PATTERN = re.compile(r"^docs/internal/\d{2}-[a-z0-9-]+\.md$")

# Module directory names: lowercase letter first, then lowercase letters,
# digits and single hyphens. Underscores are rejected outright -- every
# directory in the tree (go.work use entries, CI matrices, release tag
# paths go/<name>/<version>) is built from this name and none of them want
# underscores; the repo has no underscore-named directory anywhere.
NAME_PATTERN = re.compile(r"^[a-z][a-z0-9]*(-[a-z0-9]+)*$")


def validate_name(name: str) -> str | None:
    """Return an error message for an invalid module name, or None."""
    if not name:
        return "module name is empty"
    if name in (".", ".."):
        return f"module name {name!r} is not a directory name"
    if not NAME_PATTERN.match(name):
        return (
            f"module name {name!r} is not valid: use lowercase letters, "
            f"digits and single hyphens, starting with a letter; no "
            f"underscores (the repo names every directory this way)"
        )
    return None


def package_name_for(module_name: str) -> str:
    """Go package name for the module: hyphens removed (aigateway, ...)."""
    return module_name.replace("-", "")


def build_plan(module_name: str, description: str, design_doc: str) -> list[tuple[str, str]]:
    """Return [(module-relative path, content)] for the stub's files."""
    pkg = package_name_for(module_name)
    files: list[tuple[str, str]] = []

    go_mod = f"module {MODULE_PATH_PREFIX}/{module_name}\n\n{GO_VERSION_LINE}\n"
    files.append(("go.mod", go_mod))

    doc_go = (
        f"// Package {pkg} {description}\n"
        f"package {pkg}\n"
    )
    files.append(("doc.go", doc_go))

    agents_md = (
        f"# {module_name}\n"
        f"\n"
        f"Not yet implemented. See {design_doc} for the design.\n"
    )
    files.append(("AGENTS.md", agents_md))
    return files


def registration_checklist(module_name: str, design_doc: str) -> list[str]:
    """Return the post-scaffold reminder lines (never written anywhere)."""
    lines = [
        "Next steps -- the shared repository files below are intentionally "
        "left untouched by this script:",
        f"  1. go.work (repo root): add \"./{GO_DIR_NAME}/{module_name}\" to the "
        "use ( ... ) block -- the workspace is what makes go build / go test "
        "resolve the new module locally.",
        "  2. CI matrix: register the module in the pr-check/pr-full "
        "workflow matrix (docs/internal/18-cicd.md, reusable-workflow "
        "design section: adding a module is one row in the orchestrating "
        "workflow's matrix list). The .github/ tree is M0 work; until the "
        "workflows exist, this registration happens when they land.",
        "  3. Lockstep release list: add the module to the release "
        "pipeline's per-module tag list (docs/internal/02-repo-and-release."
        "md: each module is tagged go/<module>/<version> and tagging is "
        "scripted -- a module missing from the list never gets tagged and "
        "consumers cannot pin it).",
        "  4. Roadmap and design doc: register the module in the milestone "
        "that plans it (docs/internal/15-roadmap.md) and in the navigation "
        "of docs/internal/00-overview.md / 01-architecture.md (module "
        "dependency graph).",
    ]
    return lines


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="new_module.py",
        description=(
            "Scaffold the canonical stub (go.mod + doc.go + AGENTS.md) of a "
            "new Go module under go/<name>, reproducing the existing stub "
            "modules' exact file shapes. This is the generator behind the "
            "planned 'task new:module' (docs/internal/19-dev-workflow.md's "
            "module-generator section); the Taskfile task itself is wired "
            "in a later round."
        ),
        epilog=(
            "Intended task wiring (docs/internal/19-dev-workflow.md names "
            "task new:module; the Taskfile stanza is added in a later round "
            "-- this epilog documents the contract it will implement):\n"
            "\n"
            "  # Taskfile.yml\n"
            "  new:module:\n"
            "    desc: Scaffold a new Go module stub and print its registration checklist.\n"
            "    cmds:\n"
            "      - python3 tools/new_module.py {{.NAME}} --description '{{.DESCRIPTION}}' \\\n"
            "          --design-doc {{.DESIGN_DOC}}\n"
            "\n"
            "  # invoked as:\n"
            "  #   task new:module NAME=sharing DESCRIPTION='...' DESIGN_DOC=docs/internal/07-....md\n"
            "\n"
            "A future task new:npm-package will call this script with "
            "--category npm once the web/ workspace defines a canonical "
            "package template (refused today, see --category)."
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("name", help="module directory name under go/ "
                        "(lowercase letters, digits, single hyphens; no underscores)")
    parser.add_argument("--description", required=True,
                        help="one-line English package doc for doc.go (single "
                        "sentence; ASCII only -- CJK would fail the repo's "
                        "own scan_cjk.py language check)")
    parser.add_argument("--design-doc", required=True, metavar="FILE",
                        help="repo-relative design doc the AGENTS.md stub "
                        "points at, e.g. docs/internal/07-platform-services.md "
                        "(may not exist yet -- the script warns if missing)")
    parser.add_argument("--category", choices=("go", "npm"), default="go",
                        help="what to scaffold: go (implemented) or npm "
                        "(refused: no npm template exists in the repo yet, "
                        "docs/internal/19-dev-workflow.md only names the "
                        "future task new:npm-package)")
    parser.add_argument("--target-dir", metavar="DIR",
                        help="directory the scaffold is created under "
                        "(default: the repository root, auto-detected as "
                        "the nearest ancestor of the current directory that "
                        "contains go.work); the scaffold always lands at "
                        "<DIR>/go/<name> and nothing is ever written "
                        "outside <DIR>")
    parser.add_argument("--dry-run", action="store_true",
                        help="print the files that would be created without "
                        "writing anything")
    args = parser.parse_args(argv)

    name_error = validate_name(args.name)
    if name_error:
        print(f"error: {name_error}", file=sys.stderr)
        return 2
    if "\n" in args.description or "\r" in args.description:
        print("error: --description must be a single line (it becomes one "
              "doc.go comment line)", file=sys.stderr)
        return 2
    try:
        args.description.encode("ascii")
    except UnicodeEncodeError:
        print("error: --description must be ASCII: the repo's Language Rule "
              "requires English (ASCII) outside docs/internal/, and "
              "tools/scan_cjk.py would flag any CJK or non-ASCII text in "
              "the generated doc.go", file=sys.stderr)
        return 2
    if not DESIGN_DOC_PATTERN.match(args.design_doc):
        print(f"error: --design-doc {args.design_doc!r} is not a docs/internal/"
              "design doc path (expected docs/internal/NN-name.md)",
              file=sys.stderr)
        return 2

    if args.category == "npm":
        print("error: --category npm is not implemented yet: "
              "docs/internal/19-dev-workflow.md names a future 'task "
              "new:npm-package' alongside the Go generator, but no web/ "
              "workspace or canonical npm package template exists in the "
              "repository yet, so there is nothing to scaffold. Re-run with "
              "--category go (default).", file=sys.stderr)
        return 2

    if args.target_dir:
        target_dir = os.path.abspath(args.target_dir)
    else:
        # Default: repository root, detected by the go.work marker.
        target_dir = os.getcwd()
        while True:
            if os.path.isfile(os.path.join(target_dir, REPO_MARKER_FILE)):
                break
            parent = os.path.dirname(target_dir)
            if parent == target_dir:
                print("error: cannot find the repository root (a directory "
                      f"containing {REPO_MARKER_FILE}) from the current "
                      "directory; run from the repo or pass --target-dir "
                      "explicitly", file=sys.stderr)
                return 2
            target_dir = parent
    if not os.path.isdir(target_dir):
        print(f"error: --target-dir is not a directory: {target_dir}",
              file=sys.stderr)
        return 2

    module_dir = os.path.join(target_dir, GO_DIR_NAME, args.name)
    plan = build_plan(args.name, args.description, args.design_doc)
    module_rel = os.path.join(GO_DIR_NAME, args.name)

    # Overwrite refusal, checked before anything is created.
    if os.path.lexists(module_dir):
        print(f"error: refusing to overwrite existing directory: "
              f"{module_dir}", file=sys.stderr)
        return 2
    for rel, _ in plan:
        full = os.path.join(module_dir, rel)
        if os.path.lexists(full):
            print(f"error: refusing to overwrite existing file: {full}",
                  file=sys.stderr)
            return 2

    if args.dry_run:
        print(f"dry run: would scaffold {module_rel} under {target_dir}")
        for rel, _ in plan:
            print(f"  create {os.path.join(module_rel, rel)}")
        print("(nothing written -- --dry-run)")
        return 0

    try:
        os.makedirs(module_dir)
    except OSError as exc:
        print(f"error: cannot create {module_dir}: {exc}", file=sys.stderr)
        return 2
    for rel, content in plan:
        full = os.path.join(module_dir, rel)
        try:
            with open(full, "x", encoding="ascii") as fh:
                fh.write(content)
        except OSError as exc:
            print(f"error: cannot write {full}: {exc}", file=sys.stderr)
            return 2

    print(f"scaffolded {module_rel} under {target_dir}:")
    for rel, _ in plan:
        print(f"  created {os.path.join(module_rel, rel)}")
    if not os.path.isfile(os.path.join(target_dir, args.design_doc)):
        print(f"warning: {args.design_doc} does not exist yet -- the "
              "AGENTS.md stub already points at it; write the design doc "
              "and commit it in the same PR as this module")
    print()
    for line in registration_checklist(args.name, args.design_doc):
        print(line)
    return 0


if __name__ == "__main__":
    sys.exit(main())
