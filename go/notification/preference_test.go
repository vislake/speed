package notification

import (
	"reflect"
	"testing"

	"gorm.io/datatypes"
)

// TestChannelsJSON_ParseChannels_RoundTrip proves the channels column's
// encoding contract: channelsJSON writes the selection as a JSON array of
// strings, and parseChannels decodes exactly that back. The order written is
// the order given -- channelsJSON trusts its caller to have canonicalised
// (PreferenceService.Set does), and the round trip must preserve it.
func TestChannelsJSON_ParseChannels_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   []string
	}{
		{"full selection", []string{"in_app", "email", "sms"}},
		{"narrow selection", []string{"email"}},
		{"canonical order preserved", []string{"sms", "email", "in_app"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := channelsJSON(tc.in)
			got, err := parseChannels(stored)
			if err != nil {
				t.Fatalf("parseChannels: %v", err)
			}
			if !reflect.DeepEqual(got, tc.in) {
				t.Errorf("round trip = %v, want %v", got, tc.in)
			}
		})
	}
}

// TestChannelsJSON_EmptySelection_StoresEmptyArrayNotNULL pins the opt-out
// encoding: the empty array is the stored form of a legal opt-out, and a NULL
// would blur "opted out" into "no row" -- the two have different meanings in
// this matrix (defaults vs. nothing). The column is NOT NULL at the schema
// level; this test pins the same contract at the value level, where the
// datatypes.JSON zero value would otherwise smuggle a NULL through.
func TestChannelsJSON_EmptySelection_StoresEmptyArrayNotNULL(t *testing.T) {
	stored := channelsJSON(nil)
	if stored == nil {
		t.Fatal("channelsJSON(nil) returned nil; an empty selection must store the JSON empty array, never NULL")
	}
	if string(stored) != "[]" {
		t.Errorf("channelsJSON(nil) = %s, want []", stored)
	}

	got, err := parseChannels(stored)
	if err != nil {
		t.Fatalf("parseChannels: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("parseChannels of an empty selection = %v (len %d), want a non-nil empty slice", got, len(got))
	}
}

// TestParseChannels_CorruptStoredValue_Fails proves a stored value that is
// not a JSON array of strings is a corrupt row: the column is written only by
// channelsJSON, so anything else -- a JSON object, a bare string, a truncated
// array -- can only have arrived by hand-editing or a bug, and ResolveChannels
// must surface it as an error rather than deliver on a guessed reading.
func TestParseChannels_CorruptStoredValue_Fails(t *testing.T) {
	corrupt := []datatypes.JSON{
		datatypes.JSON(`{"in_app":true}`),
		datatypes.JSON(`"in_app"`),
		datatypes.JSON(`["in_app",`),
		datatypes.JSON(``),
	}
	for _, stored := range corrupt {
		if _, err := parseChannels(stored); err == nil {
			t.Errorf("parseChannels(%s) succeeded, want an error", stored)
		}
	}
}
