package http

import (
	"strings"
	"testing"

	"github.com/vislake/speed/pkg/core"
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

// panics reports whether fn panicked, without judging that either way. It is
// the shape the cross-check below needs: there both verdicts are observations,
// and it is their agreement that is asserted.
func panics(fn func()) (did bool) {
	defer func() {
		if recover() != nil {
			did = true
		}
	}()
	fn()
	return false
}

// TestCapabilityRuleAgreesWithCore holds this module's copy of the token rule
// against core's. core does not export its judgement and, by the design, will
// not, so the baseline is core's public behaviour: Register panics on an
// illegal token in Requires. Each row observes both verdicts and asserts they
// agree, which is what turns a drift between the two copies into a red test
// rather than two modules quietly disagreeing about what a token is.
//
// The legal row is what makes the agreement mean something: without it a
// capabilityKey that refuses everything would agree with core on every
// remaining row.
//
// Two of the rows are shapes nobody would write by hand. They are here because
// the obvious illegal tokens do not isolate the rule that refuses them: drop
// the pointer rule and a struct token is still refused, by IsNil; drop the
// non-nil rule and &concreteProbe{} is still refused, by the interface rule.
// A nil channel of an interface element type and a non-nil pointer to an
// interface are the shapes each of those two rules alone stands between, so
// they are what turns the rule's removal into a red row.
//
// The untyped-nil rule has no such row and cannot have one: the untyped nil is
// a single value, and removing that rule leaves it refused by the pointer rule
// all the same. What the rule carries is the message, and that is pinned by
// TestTokenRejectsUntypedNil rather than here.
func TestCapabilityRuleAgreesWithCore(t *testing.T) {
	var interfaceValue probe
	cases := []struct {
		name    string
		token   core.Token
		illegal bool
	}{
		{name: "an untyped nil", token: nil, illegal: true},
		{name: "a non-pointer", token: concreteProbe{}, illegal: true},
		{name: "a nil channel of an interface", token: (chan probe)(nil), illegal: true},
		{name: "a non-nil pointer", token: &concreteProbe{}, illegal: true},
		{name: "a non-nil pointer to an interface", token: &interfaceValue, illegal: true},
		{name: "a pointer to a concrete type", token: (*concreteProbe)(nil), illegal: true},
		{name: "a pointer to an interface", token: (*probe)(nil), illegal: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A fresh registry per row: a name collision panics too, and
			// reusing one would read as a verdict on the token.
			byCore := panics(func() {
				core.New().Register(core.Module{
					Name:     "probe-module",
					Requires: []core.Requirement{{Token: c.token}},
				})
			})
			byModule := panics(func() {
				capabilityKey(c.token, `Use on endpoint "public"`)
			})
			if byCore != byModule {
				t.Fatalf("%s: core.Register panicked=%v, capabilityKey panicked=%v. "+
					"The two copies of the capability token rule have drifted apart",
					c.name, byCore, byModule)
			}
			if byModule != c.illegal {
				t.Fatalf("%s: both refused=%v, want %v. "+
					"core and this module agree, so the rule itself has moved",
					c.name, byModule, c.illegal)
			}
		})
	}
}
