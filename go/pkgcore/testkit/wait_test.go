package testkit

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitFor_ConditionAlreadyTrue_ReturnsImmediately(t *testing.T) {
	if !waitFor(time.Second, func() bool { return true }) {
		t.Fatal("waitFor reported false for a condition already true")
	}
}

func TestWaitFor_ConditionFlipsWithinBudget_ReturnsTrue(t *testing.T) {
	var open atomic.Bool
	go func() {
		time.Sleep(20 * time.Millisecond)
		open.Store(true)
	}()
	if !waitFor(2*time.Second, open.Load) {
		t.Fatal("waitFor reported false for a condition that flipped within its budget")
	}
}

func TestWaitFor_ConditionNeverTrue_ReturnsFalseAfterTheBudget(t *testing.T) {
	start := time.Now()
	if waitFor(50*time.Millisecond, func() bool { return false }) {
		t.Fatal("waitFor reported true for a condition that never held")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("waitFor returned after %v, before its budget elapsed", elapsed)
	}
}

func TestEventually_ConditionAlreadyTrue_Returns(t *testing.T) {
	Eventually(t, "an already-true condition", func() bool { return true })
}

func TestEventually_ConditionFlipsWithinTheDefaultWindow_Returns(t *testing.T) {
	var open atomic.Bool
	go func() {
		time.Sleep(20 * time.Millisecond)
		open.Store(true)
	}()
	Eventually(t, "a condition that flips asynchronously", open.Load)
}

func TestEventuallyWithin_HonoursTheGivenTimeout(t *testing.T) {
	EventuallyWithin(t, 2*time.Second, "a condition checked against an explicit timeout", func() bool { return true })
}

func TestWaitForRun_ReturnsTheValueLandedOnTheChannel(t *testing.T) {
	runs := make(chan string, 1)
	runs <- "run-1"
	if got := WaitForRun(t, runs, "the handler run"); got != "run-1" {
		t.Fatalf("WaitForRun = %q, want %q", got, "run-1")
	}
}
