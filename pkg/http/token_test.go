package http

import (
	"strings"
	"testing"
)

// probe is a capability an After or Before declaration may point at. The
// legality of a token has nothing to do with the methods, so one is enough.
type probe interface{ Probe() }

// concreteProbe is a struct, and therefore the shape a capability token may
// not point at.
type concreteProbe struct{}

// wantPanic runs fn and returns the panic text, failing when fn returns
// normally. The text is handed back rather than asserted here: this module
// panics where the mistake is a call-site one, and a refusal that says nothing
// about which call was refused leaves the author exactly where they were.
func wantPanic(t *testing.T, what string, fn func()) string {
	t.Helper()
	var text string
	func() {
		defer func() {
			if r := recover(); r != nil {
				text, _ = r.(string)
			}
		}()
		fn()
		t.Fatalf("%s returned instead of panicking", what)
	}()
	if text == "" {
		t.Fatalf("%s panicked with something other than a message string", what)
	}
	return text
}

// assertNames checks that the panic text carries the offending declaration and
// the fixing action. Refusing without saying what to write instead leaves the
// author of the declaration exactly where they were.
func assertNames(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Errorf("the panic text %q does not carry %q", text, fragment)
		}
	}
}

func TestTokenRejectsUntypedNil(t *testing.T) {
	text := wantPanic(t, "an untyped nil token", func() {
		capabilityKey(nil, `Use on endpoint "public"`)
	})
	assertNames(t, text, `Use on endpoint "public"`, "untyped nil", "(*auth.Authenticator)(nil)")
}

func TestTokenRejectsNonPointer(t *testing.T) {
	text := wantPanic(t, "a non-pointer token", func() {
		capabilityKey(concreteProbe{}, `Use on endpoint "public"`)
	})
	assertNames(t, text, `Use on endpoint "public"`, "must be a typed nil pointer", "http.concreteProbe")
}

func TestTokenRejectsNonNilPointer(t *testing.T) {
	text := wantPanic(t, "a non-nil pointer token", func() {
		capabilityKey(&concreteProbe{}, `Use on endpoint "public"`)
	})
	assertNames(t, text, `Use on endpoint "public"`, "must be a nil pointer", "not an instance")
}

func TestTokenRejectsConcreteElement(t *testing.T) {
	text := wantPanic(t, "a token pointing at a concrete type", func() {
		capabilityKey((*concreteProbe)(nil), `Use on endpoint "public"`)
	})
	assertNames(t, text, `Use on endpoint "public"`, "must point to an interface", "concreteProbe")
}

// TestTokenAcceptsInterfacePointer is the positive half: the four refusals
// above would all still hold in an implementation that refuses everything.
func TestTokenAcceptsInterfacePointer(t *testing.T) {
	got := capabilityKey((*probe)(nil), `Use on endpoint "public"`)
	if got == nil {
		t.Fatal("a legal capability token yielded no key")
	}
	if got.Name() != "probe" {
		t.Errorf("the key is %s, want the interface type probe", got)
	}
	if same := capabilityKey((*probe)(nil), "another site"); same != got {
		t.Error("two tokens designating one capability yielded different keys, " +
			"so the middleware graph would treat them as two capabilities")
	}
}
