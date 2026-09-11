#!/usr/bin/env python3
"""Planted-drift suite for check_integration_tiers.py.

Stdlib-only (unittest + tempfile), matching this directory's conventions.
Run directly:

    python3 tools/test_check_integration_tiers.py

The fixture tree is a small fake repository: a go.work use block, a
tools/release/lockstep-release.py stub carrying parse_gowork_uses (the
derivation loads the release coordinator's parser from the tree under
test), a full-check.yml fragment with the integration-tiers job's
matrix, and a Taskfile.yml fragment with the INTEGRATION_DIRS variable.
Each test plants one disagreement between the tree and a copy and pins
the gate's verdict: the clean fixture stays green; a missing row/entry,
an extra row/entry, a duplicated name, a stale exclusion and an
exclusion that reappeared in a copy each fail as drift (exit 1 path);
a workflow or Taskfile in a shape the reader does not recognize, an
empty list and a non-module entry each fail as infrastructure (exit 2
path). The final case runs the gate against this repository's real
tree, so an interface drift between the gate and the real
lockstep-release.py / full-check.yml / Taskfile.yml surfaces here.
"""

from __future__ import annotations

import contextlib
import io
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_integration_tiers as m  # noqa: E402

# The fixture repository's parser stub: the release coordinator's
# interface (parse_gowork_uses + ReleaseError), enough for the fixture
# go.work texts. The real parser's semantics are its own suite's
# business (tools/release/test_lockstep_release.py); the real-tree case
# at the end of this file exercises the real file through this gate.
PARSER_STUB = '''\
"""Fixture stub of the release coordinator's go.work parser."""


class ReleaseError(Exception):
    """The go.work text is malformed."""


def parse_gowork_uses(go_work_text):
    entries = []
    in_block = False
    for raw in go_work_text.splitlines():
        s = raw.strip()
        if not s or s.startswith("//"):
            continue
        if in_block:
            if s == ")":
                in_block = False
                continue
            entries.extend(s.split())
            continue
        if s.startswith("use"):
            rest = s[3:].strip()
            if rest.startswith("("):
                rest = rest[1:].strip()
                if rest.endswith(")"):
                    rest = rest[:-1].strip()
                else:
                    in_block = True
                if rest:
                    entries.extend(rest.split())
            elif rest:
                entries.extend(rest.split())
    return [entry[2:] if entry.startswith("./") else entry for entry in entries]
'''

TAGGED_TEST = """//go:build integration

package {pkg}_test

import "testing"

func Test{Tier}(t *testing.T) {{
\t_ = t
}}
"""

PLAIN_TEST = """package beta_test

import "testing"

func TestPlain(t *testing.T) {
\t_ = t
}
"""

# A comment naming the constraint phrase after the package clause: not a
# tier (Go does not recognize a constraint there, and neither does the
# gate's reader, which stops at the package clause).
AFTER_PACKAGE_MENTION = """package gamma_test

// This suite is expected to run under //go:build integration in CI.
import "testing"

func TestGamma(t *testing.T) {
\t_ = t
}
"""

# A negated constraint: the module is gated OFF under the integration
# tag, so it is not a tier.
NEGATED_CONSTRAINT = """//go:build !integration

package beta_test

import "testing"

func TestNegated(t *testing.T) {
\t_ = t
}
"""


def workflow_text(rows: list[str], with_job: bool = True) -> str:
    """A full-check.yml fragment with the integration-tiers job shape the
    gate's reader anchors on (job key at 2-space, matrix at 6, module at
    8, items at 10)."""
    if not with_job:
        return (
            "name: full-check (fixture)\n"
            "on:\n"
            "  push:\n"
            "    branches: [main]\n"
            "\n"
            "jobs:\n"
            "  go-modules:\n"
            "    runs-on: ubuntu-latest\n"
            "    strategy:\n"
            "      matrix:\n"
            "        module:\n"
            "          - go/beta\n"
            "    steps: []\n"
        )
    lines = [
        "name: full-check (fixture)",
        "on:",
        "  push:",
        "    branches: [main]",
        "",
        "jobs:",
        "  integration-tiers:",
        "    name: integration tier (${{ matrix.module }})",
        "    runs-on: ubuntu-latest",
        "    strategy:",
        "      fail-fast: false",
        "      matrix:",
        "        module:",
    ]
    lines += [f"          - {row}" for row in rows]
    lines += [
        "    steps:",
        "      - name: Check out repository",
        "        uses: actions/checkout@x",
    ]
    return "\n".join(lines) + "\n"


