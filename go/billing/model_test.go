package billing

import "testing"

// TestGrantKind_InfersFromValueType pins grantKind's own contract
// (entitlements.go): a Grant carries no explicit Kind field of its own, so
// the Go type of its Value is the only signal Check has for which of the
// three Feature kinds it was issued under. A Value shape no Feature kind
// interprets -- a string that is not the GrantValueUnlimited sentinel --
// is reported through ok=false so Check fails closed rather than guessing
// (TestEntitlementsService_Check_NonSentinelStringGrantValue_FailsClosed
// is the service-level regression).
func TestGrantKind_InfersFromValueType(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  FeatureKind
		ok    bool
	}{
		{name: "bool -> Boolean", value: true, want: FeatureKindBoolean, ok: true},
		{name: "GrantValueUnlimited string -> Unlimited", value: GrantValueUnlimited, want: FeatureKindUnlimited, ok: true},
		{name: "int64 -> Quota", value: int64(10), want: FeatureKindQuota, ok: true},
		{name: "float64 (JSON round-trip shape) -> Quota", value: float64(10), want: FeatureKindQuota, ok: true},
		{name: "non-sentinel string is malformed, never Unlimited", value: "1000", want: "", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := grantKind(Grant{Value: tc.value})
			if got != tc.want || ok != tc.ok {
				t.Errorf("grantKind(%v) = %q, %v, want %q, %v", tc.value, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestGrantQuotaLimit_AcceptsEveryNumericShape pins grantQuotaLimit's own
// contract: a Quota grant's Value round-trips through Plan.GrantsJSON as
// float64 (encoding/json's universal number type), so every concrete
// numeric type must decode to the same int64 limit.
func TestGrantQuotaLimit_AcceptsEveryNumericShape(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  int64
		ok    bool
	}{
		{name: "int64", value: int64(10), want: 10, ok: true},
		{name: "int", value: 10, want: 10, ok: true},
		{name: "float64", value: float64(10), want: 10, ok: true},
		{name: "bool is not numeric", value: true, want: 0, ok: false},
		{name: "string is not numeric", value: "10", want: 0, ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := grantQuotaLimit(tc.value)
			if got != tc.want || ok != tc.ok {
				t.Errorf("grantQuotaLimit(%v) = %d, %v, want %d, %v", tc.value, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestPlan_Grant_ReturnsFalseWhenNotAwarded(t *testing.T) {
	p := Plan{}
	if err := p.SetGrants([]Grant{{FeatureKey: "seats", Value: int64(5)}}); err != nil {
		t.Fatalf("SetGrants: %v", err)
	}
	if _, ok := p.Grant("not_awarded"); ok {
		t.Error("Grant(not_awarded) = true, want false")
	}
	g, ok := p.Grant("seats")
	if !ok || g.FeatureKey != "seats" {
		t.Errorf("Grant(seats) = %+v, %v, want the seats grant", g, ok)
	}
}

func TestPlan_IsPlatformWide(t *testing.T) {
	if !(Plan{}).IsPlatformWide() {
		t.Error("a Plan with the zero-value TenantID must be platform-wide")
	}
	if (Plan{TenantID: "tenant-acme"}).IsPlatformWide() {
		t.Error("a Plan with a non-empty TenantID must not be platform-wide")
	}
}

func TestMoney_PriceRoundTrips(t *testing.T) {
	p := &Plan{}
	want := Money{Cents: 4900, Currency: "USD"}
	p.SetPrice(want)
	if got := p.Price(); got != want {
		t.Errorf("Price() = %+v, want %+v", got, want)
	}
}
