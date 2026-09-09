#!/usr/bin/env python3
"""Coverage gate for every released module.

Every released Go module -- the 22 go.work entries: the 21 go/* modules
plus the reference app -- must hold its unit-suite statement coverage
at or above an 80% floor and must not let it decline against a recorded
baseline. This script is the mechanism behind that rule: collect (run
`go test -coverprofile` and compute the module's exact total statement
coverage), store (tools/coverage-baselines.json, committed, regenerated
by --update), compare (fail when the measured total breaches either
bound), and self-check the baseline file's own health (--selfcheck).

History of the rule. docs/internal/20-quality-and-security.md once drew
the no-uniform-threshold line (a percentage target "breeds meaningless
tests") and asked only that the six foundation modules -- pkgcore,
dbkit, tenancy, rbac, billing, jobs -- keep "coverage must not decline
(against a baseline)". This script shipped exactly that decline
mechanism for those six. The product decision changed in 2026-09: every
released module must hold at least 80% statement coverage, measured
excluding generated files. The no-uniform-threshold argument is
superseded for the lower bound by that decision (docs/internal/20
records the change); the decline gate survives unchanged, so coverage
that merely slides within a high-but-falling band still fails, and a
deliberate re-baseline stays possible above the floor.

Generated files. The quantity the gate compares excludes statements
whose profile path contains ".gen.go" -- the committed oapi-codegen
output every spec-carrying module and the reference app ship. Generated
code is written by a pinned generator, not by a human, so its statements
are not a meaningful test target: including them would let a generator
change (or a spec growing more error paths) move the measured total
independently of the module's hand-written code. The exclusion applies
at both ends -- --update stores exclusion-rule numbers and --check
compares them -- so the baseline file was re-recorded in the same change
that defined the rule: baseline records travel with the change that
defines the quantity.

Why statement coverage of the module's whole unit suite, compared per
module

  * The rule is per module and both floor- and baseline-relative, so
    the measured quantity is one comparable number per module: total
    covered statements / total statements over the module's own unit
    suite (`go test -coverprofile ... ./...` from the module
    directory), parsed exactly from the profile -- never the rounded
    `go tool cover -func` total, whose 0.1-point rounding would blind
    the gate to every change smaller than a tenth of a point.
  * The unit suite only: the Docker-backed integration tiers
    (-tags=integration) are not part of a coverage gate on every run.
  * The comparison carries a small tolerance, EPSILON_PP, applied
    identically to both bounds: a measured total more than EPSILON_PP
    below the 80.0% floor fails, and one more than EPSILON_PP below
    the recorded baseline fails. The statement census is fixed for a
    fixed tree, but which blocks a suite executes is not perfectly
    stable under machine load (a timing-sensitive test reaches a
    different block set when the box is busy -- observed run-to-run
    swings of up to ~0.03 percentage points in this repository's own
    suites). EPSILON_PP is sized one notch above the observed jitter:
    0.05 points is a few dozen statements in these modules' suite
    sizes, far below any decline that adding code without tests
    produces, while keeping the gate stable on a loaded runner. One
    tolerance, two comparisons, one rule. The recorded TOOLCHAIN (the
    short `go version` form) is part of the baseline: a check run under
    a different Go than the baselines were recorded with fails with an
    update instruction -- a Go upgrade legitimately moves coverage
    totals, and the honest response is re-measuring, not guessing.

What a breach means and how to respond

  * A PR that adds code without tests drops the total and fails one or
    both comparisons, naming the module, the measured total and the
    breached bound -- the intended signal ("write the tests").
  * A change that deliberately lowers coverage (removing a test whose
    scenario is obsolete, restructuring) fails the decline comparison
    and must say so: re-run with --update to record the new baseline in
    the same change that lowered it, with the reason in the commit
    message. --update cannot record a total that breaches the floor,
    though: the floor is the product decision, not a baseline, so a
    sub-floor measurement fails the floor comparison no matter what the
    baseline file says, and the honest response is adding tests (or
    revisiting the decision, which means editing LOWER_BOUND_PP and
    docs/internal/20 together). The baseline file is a committed
    artifact, so a decline never disappears silently.

Modes

  --check (default): for --module X (or every gated module with
    --all), measure and compare against both bounds. A module outside
    the gated set prints a notice and passes: the gate scope is the
    released modules, and a CI row that names anything else is not a
    module this script has a rule for.
  --update [--module X | --all]: measure and rewrite the baseline file.
    Updates only ever touch gated modules' rows; the file is written
    atomically and its rows sorted. The module's suite must be green
    for the measurement to exist at all (go test fails otherwise), and
    a sub-floor measured total refuses to be recorded (above).
  --selfcheck: verify the baseline file itself -- every row names a
    gated module whose directory exists and carries a go.mod, and
    every gated module has a row. Runs in repo-checks; a dead or
    missing row goes red before any comparison can silently skip.

Usage:
    python3 tools/check_coverage_baseline.py --check --module go/pkgcore
    python3 tools/check_coverage_baseline.py --check --all
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

# The released modules, mirrored from go.work's use list and from
# docs/internal/20-quality-and-security.md's coverage section (which
# named six foundation modules -- pkgcore, dbkit, tenancy, rbac, billing,
# jobs -- until the 2026-09 decision extended the gate to every module).
# Repo-root-relative module directories. Adding a module means editing
# go.work, this list and the doc together; --selfcheck refuses a
# baseline file that names anything else.
GATED_MODULES = [
    "examples/reference-app",
    "go/admin",
    "go/ai-gateway",
    "go/authn",
    "go/billing",
    "go/compliance",
    "go/config",
    "go/dbkit",
    "go/integration",
    "go/jobs",
    "go/metering",
    "go/notification",
    "go/observability",
    "go/org",
    "go/pkgcore",
    "go/pki",
    "go/ratelimit",
    "go/rbac",
    "go/saasctl",
    "go/sharing",
    "go/storage",
    "go/tenancy",
]

BASELINE_FILE_NAME = "coverage-baselines.json"
DEFAULT_BASELINE = pathlib.Path(__file__).resolve().parent / BASELINE_FILE_NAME

# The floor every module's measured total must clear, in percentage
# points: the 2026-09 product decision (docs/internal/20). A measured
# total more than EPSILON_PP below it fails the floor comparison, and
# --update refuses to record one (see the module docstring).
LOWER_BOUND_PP = 80.0

# The tolerance band for both comparisons, in percentage points. A
# measured total more than this far below the floor -- or below the
# recorded baseline -- fails; a smaller dip is measurement jitter
# (timing-sensitive tests reach different block sets under load;
# observed swings up to ~0.03 points in this repository's own suites).
# See the module docstring.
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
    0.1-point rounding `go tool cover -func` applies to its output.

    Blocks whose path contains ".gen.go" are excluded from the census
    entirely: the committed oapi-codegen output is not a test target
    (see the module docstring)."""
    total = 0
    covered = 0
    for line in profile_text.splitlines():
        match = PROFILE_BLOCK.match(line)
        if match is None:
            continue
        if ".gen.go" in match.group(1):
            continue
        statements = int(match.group(2))
        count = int(match.group(3))
        total += statements
        if count > 0:
            covered += statements
    if total == 0:
        return 100.0
    return 100.0 * covered / total


