#!/usr/bin/env python3
"""semgrep_fixture_check.py -- the semgrep ruleset's fixture self-check.

For every rule under tools/semgrep_rules/ the script runs semgrep against
that rule's own fixture directory (tools/semgrep_rules/testdata/<rule>/)
and asserts two expectations:

  * the rule fires at least once on its planted positive fixture
    (positive.go) -- without this assertion, a semgrep upgrade that
    silently stopped matching the rule would leave the real-tree scan
    passing with nothing left to catch, and nothing would go red;
  * the rule stays silent on its negative fixture (negative.go) -- the
    shapes the rule deliberately does not fire on.

The rules are exercised one rule at a time against that rule's own
directory (never a whole-ruleset scan over the fixtures): a negative
fixture may legitimately carry a shape another rule fires on, so only
same-rule expectations are meaningful.

Usage:
  python3 tools/semgrep_fixture_check.py [SEMGREP_BIN]

SEMGREP_BIN defaults to "semgrep" on PATH. pr-check's repo-checks job
passes the venv-installed binary from its throwaway pip install; local
runs pass the pinned returntocorp/semgrep docker image through a shim.
The scan of the real tree itself (go/ examples/ tools/, fixtures
excluded) is the separate step in the same repo-checks job.

Exit codes: 0 every rule fired on its fixture and stayed clean on its
negative; 1 at least one expectation failed; 2 usage error.
"""

import glob
import json
import os
import subprocess
import sys

RULES_REL = os.path.join("tools", "semgrep_rules")
TESTDATA_REL = os.path.join(RULES_REL, "testdata")


def repo_root():
    """The repository root: this script lives in tools/."""
    return os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def check_rule(semgrep_bin, root, rule_rel):
    """Run one rule against its own fixture directory. Returns
    (ok, lines): ok False on any expectation failure or run failure.

    Semgrep is invoked with paths relative to the repository root (its
    working directory), never absolute host paths: relative paths stay
    valid when semgrep runs through a path-remapping sandbox (the pinned
    docker image bind-mounts the root at /repo) as well as directly in
    CI."""
    name = os.path.basename(rule_rel)
    if name.endswith(".yml"):
        name = name[:-4]
    fixture_rel = os.path.join(TESTDATA_REL, name)
    fixture_dir = os.path.join(root, fixture_rel)
    lines = []
    if not os.path.isdir(fixture_dir):
        return False, [f"rule {name} has no fixture directory {fixture_rel}"]
    if not os.path.isfile(os.path.join(fixture_dir, "positive.go")):
        return False, [f"rule {name} has no positive.go in its fixture "
                       f"directory {fixture_rel}"]
    proc = subprocess.run(
        [semgrep_bin, "scan", "--config", rule_rel, fixture_rel, "--json"],
        cwd=root, capture_output=True, text=True)
    if proc.returncode > 1:
        return False, [
            f"semgrep exited {proc.returncode} on rule {name} "
            f"(fixture run):", proc.stderr.strip()]
    try:
        report = json.loads(proc.stdout)
    except json.JSONDecodeError:
        return False, [
            f"semgrep produced no JSON output on rule {name} "
            f"(fixture run):", proc.stderr.strip()]
    matches = [r["path"].replace(os.sep, "/")
               for r in report.get("results", [])]
    negatives = [p for p in matches
                 if os.path.basename(p).startswith("negative")]
    positives = [p for p in matches
                 if not os.path.basename(p).startswith("negative")]
    if not positives:
        return False, [
            f"rule {name} matched nothing on its planted positive "
            f"fixture -- a semgrep change may have silently stopped the "
            f"rule from matching; rerun the per-rule fixture proof before "
            f"shipping it"]
    if negatives:
        return False, [
            f"rule {name} fired on its negative fixture: "
            f"{', '.join(negatives)}"]
    lines.append(f"ok: rule {name} fires on positive.go "
                 f"({len(positives)} match(es)) and stays clean on "
                 f"negative.go")
    return True, lines


def main(argv):
    if len(argv) > 2:
        print(__doc__)
        return 2
    semgrep_bin = argv[1] if len(argv) == 2 else "semgrep"
    root = repo_root()
    failures = 0
    lines = []
    rules = sorted(glob.glob(os.path.join(root, RULES_REL, "*.yml")))
    if not rules:
        print(f"error: no rule files under {RULES_REL}")
        return 1
    for rule in rules:
        ok, rule_lines = check_rule(semgrep_bin, root, os.path.relpath(
            rule, root))
        lines.extend(rule_lines)
        if not ok:
            failures += 1
    for line in lines:
        print(line)
    if failures:
        print(f"semgrep fixture self-check: FAILED ({failures} "
              f"rule(s) out of {len(rules)})")
        return 1
    print(f"semgrep fixture self-check: PASS ({len(rules)} rules fire on "
          f"their planted fixtures)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
