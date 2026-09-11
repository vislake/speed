#!/usr/bin/env python3
"""Integration-tier enumeration drift gate for full-check.yml and Taskfile.yml.

The repository runs its Docker-backed integration tiers (the
`-tags=integration` test suites) from two hand-written copies of the same
module set: full-check.yml's integration-tiers job matrix, one row per
module (`go test -race -tags=integration ./...` run from the module
directory), and Taskfile.yml's INTEGRATION_DIRS loop, the same command
shape for the local `task test:full`. The set itself is a property of the
tree -- the go/ modules whose directories carry a Go file with a
`//go:build` constraint naming the integration tag -- and this gate
derives it there and proves both copies against it, so a module that
ships a tier is a matrix row (and a Taskfile entry) the moment its
tagged test lands, and a row for a module without a tier cannot keep
running a vacuous leg.

The derived set: every go.work use entry under go/, in go.work file
order, whose directory tree carries at least one .go file with a
`//go:build` constraint line (column zero, before the package clause,
where Go recognizes constraints) naming the integration tag. A mention
of the phrase in prose is not a tier, and a negated constraint
(`//go:build !integration`) is not one either. The go.work use block is
read with the release coordinator's own parser
(tools/release/lockstep-release.py), so there is one copy of the parse
logic.

Declared exclusions: go/saasctl carries an integration-tier file but is
deliberately not a matrix row -- its tier materializes and boots a whole
generated project rather than exercising the module's own code against a
database, and it runs through scaffold-verify.yml's own
schedule/dispatch (the placement go/saasctl/AGENTS.md records). An
exclusion whose module no longer carries a tier fails (remove the
declaration), and one that also appears in either copy fails (the
placement changed -- update the declaration or the copy), so the
declaration cannot go stale in either direction.

Checked invariants (each goes red with an actionable message):

  * a derived module (exclusions applied) missing from full-check.yml's
    integration-tiers matrix or from Taskfile.yml's INTEGRATION_DIRS;
  * a matrix row or a Taskfile entry naming a module that is not a
    derived module -- a module without a tier (whose leg would be
    vacuous) or a declared exclusion;
  * a copy naming the same module twice.

Infrastructure errors (exit 2, never a silent skip): go.work, the
release coordinator's parser, full-check.yml or Taskfile.yml missing or
unreadable, or a full-check.yml / Taskfile.yml in a shape this gate's
reader does not recognize -- the reader is deliberately strict and names
the update when the shape moves.

Exit codes: 0 = both copies match the tree; 1 = drift; 2 = infrastructure
error. Companion planted-drift suite:
tools/test_check_integration_tiers.py.
"""

from __future__ import annotations

import argparse
import importlib.util
import os
import re
import sys

GO_WORK_REL = "go.work"
WORKFLOW_REL = os.path.join(".github", "workflows", "full-check.yml")
TASKFILE_REL = "Taskfile.yml"
PARSER_REL = os.path.join("tools", "release", "lockstep-release.py")
GATE_REL = os.path.join("tools", "check_integration_tiers.py")

# Directory trees the tier scan never enters: VCS metadata, nested
# worktree checkouts and caches, and vendored or generated dependencies --
# none of them shipped module source.
SKIP_DIRS = {".git", ".claude", "node_modules", "vendor", "__pycache__", ".venv"}

# A Go build-constraint line: exactly `//go:build` at column zero, where
# Go recognizes the constraint (before the package clause).
BUILD_CONSTRAINT = re.compile(r"^//go:build\b")

# The integration tag inside a constraint expression, as a whole word. A
# negated `!integration` has no word boundary before the tag and does not
# match, so a module gated OFF under the integration tag is not a tier.
INTEGRATION_TAG = re.compile(r"(?:^|\s)integration(?:\s|$)")

# A go/ module directory: one path segment under go/ (the form every
# consumer of this set spells its rows and entries in).
GO_MODULE = re.compile(r"^go/[a-z0-9][a-z0-9-]*$")