def comparison_failures(
    measured: float,
    baseline: float | None,
    floor: float = LOWER_BOUND_PP,
    epsilon: float = EPSILON_PP,
) -> list[str]:
    """The breached bounds for one measured total, in a fixed order
    (floor first, then the recorded baseline); empty means pass. Both
    comparisons share the same tolerance semantics: a measured total
    more than EPSILON_PP below the bound fails it."""
    failures = []
    if measured + epsilon < floor:
        failures.append(
            "measured %.4f%% sits more than the %.2f-point tolerance "
            "below the %.1f%% floor (the product decision; the response "
            "is adding tests -- --update cannot waive it)"
            % (measured, epsilon, floor)
        )
    if baseline is not None and measured + epsilon < baseline:
        failures.append(
            "measured %.4f%% sits more than the %.2f-point tolerance "
            "below the recorded baseline %.4f%% -- add tests, or record "
            "a deliberate decline with --update (with the reason in the "
            "commit message)"
            % (measured, epsilon, baseline)
        )
    return failures


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
        help="measure and compare against the floor and the stored "
        "baseline (default)",
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
        help="module directory (go/pkgcore, examples/reference-app); "
        "--all selects the gated set",
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
        gated = set(GATED_MODULES)
        for module_dir in sorted(rows):
            if module_dir not in gated:
                problems.append(
                    "baseline row %s is not a gated module (the released-"
                    "module set names %s)"
                    % (module_dir, ", ".join(sorted(gated)))
                )
            elif not (root / module_dir / "go.mod").is_file():
                problems.append(
                    "baseline row %s has no go.mod at %s"
                    % (module_dir, root / module_dir)
                )
        for module_dir in GATED_MODULES:
            if module_dir not in rows:
                problems.append(
                    "gated module %s has no baseline row -- record one "
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
        print(
            "coverage-baselines.json: rows match the gated set "
            "(%d modules)" % len(GATED_MODULES)
        )
        return 0

    selected: list[str]
    if args.module is not None:
        if args.all:
            print("--module and --all are mutually exclusive", file=sys.stderr)
            return 2
        selected = [args.module]
    elif args.all:
        selected = list(GATED_MODULES)
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
        measured_by_module: dict[str, float] = {}
        refusals: list[str] = []
        for module_dir in selected:
            if module_dir not in GATED_MODULES:
                print(
                    "update refuses %s: the baseline gate covers the "
                    "released modules only (%d of them: go.work's use "
                    "list plus examples/reference-app)"
                    % (module_dir, len(GATED_MODULES)),
                    file=sys.stderr,
                )
                return 1
            total = measure_module_coverage(root, module_dir)
            measured_by_module[module_dir] = total
            if comparison_failures(total, baseline=None):
                refusals.append(
                    "update refuses to record %s at %.4f%%: the measured "
                    "total breaches the %.1f%% floor, and the floor is "
                    "the product decision -- not a baseline -- so the "
                    "response is adding tests, not re-recording"
                    % (module_dir, total, LOWER_BOUND_PP)
                )
        if refusals:
            for refusal in refusals:
                print(
                    "check_coverage_baseline: %s" % refusal, file=sys.stderr
                )
            return 1
        for module_dir, total in measured_by_module.items():
            rows[module_dir] = round(total, 6)
            print("recorded %s: %.4f%%" % (module_dir, total))
        save_baselines(
            baseline_path,
            {"toolchain": toolchain, "baselines": rows},
        )
        return 0

    # --check (default)
    failures = 0
    for module_dir in selected:
        if module_dir not in GATED_MODULES:
            print(
                "%s: outside the coverage gate (the released modules: "
                "go.work's use list plus examples/reference-app) -- "
                "nothing to compare"
                % module_dir
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
        reasons = comparison_failures(measured, baseline)
        if reasons:
            for reason in reasons:
                print(
                    "check_coverage_baseline: FAIL %s: %s"
                    % (module_dir, reason),
                    file=sys.stderr,
                )
            failures += 1
        else:
            print(
                "%s: %.4f%% (floor %.1f%%, baseline %.4f%%)"
                % (module_dir, measured, LOWER_BOUND_PP, baseline)
            )
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
