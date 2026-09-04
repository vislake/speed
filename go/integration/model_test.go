package integration

import (
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/vislake/speed/go/dbkit"
)

// TestAPIKey_ImplementsTenantScoped is the compile-time assertion's runtime
// companion: proves GetTenantID actually returns what Create wrote, not
// merely that the method exists.
func TestAPIKey_ImplementsTenantScoped(t *testing.T) {
	k := APIKey{TenantModel: dbkit.TenantModel{TenantID: "tenant-1"}}
	if got := k.GetTenantID(); string(got) != "tenant-1" {
		t.Errorf("GetTenantID() = %q, want %q", got, "tenant-1")
	}
}

func TestAPIKey_IsRevoked(t *testing.T) {
	live := APIKey{}
	if live.IsRevoked() {
		t.Error("IsRevoked() = true for a key with a nil RevokedAt, want false")
	}
	now := time.Now()
	revoked := APIKey{RevokedAt: &now}
	if !revoked.IsRevoked() {
		t.Error("IsRevoked() = false for a key with RevokedAt set, want true")
	}
}

func TestAPIKey_IsExpired(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"future", now.Add(time.Hour), false},
		{"exactly now", now, true},
		{"past", now.Add(-time.Hour), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := APIKey{ExpiresAt: tt.expiresAt}
			if got := k.IsExpired(now); got != tt.want {
				t.Errorf("IsExpired(%v) = %v, want %v", now, got, tt.want)
			}
		})
	}
}

func TestScopesJSON_RoundTrip(t *testing.T) {
	scopes := []string{"notes:read", "notes:write"}
	stored := scopesJSON(scopes)

	got, err := parseScopes(stored)
	if err != nil {
		t.Fatalf("parseScopes: %v", err)
	}
	if len(got) != len(scopes) || got[0] != scopes[0] || got[1] != scopes[1] {
		t.Errorf("round-trip = %v, want %v", got, scopes)
	}
}

// TestScopesJSON_Nil_StoresEmptyArray_NeverNull proves the nil-vs-empty-array
// contract scopesJSON's own doc comment promises: a nil selection persists as
// the JSON empty array "[]", never JSON null, which matters because the
// column is NOT NULL and json.Unmarshal into a []string would otherwise
// leave a nil slice, not an error, on "null" -- silently blurring "no
// scopes" and "corrupt row" together were a caller ever to write null by
// hand.
func TestScopesJSON_Nil_StoresEmptyArray_NeverNull(t *testing.T) {
	stored := scopesJSON(nil)
	if string(stored) != "[]" {
		t.Errorf("scopesJSON(nil) = %q, want %q", stored, "[]")
	}

	got, err := parseScopes(stored)
	if err != nil {
		t.Fatalf("parseScopes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parseScopes(scopesJSON(nil)) = %v, want an empty slice", got)
	}
}

func TestParseScopes_CorruptValue_ReturnsError(t *testing.T) {
	invalid := datatypes.JSON([]byte(`not json`))
	if _, err := parseScopes(invalid); err == nil {
		t.Error("parseScopes(corrupt) error = nil, want a decode error")
	}
}