# The workflow anchors: the integration-tiers job key (2-space indent,
# under jobs:), its strategy matrix block, and the module list inside it.
JOB_LINE = "  integration-tiers:"
OTHER_JOB_KEY = re.compile(r"^  [A-Za-z0-9_-]+:")
MATRIX_LINE = "      matrix:"
MATRIX_MODULE_LINE = "        module:"
ITEM_LINE = re.compile(r"^          - (.+)$")

# The Taskfile variable that mirrors the matrix.
TASKFILE_VAR = re.compile(r"^  INTEGRATION_DIRS:\s*(.*)$")

# Declared exclusions from the integration-tiers matrix, each with the
# current-state reason it is not a row. Both directions are checked
# (stale exclusion, exclusion present in a copy), so this declaration
# stays honest.
EXCLUDED = {
    "go/saasctl": (
        "its tier materializes and boots a whole generated project "
        "instead of exercising this module's own code against a "
        "database, and runs through scaffold-verify.yml's own "
        "schedule/dispatch -- the placement go/saasctl/AGENTS.md records"
    ),
}


def _infra(message: str) -> None:
    """Report an infrastructure failure and exit 2.

    The exit-code contract (module docstring): 2 = infrastructure error
    (a file this gate reads is missing, unreadable, or in a shape the
    reader does not understand), and only 1 = drift. SystemExit alone
    cannot carry the distinction -- sys.exit("message") exits 1 -- so
    the message is printed to stderr first and the bare code raised.
    """
    print(message, file=sys.stderr)
    raise SystemExit(2)


def _load_gowork_parser(root: str):
    """The release coordinator's go.work parser, loaded from the tree.

    tools/release/lockstep-release.py is the repository's canonical
    reader of the go.work use block (the reusable module-set derivation
    and the coverage gate load the same file), so the derivation here
    shares its one copy of the parse logic."""
    path = os.path.join(root, PARSER_REL)
    if not os.path.isfile(path):
        _infra(
            f"integration-tiers: {PARSER_REL} is missing under {root!r} "
            f"-- the tier derivation reads the go.work use block with the "
            f"release coordinator's parser"
        )
    spec = importlib.util.spec_from_file_location("lockstep_release", path)
    module = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(module)
    except Exception as exc:  # noqa: BLE001 -- any load failure is infrastructure
        _infra(f"integration-tiers: cannot load {PARSER_REL}: {exc}")
    return module.parse_gowork_uses


def _carries_integration_tier(root: str, module_dir: str) -> bool:
    """True when the module's tree carries an integration build constraint.

    A Go file in the module's directory tree whose pre-package region
    carries a `//go:build` line naming the integration tag. The scan
    stops reading a file at its package clause (Go recognizes
    constraints only before it), so the phrase in prose later in a file
    is not a tier. Non-UTF-8 and unreadable files are skipped -- a
    missing tier from an unreadable file surfaces as drift at the
    copies, never as a silent pass for a module whose reader failed."""
    base = os.path.join(root, module_dir)
    for dirpath, dirnames, filenames in os.walk(base):
        dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS)
        for name in sorted(filenames):
            if not name.endswith(".go"):
                continue
            try:
                with open(os.path.join(dirpath, name), encoding="utf-8") as fh:
                    for line in fh:
                        if line.startswith("package "):
                            break
                        if BUILD_CONSTRAINT.match(line) and (
                            INTEGRATION_TAG.search(line[len("//go:build"):])
                        ):
                            return True
            except (OSError, UnicodeDecodeError):
                continue
    return False


