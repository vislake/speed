#!/usr/bin/env python3
"""Unit tests for check_toolchain.py's exit-code contract and go.work read.

Stdlib-only (unittest + tempfile), matching this directory's own "plain
executables with no third-party dependencies" convention (tools/README.md's
"Running in CI and locally" section). Run directly:

    python3 tools/test_check_toolchain.py

The contract under test is the gate's exit codes: an infrastructure error
-- a source file missing or unparsable, or an expected tool absent from
.mise.toml -- exits 2, while genuine version drift stays a content error
exiting 1. The readers must not fall through sys.exit("error: ..."),
which exits 1 and would make a missing input file indistinguishable from
drift.
"""

from __future__ import annotations

import contextlib
import io
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_toolchain as m  # noqa: E402

# A coherent fixture tree: the .mise.toml mirror equals its source (values
# copied from the real tree), so a test can drift or delete one piece and
# know which failure it caused.
MISE = """\
[tools]
go = "1.26.8"
golangci-lint = "2.11.4"
python = "3.14.7"
"""


def write_fixture(root: pathlib.Path) -> None:
    """Write the fully-consistent fixture tree under root."""
    (root / ".mise.toml").write_text(MISE, encoding="utf-8")
    (root / "go.work").write_text("go 1.26.8\n", encoding="utf-8")


class ToolchainGateTests(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.root = pathlib.Path(self._tmp.name)
        write_fixture(self.root)

    def _run(self) -> tuple[int, str]:
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            code = m.main(["--root", str(self.root)])
        return code, buf.getvalue()

    def _run_expect_system_exit(self) -> SystemExit:
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            with self.assertRaises(SystemExit) as cm:
                m.main(["--root", str(self.root)])
        return cm.exception

    def test_consistent_tree_passes(self) -> None:
        code, out = self._run()
        self.assertEqual(code, 0)
        self.assertIn("toolchain: ok          go:", out)

    def test_unmirrored_tools_are_not_checked(self) -> None:
        # Only the tools in SOURCES are mirrors. Everything else
        # .mise.toml pins is pinned there alone, so changing it is not
        # drift and must not fail the gate.
        (self.root / ".mise.toml").write_text(
            MISE.replace('golangci-lint = "2.11.4"', 'golangci-lint = "9.9.9"'),
            encoding="utf-8",
        )
        code, _ = self._run()
        self.assertEqual(code, 0)

    def test_drift_stays_a_content_error_exit_1(self) -> None:
        # Genuine version drift is the gate's content finding: exit 1,
        # never 2.
        (self.root / ".mise.toml").write_text(
            MISE.replace('go = "1.26.8"', 'go = "9.9.9"'),
            encoding="utf-8",
        )
        code, out = self._run()
        self.assertEqual(code, 1)
        self.assertIn("MISMATCH", out)

    def test_missing_source_file_exits_2(self) -> None:
        # A missing source file is an infrastructure error: exit 2 (the
        # module docstring's contract). The reader must not fall through
        # sys.exit("error: ..."), which exits 1 like drift.
        (self.root / "go.work").unlink()
        exc = self._run_expect_system_exit()
        self.assertEqual(exc.code, 2)

    def test_missing_mise_exits_2(self) -> None:
        (self.root / ".mise.toml").unlink()
        exc = self._run_expect_system_exit()
        self.assertEqual(exc.code, 2)

    def test_absent_tool_exits_2(self) -> None:
        # An expected tool absent from .mise.toml is infrastructure
        # (nothing to compare a mirror against): exit 2.
        (self.root / ".mise.toml").write_text(
            MISE.replace('go = "1.26.8"\n', ""), encoding="utf-8"
        )
        code, out = self._run()
        self.assertEqual(code, 2)
        self.assertIn("go is absent from .mise.toml", out)

    def test_go_work_without_directive_exits_2(self) -> None:
        # A go.work that exists but carries no go directive is an
        # unparsable source: exit 2, not the drift code.
        (self.root / "go.work").write_text("use ./pkg/core\n", encoding="utf-8")
        exc = self._run_expect_system_exit()
        self.assertEqual(exc.code, 2)


if __name__ == "__main__":
    unittest.main()
