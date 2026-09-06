package pki

import (
	"reflect"
	"strings"
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

// TestKeyRef_ModelTagsMatchMigration0009 pins the widened key_ref size tag
// on the three tables migration 0009 widens (pki_signing_keys,
// pki_authorities, pki_certificates) to 4096 -- the model is the
// Atlas-style source of the schema, so a tag that drifts from the shipped
// migration is exactly the drift class the codebase punishes. LocalKey is
// deliberately absent: pki_local_keys.key_ref is LocalSigner's own 64-char
// handle (uuid.NewString), not the provider-ciphertext handle this widening
// exists for.
func TestKeyRef_ModelTagsMatchMigration0009(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model any
	}{
		{name: "SigningKey", model: SigningKey{}},
		{name: "Authority", model: Authority{}},
		{name: "Certificate", model: Certificate{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			field, ok := reflect.TypeOf(tc.model).FieldByName("KeyRef")
			if !ok {
				t.Fatal("KeyRef field not found")
			}
			tag := field.Tag.Get("gorm")
			if !strings.Contains(tag, "size:4096") {
				t.Errorf("KeyRef gorm tag = %q, want it to carry size:4096 (migration 0009)", tag)
			}
			if strings.Contains(tag, "size:255") {
				t.Errorf("KeyRef gorm tag = %q, still carries the pre-0009 size:255", tag)
			}
		})
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