def derive_tiers(root: str) -> list[str]:
    """The integration-tier module set, derived from the tree.

    Every go.work use entry under go/ whose directory carries an
    integration build constraint, in go.work file order. Fails closed
    (exit 2) when go.work is missing or unreadable and when the
    derivation yields nothing -- an empty set means the scan or the tree
    broke, not that the repository ships no tiers."""
    gowork = os.path.join(root, GO_WORK_REL)
    if not os.path.isfile(gowork):
        _infra(
            f"integration-tiers: {GO_WORK_REL} is missing under {root!r} "
            f"-- it is the membership source the tier set derives from"
        )
    parse = _load_gowork_parser(root)
    try:
        with open(gowork, encoding="utf-8") as fh:
            entries = parse(fh.read())
    except Exception as exc:  # noqa: BLE001 -- ReleaseError/OSError: infrastructure
        _infra(f"integration-tiers: cannot derive the module set from "
               f"{GO_WORK_REL}: {exc}")
    tiers = [
        entry
        for entry in entries
        if GO_MODULE.match(entry) and _carries_integration_tier(root, entry)
    ]
    if not tiers:
        _infra(
            f"integration-tiers: {GO_WORK_REL} yielded no go/ module "
            f"carrying an integration build constraint -- the derivation "
            f"cannot be trusted to be complete"
        )
    return tiers


def _read_workflow_rows(root: str) -> list[str]:
    """The integration-tiers job's matrix rows from full-check.yml.

    Anchored on the job key, then the strategy matrix block, then its
    module list. Any shape that does not match the anchors fails as an
    infrastructure error naming the reader update, so a restructured
    workflow is never silently read as a shorter or longer list."""
    path = os.path.join(root, WORKFLOW_REL)
    if not os.path.isfile(path):
        _infra(f"integration-tiers: {WORKFLOW_REL} is missing under {root!r}")
    with open(path, encoding="utf-8") as fh:
        lines = fh.read().splitlines()

    job_at = next(
        (i for i, line in enumerate(lines) if line.rstrip() == JOB_LINE), -1
    )
    if job_at < 0:
        _infra(
            f"integration-tiers: {WORKFLOW_REL} carries no "
            f"{JOB_LINE.strip()!r} job -- update this gate's reader for "
            f"the new shape"
        )

    def _left_job(line: str) -> bool:
        return OTHER_JOB_KEY.match(line) is not None or (
            line.strip() and not line.startswith(" ")
        )

    matrix_at = -1
    for i in range(job_at + 1, len(lines)):
        if _left_job(lines[i]):
            break
        if lines[i].rstrip() == MATRIX_LINE:
            matrix_at = i
            break
    if matrix_at < 0:
        _infra(
            f"integration-tiers: {WORKFLOW_REL}: job "
            f"{JOB_LINE.strip()!r} carries no {MATRIX_LINE.strip()!r} "
            f"block before the next job -- update this gate's reader"
        )

    module_at = -1
    for i in range(matrix_at + 1, len(lines)):
        line = lines[i]
        if line.strip() and len(line) - len(line.lstrip(" ")) <= 6:
            break
        if line.rstrip() == MATRIX_MODULE_LINE:
            module_at = i
            break
    if module_at < 0:
        _infra(
            f"integration-tiers: {WORKFLOW_REL}: the integration-tiers "
            f"matrix carries no {MATRIX_MODULE_LINE.strip()!r} list -- "
            f"update this gate's reader"
        )

    rows: list[str] = []
    for i in range(module_at + 1, len(lines)):
        match = ITEM_LINE.match(lines[i])
        if match is None:
            break
        rows.append(match.group(1).split(" #", 1)[0].strip())
    # An anchored but empty list is an empty copy, not a shape error:
    # check() then reports every derived module as missing from it.
    for row in rows:
        if not GO_MODULE.match(row):
            _infra(
                f"integration-tiers: {WORKFLOW_REL}: matrix row {row!r} "
                f"is not a go/ module directory -- update this gate's "
                f"reader"
            )
    return rows


def _read_taskfile_dirs(root: str) -> list[str]:
    """The INTEGRATION_DIRS variable from Taskfile.yml (space-separated)."""
    path = os.path.join(root, TASKFILE_REL)
    if not os.path.isfile(path):
        _infra(f"integration-tiers: {TASKFILE_REL} is missing under {root!r}")
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            match = TASKFILE_VAR.match(line.rstrip("\n"))
            if match is None:
                continue
            dirs = match.group(1).split(" #", 1)[0].split()
            # An anchored but empty value is an empty copy, not a shape
            # error: check() then reports every derived module as
            # missing from it.
            for entry in dirs:
                if not GO_MODULE.match(entry):
                    _infra(
                        f"integration-tiers: {TASKFILE_REL}: "
                        f"INTEGRATION_DIRS entry {entry!r} is not a go/ "
                        f"module directory -- update this gate's reader"
                    )
            return dirs
    _infra(
        f"integration-tiers: {TASKFILE_REL} carries no INTEGRATION_DIRS "
        f"variable -- update this gate's reader for the new shape"
    )


