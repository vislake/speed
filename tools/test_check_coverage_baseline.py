#!/usr/bin/env python3
"""Unit tests for check_coverage_baseline.py's parsing and gating rules.

Stdlib-only (unittest), matching this directory's conventions. Run
directly:

    python3 tools/test_check_coverage_baseline.py

The Go-measurement leg is exercised against the real repository by the
go-module-ci coverage leg and by `python3 tools/check_coverage_baseline.py
--update` itself; this suite pins the pure parts:

  * coverage_from_profile -- the exact statement-coverage math, proven
    against a hand-built profile (all covered, none covered, partial),
    plus the guard against rounding: a profile whose true total sits
    between two 0.1 display steps must come out exact, not rounded.
  * short_toolchain -- the go version line's short form.
  * The gating rule's fail direction -- the mechanism must not go soft
    on the decline it exists to catch -- is proven against the real
    repository instead (plant a baseline above the measured total and
    --check fails; see the tool's own doc comment), because the rule
    needs a real measurement to mean anything.
"""

from __future__ import annotations

import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_coverage_baseline as m  # noqa: E402


class CoverageMath(unittest.TestCase):
    def test_profile_all_covered(self):
        profile = "\n".join(
            [
                "mode: set",
                "go/pkgcore/kernel.go:10.2,20.14 8 1",
                "go/pkgcore/kernel.go:21.2,30.14 4 1",
            ]
        )
        self.assertEqual(m.coverage_from_profile(profile), 100.0)

    def test_profile_none_covered(self):
        profile = "\n".join(
            [
                "mode: set",
                "go/pkgcore/kernel.go:10.2,20.14 8 0",
                "go/pkgcore/kernel.go:21.2,30.14 4 0",
            ]
        )
        self.assertEqual(m.coverage_from_profile(profile), 0.0)

    def test_profile_partial_is_exact_not_rounded(self):
        # Coverage in Go's "set" mode is block-granular: a block whose
        # count is > 0 is fully covered (all its statements), one whose
        # count is 0 contributes nothing. One executed 8-statement
        # block and one unexecuted 4-statement block therefore measure
        # 8/12 = 66.666...% -- a value go tool cover -func would print
        # rounded to 66.7, which would blind a baseline comparison to a
        # 0.05-point decline. The parser must return the exact total.
        profile = "\n".join(
            [
                "mode: set",
                "go/dbkit/repository.go:10.2,20.14 8 1",
                "go/dbkit/repository.go:21.2,30.14 4 0",
            ]
        )
        self.assertAlmostEqual(m.coverage_from_profile(profile), 8 / 12 * 100)

    def test_profile_skips_non_block_lines(self):
        profile = "\n".join(
            [
                "mode: set",
                "some noise line that is not a block",
                "go/jobs/worker.go:1.1,2.2 3 3",
                "",
                "go/jobs/worker.go:3.1,4.2 1 0",
            ]
        )
        self.assertEqual(m.coverage_from_profile(profile), 75.0)

    def test_profile_empty_total_is_100(self):
        # No statements at all: nothing to regress, so the comparison
        # cannot fail on an empty census.
        self.assertEqual(m.coverage_from_profile("mode: set\n"), 100.0)


class Toolchain(unittest.TestCase):
    def test_short_toolchain_parses_go_version_line(self):
        self.assertEqual(
            m.short_toolchain("go version go1.26.8 darwin/arm64"), "go1.26.8"
        )
        self.assertEqual(
            m.short_toolchain("go version go1.26.8 linux/amd64"), "go1.26.8"
        )

    def test_short_toolchain_refuses_garbage(self):
        with self.assertRaises(ValueError):
            m.short_toolchain("not a go version line")


if __name__ == "__main__":
    unittest.main()
