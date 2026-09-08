#!/usr/bin/env python3
"""Unit tests for check_toolchain.py's exit-code contract and Taskfile read.

Stdlib-only (unittest + tempfile), matching this directory's own "plain
executables with no third-party dependencies" convention (tools/README.md's
"Running in CI and locally" section). Run directly:

    python3 tools/test_check_toolchain.py

Regression coverage for two defects on this gate:

  * Exit-code contract (module docstring): an infrastructure error -- a
    source file missing or unparsable, or an expected tool absent from
    .mise.toml -- must exit 2, while genuine version drift stays a
    content error exiting 1. The readers must not fall through
    sys.exit("error: ..."), which exits 1 and would make a missing
    input file indistinguishable from drift (test_missing_source_file_exits_2,
    test_missing_mise_exits_2, test_absent_tool_exits_2).
  * The task pin is read by pulling TASK_HEADER_LIMIT lines with
    next(fh); a file shorter than the limit must keep every line
    already read rather than discarding them, so a freshly scaffolded
    2-line Taskfile whose pin sits on line 1 resolves its pin. The read
    iterates readline() and keeps every line it gets
    (test_short_taskfile_pin_is_found).
"""

from __future__ import annotations

import contextlib
import io
import json
import pathlib
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_toolchain as m  # noqa: E402

# A coherent fixture tree: every .mise.toml mirror equals its source
# (values copied from the real tree), so tests can drift or delete one
# piece and know which failure they caused.
MISE = """\
[tools]
task = "3.53.1"
go = "1.26.8"
node = "24"
pnpm = "11.1.2"
golangci-lint = "2.11.4"
hugo = "0.165.0"
"""


def write_fixture(root: pathlib.Path) -> None:
    """Write the fully-consistent fixture tree under root."""
    (root / ".mise.toml").write_text(MISE, encoding="utf-8")
    (root / "Taskfile.yml").write_text(
        "version: '3'\n\n"
        "# Taskfile.yml header comment: task 3.53.1 (the\n"
        "# version verified against this file)\n",
        encoding="utf-8",
    )
    (root / "go.work").write_text("go 1.26.8\n", encoding="utf-8")
    web = root / "web"
    web.mkdir()
    (web / ".nvmrc").write_text("24\n", encoding="utf-8")
    (web / "package.json").write_text(
        json.dumps({"packageManager": "pnpm@11.1.2"}), encoding="utf-8"
    )
    go_env = root / ".github" / "actions" / "setup-go-env"
    go_env.mkdir(parents=True)
    (go_env / "action.yml").write_text(
        'env:\n  GOLANGCI_VERSION: "2.11.4"\n', encoding="utf-8"
    )
    hugo_env = root / ".github" / "actions" / "setup-hugo-env"
    hugo_env.mkdir(parents=True)
    (hugo_env / "action.yml").write_text(
        'env:\n  HUGO_VERSION: "0.165.0"\n', encoding="utf-8"
    )


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
        self.assertIn("toolchain: ok          task:", out)

    def test_short_taskfile_pin_is_found(self) -> None:
        # A 2-line Taskfile whose pin sits on line 1 (a freshly
        # scaffolded file, say) must report the pin: the read is bounded
        # by the header limit but keeps what it got. Fails before the
        # fix -- the StopIteration branch discarded the line already read
        # and the gate reported "no 'task <version>' pin found".
        (self.root / "Taskfile.yml").write_text(
            "task 3.53.1 (the version verified against this file)\n"
            "# a second line\n",
            encoding="utf-8",
        )
        code, out = self._run()
        self.assertEqual(code, 0)
        self.assertIn("toolchain: ok          task:", out)

    def test_drift_stays_a_content_error_exit_1(self) -> None:
        # Genuine version drift is the gate's content finding: exit 1,
        # never 2.
        (self.root / ".mise.toml").write_text(
            MISE.replace('task = "3.53.1"', 'task = "9.9.9"'),
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
        # (nothing to compare a mirror against): exit 2. Fails before the
        # fix -- absence was tallied as drift and exited 1.
        (self.root / ".mise.toml").write_text(
            MISE.replace("hugo = \"0.165.0\"\n", ""), encoding="utf-8"
        )
        code, out = self._run()
        self.assertEqual(code, 2)
        self.assertIn("hugo is absent from .mise.toml", out)

    def test_taskfile_without_pin_exits_2(self) -> None:
        # A Taskfile that exists but carries no task pin is an unparsable
        # source: exit 2, not the drift code.
        (self.root / "Taskfile.yml").write_text(
            "version: '3'\n# nothing pinned here\n", encoding="utf-8"
        )
        exc = self._run_expect_system_exit()
        self.assertEqual(exc.code, 2)


if __name__ == "__main__":
    unittest.main()