def check(root: str) -> tuple[list[str], list[str], list[str], list[str]]:
    """Prove both copies against the tree.

    Returns (problems, tiers, workflow_rows, taskfile_dirs); problems is
    empty when both copies name exactly the derived modules outside the
    declared exclusions."""
    tiers = derive_tiers(root)
    rows = _read_workflow_rows(root)
    dirs = _read_taskfile_dirs(root)
    expected = [tier for tier in tiers if tier not in EXCLUDED]

    problems: list[str] = []
    for module in expected:
        if module not in rows:
            problems.append(
                f"{module}: carries an integration tier but has no row in "
                f"{WORKFLOW_REL}'s integration-tiers matrix -- add one "
                f"(the row runs `go test -race -tags=integration ./...` "
                f"from the module)"
            )
        if module not in dirs:
            problems.append(
                f"{module}: carries an integration tier but is missing "
                f"from {TASKFILE_REL}'s INTEGRATION_DIRS -- add it so "
                f"task test:full runs the tier too"
            )
    for copy_name, entries in (
        (f"{WORKFLOW_REL}'s integration-tiers matrix", rows),
        (f"{TASKFILE_REL}'s INTEGRATION_DIRS", dirs),
    ):
        seen: set[str] = set()
        for entry in entries:
            if entry in seen:
                problems.append(f"{copy_name}: names {entry} twice")
                continue
            seen.add(entry)
            if entry in EXCLUDED:
                problems.append(
                    f"{copy_name}: names {entry}, which is a declared "
                    f"exclusion of this gate ({EXCLUDED[entry]}) -- if "
                    f"that placement changed, update the exclusion's "
                    f"declaration in {GATE_REL}"
                )
            elif entry not in expected:
                problems.append(
                    f"{copy_name}: names {entry}, which carries no "
                    f"integration tier in the tree (no .go file under it "
                    f"has a //go:build constraint naming the integration "
                    f"tag) -- remove it, or restore the module's tier if "
                    f"it was lost"
                )
    for excluded, reason in sorted(EXCLUDED.items()):
        if excluded not in tiers:
            problems.append(
                f"{excluded}: declared as an exclusion from the "
                f"integration-tiers matrix ({reason}), but it no longer "
                f"carries an integration tier in the tree -- remove the "
                f"exclusion from {GATE_REL}"
            )
    return problems, tiers, rows, dirs


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Integration-tier enumeration drift gate: full-check.yml's "
            "integration-tiers matrix and Taskfile.yml's INTEGRATION_DIRS "
            "are two hand-written copies of the go/ modules carrying an "
            "integration build constraint; this gate derives the set from "
            "the tree and fails when either copy disagrees. Exit 0 clean, "
            "1 drift, 2 infrastructure error."
        )
    )
    parser.add_argument(
        "--root",
        default=".",
        help="repository root (default: the current directory)",
    )
    args = parser.parse_args(argv)
    problems, tiers, rows, dirs = check(args.root)
    if problems:
        for problem in problems:
            print(f"integration-tiers: drift    {problem}")
        print(
            "integration-tiers: the tier set is derived from the tree -- "
            "every go/ module with a //go:build integration constraint in "
            f"{GO_WORK_REL} order -- and both copies follow it; see "
            f"{GATE_REL} for the checked invariants"
        )
        return 1
    excluded = ", ".join(sorted(EXCLUDED)) or "none"
    print(
        "integration-tiers: ok    "
        f"{len(rows)} matrix rows in {WORKFLOW_REL} and {len(dirs)} "
        f"INTEGRATION_DIRS entries in {TASKFILE_REL} match the "
        f"{len(tiers)} derived tier modules (excluded: {excluded})"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