def taskfile_text(dirs: list[str], with_var: bool = True) -> str:
    if not with_var:
        return "version: '3'\n\nvars:\n  APP_DIR: examples/app\n\ntasks:\n  test: {}\n"
    joined = " ".join(dirs)
    return (
        "version: '3'\n"
        "\n"
        "vars:\n"
        "  APP_DIR: examples/app\n"
        f"  INTEGRATION_DIRS: {joined}\n"
        "\n"
        "tasks:\n"
        "  test: {}\n"
    )


class FixtureCase(unittest.TestCase):
    """Base: a fixture repository with one tagged module (go/alpha), one
    untagged module (go/beta), the excluded go/saasctl (tagged), the
    examples/app consumer entry and both copies wired to go/alpha."""

    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory(prefix="integration-tiers-")
        self.addCleanup(tmp.cleanup)
        self.root = pathlib.Path(tmp.name)

    def write(self, rel: str, text: str) -> None:
        path = self.root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")

    def write_dir(self, rel: str) -> None:
        (self.root / rel).mkdir(parents=True, exist_ok=True)

    def _write_fixture(
        self,
        workflow_rows: list[str] | None = None,
        taskfile_dirs: list[str] | None = None,
        saasctl_tagged: bool = True,
        beta_negated: bool = False,
        gamma_after_package: bool = False,
        workflow_job: bool = True,
        taskfile_var: bool = True,
    ) -> None:
        self.write(
            "go.work",
            "go 1.26.8\n"
            "\n"
            "use (\n"
            "\t./go/alpha\n"
            "\t./go/beta\n"
            "\t./go/saasctl\n"
            "\t./examples/app\n"
            ")\n",
        )
        self.write("tools/release/lockstep-release.py", PARSER_STUB)
        self.write("go/alpha/x_integration_test.go", TAGGED_TEST.format(pkg="alpha", Tier="Alpha"))
        self.write("go/beta/plain_test.go", NEGATED_CONSTRAINT if beta_negated else PLAIN_TEST)
        if saasctl_tagged:
            self.write(
                "go/saasctl/tier_test.go",
                TAGGED_TEST.format(pkg="saasctl", Tier="Saasctl"),
            )
        if gamma_after_package:
            self.write_dir("go/gamma")
            self.write("go/gamma/gamma_test.go", AFTER_PACKAGE_MENTION)
        if workflow_rows is None:
            workflow_rows = ["go/alpha"]
        if taskfile_dirs is None:
            taskfile_dirs = ["go/alpha"]
        self.write(
            ".github/workflows/full-check.yml",
            workflow_text(workflow_rows, with_job=workflow_job),
        )
        self.write("Taskfile.yml", taskfile_text(taskfile_dirs, with_var=taskfile_var))

    def _check(self) -> tuple[list[str], list[str], list[str], list[str]]:
        return m.check(str(self.root))

    def _problems(self) -> list[str]:
        problems, _, _, _ = self._check()
        return problems

    def _infra_error(self) -> str:
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            with self.assertRaises(SystemExit) as caught:
                m.check(str(self.root))
        self.assertEqual(caught.exception.code, 2)
        return stderr.getvalue()


class CleanFixture(FixtureCase):
    def test_clean_fixture_is_green(self) -> None:
        self._write_fixture()
        problems, tiers, rows, dirs = self._check()
        self.assertEqual(problems, [])
        # go.work file order, saasctl included in the derived set (it is
        # excluded from the copies, not from the tree).
        self.assertEqual(tiers, ["go/alpha", "go/saasctl"])
        self.assertEqual(rows, ["go/alpha"])
        self.assertEqual(dirs, ["go/alpha"])


class MissingFromCopies(FixtureCase):
    def test_tier_module_missing_from_workflow_matrix_is_drift(self) -> None:
        self._write_fixture(workflow_rows=[])
        problems = self._problems()
        self.assertEqual(len(problems), 1)
        self.assertIn("go/alpha", problems[0])
        self.assertIn("no row in", problems[0])

    def test_tier_module_missing_from_taskfile_is_drift(self) -> None:
        self._write_fixture(taskfile_dirs=[])
        problems = self._problems()
        self.assertEqual(len(problems), 1)
        self.assertIn("go/alpha", problems[0])
        self.assertIn("INTEGRATION_DIRS", problems[0])

    def test_missing_from_both_copies_reports_both(self) -> None:
        self._write_fixture(workflow_rows=[], taskfile_dirs=[])
        problems = self._problems()
        self.assertEqual(len(problems), 2)


