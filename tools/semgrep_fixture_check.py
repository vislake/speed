#!/usr/bin/env python3
"""semgrep_fixture_check.py -- the semgrep ruleset's fixture self-check.

For every rule under tools/semgrep_rules/ the script runs semgrep against
that rule's own fixture directory (tools/semgrep_rules/testdata/<rule>/)
and asserts two semgrep expectations:

  * the rule fires at least once on its planted positive fixture
    (positive.go) -- without this assertion, a semgrep upgrade that
    silently stopped matching the rule would leave the real-tree scan
    passing with nothing left to catch, and nothing would go red;
  * the rule stays silent on its negative fixture (negative.go) -- the
    shapes the rule deliberately does not fire on.

Both expectations compare the rule to its own fixture, so a rename that
aged the rule and the fixture TOGETHER keeps them green while the real
tree moved on -- the teeth prove "every rule fires on its own fixture",
not "the fixture still looks like real code". A third, reverse expectation
closes that blind spot, without any semgrep run:

  * every environment-variable name literal a rule's positive fixture
    exercises must still be read by real code somewhere under go/
    examples/ tools/ -- a fixture naming an env variable no non-test file
    reads any more has aged away from the tree and proves nothing. A name
    counts as read when it appears as a direct os.Getenv/os.LookupEnv
    string argument or as the value of a same-file literal const passed
    to one (the two shapes semgrep's own matching sees, since it
    constant-propagates same-file literal-valued consts). Test files,
    integration_test/ directories and the rules' own testdata subtree are
    excluded, the same exclusion shape the real-tree scan applies. A
    fixture whose positives exercise no env literal is unconstrained.

The rules are exercised one rule at a time against that rule's own
directory (never a whole-ruleset scan over the fixtures): a negative
fixture may legitimately carry a shape another rule fires on, so only
same-rule expectations are meaningful.

Usage:
  python3 tools/semgrep_fixture_check.py [SEMGREP_BIN]

SEMGREP_BIN defaults to "semgrep" on PATH. fast-check's repo-checks job
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
import re
import subprocess
import sys

RULES_REL = os.path.join("tools", "semgrep_rules")
TESTDATA_REL = os.path.join(RULES_REL, "testdata")

# The scanned real tree (mirrors the repo-checks semgrep invocation).
SCAN_ROOTS = ("go", "examples", "tools")

_CONST_SINGLE_RE = re.compile(r'\bconst\s+([A-Za-z_]\w*)\s*=\s*"([^"]*)"')
_CONST_BLOCK_RE = re.compile(r"\bconst\s*\((.*?)\)", re.S)
_CONST_BLOCK_ENTRY_RE = re.compile(r'([A-Za-z_]\w*)\s*=\s*"([^"]*)"')
_ENV_READ_RE = re.compile(r"\bos\.(?:Getenv|LookupEnv)\(\s*([^)]*?)\s*\)")


def env_literals_read(text):
    """The env-name string literals a Go source file READS: direct string
    arguments of os.Getenv/os.LookupEnv calls plus the literal values of
    same-file consts those calls receive as their argument. Semgrep
    constant-propagates same-file literal-valued consts before matching,
    so both shapes are what a rule's env-read pattern can see."""
    literals = set()
    values = {}
    for name, value in _CONST_SINGLE_RE.findall(text):
        values[name] = value
    for block in _CONST_BLOCK_RE.findall(text):
        for name, value in _CONST_BLOCK_ENTRY_RE.findall(block):
            values[name] = value
    for arg in _ENV_READ_RE.findall(text):
        arg = arg.strip()
        if arg.startswith('"'):
            match = re.fullmatch(r'"([^"]*)"', arg)
            if match:
                literals.add(match.group(1))
        elif arg in values:
            literals.add(values[arg])
    return literals


def live_env_reads(root):
    """The env-name literals the scanned real tree reads: union of
    env_literals_read over every non-test .go file under go/ examples/
    tools/, with tools/semgrep_rules/ and integration_test/ directories
    pruned -- the same exclusion shape the repo-checks real-tree scan
    applies."""
    live = set()
    for scan_root in SCAN_ROOTS:
        scan_dir = os.path.join(root, scan_root)
        for dirpath, dirnames, filenames in os.walk(scan_dir):
            for excluded in ("semgrep_rules", "integration_test"):
                if excluded in dirnames:
                    dirnames.remove(excluded)
            for filename in filenames:
                if not filename.endswith(".go"):
                    continue
                if filename.endswith("_test.go"):
                    continue
                with open(os.path.join(dirpath, filename),
                          encoding="utf-8") as handle:
                    live |= env_literals_read(handle.read())
    return live


def check_fixture_env_liveness(rule_name, fixture_dir, live):
    """The reverse expectation: every env literal the rule's positive
    fixture exercises must still be read by real code. Returns
    (ok, lines)."""
    names = set()
    for filename in sorted(os.listdir(fixture_dir)):
        if not filename.endswith(".go"):
            continue
        if filename.startswith("negative"):
            continue
        with open(os.path.join(fixture_dir, filename),
                  encoding="utf-8") as handle:
            names |= env_literals_read(handle.read())
    if not names:
        return True, []
    missing = sorted(names - live)
    if missing:
        return False, [
            f"rule {rule_name}: fixture env literal(s) "
            f"{', '.join(missing)} read by no real code under go/ "
            f"examples/ tools/ -- the fixture has aged away from the tree "
            f"(an env rename ages rule and fixture together while the two "
            f"semgrep expectations above stay green); update the fixture "
            f"to the live spelling"]
    return True, [
        f"ok: rule {rule_name}: fixture env literal(s) "
        f"{', '.join(sorted(names))} still read by real code"]


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
    live = live_env_reads(root)
    for rule in rules:
        name = os.path.basename(rule)
        if name.endswith(".yml"):
            name = name[:-4]
        fixture_dir = os.path.join(root, TESTDATA_REL, name)
        if not os.path.isdir(fixture_dir):
            continue  # the per-rule semgrep check above already reported it
        ok, rule_lines = check_fixture_env_liveness(
            name, fixture_dir, live)
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
          f"their planted fixtures and their fixture env literals stay "
          f"live in the scanned tree)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
