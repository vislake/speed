#!/usr/bin/env python3
"""Coverage-baseline gate for the foundation modules.

docs/internal/20-quality-and-security.md's coverage section draws the
distinction this script enforces: no uniform percentage threshold (it
"breeds meaningless tests"), but for the FOUNDATION modules -- the doc
names pkgcore, dbkit, tenancy, rbac, billing, jobs -- "coverage must
not decline (compared against a baseline)". Until now that sentence was
design intent only: no workflow collected coverage, stored a baseline or
compared anything, and the doc's own implementation-status note said so.
This script is the minimal genuine mechanism behind it: collect (run
`go test -coverprofile` and compute the module's exact total statement
coverage), store (tools/coverage-baselines.json, committed, regenerated
by --update), compare (fail when the measured total drops below the
stored baseline).

The gate scope is exactly the six foundation modules the doc names. The
FOUNDATION_MODULES list below is that sentence's mirror: adding a
foundation module means editing the doc and this list together, and
--selfcheck refuses a baseline file that names anything else, so a
stale row (module renamed or dropped from the set) cannot rot silently
into a row nobody measures.

Why statement coverage of the module's whole unit suite, compared per
module

  * The doc's rule is per foundation module and baseline-relative, so
    the measured quantity is one comparable number per module: total
    covered statements / total statements over the module's own unit
    suite (`go test -coverprofile ... ./...` from the module
    directory), parsed exactly from the profile -- never the rounded
    `go tool cover -func` total, whose 0.1-point rounding would blind
    the gate to every decline smaller than a tenth of a point.
  * The unit suite only: the Docker-backed integration tiers
    (-tags=integration) are not part of a coverage gate on every PR.
  * The comparison carries a small tolerance, EPSILON_PP below: the
    statement census is fixed for a fixed tree, but which blocks a
    suite executes is not perfectly stable under machine load (a
    timing-sensitive test reaches a different block set when the box is
    busy -- observed run-to-run swings of up to ~0.03 percentage
    points in this repository's own foundation suites). A measured
    total at least EPSILON_PP below the baseline fails; anything within
    the tolerance band passes. EPSILON_PP is sized one notch above the
    observed jitter: 0.05 points is a few dozen statements in these
    modules' suite sizes, far below any decline that adding code
    without tests produces, while keeping the gate stable on a loaded
    runner. The recorded TOOLCHAIN (the short `go version` form) is
    part of the baseline: a check run under a different Go than the
    baselines were recorded with fails with an update instruction -- a
    Go upgrade legitimately moves coverage totals, and the honest
    response is re-measuring, not guessing.

What a decline means and how to respond

  * A PR that adds code without tests drops the total and fails,
    naming the module, the measured total and the baseline -- the
    intended signal ("write the tests").
  * A change that deliberately reduces coverage (removing a test whose
    scenario is obsolete, restructuring) fails the same way and must
    say so: re-run with --update to record the new baseline in the
    same change that lowered it, with the reason in the commit
    message. The baseline file is a committed artifact, so a decline
    never disappears silently -- it is either prevented (tests added)
    or explicitly recorded (--update with justification).

Modes

  --check (default): for --module X (or every gated module with
    --all), measure and compare against the stored baseline. A module
    outside the gated set prints a notice and passes: the gate scope
    is the doc's six foundation modules, and anything else is outside
    the rule this script enforces.
  --update [--module X | --all]: measure and rewrite the baseline file.
    Updates only ever touch gated modules' rows; the file is written
    atomically and its rows sorted. The module's suite must be green
    for the measurement to exist at all (go test fails otherwise).
  --selfcheck: verify the baseline file itself -- every row names a
    gated module whose directory exists and carries a go.mod, and
    every gated module has a row. Runs in repo-checks; a dead or
    missing row goes red before any comparison can silently skip.

Usage:
    python3 tools/check_coverage_baseline.py --check --module go/pkgcore
    python3 tools/check_coverage_baseline.py --update --all
    python3 tools/check_coverage_baseline.py --selfcheck

Exit codes: 0 = clean / nothing to do; 1 = a comparison failed, a
baseline is stale or missing, or the toolchain moved; 2 = usage error.
Standard library only, Python >= 3.11. Needs a Go toolchain on PATH.
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import re
import subprocess
import sys
import tempfile

# The doc's foundation set, mirrored from docs/internal/
# 20-quality-and-security.md's coverage section ("pkgcore, dbkit,
# tenancy, rbac, billing, jobs"). Repo-root-relative module directories,
# go.work form. Edit the doc and this list together.
FOUNDATION_MODULES = [
    "go/pkgcore",
    "go/dbkit",
    "go/tenancy",
    "go/rbac",
    "go/billing",
    "go/jobs",
]

BASELINE_FILE_NAME = "coverage-baselines.json"
DEFAULT_BASELINE = pathlib.Path(__file__).resolve().parent / BASELINE_FILE_NAME

# The tolerance band for a decline, in percentage points. A measured
# total at least this far below the recorded baseline fails; a smaller
# dip is measurement jitter (timing-sensitive tests reach different
# block sets under load; observed swings up to ~0.03 points in this
# repository's own foundation suites). See the module docstring.
EPSILON_PP = 0.05

# The profile format line for one function block:
#   go/pkgcore/kernel.go:12.17,18.2 3 2
# path:startline.startcol,endline.endcol <numstmt> <count>
PROFILE_BLOCK = re.compile(
    r"^([^:]+):\d+\.\d+,\d+\.\d+\s+(\d+)\s+(\d+)\s*$"
)

TOOLCHAIN_RE = re.compile(r"^go version go(\S+)")


def short_toolchain(go_version_output: str) -> str:
    """The short toolchain form recorded beside each baseline
    ('go1.26.8' from 'go version go1.26.8 darwin/arm64')."""
    match = TOOLCHAIN_RE.match(go_version_output.strip())
    if not match:
        raise ValueError(
            "unrecognized go version output: %r" % go_version_output
        )
    return "go" + match.group(1)


def coverage_from_profile(profile_text: str) -> float:
    """Total statement coverage percent from a go test -coverprofile
    file, computed exactly: covered statements (count > 0) over all
    statements across every block the profile carries. Unrounded, so a
    comparison against a stored baseline is not quantized by the
    0.1-point rounding `go tool cover -func` applies to its output."""
    total = 0
    covered = 0
    for line in profile_text.splitlines():
        match = PROFILE_BLOCK.match(line)
        if match is None:
            continue
        statements = int(match.group(2))
        count = int(match.group(3))
        total += statements
        if count > 0:
            covered += statements
    if total == 0:
        return 100.0
    return 100.0 * covered / total


def measure_module_coverage(root: pathlib.Path, module_dir: str) -> float:
    """Run the module's unit suite under -coverprofile and return its
    exact total statement coverage percent. The profile is written to a
    temp file inside the module directory (go test names profile paths
    relative to its working directory, but the parse only needs the
    statement census, so an absolute temp path works too) and removed
    afterwards. A failing suite fails the measurement -- a baseline can
    only ever be recorded against a green tree."""
    module_path = root / module_dir
    if not (module_path / "go.mod").is_file():
        raise FileNotFoundError(
            "module %s has no go.mod at %s" % (module_dir, module_path)
        )
    fd, profile = tempfile.mkstemp(
        prefix="speed-cov-", suffix=".out", dir=str(module_path)
    )
    os.close(fd)
    try:
        proc = subprocess.run(
            ["go", "test", "-coverprofile=%s" % profile, "./..."],
            cwd=module_path,
            capture_output=True,
            text=True,
            check=False,
        )
        if proc.returncode != 0:
            raise RuntimeError(
                "go test -coverprofile failed in %s (exit %d):\n%s"
                % (module_dir, proc.returncode, proc.stderr[-2000:])
            )
        profile_text = pathlib.Path(profile).read_text(encoding="utf-8")
    finally:
        pathlib.Path(profile).unlink(missing_ok=True)
    return coverage_from_profile(profile_text)


def load_baselines(path: pathlib.Path) -> dict:
    if not path.is_file():
        return {}
    return json.loads(path.read_text(encoding="utf-8"))


def save_baselines(path: pathlib.Path, data: dict) -> None:
    text = json.dumps(
        data, indent=2, sort_keys=True
    ) + "\n"
    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text(text, encoding="utf-8")
    tmp.replace(path)


def current_toolchain() -> str:
    proc = subprocess.run(
        ["go", "version"], capture_output=True, text=True, check=False
    )
    if proc.returncode != 0:
        raise RuntimeError("go version failed: %s" % proc.stderr)
    return short_toolchain(proc.stdout)


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument(
        "--check",
        action="store_true",
        default=True,
        help="measure and compare against the stored baseline (default)",
    )
    mode.add_argument(
        "--update", action="store_true", help="measure and record baselines"
    )
    mode.add_argument(
        "--selfcheck",
        action="store_true",
        help="verify the baseline file's rows against the gated set",
    )
    parser.add_argument(
        "--module",
        default=None,
        help="module directory (go/pkgcore); --all selects the gated set",
    )
    parser.add_argument(
        "--all", action="store_true", help="act on every gated module"
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
    baseline_path = DEFAULT_BASELINE
    if args.root:
        baseline_path = root / "tools" / BASELINE_FILE_NAME

    if args.selfcheck:
        data = load_baselines(baseline_path)
        rows = data.get("baselines", {}) if isinstance(data, dict) else {}
        toolchain = data.get("toolchain") if isinstance(data, dict) else None
        problems: list[str] = []
        gated = set(FOUNDATION_MODULES)
        for module_dir in sorted(rows):
            if module_dir not in gated:
                problems.append(
                    "baseline row %s is not a foundation module (the doc-"
                    "mirrored gated set names %s)"
                    % (module_dir, ", ".join(sorted(gated)))
                )
            elif not (root / module_dir / "go.mod").is_file():
                problems.append(
                    "baseline row %s has no go.mod at %s"
                    % (module_dir, root / module_dir)
                )
        for module_dir in FOUNDATION_MODULES:
            if module_dir not in rows:
                problems.append(
                    "foundation module %s has no baseline row -- record one "
                    "with --update" % module_dir
                )
        if toolchain != current_toolchain():
            problems.append(
                "baselines recorded under %r, this toolchain is %r -- "
                "re-record with --update"
                % (toolchain, current_toolchain())
            )
        if problems:
            for problem in problems:
                print("check_coverage_baseline: %s" % problem, file=sys.stderr)
            return 1
        print("coverage-baselines.json: rows match the foundation set")
        return 0

    selected: list[str]
    if args.module is not None:
        if args.all:
            print("--module and --all are mutually exclusive", file=sys.stderr)
            return 2
        selected = [args.module]
    elif args.all:
        selected = list(FOUNDATION_MODULES)
    else:
        print("one of --module X or --all is required", file=sys.stderr)
        return 2

    toolchain = current_toolchain()
    data = load_baselines(baseline_path)
    rows = data.get("baselines", {}) if isinstance(data, dict) else {}
    recorded_toolchain = (
        data.get("toolchain") if isinstance(data, dict) else None
    )

    if args.update:
        changed = False
        for module_dir in selected:
            if module_dir not in FOUNDATION_MODULES:
                print(
                    "update refuses %s: the baseline gate covers the "
                    "foundation modules only (%s)"
                    % (module_dir, ", ".join(FOUNDATION_MODULES)),
                    file=sys.stderr,
                )
                return 1
            total = measure_module_coverage(root, module_dir)
            rows[module_dir] = round(total, 6)
            changed = True
            print("recorded %s: %.4f%%" % (module_dir, total))
        if changed:
            save_baselines(
                baseline_path,
                {"toolchain": toolchain, "baselines": rows},
            )
        return 0

    # --check (default)
    failures = 0
    for module_dir in selected:
        if module_dir not in FOUNDATION_MODULES:
            print(
                "%s: outside the baseline gate (docs/internal/20 "
                "foundation modules: %s) -- nothing to compare"
                % (module_dir, ", ".join(FOUNDATION_MODULES))
            )
            continue
        if module_dir not in rows:
            print(
                "check_coverage_baseline: %s has no baseline row -- record "
                "one with --update" % module_dir,
                file=sys.stderr,
            )
            failures += 1
            continue
        if recorded_toolchain != toolchain:
            print(
                "check_coverage_baseline: %s baseline recorded under %r, "
                "measured under %r -- re-record with --update before "
                "comparing" % (module_dir, recorded_toolchain, toolchain),
                file=sys.stderr,
            )
            failures += 1
            continue
        measured = measure_module_coverage(root, module_dir)
        baseline = rows[module_dir]
        if measured + EPSILON_PP < baseline:
            print(
                "check_coverage_baseline: FAIL %s: measured %.4f%% is more "
                "than the %.2f-point tolerance below the recorded baseline "
                "%.4f%% -- add tests, or record a deliberate decline with "
                "--update (with the reason in the commit message)"
                % (module_dir, measured, EPSILON_PP, baseline),
                file=sys.stderr,
            )
            failures += 1
        else:
            print(
                "%s: %.4f%% (baseline %.4f%%)"
                % (module_dir, measured, baseline)
            )
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
