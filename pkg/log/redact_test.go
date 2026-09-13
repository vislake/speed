package log

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// isolateRules gives a test the rule set to itself. Registrations are
// append-only by contract, so a test that registers would otherwise leave its
// keys behind for every test that runs after it.
func isolateRules(t *testing.T) {
	t.Helper()
	previous := rules.Load()
	rules.Store(&ruleSet{})
	t.Cleanup(func() { rules.Store(previous) })
}

// captureState is what a capturing handler and the handlers derived from it
// share, so a test can read what reached the end of the chain.
type captureState struct {
	mu      sync.Mutex
	records []slog.Record
	enabled bool
}

// captureHandler is a downstream handler that keeps what it is given.
type captureHandler struct {
	state  *captureState
	groups []string
}

func newCapture() *captureHandler {
	return &captureHandler{state: &captureState{enabled: true}}
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return h.state.enabled }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	h.state.records = append(h.state.records, r)
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler {
	return &captureHandler{state: h.state, groups: h.groups}
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	return &captureHandler{state: h.state, groups: append(slices.Clip(h.groups), name)}
}

func (s *captureState) last(t *testing.T) slog.Record {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) == 0 {
		t.Fatal("no record reached the downstream handler")
	}
	return s.records[len(s.records)-1]
}

// valuerFunc is an attribute value that is only resolved when asked. The
// standard library resolves one of these in the terminal handler, which is
// downstream of redaction — so what this layer sees is the unresolved value
// unless it resolves the value itself.
type valuerFunc func() slog.Value

func (f valuerFunc) LogValue() slog.Value { return f() }

// textLogger builds the redaction layer over a text handler, which is how a
// test reads the rendered output a destination would receive.
func textLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	h := newRedactHandler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return slog.New(h), &buf
}

// jsonLogger is textLogger's counterpart for the assertions about structure.
func jsonLogger(opts *slog.HandlerOptions) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	h := newRedactHandler(slog.NewJSONHandler(&buf, opts))
	return slog.New(h), &buf
}

func assertMasked(t *testing.T, out, secret string) {
	t.Helper()
	if strings.Contains(out, secret) {
		t.Errorf("the secret reached the destination in the clear: %s", out)
	}
	if !strings.Contains(out, maskText) {
		t.Errorf("nothing was masked: %s", out)
	}
}

func assertContains(t *testing.T, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Errorf("output does not contain %q: %s", want, out)
	}
}

// TestRedactResolvesLogValuerBeforeKeyMatch hides a credential behind a
// deferred value that resolves to a group. A layer that does not resolve the
// value itself sees the key path "creds" and a value of kind LogValuer: neither
// rule matches, and the credential reaches the destination in the clear.
func TestRedactResolvesLogValuerBeforeKeyMatch(t *testing.T) {
	isolateRules(t)
	const secret = "pw-behind-a-deferred-value"
	processRedaction.AddKeys("password-under-valuer")

	creds := valuerFunc(func() slog.Value {
		return slog.GroupValue(slog.String("password-under-valuer", secret))
	})

	lg, buf := textLogger()
	lg.Info("connect", "creds", creds)

	out := buf.String()
	assertMasked(t, out, secret)
	assertContains(t, out, "creds.password-under-valuer="+maskText)
}

// TestRedactResolvesLogValuerNestedInsideAResolvedGroup puts a second deferred
// value inside the group the first one resolves to. Resolving once at the top
// and then walking the group members leaves this one unresolved, so this case
// is red while the previous one is green.
func TestRedactResolvesLogValuerNestedInsideAResolvedGroup(t *testing.T) {
	isolateRules(t)
	const secret = "tok-two-layers-deep"
	processRedaction.AddKeys("token-nested-under-valuer")

	inner := valuerFunc(func() slog.Value {
		return slog.GroupValue(slog.String("token-nested-under-valuer", secret))
	})
	outer := valuerFunc(func() slog.Value {
		return slog.GroupValue(slog.Any("inner", inner))
	})

	lg, buf := textLogger()
	lg.Info("connect", "creds", outer)

	out := buf.String()
	assertMasked(t, out, secret)
	assertContains(t, out, "creds.inner.token-nested-under-valuer="+maskText)
}

