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
    Generated-file exclusion is proven the same way: a .gen.go block's
    statements leave the census whether covered or not.
  * comparison_failures -- the two-bounds decision rule (the 80.0%
    floor and the recorded baseline, both under the same tolerance),
    including which bound a measurement breaches when both do.
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

    def test_profile_excludes_gen_go_statements(self):
        # Generated files (committed oapi-codegen output, named
        # *.gen.go) are not a test target: their blocks must leave the
        # census entirely. An 80-statement gen block with zero count
        # must not drag the total down...
        profile = "\n".join(
            [
                "mode: set",
                "go/authn/api/authn-server.gen.go:10.2,90.14 80 0",
                "go/authn/service.go:10.2,20.14 8 1",
                "go/authn/service.go:21.2,30.14 4 0",
            ]
        )
        self.assertAlmostEqual(m.coverage_from_profile(profile), 8 / 12 * 100)

    def test_profile_excludes_fully_covered_gen_go_too(self):
        # ...and an entirely covered gen block must not inflate the
        # total either: the census is hand-written code alone.
        profile = "\n".join(
            [
                "mode: set",
                "go/authn/api/authn-server.gen.go:10.2,90.14 80 1",
                "go/authn/service.go:10.2,20.14 6 1",
            ]
        )
        self.assertEqual(m.coverage_from_profile(profile), 100.0)


class Comparisons(unittest.TestCase):
    """The two-bounds decision rule: the 80.0% floor and the recorded
    baseline, both breached only more than EPSILON_PP below the bound
    (the measurement-jitter band; see the tool's module docstring)."""

    def test_pass_above_floor_and_baseline(self):
        self.assertEqual(m.comparison_failures(84.0, baseline=83.0), [])

    def test_pass_within_tolerance_of_the_floor(self):
        # 79.96 + 0.05 clears 80.0: a sub-floor dip inside the jitter
        # band passes, exactly as a dip inside the band passes the
        # baseline comparison.
        self.assertEqual(m.comparison_failures(79.96, baseline=None), [])

    def test_floor_breach_fails_even_above_a_low_baseline(self):
        # A measured total below the floor fails no matter what the
        # baseline file says -- the floor is the product decision, not
        # a re-baselineable row.
        reasons = m.comparison_failures(79.5, baseline=79.5)
        self.assertEqual(len(reasons), 1)
        self.assertIn("floor", reasons[0])

    def test_decline_breach_fails_above_the_floor(self):
        # A decline against the baseline still fails while the floor
        # holds: the decline gate exists so coverage that merely slides
        # within a high-but-falling band cannot pass.
        reasons = m.comparison_failures(83.0, baseline=84.0)
        self.assertEqual(len(reasons), 1)
        self.assertIn("baseline", reasons[0])

    def test_both_breaches_report_both_bounds(self):
        reasons = m.comparison_failures(79.0, baseline=84.0)
        self.assertEqual(len(reasons), 2)
        self.assertIn("floor", reasons[0])
        self.assertIn("baseline", reasons[1])

    def test_floor_edge_is_exact_not_rounded(self):
        # 79.95 + 0.05 == 80.0 exactly: the band edge passes. The
        # comparisons run on unrounded totals, so a floor crossing
        # cannot hide behind display rounding.
        self.assertEqual(m.comparison_failures(79.95, baseline=None), [])
        self.assertEqual(len(m.comparison_failures(79.94, baseline=None)), 1)


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
