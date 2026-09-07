//go:build integration

package authn_test

import (
	"strings"
	"testing"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// assertSSOErrorCode fails unless err is an *apperr.Error carrying wantCode,
// the external-package twin of the unit tier's assertErrorCode
// (provider_test.go, package authn).
func assertSSOErrorCode(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %s", wantCode)
	}
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error = %v (%T), want an *apperr.Error with code %s", err, err, wantCode)
	}
	if appErr.Code != wantCode {
		t.Fatalf("error code = %s, want %s", appErr.Code, wantCode)
	}
}

// The two authn batch-2 width findings this file closes are the truncation
// round's siblings (postgres_provider_field_widths_test.go): the same
// dual-dialect write divergence, on the two remaining surfaces where the
// right semantic is REFUSE rather than cut. The unit tier (oidc_test.go,
// package authn) pins the named refusals on SQLite; these legs re-run them
// against real PostgreSQL, where the pre-fix behaviour of each surface was a
// refused write -- SQLSTATE 22001 -- that SQLite never reproduced:
//
//   - A tenant id of 60 runes makes the synthetic "oidc:<tenant>" provider
//     name overflow user_identities.provider (VARCHAR(64), migration 0005)
//     at the identity write of the tenant's first enterprise sign-in. The
//     configuration write itself fit (tenant_sso_configs.tenant_id is
//     VARCHAR(64)), so the divergence was silent until a random login.
//     The fix refuses such a tenant id at SaveConfig, AuthorizeURL and
//     Callback with authn.sso_tenant_id_too_long -- the same named answer
//     on both dialects.
//   - A tenant administrator's configuration value longer than its column
//     (issuer VARCHAR(512), client_id VARCHAR(255), allowed_domains
//     VARCHAR(1024), migration 0006) was stored verbatim by SQLite and
//     refused raw by PostgreSQL. The fix refuses it at SaveConfig and at
//     SSOConfigRepository's own Create/Update with an error naming the
//     field.
//
// The widths and expected codes below are restated as literals (with the
// unit tier's fixtures as the source of truth) because this external test
// package cannot import the module's unexported width constants -- the same
// reason postgres_provider_field_widths_test.go restates pgExternalIDWidth.
const (
	pgSSOTenantBudgetRunes       = 59
	pgIssuerColumnWidth          = 512
	pgClientIDColumnWidth        = 255
	pgAllowedDomainsColumnWidth  = 1024
	ssoTenantIDTooLongCode       = "authn.sso_tenant_id_too_long"
	ssoIssuerTooLongCode         = "authn.sso_issuer_too_long"
	ssoClientIDTooLongCode       = "authn.sso_client_id_too_long"
	ssoAllowedDomainsTooLongCode = "authn.sso_allowed_domains_too_long"
	pgSSOPublicIssuerLiteral     = "https://93.184.216.34/"
)

var (
	// pgOverLongTenantID is one rune past the enterprise channel's tenant-id
	// budget.
	pgOverLongTenantID = pkgcore.TenantID(strings.Repeat("t", pgSSOTenantBudgetRunes+1))
	// pgBoundaryTenantID is exactly at the budget.
	pgBoundaryTenantID = pkgcore.TenantID(strings.Repeat("t", pgSSOTenantBudgetRunes))
	// pgOverWidthIssuer is a valid https URL one rune past issuer's width.
	pgOverWidthIssuer = pgSSOPublicIssuerLiteral + strings.Repeat("i", pgIssuerColumnWidth+1-len(pgSSOPublicIssuerLiteral))
	// pgBoundaryIssuer is a valid https URL exactly at issuer's width.
	pgBoundaryIssuer = pgSSOPublicIssuerLiteral + strings.Repeat("i", pgIssuerColumnWidth-len(pgSSOPublicIssuerLiteral))
	// pgBoundaryClientID is exactly at client_id's width.
	pgBoundaryClientID = strings.Repeat("c", pgClientIDColumnWidth)
	// pgBoundaryAllowedDomains joins to exactly allowed_domains' width.
	pgBoundaryAllowedDomains = []string{strings.Repeat("d", pgAllowedDomainsColumnWidth)}
	// pgOverWidthClientID is one rune past client_id's width.
	pgOverWidthClientID = strings.Repeat("c", pgClientIDColumnWidth+1)
	// pgOverWidthAllowedDomains joins to one rune past allowed_domains'
	// width.
	pgOverWidthAllowedDomains = []string{strings.Repeat("d", pgAllowedDomainsColumnWidth+1)}
)

// TestSaveConfig_OverLongTenantID_Refused_Postgres re-runs the unit tier's
// finding (1) regression against real PostgreSQL: configuring enterprise SSO
// under a 60-rune tenant id must answer authn.sso_tenant_id_too_long and
// persist nothing. Before the fix this call SUCCEEDED on PostgreSQL too --
// the config row's own tenant_id column held the 60-rune id -- leaving the
// divergence for the tenant's first sign-in, whose identity insert would
// have been refused with SQLSTATE 22001 for the 65-rune "oidc:<tenant>"
// provider name. The identity-column boundary itself is pinned by
// TestSSOIdentityProviderBoundary_Postgres below.
func TestSaveConfig_OverLongTenantID_Refused_Postgres(t *testing.T) {
	t.Parallel()

	db := testutil.NewPostgresDB(t)
	svc := newIntegrationService(t, db, testutil.NewMemberships())
	ctx := pkgcore.WithTenant(t.Context(), pgOverLongTenantID)

	_, err := svc.SSO().SaveConfig(ctx, authn.SSOConfigInput{
		Issuer: "https://93.184.216.34/oidc", ClientID: "client-id", Enabled: true,
	})
	assertSSOErrorCode(t, err, ssoTenantIDTooLongCode)

	if _, findErr := svc.SSO().Configs().Current(ctx); findErr == nil {
		t.Error("a refused configuration must not have been persisted")
	}
}

