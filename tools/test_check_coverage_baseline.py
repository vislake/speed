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
  * measure_module_coverage's failure diagnostic -- a red go test run
    raises with BOTH captured streams, labeled and bounded: the failing
    test's "--- FAIL: TestName" line, which go test prints to stdout,
    stays readable from CI even when stderr is empty, and an oversized
    stream keeps only its tail with the truncation named.
  * captured_streams_report's failure extract -- the whole-stream
    "--- FAIL: " / "panic: " / "fatal error: " lines are pulled ahead
    of the bounded tail, in source order, exact duplicates collapsed,
    capped at STREAM_FAILURE_LINE_LIMIT with the true match count in
    the header, so a long clean tail cannot truncate the failing test's
    name away; a verdict line brings the indented test output beneath
    it along (the assertion text saying why the test failed), while
    the crash prefixes open no such block -- their message rides on
    the same line, followed by a goroutine dump, not test output; a
    stream with no failure line contributes only its tail.
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
import shutil
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
            # The gated set is derived from go.work at run time (with the
            # release coordinator's parser), so the fixture root carries
            # both: go.work registering the module under test, and a copy
            # of the real parser at the path the derivation loads.
            (root / "go.work").write_text(
                "go 1.26.8\n\nuse (\n\t./go/pkgcore\n)\n", encoding="utf-8"
            )
            parser_dir = root / "tools" / "release"
            parser_dir.mkdir(parents=True)
            shutil.copy(
                pathlib.Path(__file__).resolve().parent
                / "release"
                / "lockstep-release.py",
                parser_dir / "lockstep-release.py",
            )
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


class MeasurementFailureDiagnostic(unittest.TestCase):
    """measure_module_coverage's failure message: a red suite raises
    RuntimeError carrying BOTH captured streams, labeled and bounded.
    go test prints a failing test's own output -- the "--- FAIL:
    TestName" line and its log -- to stdout, so a message built from
    stderr alone left a red run with an empty body and no way to name
    the failing test from CI."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = pathlib.Path(tmp.name)
        self.module_dir = "go/widgets"
        module = self.root / self.module_dir
        module.mkdir(parents=True)
        (module / "go.mod").write_text("module example.com/widgets\n")

    def _fail_with(self, stdout, stderr):
        """Run measure_module_coverage with a stubbed go test that
        exited 1 with the given captured streams, returning the raised
        RuntimeError's message."""
        proc = unittest.mock.Mock(returncode=1, stdout=stdout, stderr=stderr)
        with unittest.mock.patch.object(
            m.subprocess, "run", return_value=proc
        ):
            with self.assertRaises(RuntimeError) as raised:
                m.measure_module_coverage(self.root, self.module_dir)
        return str(raised.exception)

    def test_stdout_carries_the_failing_test_verdict(self):
        # The regression this pins: with only stderr in the message, a
        # red run whose failure lives on stdout (as go test prints it)
        # reported no failing test name.
        stdout = (
            "=== RUN   TestWidget\n"
            "    widget_test.go:9: Add(2,2) = 4, want 5\n"
            "--- FAIL: TestWidget (0.00s)\n"
            "FAIL\n"
            "FAIL\texample.com/widgets\t0.005s\n"
        )
        message = self._fail_with(stdout, "")
        self.assertIn("--- FAIL: TestWidget", message)
        self.assertIn("Add(2,2) = 4, want 5", message)
        self.assertIn("exit 1", message)
        self.assertIn(self.module_dir, message)

    def test_both_streams_are_labeled_and_present(self):
        stdout, stderr = "go test stdout body\n", "go: tooling stderr\n"
        message = self._fail_with(stdout, stderr)
        self.assertIn("stdout (%d chars):\ngo test stdout body"
                      % len(stdout), message)
        self.assertIn("stderr (%d chars):\ngo: tooling stderr"
                      % len(stderr), message)

    def test_each_stream_keeps_only_its_bounded_tail(self):
        text = "HEAD-MARKER " + "x" * 5000 + "\nTAIL-MARKER verdict\n"
        message = self._fail_with(text, "")
        self.assertIn("TAIL-MARKER verdict", message)
        self.assertNotIn("HEAD-MARKER", message)
        self.assertIn(
            "stdout (last %d of %d chars)" % (m.STREAM_TAIL_LIMIT, len(text)),
            message,
        )
        self.assertIn("stderr (empty)", message)

    def test_empty_streams_are_named_empty(self):
        message = self._fail_with("", "")
        self.assertIn("stdout (empty)", message)
        self.assertIn("stderr (empty)", message)


