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
    statements leave the census whether covered or not. Merged
    multi-binary fragments (a -coverpkg run lists each block once per
    test binary) are deduplicated: each block counts once, covered
    when any fragment executed it.
  * uncovered_blocks_from_profile / uncovered_diagnostics -- the
    failure diagnostics' block census: the same dedup and .gen.go
    exclusion as the coverage math, largest blocks first, and the cap
    that keeps the printed list short while the summary line still
    carries the true uncovered totals.
  * comparison_failures / effective_tolerance -- the two-bounds
    decision rule (the 80.0% floor and the recorded baseline, both
    under the same size-calibrated tolerance): both regimes of the
    calibration -- the main tolerance governing a large module, the
    2-statement floor governing a small one -- pinned in both
    directions, along with which bound a measurement breaches when
    both do and the failure text naming the effective tolerance and
    the census it was calibrated against.
  * main's --update refusal path -- a measured total breaching the
    floor is refused with its total named, and nothing is recorded.
  * short_toolchain -- the go version line's short form.
  * The gating rule's fail direction -- the mechanism must not go soft
    on the decline it exists to catch -- is proven against the real
    repository instead (plant a baseline above the measured total and
    --check fails; see the tool's own doc comment), because the rule
    needs a real measurement to mean anything.
"""

from __future__ import annotations

import contextlib
import io
import pathlib
import sys
import tempfile
import unittest
import unittest.mock

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
        # decline inside the 0.15-point tolerance band. The parser must
        # return the exact total.
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

    def test_profile_merged_fragments_count_each_block_once(self):
        # A go test run over many packages writes one profile fragment
        # per test binary, and under -coverpkg every fragment carries
        # the whole module's blocks, so the merged profile lists the
        # same block once per binary that ran. The census counts each
        # block once, covered when any fragment's count is above zero:
        # 8 statements executed by one binary of two still measure
        # 8/12, and a 4-statement block neither binary executed stays
        # uncovered even though both fragments list it.
        profile = "\n".join(
            [
                "mode: set",
                "go/jobs/worker.go:10.2,20.14 8 1",
                "go/jobs/worker.go:10.2,20.14 8 0",
                "go/jobs/store.go:21.2,30.14 4 0",
                "go/jobs/store.go:21.2,30.14 4 0",
            ]
        )
        self.assertAlmostEqual(m.coverage_from_profile(profile), 8 / 12 * 100)


class Diagnostics(unittest.TestCase):
    """The uncovered-block census and the lines a failed comparison
    prints: same dedup and .gen.go exclusion as the coverage math,
    largest blocks first, capped at DIAGNOSTIC_LIMIT with the true
    totals on the summary line."""

    def test_lists_uncovered_blocks_largest_first(self):
        # worker.go's block is executed by one of the two binaries that
        # carried it, so it counts as covered and stays off the list;
        # queue.go's 6-statement block outranks store.go's 4-statement
        # one.
        profile = "\n".join(
            [
                "mode: set",
                "go/jobs/worker.go:10.2,20.14 8 0",
                "go/jobs/worker.go:10.2,20.14 8 1",
                "go/jobs/store.go:21.2,30.14 4 0",
                "go/jobs/queue.go:31.2,40.14 6 0",
            ]
        )
        self.assertEqual(
            m.uncovered_blocks_from_profile(profile),
            [
                ("go/jobs/queue.go", "31.2,40.14", 6),
                ("go/jobs/store.go", "21.2,30.14", 4),
            ],
        )

    def test_uncovered_blocks_exclude_gen_go(self):
        # A big uncovered generated block is still not a test target:
        # it must not appear in the diagnostics either.
        profile = "\n".join(
            [
                "mode: set",
                "go/authn/api/authn-server.gen.go:10.2,90.14 80 0",
                "go/authn/service.go:10.2,20.14 8 0",
            ]
        )
        self.assertEqual(
            m.uncovered_blocks_from_profile(profile),
            [("go/authn/service.go", "10.2,20.14", 8)],
        )

    def test_line_span_renders_start_and_end_line(self):
        self.assertEqual(m.line_span("12.17,18.2"), "12-18")

    def test_diagnostics_format_and_totals(self):
        profile = "\n".join(
            [
                "mode: set",
                "go/jobs/queue.go:31.2,40.14 6 0",
                "go/jobs/store.go:21.2,30.14 4 0",
            ]
        )
        self.assertEqual(
            m.uncovered_diagnostics("go/jobs", profile),
            [
                "check_coverage_baseline: go/jobs: uncovered blocks "
                "(showing 2 of 2, largest first):",
                "go/jobs/queue.go:31-40 (6 stmts)",
                "go/jobs/store.go:21-30 (4 stmts)",
                "check_coverage_baseline: go/jobs: 10 uncovered "
                "statements in 2 blocks",
            ],
        )

    def test_diagnostics_cap_keeps_true_totals(self):
        # The list stops at DIAGNOSTIC_LIMIT block lines, but the
        # summary line still carries the module's true uncovered
        # totals, so the cap never understates the gap.
        profile = "\n".join(
            ["mode: set"]
            + [
                "go/jobs/f%02d.go:1.1,2.2 1 0" % i
                for i in range(m.DIAGNOSTIC_LIMIT + 5)
            ]
        )
        lines = m.uncovered_diagnostics("go/jobs", profile)
        self.assertEqual(len(lines), m.DIAGNOSTIC_LIMIT + 2)
        self.assertEqual(
            lines[0],
            "check_coverage_baseline: go/jobs: uncovered blocks "
            "(showing %d of %d, largest first):"
            % (m.DIAGNOSTIC_LIMIT, m.DIAGNOSTIC_LIMIT + 5),
        )
        self.assertEqual(lines[1], "go/jobs/f00.go:1-2 (1 stmts)")
        self.assertEqual(
            lines[m.DIAGNOSTIC_LIMIT], "go/jobs/f29.go:1-2 (1 stmts)"
        )
        self.assertEqual(
            lines[-1],
            "check_coverage_baseline: go/jobs: %d uncovered statements in "
            "%d blocks" % (m.DIAGNOSTIC_LIMIT + 5, m.DIAGNOSTIC_LIMIT + 5),
        )


class Comparisons(unittest.TestCase):
    """The two-bounds decision rule: the 80.0% floor and the recorded
    baseline, both breached only more than the module's effective
    tolerance below the bound -- the wider of the 0.15-point main
    tolerance and the points 2 statements are worth in that module
    (the measurement-jitter band; see the tool's module docstring)."""

    # Census sizes that put each regime of the calibration under test.
    # In a 4000-statement module two statements are worth 0.05 points
    # (below 0.15), so the main tolerance governs; in a 400-statement
    # module they are worth 0.5 points, so the statement floor governs.
    LARGE_CENSUS = 4000
    SMALL_CENSUS = 400

    def test_effective_tolerance_is_the_wider_of_the_two(self):
        # The main tolerance governs while two statements are worth
        # less than it (more than 1333.33 statements); the two-statement
        # worth governs below that. An empty census has no statement
        # basis, so the main tolerance governs.
        self.assertEqual(m.effective_tolerance(self.LARGE_CENSUS), 0.15)
        self.assertEqual(
            m.effective_tolerance(self.SMALL_CENSUS),
            2 * 100.0 / self.SMALL_CENSUS,
        )
        self.assertEqual(m.effective_tolerance(1333), 200.0 / 1333)
        self.assertEqual(m.effective_tolerance(1334), 0.15)
        self.assertEqual(m.effective_tolerance(0), 0.15)

    def test_pass_above_floor_and_baseline(self):
        self.assertEqual(
            m.comparison_failures(
                84.0, baseline=83.0, total_statements=self.LARGE_CENSUS
            ),
            [],
        )

    def test_pass_within_tolerance_of_the_floor(self):
        # 79.86 + 0.15 clears 80.0: a sub-floor dip inside the jitter
        # band passes, exactly as a dip inside the band passes the
        # baseline comparison.
        self.assertEqual(
            m.comparison_failures(
                79.86, baseline=None, total_statements=self.LARGE_CENSUS
            ),
            [],
        )

    def test_floor_breach_fails_even_above_a_low_baseline(self):
        # A measured total below the floor fails no matter what the
        # baseline file says -- the floor is the product decision, not
        # a re-baselineable row.
        reasons = m.comparison_failures(
            79.5, baseline=79.5, total_statements=self.LARGE_CENSUS
        )
        self.assertEqual(len(reasons), 1)
        self.assertIn("floor", reasons[0])

    def test_decline_breach_fails_above_the_floor(self):
        # A decline against the baseline still fails while the floor
        # holds: the decline gate exists so coverage that merely slides
        # within a high-but-falling band cannot pass.
        reasons = m.comparison_failures(
            83.0, baseline=84.0, total_statements=self.LARGE_CENSUS
        )
        self.assertEqual(len(reasons), 1)
        self.assertIn("baseline", reasons[0])

    def test_both_breaches_report_both_bounds(self):
        reasons = m.comparison_failures(
            79.0, baseline=84.0, total_statements=self.LARGE_CENSUS
        )
        self.assertEqual(len(reasons), 2)
        self.assertIn("floor", reasons[0])
        self.assertIn("baseline", reasons[1])

    def test_floor_edge_is_exact_not_rounded(self):
        # 79.85 + 0.15 == 80.0 exactly: the band edge passes. The
        # comparisons run on unrounded totals, so a floor crossing
        # cannot hide behind display rounding.
        self.assertEqual(
            m.comparison_failures(
                79.85, baseline=None, total_statements=self.LARGE_CENSUS
            ),
            [],
        )
        self.assertEqual(
            len(
                m.comparison_failures(
                    79.84, baseline=None, total_statements=self.LARGE_CENSUS
                )
            ),
            1,
        )

    def test_large_module_main_tolerance_governs(self):
        # 4000 statements: two statements are worth 0.05 points, below
        # the 0.15-point main tolerance, so the main tolerance governs.
        # A 0.15-point drop sits on the band edge and passes; any wider
        # drop fails.
        self.assertEqual(
            m.comparison_failures(
                89.85, baseline=90.0, total_statements=self.LARGE_CENSUS
            ),
            [],
        )
        self.assertEqual(
            len(
                m.comparison_failures(
                    89.84, baseline=90.0, total_statements=self.LARGE_CENSUS
                )
            ),
            1,
        )

    def test_small_module_two_statement_drop_passes(self):
        # 400 statements: the effective tolerance is the two-statement
        # worth, 0.5 points. A drop of exactly two statements sits on
        # the band edge and passes -- under a fixed 0.15-point band it
        # would already have failed, because one statement is 0.25
        # points here.
        self.assertEqual(
            m.comparison_failures(
                89.5, baseline=90.0, total_statements=self.SMALL_CENSUS
            ),
            [],
        )

    def test_small_module_three_statement_drop_fails(self):
        # Three statements are 0.75 points, beyond the 0.5-point band:
        # the floor is a floor, not a licence.
        self.assertEqual(
            len(
                m.comparison_failures(
                    89.25, baseline=90.0, total_statements=self.SMALL_CENSUS
                )
            ),
            1,
        )

    def test_one_statement_of_jitter_passes_a_small_module(self):
        # observability's 424-statement census: one statement is 0.236
        # points, wider than the whole 0.15-point main tolerance, so a
        # single statement of scheduling jitter would red a fixed-band
        # gate. The statement floor (0.47 points here) absorbs it.
        self.assertEqual(
            m.comparison_failures(
                90.0 - 100.0 / 424, baseline=90.0, total_statements=424
            ),
            [],
        )

    def test_small_module_calibrated_band_governs_the_floor_too(self):
        # The calibrated band applies to both comparisons: in a
        # 400-statement module a dip of 0.5 points below the floor
        # still passes (the band is 0.5), while a dip beyond it fails.
        self.assertEqual(
            m.comparison_failures(
                79.5, baseline=None, total_statements=self.SMALL_CENSUS
            ),
            [],
        )
        self.assertEqual(
            len(
                m.comparison_failures(
                    79.49, baseline=None, total_statements=self.SMALL_CENSUS
                )
            ),
            1,
        )

    def test_failure_text_names_the_effective_tolerance_and_census(self):
        # A breach names the tolerance it was judged under and the
        # census that calibrated it, so the number in the message can
        # be recomputed from the module alone.
        baseline_reason = m.comparison_failures(
            89.25, baseline=90.0, total_statements=self.SMALL_CENSUS
        )[0]
        self.assertIn("effective 0.5000-point tolerance", baseline_reason)
        self.assertIn("400-statement census", baseline_reason)
        floor_reason = m.comparison_failures(
            79.4, baseline=None, total_statements=self.SMALL_CENSUS
        )[0]
        self.assertIn("effective 0.5000-point tolerance", floor_reason)
        self.assertIn("400-statement census", floor_reason)


class UpdateRefusal(unittest.TestCase):
    """main's --update refusal path: a measured total breaching the
    floor is refused (exit 1) with its total named, and nothing is
    recorded -- the floor is not a re-baselineable row."""

    def test_sub_floor_measurement_is_refused_with_its_total(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            measurement = m.ModuleMeasurement(
                total=50.0,
                total_statements=100,
                profile_text="mode: set\n",
            )
            stderr = io.StringIO()
            with (
                contextlib.redirect_stderr(stderr),
                unittest.mock.patch.object(
                    m, "current_toolchain", return_value="go1.26.8"
                ),
                unittest.mock.patch.object(
                    m, "measure_module_coverage", return_value=measurement
                ),
            ):
                code = m.main(
                    [
                        "--update",
                        "--module",
                        "go/pkgcore",
                        "--root",
                        str(root),
                    ]
                )
            self.assertEqual(code, 1)
            self.assertIn(
                "update refuses to record go/pkgcore at 50.0000%",
                stderr.getvalue(),
            )
            self.assertFalse(
                (root / "tools" / m.BASELINE_FILE_NAME).exists()
            )


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