// TestRedactResolvesLogValuerBoundThroughWithAttrs runs the same fixture
// through binding instead of through a record. The standard library resolves
// and preformats a bound attribute at binding time, so a layer that only
// resolves in Handle lets this one through in the clear; the two paths are
// separate code and each needs its own case.
func TestRedactResolvesLogValuerBoundThroughWithAttrs(t *testing.T) {
	isolateRules(t)
	const secret = "tok-bound-two-layers-deep"
	processRedaction.AddKeys("token-bound-under-valuer")

	inner := valuerFunc(func() slog.Value {
		return slog.GroupValue(slog.String("token-bound-under-valuer", secret))
	})
	outer := valuerFunc(func() slog.Value {
		return slog.GroupValue(slog.Any("inner", inner))
	})

	lg, buf := textLogger()
	lg.With("creds", outer).Info("connect")

	out := buf.String()
	assertMasked(t, out, secret)
	assertContains(t, out, "creds.inner.token-bound-under-valuer="+maskText)
}

// TestRedactAppliesPatternToResolvedStringValue pins resolution ahead of the
// value-shape judgement, not only ahead of the key-name one.
func TestRedactAppliesPatternToResolvedStringValue(t *testing.T) {
	isolateRules(t)
	const secret = "sk-live-resolved-string"
	processRedaction.AddPattern("live-key", prefixMatcher("sk-live-"))

	deferred := valuerFunc(func() slog.Value { return slog.StringValue("key " + secret + " end") })

	lg, buf := textLogger()
	lg.Info("charge", "credential", deferred)

	out := buf.String()
	assertMasked(t, out, secret)
	assertContains(t, out, "key "+maskText+" end")
}

// TestRedactAppliesPatternToErrorText pins the other half of the value-shape
// rule's reach: the text of an error, not just a string value.
func TestRedactAppliesPatternToErrorText(t *testing.T) {
	isolateRules(t)
	const secret = "sk-live-inside-an-error"
	processRedaction.AddPattern("live-key", prefixMatcher("sk-live-"))

	lg, buf := textLogger()
	lg.Info("charge", "err", errors.New("refused with "+secret+" attached"))

	out := buf.String()
	assertMasked(t, out, secret)
	assertContains(t, out, "refused with "+maskText+" attached")
}

// TestSourceLocationSurvivesRedaction pins the call site through the rebuild. A
// rebuilt record that drops the program counter makes the source location
// vanish or point at this package instead of the call site.
func TestSourceLocationSurvivesRedaction(t *testing.T) {
	isolateRules(t)
	lg, buf := jsonLogger(&slog.HandlerOptions{Level: slog.LevelDebug, AddSource: true})

	_, wantFile, wantLine, ok := runtime.Caller(0)
	lg.Info("located") // must stay on the line right after runtime.Caller
	if !ok {
		t.Fatal("the test cannot read its own call site")
	}

	var record struct {
		Source struct {
			File string `json:"file"`
			Line int    `json:"line"`
		} `json:"source"`
	}
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("the record is not valid JSON: %v: %s", err, buf.String())
	}
	if record.Source.File != wantFile {
		t.Errorf("source file is %q, want the test file %q", record.Source.File, wantFile)
	}
	if record.Source.Line != wantLine+1 {
		t.Errorf("source line is %d, want %d", record.Source.Line, wantLine+1)
	}
}

// TestRedactedRecordKeepsAttrCountAndHasNoDuplicateKey pins the shape of the
// rebuilt record: a layer that appends a masked copy alongside the original
// leaves the plaintext in place and the key twice over.
func TestRedactedRecordKeepsAttrCountAndHasNoDuplicateKey(t *testing.T) {
	isolateRules(t)
	const secret = "pw-count-and-keys"
	processRedaction.AddKeys("password-count-case")

	capture := newCapture()
	lg := slog.New(newRedactHandler(capture))
	lg.Info("save", "user", "ada", "password-count-case", secret, "attempt", 2)

	got := capture.state.last(t)
	if got.NumAttrs() != 3 {
		t.Errorf("the rebuilt record carries %d attributes, the original carried 3", got.NumAttrs())
	}
	seen := map[string]int{}
	got.Attrs(func(a slog.Attr) bool {
		seen[a.Key]++
		if strings.Contains(fmt.Sprint(a.Value.Any()), secret) {
			t.Errorf("attribute %q still carries the secret", a.Key)
		}
		return true
	})
	for key, count := range seen {
		if count > 1 {
			t.Errorf("key %q appears %d times in the rebuilt record", key, count)
		}
	}
	if seen["password-count-case"] != 1 {
		t.Errorf("the masked attribute is missing: %v", seen)
	}
}

