package testutil

import (
	"os"
	"strings"
	"testing"
)

// runFailingChildEnv and runPassingChildEnv gate the two helper tests below:
// each helper skips itself unless its env var is set, so it only ever acts
// when invoked as the subprocess of the ExpectFailingSubprocess self-tests —
// the same env-gated-helper pattern ExpectFailingSubprocess's own callers in
// the tenancytest package use, and the same reason: a scenario that must
// fail (or must merely act) has to run in a separate process before its
// outcome can be observed without failing the ordinary suite.
const (
	runFailingChildEnv = "TENANCYTEST_INTERNAL_RUN_FAILING_CHILD_HELPER"
	runPassingChildEnv = "TENANCYTEST_INTERNAL_RUN_PASSING_CHILD_HELPER"
)

// TestExpectFailingSubprocess_FailingChildHelper is not meant to run as part
// of the ordinary suite — it fails itself only when runFailingChildEnv is
// set — and exists solely to be invoked as a subprocess by
// TestExpectFailingSubprocess_ReportsAFailingChild. Its failure message is
// deliberately distinctive so that test can confirm the output it receives
// really came from this test's own failure.
func TestExpectFailingSubprocess_FailingChildHelper(t *testing.T) {
	if os.Getenv(runFailingChildEnv) != "1" {
		t.Skip("only meant to run as TestExpectFailingSubprocess_ReportsAFailingChild's subprocess")
	}
	t.Fatal("deliberate test failure marker for ExpectFailingSubprocess's own self-test")
}

// TestExpectFailingSubprocess_PassingChildHelper is the passing twin of
// TestExpectFailingSubprocess_FailingChildHelper: it skips itself unless
// runPassingChildEnv is set and exists solely to be invoked as a subprocess
// by TestExpectFailingSubprocess_ReportsAPassingChild.
func TestExpectFailingSubprocess_PassingChildHelper(t *testing.T) {
	if os.Getenv(runPassingChildEnv) != "1" {
		t.Skip("only meant to run as TestExpectFailingSubprocess_ReportsAPassingChild's subprocess")
	}
}

// TestExpectFailingSubprocess_ReportsAFailingChild proves the first half of
// ExpectFailingSubprocess's contract: a subprocess whose selected test
// genuinely fails is reported as (output, failed == true), with output
// carrying the failing test's own message so a caller's "which check fired"
// inspection has real text to find. Every one of ExpectFailingSubprocess's
// other callers (tenancytest's negative tests) exercises exactly this
// branch; this is this package's own copy of the proof, so the contract is
// pinned here rather than only by whoever happens to use the helper.
func TestExpectFailingSubprocess_ReportsAFailingChild(t *testing.T) {
	output, failed := ExpectFailingSubprocess(t,
		"TestExpectFailingSubprocess_FailingChildHelper", runFailingChildEnv)
	if !failed {
		t.Fatalf("ExpectFailingSubprocess reported a genuinely failing subprocess as passed; output:\n%s", output)
	}
	if !strings.Contains(output, "deliberate test failure marker") {
		t.Errorf("failing subprocess output does not carry the failing test's own message; output:\n%s", output)
	}
}

// TestExpectFailingSubprocess_ReportsAPassingChild proves the other half of
// the same contract: a subprocess whose selected test passes is reported as
// (output, failed == false). The "--- PASS" assertion rules out the
// degenerate no-tests-matched run, which also exits 0 — the child must have
// genuinely run and passed the selected test for this to count as a
// passing-child report.
func TestExpectFailingSubprocess_ReportsAPassingChild(t *testing.T) {
	output, failed := ExpectFailingSubprocess(t,
		"TestExpectFailingSubprocess_PassingChildHelper", runPassingChildEnv)
	if failed {
		t.Fatalf("ExpectFailingSubprocess reported a genuinely passing subprocess as failed; output:\n%s", output)
	}
	if !strings.Contains(output, "--- PASS") {
		t.Errorf("passing subprocess output does not show the selected test passing; output:\n%s", output)
	}
}