// TestSaveConfig_OverWidthConfigFields_Refused_Postgres re-runs the unit
// tier's finding (2) regressions against real PostgreSQL: each over-width
// configuration value must answer the field-naming refusal before the
// database is touched. Before the fix these writes were refused here by
// PostgreSQL with SQLSTATE 22001 while SQLite stored the values verbatim.
func TestSaveConfig_OverWidthConfigFields_Refused_Postgres(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		wantCode string
		in       authn.SSOConfigInput
	}{
		{
			name:     "issuer one rune past 512",
			wantCode: ssoIssuerTooLongCode,
			in:       authn.SSOConfigInput{Issuer: pgOverWidthIssuer, ClientID: "client-id", Enabled: true},
		},
		{
			name:     "client id one rune past 255",
			wantCode: ssoClientIDTooLongCode,
			in:       authn.SSOConfigInput{Issuer: "https://93.184.216.34/oidc", ClientID: pgOverWidthClientID, Enabled: true},
		},
		{
			name:     "allowed domains one rune past 1024",
			wantCode: ssoAllowedDomainsTooLongCode,
			in: authn.SSOConfigInput{
				Issuer: "https://93.184.216.34/oidc", ClientID: "client-id",
				AllowedDomains: pgOverWidthAllowedDomains, Enabled: true,
			},
		},
	}

	db := testutil.NewPostgresDB(t)
	svc := newIntegrationService(t, db, testutil.NewMemberships())
	ctx := pkgcore.WithTenant(t.Context(), pkgcore.TenantID("tenant-pg-widths"))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.SSO().SaveConfig(ctx, tc.in)
			assertSSOErrorCode(t, err, tc.wantCode)
		})
	}
}

// TestSaveConfig_ColumnWidthBoundary_Accepted_Postgres is the honest
// acceptance boundary against real PostgreSQL: a 59-rune tenant id (the
// longest the enterprise channel can represent) and configuration values
// exactly at their columns' widths still save and read back end to end --
// nothing about the fix may shrink what the columns themselves accept.
func TestSaveConfig_ColumnWidthBoundary_Accepted_Postgres(t *testing.T) {
	t.Parallel()

	db := testutil.NewPostgresDB(t)
	svc := newIntegrationService(t, db, testutil.NewMemberships())
	ctx := pkgcore.WithTenant(t.Context(), pgBoundaryTenantID)

	if _, err := svc.SSO().SaveConfig(ctx, authn.SSOConfigInput{
		Issuer: pgBoundaryIssuer, ClientID: pgBoundaryClientID,
		AllowedDomains: pgBoundaryAllowedDomains, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveConfig(boundary tenant id, boundary-width values) error = %v", err)
	}

	stored, err := svc.SSO().Configs().Current(ctx)
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if stored.TenantID != string(pgBoundaryTenantID) {
		t.Errorf("stored tenant id = %q-ish, want the 59-rune tenant", stored.TenantID)
	}
	if stored.Issuer != pgBoundaryIssuer {
		t.Errorf("stored issuer = %d runes, want the %d-rune value", len([]rune(stored.Issuer)), pgIssuerColumnWidth)
	}
	if len([]rune(stored.AllowedDomains)) != pgAllowedDomainsColumnWidth {
		t.Errorf("stored allowed domains = %d runes, want %d", len([]rune(stored.AllowedDomains)), pgAllowedDomainsColumnWidth)
	}
}

// TestSSOIdentityProviderBoundary_Postgres pins the exact column where
// finding (1)'s divergence lived: the synthetic provider name for the
// longest representable tenant id -- "oidc:" plus 59 runes, 64 runes in all
// -- must insert into user_identities.provider (VARCHAR(64)) on real
// PostgreSQL, the dialect that enforces the width. This is what makes the
// refusal boundary in TestSaveConfig_OverLongTenantID_Refused_Postgres the
// honest one: 59 runes really fit, and 60 really cannot.
func TestSSOIdentityProviderBoundary_Postgres(t *testing.T) {
	t.Parallel()

	db := testutil.NewPostgresDB(t)
	repo, err := authn.NewUserIdentityRepository(db)
	if err != nil {
		t.Fatalf("NewUserIdentityRepository() error = %v", err)
	}

	boundaryProvider := authn.SSOChannelName(pgBoundaryTenantID)
	if len(boundaryProvider) != 64 {
		t.Fatalf("boundary provider name = %d runes, want exactly 64 (the VARCHAR(64) width)", len([]rune(boundaryProvider)))
	}

	identity := &authn.UserIdentity{
		ID:          "boundary-provider-identity",
		UserID:      "boundary-provider-user",
		Provider:    boundaryProvider,
		ExternalID:  "boundary-provider-subject",
		DisplayName: "Boundary Member",
		AvatarURL:   "https://cdn.example.com/boundary.png",
	}
	if err := repo.Create(t.Context(), identity); err != nil {
		t.Fatalf("identity insert under the 64-rune provider name error = %v, want success on real PostgreSQL", err)
	}

	readBack, err := repo.FindByExternal(t.Context(), boundaryProvider, "boundary-provider-subject")
	if err != nil {
		t.Fatalf("FindByExternal() error = %v", err)
	}
	if readBack.ID != identity.ID {
		t.Errorf("read-back identity = %q, want %q", readBack.ID, identity.ID)
	}
}