// TestRedactDoesNotAliasTheRecordItWasGiven pins that the record handed to this
// layer is not written into. The copies of a record share a backing array once
// the inline space is full, so the corruption only shows when both holders
// append; the table exists so that at least one size is in the shape that
// aliases.
func TestRedactDoesNotAliasTheRecordItWasGiven(t *testing.T) {
	isolateRules(t)

	for count := 6; count <= 12; count++ {
		t.Run(fmt.Sprintf("attrs=%d", count), func(t *testing.T) {
			capture := newCapture()
			handler := newRedactHandler(capture)

			record := slog.NewRecord(time.Now(), slog.LevelInfo, "alias", 0)
			filler := make([]slog.Attr, 0, 5)
			for i := range 5 {
				filler = append(filler, slog.Int(fmt.Sprintf("inline%d", i), i))
			}
			record.AddAttrs(filler...)
			for i := 5; i < count; i++ {
				record.AddAttrs(slog.Int(fmt.Sprintf("spill%d", i), i))
			}

			holder := record
			if err := handler.Handle(context.Background(), record); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			downstream := capture.state.last(t)
			downstream.AddAttrs(slog.String("downstream-mark", "1"))
			holder.AddAttrs(slog.String("holder-mark", "1"))

			assertMarks(t, "the record the holder kept", holder, "holder-mark", "downstream-mark")
			assertMarks(t, "the record handed downstream", downstream, "downstream-mark", "holder-mark")
		})
	}
}

// assertMarks checks that a record carries its own mark, not the other
// holder's, and that the standard library did not have to report an aliased
// append.
func assertMarks(t *testing.T, what string, r slog.Record, own, foreign string) {
	t.Helper()
	keys := map[string]bool{}
	r.Attrs(func(a slog.Attr) bool {
		keys[a.Key] = true
		return true
	})
	if !keys[own] {
		t.Errorf("%s lost its own attribute %q", what, own)
	}
	if keys[foreign] {
		t.Errorf("%s picked up %q from the other holder", what, foreign)
	}
	if keys["!BUG"] {
		t.Errorf("%s carries the standard library's aliased-append report", what)
	}
}

// TestRedactMatchesGroupNameOpenedByWithGroup is one of the two silent-failure
// cases. The group name WithGroup opened lives in the handler's state, not in
// the record: a layer that does not implement WithGroup, or implements it
// without pushing the name onto its key path, receives the leaf key alone and a
// rule registered against the group name quietly matches nothing.
func TestRedactMatchesGroupNameOpenedByWithGroup(t *testing.T) {
	isolateRules(t)
	const secret = "tok-under-an-opened-group"
	processRedaction.AddKeys("credentials-opened-group")

	lg, buf := textLogger()
	lg.WithGroup("credentials-opened-group").Info("connect", "token", secret)

	out := buf.String()
	assertMasked(t, out, secret)
	assertContains(t, out, "credentials-opened-group.token="+maskText)
}

// TestRedactMatchesAttrBoundUnderWithGroup runs the same registration through
// the binding path, which is WithAttrs rather than Handle. Both paths have to
// read the same prefix; a layer that only pushes the prefix in Handle leaves
// this one red.
func TestRedactMatchesAttrBoundUnderWithGroup(t *testing.T) {
	isolateRules(t)
	const secret = "tok-bound-under-an-opened-group"
	processRedaction.AddKeys("credentials-bound-group")

	lg, buf := textLogger()
	lg.WithGroup("credentials-bound-group").With("token", secret).Info("connect")

	out := buf.String()
	assertMasked(t, out, secret)
	assertContains(t, out, "credentials-bound-group.token="+maskText)
}

// TestRedactMatchesNestedWithGroupPath pins that every segment of the prefix is
// compared, not only the innermost one.
func TestRedactMatchesNestedWithGroupPath(t *testing.T) {
	for _, registered := range []string{"outer-nested-group", "inner-nested-group"} {
		t.Run(registered, func(t *testing.T) {
			isolateRules(t)
			secret := "tok-under-" + registered
			processRedaction.AddKeys(registered)

			lg, buf := textLogger()
			lg.WithGroup("outer-nested-group").WithGroup("inner-nested-group").Info("connect", "token", secret)

			out := buf.String()
			assertMasked(t, out, secret)
			assertContains(t, out, "outer-nested-group.inner-nested-group.token="+maskText)
		})
	}
}

// TestRedactMatchesInlineGroupName pins that a group carried by the record
// itself contributes its name to the key path, just as an opened group does.
func TestRedactMatchesInlineGroupName(t *testing.T) {
	isolateRules(t)
	const secret = "tok-under-an-inline-group"
	processRedaction.AddKeys("credentials-inline-group")

	lg, buf := textLogger()
	lg.Info("connect", slog.Group("credentials-inline-group", slog.String("token", secret)))

	out := buf.String()
	assertMasked(t, out, secret)
	assertContains(t, out, "credentials-inline-group.token="+maskText)
}

