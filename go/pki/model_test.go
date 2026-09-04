package pki

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

func TestSigningKey_TableName(t *testing.T) {
	if got := (SigningKey{}).TableName(); got != tableSigningKeys {
		t.Errorf("TableName() = %q, want %q", got, tableSigningKeys)
	}
}

func TestAuthority_TableName(t *testing.T) {
	if got := (Authority{}).TableName(); got != tableAuthorities {
		t.Errorf("TableName() = %q, want %q", got, tableAuthorities)
	}
}

func TestCertificate_TableName(t *testing.T) {
	if got := (Certificate{}).TableName(); got != tableCertificates {
		t.Errorf("TableName() = %q, want %q", got, tableCertificates)
	}
}

func TestLocalKey_TableName(t *testing.T) {
	if got := (LocalKey{}).TableName(); got != tableLocalKeys {
		t.Errorf("TableName() = %q, want %q", got, tableLocalKeys)
	}
}

// TestCertificate_GetTenantID_ReadsTheEmbeddedTenantModel pins that
// Certificate's tenant accessor actually reports the field GORM populates.
// dbkit's TenantModel doc comment describes exactly how shadowing the
// promoted TenantID field silently breaks this -- leaving the column
// correct while GetTenantID returns "" and FindByID denies the row's own
// owner -- so this is a guard against that specific future edit, not a
// tautology.
func TestCertificate_GetTenantID_ReadsTheEmbeddedTenantModel(t *testing.T) {
	tests := []struct {
		name string
		cert Certificate
		want pkgcore.TenantID
	}{
		{name: "populated", cert: Certificate{TenantModel: dbkit.TenantModel{TenantID: "tenant-a"}}, want: "tenant-a"},
		{name: "zero value", cert: Certificate{}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cert.GetTenantID(); got != tc.want {
				t.Errorf("GetTenantID() = %q, want %q", got, tc.want)
			}
		})
	}
}