class StreamFailureExtraction(unittest.TestCase):
    """captured_streams_report's whole-stream failure extract: the
    lines that name why a run went red -- the go test per-test
    "--- FAIL: TestName" verdict and the "panic: " / "fatal error: "
    crash lines -- are pulled from the whole stream ahead of the
    bounded tail, so a long clean tail cannot truncate the failing
    test's name away. A verdict line brings the indented test output
    beneath it along -- the "file_test.go:123: ..." lines carrying the
    assertion text -- so the reason the test failed travels with the
    line that names it. Extraction keeps source order, collapses exact
    duplicates, caps at STREAM_FAILURE_LINE_LIMIT with the true match
    count in the header, and a stream with no failure line contributes
    only its tail (the tail keeping its own role)."""

    # One clean package-verdict line; enough of them push anything
    # before them beyond STREAM_TAIL_LIMIT characters from the end.
    CLEAN_LINE = "ok  \texample.com/widgets/alpha\t0.05s\n"

    def _long_clean_tail(self):
        return self.CLEAN_LINE * 100

    def test_fail_line_beyond_the_tail_still_names_the_test(self):
        # A run whose "--- FAIL: TestName" verdict prints far from the
        # stream's end, with clean package output after it, must still
        # name the test: the retained tail alone does not carry it.
        fail_line = "--- FAIL: TestRace (0.31s)"
        stdout = (
            "=== RUN   TestRace\n%s\n%sFAIL\n"
            % (fail_line, self._long_clean_tail())
        )
        # Fixture invariant: the verdict line really sits outside the
        # retained window, so the extract is the only thing that can
        # carry it into the report.
        self.assertGreater(
            len(stdout) - stdout.index(fail_line), m.STREAM_TAIL_LIMIT
        )
        self.assertNotIn(fail_line, stdout[-m.STREAM_TAIL_LIMIT:])
        report = m.captured_streams_report(stdout, "")
        self.assertIn(
            "stdout failure lines (showing 1 of 1):\n" + fail_line, report
        )

    def test_assertion_lines_below_the_verdict_survive_the_tail(self):
        # go test prints a failed test's buffered log -- the
        # "file_test.go:123: ..." assertion lines -- directly beneath
        # its "--- FAIL: TestName" verdict line: the verdict names the
        # test, the indented lines say why it failed. A long clean tail
        # can push the whole block beyond STREAM_TAIL_LIMIT characters,
        # so the extract must carry the indented lines with the verdict
        # -- a red run that reports only the test's name but not its
        # assertion is the gap this pins.
        fail_line = "--- FAIL: TestRace (11.06s)"
        assertion = (
            "    service_test.go:1097: iteration 3: no view was recorded "
            "before the revoke deadline"
        )
        stdout = (
            "=== RUN   TestRace\n%s\n%s\n%sFAIL\n"
            % (fail_line, assertion, self._long_clean_tail())
        )
        # Fixture invariant: the assertion really sits outside the
        # retained window, so the extract is the only thing that can
        # carry it into the report.
        self.assertGreater(
            len(stdout) - stdout.index(assertion), m.STREAM_TAIL_LIMIT
        )
        self.assertNotIn(assertion, stdout[-m.STREAM_TAIL_LIMIT:])
        report = m.captured_streams_report(stdout, "")
        self.assertIn(
            "stdout failure lines (showing 2 of 2):\n%s\n%s"
            % (fail_line, assertion),
            report,
        )

    def test_detail_block_is_exactly_the_indented_output(self):
        # The block a verdict line opens is the indented test output
        # beneath it: the first unindented line -- a package verdict,
        # the stream trailer -- ends the block, so unrelated stream
        # text is never swept into the extract.
        stdout = (
            "--- FAIL: TestAlpha (0.00s)\n"
            "    alpha_test.go:7: want 2, got 3\n"
            "FAIL\n"
            "FAIL\texample.com/widgets\t0.005s\n"
        )
        self.assertEqual(
            m.failure_lines_from_stream(stdout),
            [
                "--- FAIL: TestAlpha (0.00s)",
                "    alpha_test.go:7: want 2, got 3",
            ],
        )

    def test_detail_blocks_interleave_with_later_verdicts_in_order(self):
        # Two failing tests each bring their own assertion lines: the
        # extract interleaves verdicts and their blocks in source order,
        # so which assertion belongs to which test stays readable.
        stdout = (
            "--- FAIL: TestAlpha (0.00s)\n"
            "    alpha_test.go:7: want 2, got 3\n"
            "--- FAIL: TestBeta (0.00s)\n"
            "    beta_test.go:9: connection refused\n"
            "FAIL\n"
        )
        self.assertEqual(
            m.failure_lines_from_stream(stdout),
            [
                "--- FAIL: TestAlpha (0.00s)",
                "    alpha_test.go:7: want 2, got 3",
                "--- FAIL: TestBeta (0.00s)",
                "    beta_test.go:9: connection refused",
            ],
        )

    def test_duplicate_detail_lines_collapse_like_verdict_lines(self):
        # Dedup covers the whole extract: a test failing the same
        # assertion in a loop prints the identical line once per
        # iteration, and the extract collapses exact duplicates in
        # source order exactly as it already does for verdict lines.
        fail_line = "--- FAIL: TestRace (0.31s)"
        assertion = "    race_test.go:42: view was dropped"
        stdout = (
            "%s\n%s\n%s\n%s\n"
            % (fail_line, assertion, assertion, self._long_clean_tail())
        )
        report = m.captured_streams_report(stdout, "")
        self.assertIn(
            "stdout failure lines (showing 2 of 2):\n%s\n%s"
            % (fail_line, assertion),
            report,
        )
        self.assertEqual(report.count(assertion), 1)

    def test_detail_lines_fill_the_cap_and_the_header_counts_them(self):
        # The cap governs the whole extract, verdicts and detail lines
        # alike: a verdict whose block of distinct assertion lines is
        # longer than the window fills it, and the header's true count
        # keeps the cap from hiding the block's size.
        assertions = [
            "    race_test.go:%d: iteration %d diverged" % (i, i)
            for i in range(m.STREAM_FAILURE_LINE_LIMIT + 5)
        ]
        stdout = (
            "--- FAIL: TestRace (0.31s)\n"
            + "\n".join(assertions)
            + "\n"
            + self._long_clean_tail()
        )
        report = m.captured_streams_report(stdout, "")
        self.assertIn(
            "stdout failure lines (showing %d of %d):"
            % (m.STREAM_FAILURE_LINE_LIMIT, len(assertions) + 1),
            report,
        )
        self.assertIn(assertions[m.STREAM_FAILURE_LINE_LIMIT - 2], report)
        self.assertNotIn(assertions[m.STREAM_FAILURE_LINE_LIMIT - 1], report)

    def test_crash_lines_do_not_open_a_detail_block(self):
        # "panic: " and "fatal error: " carry their message on the same
        # line; what follows them is a goroutine dump, not the failing
        # test's own output. The extract keeps the crash line and
        # leaves the dump to the tail window, so a crash report does
        # not fill the extract with stack frames.
        stdout = (
            "panic: nil map write\n"
            "\tgoroutine 1 [running]:\n"
            "\tmain.main()\n"
            + self._long_clean_tail()
        )
        report = m.captured_streams_report(stdout, "")
        self.assertIn(
            "stdout failure lines (showing 1 of 1):\npanic: nil map write",
            report,
        )
        self.assertNotIn("goroutine 1", report)

    def test_panic_and_fatal_error_lines_are_extracted(self):
        # A test-binary crash may print no "--- FAIL: " verdict at all
        # (a runtime fault outside test accounting), so the crash's own
        # opening lines are extracted too.
        stdout = (
            "=== RUN   TestCrash\n"
            "panic: nil map write\n"
            "fatal error: all goroutines are asleep - deadlock!\n"
            + self._long_clean_tail()
        )
        report = m.captured_streams_report(stdout, "")
        self.assertIn(
            "stdout failure lines (showing 2 of 2):\n"
            "panic: nil map write\n"
            "fatal error: all goroutines are asleep - deadlock!",
            report,
        )

    def test_exact_duplicates_collapse_keeping_source_order(self):
        stdout = (
            "--- FAIL: TestAlpha (0.00s)\n"
            "--- FAIL: TestBeta (0.00s)\n"
            "--- FAIL: TestAlpha (0.00s)\n"
            + self._long_clean_tail()
        )
        report = m.captured_streams_report(stdout, "")
        self.assertIn("stdout failure lines (showing 2 of 2):", report)
        self.assertEqual(report.count("--- FAIL: TestAlpha (0.00s)"), 1)
        self.assertEqual(report.count("--- FAIL: TestBeta (0.00s)"), 1)
        self.assertLess(report.index("TestAlpha"), report.index("TestBeta"))

    def test_extract_is_capped_with_the_true_count_in_the_header(self):
        # A pathological run (one "panic:" line per test in a crash
        # loop) cannot fill the report: the extract stops at
        # STREAM_FAILURE_LINE_LIMIT, and the header's true match count
        # keeps the cap from hiding the failure set's size.
        fail_lines = [
            "--- FAIL: Test%03d (0.00s)" % i
            for i in range(m.STREAM_FAILURE_LINE_LIMIT + 5)
        ]
        stdout = "\n".join(fail_lines) + "\n" + self._long_clean_tail()
        report = m.captured_streams_report(stdout, "")
        self.assertIn(
            "stdout failure lines (showing %d of %d):"
            % (m.STREAM_FAILURE_LINE_LIMIT, len(fail_lines)),
            report,
        )
        self.assertIn(fail_lines[0], report)
        self.assertIn(fail_lines[m.STREAM_FAILURE_LINE_LIMIT - 1], report)
        self.assertNotIn(fail_lines[m.STREAM_FAILURE_LINE_LIMIT], report)
        self.assertNotIn(fail_lines[-1], report)

    def test_stream_without_failure_lines_renders_only_its_labeled_tail(self):
        # No failure line: no extract section, the report is the
        # labeled tail alone, byte for byte.
        stdout = (
            "=== RUN   TestWidget\n"
            "--- PASS: TestWidget (0.00s)\n"
            "PASS\n"
        )
        self.assertEqual(
            m.captured_streams_report(stdout, ""),
            "stdout (%d chars):\n"
            "=== RUN   TestWidget\n"
            "--- PASS: TestWidget (0.00s)\n"
            "PASS\n"
            "stderr (empty)" % len(stdout),
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