// TestRedactWithEmptyGroupNameReturnsReceiver pins the standard library's
// contract for an empty group name, and that no empty segment enters the key
// path.
func TestRedactWithEmptyGroupNameReturnsReceiver(t *testing.T) {
	isolateRules(t)
	const secret = "tok-after-an-empty-group"
	processRedaction.AddKeys("token-after-empty-group")

	handler := newRedactHandler(newCapture())
	derived, ok := handler.WithGroup("").(*redactHandler)
	if !ok || derived != handler {
		t.Errorf("WithGroup(\"\") returned %#v, want the receiver itself", derived)
	}

	lg, buf := textLogger()
	lg.WithGroup("").Info("connect", "token-after-empty-group", secret)

	out := buf.String()
	assertMasked(t, out, secret)
	assertContains(t, out, "token-after-empty-group="+maskText)
	if strings.Contains(out, ".token-after-empty-group") {
		t.Errorf("an empty segment entered the key path: %s", out)
	}
}

// TestRedactWithGroupReachesDownstream pins the other half of WithGroup: a
// layer that keeps the prefix for itself but does not open the group downstream
// masks the right attribute and flattens the output.
func TestRedactWithGroupReachesDownstream(t *testing.T) {
	isolateRules(t)

	lg, buf := textLogger()
	lg.WithGroup("credentials-structure").Info("connect", "token", "public-value")
	assertContains(t, buf.String(), "credentials-structure.token=public-value")

	jlg, jbuf := jsonLogger(&slog.HandlerOptions{Level: slog.LevelDebug})
	jlg.WithGroup("credentials-structure").Info("connect", "token", "public-value")

	var decoded map[string]any
	if err := json.Unmarshal(jbuf.Bytes(), &decoded); err != nil {
		t.Fatalf("the record is not valid JSON: %v: %s", err, jbuf.String())
	}
	group, ok := decoded["credentials-structure"].(map[string]any)
	if !ok {
		t.Fatalf("the group is not an object in the JSON output: %s", jbuf.String())
	}
	if group["token"] != "public-value" {
		t.Errorf("the grouped attribute is %v, want the value it was written with", group["token"])
	}
}

// TestBoundAttrIsJudgedOnceAtBindTime pins the two moments a judgement happens:
// a bound attribute is judged once, when it is bound, and a rule registered
// after that binding does not reach it. This is the whole basis for the order a
// module is asked to take things up in — register, then take a logger.
func TestBoundAttrIsJudgedOnceAtBindTime(t *testing.T) {
	t.Run("matcher runs once per binding", func(t *testing.T) {
		isolateRules(t)
		var calls int
		processRedaction.AddPattern("counting", func(value string) []Span {
			calls++
			return nil
		})

		lg, _ := textLogger()
		bound := lg.With("bind-count-key", "a-plain-value")
		for range 3 {
			bound.Info("tick")
		}

		if calls != 1 {
			t.Errorf("the matcher ran %d times, want once at binding time", calls)
		}
	})

	t.Run("a later registration does not reach a bound attribute", func(t *testing.T) {
		isolateRules(t)
		const value = "value-bound-before-registration"

		lg, buf := textLogger()
		bound := lg.With("bind-time-key", value)
		processRedaction.AddKeys("bind-time-key")
		bound.Info("tick")

		if !strings.Contains(buf.String(), value) {
			t.Errorf("a rule registered after the binding reached it: %s", buf.String())
		}
	})
}

// TestRedactEnabledDelegatesDownstream pins that this layer does not judge the
// level: the level layer at the head of the chain does.
func TestRedactEnabledDelegatesDownstream(t *testing.T) {
	capture := newCapture()
	handler := newRedactHandler(capture)

	if !handler.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Enabled says no while the downstream handler says yes")
	}
	capture.state.enabled = false
	if handler.Enabled(context.Background(), slog.LevelError) {
		t.Error("Enabled says yes while the downstream handler says no")
	}
}

// prefixMatcher reports the span running from a prefix to the next space, which
// is the shape of an API key embedded in a longer text.
func prefixMatcher(prefix string) Matcher {
	return func(value string) []Span {
		var spans []Span
		for at := 0; ; {
			index := strings.Index(value[at:], prefix)
			if index < 0 {
				return spans
			}
			start := at + index
			end := strings.IndexByte(value[start:], ' ')
			if end < 0 {
				spans = append(spans, Span{Start: start, End: len(value)})
				return spans
			}
			spans = append(spans, Span{Start: start, End: start + end})
			at = start + end
		}
	}
}
