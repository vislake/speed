package queuetest

import (
	"context"
	"testing"
	"time"
)

// This file proves the fault tier's checks have teeth: each check returns
// an error rather than failing a test directly (see assert_fails_closed.go),
// so these tests drive the checks against a queue in the swallow mode —
// one that drops the injected cancellation-state read failure and answers
// its Jobs' natural record state, the exact behaviour the tier exists to
// reject — and require the check to reject it. A check
// that accepted the swallowing queue would be a constant-true harness:
// no better than no injection tier at all, since a rejection nobody proved
// possible is a rejection nobody can trust (a passing test that cannot fail
// does not count). Each test first runs the same check against the
// fail-closed twin of the same fake machinery (only the swallow flag
// differs), so the rejection below is the swallow's own work — the one
// dimension the tier exists to detect — never a brokenness of the fake's
// unrelated machinery.

// TestGetCheck_RejectsAQueueThatSwallowsTheCancellationReadFailure pins the
// Get check's rejection direction: on a swallowing queue, Get under the
// sabotage answers the Job's natural record state with nil error — a
// possibly-cancelled Job reported as if the read had succeeded — and the
// check must refuse to accept that as conformant.
func TestGetCheck_RejectsAQueueThatSwallowsTheCancellationReadFailure(t *testing.T) {
	// The fail-closed twin passes first: the rejection below is the
	// swallow's own doing, not the fake's.
	if err := checkGetFailsClosedOnUnreadableCancellationState(newFaultTierFake(false)); err != nil {
		t.Fatalf("the Get check rejected the fail-closed twin of the same fake machinery: %v", err)
	}

	q := newFaultTierFake(true)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = q.Close(ctx)
	})
	err := checkGetFailsClosedOnUnreadableCancellationState(q)
	if err == nil {
		t.Fatal("the Get check accepted a queue that swallows the cancellation-state read failure and reports its Jobs' natural state: the fail-closed assertion has no teeth")
	}
}

// TestDeadLetterCheck_RejectsAQueueThatSwallowsTheCancellationReadFailure
// pins the DeadLetterJobs check's rejection direction the same way: a
// swallowing listing answers with the Job's natural StatusDeadLetter, and
// the check must refuse it.
func TestDeadLetterCheck_RejectsAQueueThatSwallowsTheCancellationReadFailure(t *testing.T) {
	if err := checkDeadLetterListingFailsClosedOnUnreadableCancellationState(newFaultTierFake(false)); err != nil {
		t.Fatalf("the DeadLetterJobs check rejected the fail-closed twin of the same fake machinery: %v", err)
	}

	q := newFaultTierFake(true)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = q.Close(ctx)
	})
	err := checkDeadLetterListingFailsClosedOnUnreadableCancellationState(q)
	if err == nil {
		t.Fatal("the DeadLetterJobs check accepted a queue that swallows the cancellation-state read failure and lists its Jobs' natural state: the fail-closed assertion has no teeth")
	}
}
