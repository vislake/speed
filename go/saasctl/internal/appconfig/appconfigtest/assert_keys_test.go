package appconfigtest

import (
	"strings"
	"testing"
)

// TestDiscrepancies pins the comparison AssertKeys reports through, against
// the real resolved surface: a list naming that surface exactly yields no
// problems, and each way a list can diverge -- a resolved variable dropped,
// a name Load never resolves, a name listed twice -- yields exactly one
// message naming the variable. AssertKeys cannot run its own failure branch
// in-process (a failing assertion fails the test running it), so this is
// where the comparison's reject paths are exercised.
func TestDiscrepancies(t *testing.T) {
	surface := surfaceKeys(t)

	if problems := discrepancies(surface, surface); len(problems) != 0 {
		t.Errorf("a list naming the resolved surface exactly yielded problems: %v", problems)
	}

	dropped := surface[len(surface)-1]
	if problems := discrepancies(surface, surface[:len(surface)-1]); len(problems) != 1 || !strings.Contains(problems[0], dropped) {
		t.Errorf("dropping %s yielded problems %v, want exactly one naming it", dropped, problems)
	}

	const unknown = "APP_NOT_A_BOOTSTRAP_VARIABLE"
	withUnknown := append(append([]string{}, surface...), unknown)
	if problems := discrepancies(surface, withUnknown); len(problems) != 1 || !strings.Contains(problems[0], unknown) {
		t.Errorf("adding %s yielded problems %v, want exactly one naming it", unknown, problems)
	}

	withDuplicate := append(append([]string{}, surface...), surface[0])
	if problems := discrepancies(surface, withDuplicate); len(problems) != 1 || !strings.Contains(problems[0], surface[0]) {
		t.Errorf("duplicating %s yielded problems %v, want exactly one naming it", surface[0], problems)
	}
}
