package log

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// TestRuleSetSnapshotIsReadWithoutLock pins the two properties of the snapshot:
// a judgement never blocks a registration, and a record is judged against one
// snapshot from end to end. A layer that reads the pointer per attribute
// instead of per record produces records that are masked in part, which is the
// shape this case rules out.
func TestRuleSetSnapshotIsReadWithoutLock(t *testing.T) {
	isolateRules(t)
	const key = "snapshot-consistency-key"
	const attrsPerRecord = 8
	const records = 500

	capture := newCapture()
	logger := slog.New(newRedactHandler(capture))
	attrs := make([]slog.Attr, attrsPerRecord)
	for i := range attrs {
		attrs[i] = slog.String(key, "plain-value")
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range records {
			logger.LogAttrs(context.Background(), slog.LevelInfo, "judge", attrs...)
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 50 {
			processRedaction.AddKeys(fmt.Sprintf("noise-key-%d", i))
		}
		processRedaction.AddKeys(key)
	}()
	wg.Wait()

	capture.state.mu.Lock()
	seen := append([]slog.Record(nil), capture.state.records...)
	capture.state.mu.Unlock()

	for i, record := range seen {
		masked := 0
		record.Attrs(func(a slog.Attr) bool {
			if a.Value.String() == MaskedText {
				masked++
			}
			return true
		})
		if masked != 0 && masked != attrsPerRecord {
			t.Fatalf("record %d was judged against two snapshots: %d of %d attributes masked",
				i, masked, attrsPerRecord)
		}
	}

	// The registration has completed by now, so the next record is fully
	// masked; without this the case would pass on a rule set nobody wrote to.
	logger.LogAttrs(context.Background(), slog.LevelInfo, "judge", attrs...)
	last := capture.state.last(t)
	last.Attrs(func(a slog.Attr) bool {
		if a.Value.String() != MaskedText {
			t.Errorf("a record written after the registration carries %q", a.Value.String())
		}
		return true
	})
}

// TestAddKeysIsAppendOnly pins that a registration adds to the rule set rather
// than replacing it: a key registered earlier still matches afterwards.
func TestAddKeysIsAppendOnly(t *testing.T) {
	isolateRules(t)
	processRedaction.AddKeys("first-registered-key")
	processRedaction.AddPattern("first-pattern", prefixMatcher("sk-first-"))
	processRedaction.AddKeys("second-registered-key")
	processRedaction.AddPattern("second-pattern", prefixMatcher("sk-second-"))

	rules := currentRules()
	for _, key := range []string{"first-registered-key", "second-registered-key"} {
		if !rules.matchesKey(key) {
			t.Errorf("%q no longer matches after a later registration", key)
		}
	}
	for _, secret := range []string{"sk-first-value", "sk-second-value"} {
		masked, ok := rules.maskString("carrying " + secret + " onward")
		if !ok || strings.Contains(masked, secret) {
			t.Errorf("%q is not masked after a later registration: %q", secret, masked)
		}
	}
}

// TestPatternMasksOnlyTheMatchedSpans pins that a value-shape rule masks what it
// reported and leaves the rest of the text alone, which is what makes the rule
// usable on a message-like value.
func TestPatternMasksOnlyTheMatchedSpans(t *testing.T) {
	isolateRules(t)
	processRedaction.AddPattern("live-key", prefixMatcher("sk-live-"))

	got, ok := currentRules().maskString("head sk-live-one middle sk-live-two tail")
	if !ok {
		t.Fatal("nothing was masked")
	}
	want := "head " + MaskedText + " middle " + MaskedText + " tail"
	if got != want {
		t.Errorf("masked value is %q, want %q", got, want)
	}
}

// TestKeyPathMatchesAnySegment pins that a registered name is compared against
// every segment of the key path, so a group name matches as well as a leaf key.
func TestKeyPathMatchesAnySegment(t *testing.T) {
	for _, registered := range []string{"a-path-segment", "token-path-leaf"} {
		t.Run(registered, func(t *testing.T) {
			isolateRules(t)
			secret := "value-under-" + registered
			processRedaction.AddKeys(registered)

			lg, buf := textLogger()
			lg.Info("walk", slog.Group("a-path-segment",
				slog.Group("b-path-segment", slog.String("token-path-leaf", secret))))

			out := buf.String()
			assertMasked(t, out, secret)
			assertContains(t, out, "a-path-segment.b-path-segment.token-path-leaf="+MaskedText)
		})
	}
}

// TestMalformedSpansAreIgnored pins that a matcher from another module cannot
// take the logging path of the whole process down with a bad offset. The spans
// here are the four ways to be out of range; none of them may panic, and none
// of them may mask anything.
func TestMalformedSpansAreIgnored(t *testing.T) {
	isolateRules(t)
	const value = "0123456789"
	processRedaction.AddPattern("malformed", func(string) []Span {
		return []Span{
			{Start: -3, End: 4},
			{Start: 2, End: len(value) + 5},
			{Start: 6, End: 6},
			{Start: 8, End: 3},
		}
	})

	got, ok := currentRules().maskString(value)
	if ok || got != value {
		t.Errorf("a malformed span changed the value: %q", got)
	}
}

// TestOverlappingSpansAreMaskedOnce pins that two rules reporting overlapping
// spans of one value produce one mask over the union, not a mask per rule.
func TestOverlappingSpansAreMaskedOnce(t *testing.T) {
	isolateRules(t)
	processRedaction.AddPattern("left", func(string) []Span { return []Span{{Start: 5, End: 12}} })
	processRedaction.AddPattern("right", func(string) []Span { return []Span{{Start: 9, End: 19}} })

	got, ok := currentRules().maskString("head sensitive-part tail")
	if !ok {
		t.Fatal("nothing was masked")
	}
	want := "head " + MaskedText + " tail"
	if got != want {
		t.Errorf("masked value is %q, want %q", got, want)
	}
}

// TestUnusableRegistrationsPanic pins that a registration carrying no rule
// fails at the call site instead of being dropped. An empty key name would
// have to be compared against the empty segments of a key path, and a nil
// matcher would panic on the first record that reached it; both are
// programming errors at the registering call site.
//
// Dropping one quietly is the failure shape redaction can least afford: a
// registration returns nothing, so the registrant goes on believing it holds a
// protection it does not have, and the difference shows up only once something
// has leaked.
func TestUnusableRegistrationsPanic(t *testing.T) {
	// The assertions name the position of the unusable argument, not its
	// value: the value is the empty string, and AddKeys is variadic, so only
	// the index says which of the names a caller passed is at fault.
	t.Run("empty key name", func(t *testing.T) {
		isolateRules(t)
		got := panicText(t, func() { processRedaction.AddKeys("") })
		if !strings.Contains(got, "AddKeys[0]") {
			t.Errorf("the panic reads %q, want it to name the position of the unusable name", got)
		}
	})

	t.Run("nil matcher", func(t *testing.T) {
		isolateRules(t)
		got := panicText(t, func() { processRedaction.AddPattern("nil-matcher", nil) })
		for _, want := range []string{"AddPattern", "nil-matcher"} {
			if !strings.Contains(got, want) {
				t.Errorf("the panic reads %q, want it to name %q", got, want)
			}
		}
	})

	// The whole call is checked before anything is registered, so a call that
	// carries one usable name and one unusable one registers neither. An
	// implementation that wrote each name as it walked them, and panicked only
	// on reaching the empty one, passes the two cases above and leaves half a
	// registration behind.
	t.Run("nothing is registered before the check", func(t *testing.T) {
		isolateRules(t)
		panicText(t, func() { processRedaction.AddKeys("kept", "") })
		if currentRules().matchesKey("kept") {
			t.Error("the name ahead of the unusable one was registered")
		}
	})
}

// panicText runs fn and renders what it panicked with, for an assertion on the
// message. It fails the test when fn returns normally.
func panicText(t *testing.T, fn func()) string {
	t.Helper()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		fn()
	}()
	if recovered == nil {
		t.Fatal("the registration returned instead of panicking")
	}
	return fmt.Sprint(recovered)
}
