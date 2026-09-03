package notification

import (
	"reflect"
	"testing"
)

// TestIsKnownChannel pins the platform's closed channel vocabulary: the three
// canonical names are known, and everything else -- empty, a near-miss, a
// capitalised variant, a name with whitespace -- is not. PreferenceService.Set
// refuses a selection naming anything outside this set (see types.go), so the
// table below is the enumeration a future fourth channel must extend in two
// places at once: the vocabulary switch in types.go and the known-name cases
// here.
func TestIsKnownChannel(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{ChannelInApp, true},
		{ChannelEmail, true},
		{ChannelSMS, true},
		{"", false},
		{"push", false},
		{"In_App", false},
		{"in app", false},
		{"in_app ", false},
		{"email2", false},
	}
	for _, tc := range cases {
		if got := isKnownChannel(tc.name); got != tc.want {
			t.Errorf("isKnownChannel(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSortedChannels pins the canonicalisation every stored selection passes
// through: whatever order the caller listed the channels in, the result is the
// platform's canonical vocabulary order with duplicates removed. The canonical
// order is what makes a preference row's JSON deterministic -- the property
// the delivery subscriber's dedupe-key derivation reasons from.
func TestSortedChannels(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"already canonical", []string{"in_app", "email", "sms"}, []string{"in_app", "email", "sms"}},
		{"reversed", []string{"sms", "email", "in_app"}, []string{"in_app", "email", "sms"}},
		{"shuffled", []string{"sms", "in_app", "email"}, []string{"in_app", "email", "sms"}},
		{"duplicates removed", []string{"in_app", "in_app", "sms", "sms", "sms"}, []string{"in_app", "sms"}},
		{"duplicates across a shuffle", []string{"sms", "in_app", "sms", "email", "in_app"}, []string{"in_app", "email", "sms"}},
		{"single", []string{"email"}, []string{"email"}},
		{"empty", nil, []string{}},
		{"unknown names are dropped", []string{"push", "email", "in_app"}, []string{"in_app", "email"}},
		{"only unknown", []string{"push", "webhook"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sortedChannels(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("sortedChannels(%v) = %v, want %v", tc.in, got, tc.want)
			}
			// The result must never alias the input: a caller mutating one
			// must not corrupt the other.
			if len(tc.in) > 0 && len(got) > 0 && &got[0] == &tc.in[0] {
				t.Error("sortedChannels returned a slice aliasing its input")
			}
		})
	}
}