class ExtraInCopies(FixtureCase):
    def test_workflow_row_for_untagged_module_is_drift(self) -> None:
        self._write_fixture(workflow_rows=["go/alpha", "go/beta"])
        problems = self._problems()
        self.assertEqual(len(problems), 1)
        self.assertIn("go/beta", problems[0])
        self.assertIn("no integration tier", problems[0])

    def test_taskfile_entry_for_untagged_module_is_drift(self) -> None:
        self._write_fixture(taskfile_dirs=["go/alpha", "go/beta"])
        problems = self._problems()
        self.assertEqual(len(problems), 1)
        self.assertIn("go/beta", problems[0])

    def test_negated_constraint_does_not_count_as_a_tier(self) -> None:
        # go/beta is tagged OFF under the integration tag; a copy naming
        # it is drift, and the derived set is unchanged.
        self._write_fixture(workflow_rows=["go/alpha", "go/beta"], beta_negated=True)
        problems, tiers, _, _ = self._check()
        self.assertEqual(tiers, ["go/alpha", "go/saasctl"])
        self.assertEqual(len(problems), 1)
        self.assertIn("go/beta", problems[0])

    def test_post_package_mention_does_not_count_as_a_tier(self) -> None:
        # go/gamma was never given a copy row, so a green result proves
        # the after-package mention did not enter the derived set; the
        # row-naming-gamma case proves the other direction.
        self._write_fixture(gamma_after_package=True)
        problems, tiers, _, _ = self._check()
        self.assertEqual(problems, [])
        self.assertNotIn("go/gamma", tiers)

    def test_row_naming_a_post_package_mention_module_is_drift(self) -> None:
        self._write_fixture(
            workflow_rows=["go/alpha", "go/gamma"], gamma_after_package=True
        )
        problems, tiers, _, _ = self._check()
        self.assertNotIn("go/gamma", tiers)
        self.assertEqual(len(problems), 1)
        self.assertIn("go/gamma", problems[0])

    def test_duplicate_copy_entries_are_drift(self) -> None:
        self._write_fixture(workflow_rows=["go/alpha", "go/alpha"])
        problems = self._problems()
        self.assertEqual(len(problems), 1)
        self.assertIn("twice", problems[0])


class DeclaredExclusion(FixtureCase):
    def test_exclusion_in_a_copy_is_drift(self) -> None:
        self._write_fixture(workflow_rows=["go/alpha", "go/saasctl"])
        problems = self._problems()
        self.assertEqual(len(problems), 1)
        self.assertIn("go/saasctl", problems[0])
        self.assertIn("declared", problems[0])

    def test_stale_exclusion_is_drift(self) -> None:
        # go/saasctl loses its tier while the exclusion remains declared.
        self._write_fixture(saasctl_tagged=False)
        problems = self._problems()
        self.assertEqual(len(problems), 1)
        self.assertIn("go/saasctl", problems[0])
        self.assertIn("remove the exclusion", problems[0])


class ReaderShapeErrors(FixtureCase):
    def test_workflow_without_the_job_is_infrastructure_error(self) -> None:
        self._write_fixture(workflow_job=False)
        message = self._infra_error()
        self.assertIn("integration-tiers", message)
        self.assertIn("update this gate's reader", message)

    def test_taskfile_without_the_var_is_infrastructure_error(self) -> None:
        self._write_fixture(taskfile_var=False)
        message = self._infra_error()
        self.assertIn("INTEGRATION_DIRS", message)

    def test_non_module_row_is_infrastructure_error(self) -> None:
        self._write_fixture(workflow_rows=["go/alpha", "web/packages/x"])
        message = self._infra_error()
        self.assertIn("web/packages/x", message)


class RealTree(unittest.TestCase):
    def test_real_repository_is_green(self) -> None:
        # Exercises the real lockstep-release.py, full-check.yml and
        # Taskfile.yml through this gate, so an interface drift between
        # them surfaces here instead of only in CI.
        root = pathlib.Path(__file__).resolve().parent.parent
        problems, tiers, rows, dirs = m.check(str(root))
        self.assertEqual(problems, [], "\n".join(problems))
        self.assertIn("go/sharing", tiers)
        self.assertIn("go/dbkit", tiers)
        self.assertNotIn("go/saasctl", rows)
        # Membership, not order: the matrix and the Taskfile loop are
        # read in their own orders; the derived set is compared as a set
        # (check() already pins membership exactly).
        self.assertEqual(sorted(rows), sorted(dirs))
        self.assertEqual(
            sorted(rows), sorted(t for t in tiers if t not in m.EXCLUDED)
        )


if __name__ == "__main__":
    unittest.main()
