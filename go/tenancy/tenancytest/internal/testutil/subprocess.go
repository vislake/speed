package testutil

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// ExpectFailingSubprocess re-runs the current test binary (os.Args[0]) with
// -test.run anchored to exactly testName and the given environment variable
// set to "1", and reports whether that single test failed.
//
// This exists because Go's testing package gives a failing subtest no way
// to avoid marking its parent failed too: (*testing.common).Fail walks
// c.parent all the way to the top-level test unconditionally —
//
//	func (c *common) Fail() {
//		if c.parent != nil {
//			c.parent.Fail()
//		}
//		...
//	}
//
// — so a package's own negative test ("prove this assertion helper detects
// a real violation") cannot run the failing scenario through an ordinary
// t.Run and merely inspect its bool return: by the time Run returns, the
// failure has already been recorded against every ancestor, including the
// test binary's own exit code for the whole package. Re-running the
// scenario in a separate OS process and inspecting only that process's exit
// code is the standard way around this — the same technique net/http and
// os/exec's own test suites use to test code that is expected to fail or
// exit.
//
// testName must name an existing top-level Test function in the same
// package, guarded to do nothing unless envVar is set to "1" — see this
// package's own callers for the pattern. output is the subprocess's
// combined stdout+stderr, returned so a caller can additionally confirm
// which specific check fired, not merely that some check did.
func ExpectFailingSubprocess(t *testing.T, testName, envVar string) (output string, failed bool) {
	t.Helper()

	// gosec's G204 flags exec.Command calls built from a variable argument
	// on principle, since that is usually how untrusted input reaches a
	// subprocess. Neither argument here is untrusted: os.Args[0] is this
	// same test binary's own path (not attacker-influenced), and testName
	// is always a Go string literal a caller in this same package hard-codes
	// at the call site (see ExpectFailingSubprocess's callers) — never data
	// read from a request, a file, or any other external source.
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$", "-test.v") //nolint:gosec // testName is always an internal compile-time literal, never external input; see comment above
	cmd.Env = append(os.Environ(), envVar+"=1")
	raw, err := cmd.CombinedOutput()
	output = string(raw)

	if err == nil {
		return output, false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("subprocess for %q failed to run at all (not a reported test failure): %v\noutput:\n%s", testName, err, output)
	}
	return output, true
}
