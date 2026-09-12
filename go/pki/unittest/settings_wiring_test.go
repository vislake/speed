// This suite lives in package unittest -- this module's dedicated unit-test
// directory for unit-tier checks with no single source file as their target
// (the backend coding standard's testing-layout rule). It exercises pki's
// dynamic-configuration wiring end to end, black-box: a REAL config module
// stores each declared item, the module's SettingsReader seam carries it,
// and the behavior the declaration promises -- the validity bounds, the CRL
// window and distribution-point default, and the two rotation settings --
// is observed through the public issuance and lifecycle APIs.
package unittest

import (
	"context"
	"crypto/x509/pkix"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/config"
	configmigrations "github.com/vislake/speed/go/config/migrations"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/pki"
	pkimigrations "github.com/vislake/speed/go/pki/migrations"
)

// wiringTestCipherKey is the fixed 32-byte fixture key LocalKeySerializerName
// is registered under, and the key config's cipher is built from -- the same
// fixed-fixture shape pki's in-package tests use.
const wiringTestCipherKey = "0123456789abcdef0123456789abcdef"

var registerWiringCipherOnce sync.Once

// registerWiringCipher installs the pki_local_key_enc gorm serializer once
// per test process (CreateRootCA generates real keys through LocalSigner,
// which stores the private key through that serializer).
func registerWiringCipher() {
	registerWiringCipherOnce.Do(func() {
		cipher, err := dbkit.NewCipher([]byte(wiringTestCipherKey))
		if err != nil {
			panic("unittest: NewCipher on the fixed 32-byte fixture key: " + err.Error())
		}
		if err := pki.RegisterLocalKeySerializer(cipher); err != nil {
			panic("unittest: RegisterLocalKeySerializer: " + err.Error())
		}
	})
}

// wiringScenario is one end-to-end configuration: a pki module over a fresh
// database carrying both migration sets, with a real config module beside it
// holding whatever system rows the case wrote.
type wiringScenario struct {
	module *pki.Module
	cfgSvc *config.Service
	db     *gorm.DB
}

// newWiringScenario builds the scenario. wired says whether the pki module
// was given the config module's handle as its SettingsReader -- the
// difference under test. rows are written as system-scope config rows after
// the schema snapshot froze.
func newWiringScenario(t *testing.T, wired bool, rows map[string]any) *wiringScenario {
	t.Helper()
	registerWiringCipher()

	db := dbtest.NewSQLite(t)
	dbtest.Migrate(t, db, dbkit.DialectSQLite,
		dbtest.Migration{Module: "pki", FS: pkimigrations.FS},
		dbtest.Migration{Module: "config", FS: configmigrations.FS},
	)

	cipher, err := dbkit.NewCipher([]byte(wiringTestCipherKey))
	if err != nil {
		t.Fatalf("dbkit.NewCipher: %v", err)
	}
	cfgModule := config.NewModule(db, config.WithCipher(cipher), config.WithPollInterval(0))

	opts := []pki.Option{}
	if wired {
		opts = append(opts, pki.WithSettingsReader(cfgModule.Handle()))
	}
	m := pki.NewModule(db, opts...)

	// The config module's schema is folded from the declarations on the
	// registry, so pki's own Register must run first -- the same order an
	// assembly drives.
	reg := componenttest.NewRegistry()
	if declareErr := componenttest.DeclareInto(reg, m, cfgModule); declareErr != nil {
		t.Fatalf("declare both modules: %v", declareErr)
	}
	cfgSvc, err := cfgModule.Attach(reg)
	if err != nil {
		t.Fatalf("config module Attach: %v", err)
	}

	s := &wiringScenario{module: m, cfgSvc: cfgSvc, db: db}
	for key, data := range rows {
		s.setSystemRow(t, key, data)
	}
	return s
}

// setSystemRow writes one system-scope row through the real config service,
// the same Set path an operator's configuration write takes.
func (s *wiringScenario) setSystemRow(t *testing.T, key string, data any) {
	t.Helper()
	pkgcore.RegisterSystemPurpose(config.SystemPurposeSystemWrite)
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor:   "ops-1",
		Purpose: config.SystemPurposeSystemWrite,
		Ticket:  "ticket-42",
	})
	if err != nil {
		t.Fatalf("pkgcore.WithSystemContext: %v", err)
	}
	if err := s.cfgSvc.Set(sysCtx, config.ScopeSystem, key, config.Value{Data: data}, "ops-1"); err != nil {
		t.Fatalf("Set %s: %v", key, err)
	}
}

const validityTolerance = 5 * time.Second

// assertValiditySpan asserts the issued NotAfter sits the wanted span after
// NotBefore (the row under test), measured against the wall clock the
// issuance actually read.
func assertValiditySpan(t *testing.T, notBefore, notAfter time.Time, want time.Duration) {
	t.Helper()
	if got := notAfter.Sub(notBefore); (got - want).Abs() > validityTolerance {
		t.Fatalf("issued validity span = %v; want %v (the declared item's value)", got, want)
	}
}

const (
	wiringCADefaultRow = 90 * 24 * time.Hour
	wiringCAMaxRow     = 2 * 365 * 24 * time.Hour
	packageCADefault   = 10 * 365 * 24 * time.Hour
	packageCAMax       = 15 * 365 * 24 * time.Hour

	wiringCertDefaultRow = 30 * 24 * time.Hour
	wiringCertMaxRow     = 90 * 24 * time.Hour
	packageCertDefault   = 365 * 24 * time.Hour
	packageCertMax       = 2 * 365 * 24 * time.Hour
)

// TestCAValidityConfigItems_BoundCAIssuanceEndToEnd is the end-to-end proof
// for the pki.ca_default_validity / pki.ca_max_validity pair: with the
// settings seam wired, each declared row controls what CreateRootCA and
// CreateIntermediateCA issue -- a zero NotAfter takes the default, a request
// beyond the max is clamped to it, and an unwired reader leaves the package
// defaults in force.
func TestCAValidityConfigItems_BoundCAIssuanceEndToEnd(t *testing.T) {
	t.Run("a zero NotAfter takes the declared default row", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigCADefaultValidity: wiringCADefaultRow})
		authority, err := s.module.CA().CreateRootCA(context.Background(), pki.CAParams{
			Subject: pkix.Name{CommonName: "speed Root CA"},
		})
		if err != nil {
			t.Fatalf("CreateRootCA with a zero NotAfter: %v", err)
		}
		assertValiditySpan(t, authority.NotBefore, authority.NotAfter, wiringCADefaultRow)
	})

	t.Run("the defaulted span is still clamped to the declared max row", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{
			pki.ConfigCADefaultValidity: 20 * 365 * 24 * time.Hour,
			pki.ConfigCAMaxValidity:     365 * 24 * time.Hour,
		})
		authority, err := s.module.CA().CreateRootCA(context.Background(), pki.CAParams{
			Subject: pkix.Name{CommonName: "speed Root CA"},
		})
		if err != nil {
			t.Fatalf("CreateRootCA: %v", err)
		}
		assertValiditySpan(t, authority.NotBefore, authority.NotAfter, 365*24*time.Hour)
	})

	t.Run("a request beyond the declared max row is clamped to it", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigCAMaxValidity: wiringCAMaxRow})
		authority, err := s.module.CA().CreateRootCA(context.Background(), pki.CAParams{
			Subject:  pkix.Name{CommonName: "speed Root CA"},
			NotAfter: time.Now().Add(20 * 365 * 24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("CreateRootCA: %v", err)
		}
		assertValiditySpan(t, authority.NotBefore, authority.NotAfter, wiringCAMaxRow)
	})

	t.Run("a request within the bounds passes through verbatim", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigCAMaxValidity: wiringCAMaxRow})
		requested := time.Now().Add(365 * 24 * time.Hour)
		authority, err := s.module.CA().CreateRootCA(context.Background(), pki.CAParams{
			Subject:  pkix.Name{CommonName: "speed Root CA"},
			NotAfter: requested,
		})
		if err != nil {
			t.Fatalf("CreateRootCA: %v", err)
		}
		if !authority.NotAfter.Equal(requested) {
			t.Fatalf("NotAfter = %v; want the caller's own %v", authority.NotAfter, requested)
		}
	})

	t.Run("the intermediate path resolves through the same pair", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigCADefaultValidity: wiringCADefaultRow})
		root, err := s.module.CA().CreateRootCA(context.Background(), pki.CAParams{
			Subject:  pkix.Name{CommonName: "speed Root CA"},
			NotAfter: time.Now().Add(24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("CreateRootCA: %v", err)
		}
		intermediate, err := s.module.CA().CreateIntermediateCA(context.Background(), root.ID, pki.CAParams{
			Subject: pkix.Name{CommonName: "speed Intermediate CA"},
		})
		if err != nil {
			t.Fatalf("CreateIntermediateCA with a zero NotAfter: %v", err)
		}
		assertValiditySpan(t, intermediate.NotBefore, intermediate.NotAfter, wiringCADefaultRow)
	})

	t.Run("an unwired reader leaves the package defaults in force", func(t *testing.T) {
		s := newWiringScenario(t, false, map[string]any{
			pki.ConfigCADefaultValidity: wiringCADefaultRow,
			pki.ConfigCAMaxValidity:     wiringCAMaxRow,
		})
		defaulted, err := s.module.CA().CreateRootCA(context.Background(), pki.CAParams{
			Subject: pkix.Name{CommonName: "speed Root CA"},
		})
		if err != nil {
			t.Fatalf("CreateRootCA: %v", err)
		}
		assertValiditySpan(t, defaulted.NotBefore, defaulted.NotAfter, packageCADefault)

		clamped, err := s.module.CA().CreateRootCA(context.Background(), pki.CAParams{
			Subject:  pkix.Name{CommonName: "speed Root CA 2"},
			NotAfter: time.Now().Add(20 * 365 * 24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("CreateRootCA: %v", err)
		}
		assertValiditySpan(t, clamped.NotBefore, clamped.NotAfter, packageCAMax)
	})
}

// TestCertificateValidityConfigItems_BoundIssuanceEndToEnd is the
// end-to-end proof for the pki.certificate_default_validity /
// pki.certificate_max_validity pair on IssueCertificate.
func TestCertificateValidityConfigItems_BoundIssuanceEndToEnd(t *testing.T) {
	issue := func(t *testing.T, s *wiringScenario, requested time.Time) *pki.Certificate {
		t.Helper()
		ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-wiring"))
		authority, err := s.module.CA().CreateRootCA(ctx, pki.CAParams{
			Subject:  pkix.Name{CommonName: "speed Root CA"},
			NotAfter: time.Now().Add(24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("CreateRootCA: %v", err)
		}
		cert, err := s.module.CA().IssueCertificate(ctx, authority.ID, pki.CertificateParams{
			Purpose:  "wiring.test",
			Subject:  pkix.Name{CommonName: "wiring end-entity"},
			NotAfter: requested,
		})
		if err != nil {
			t.Fatalf("IssueCertificate: %v", err)
		}
		return cert
	}

	t.Run("a zero NotAfter takes the declared default row", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigCertificateDefaultValidity: wiringCertDefaultRow})
		cert := issue(t, s, time.Time{})
		assertValiditySpan(t, cert.NotBefore, cert.NotAfter, wiringCertDefaultRow)
	})

	t.Run("a request beyond the declared max row is clamped to it", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigCertificateMaxValidity: wiringCertMaxRow})
		cert := issue(t, s, time.Now().Add(5*365*24*time.Hour))
		assertValiditySpan(t, cert.NotBefore, cert.NotAfter, wiringCertMaxRow)
	})

	t.Run("without rows the package defaults apply", func(t *testing.T) {
		s := newWiringScenario(t, true, nil)
		defaulted := issue(t, s, time.Time{})
		assertValiditySpan(t, defaulted.NotBefore, defaulted.NotAfter, packageCertDefault)
		clamped := issue(t, s, time.Now().Add(5*365*24*time.Hour))
		assertValiditySpan(t, clamped.NotBefore, clamped.NotAfter, packageCertMax)
	})
}

// seedPendingKey writes one pending signing key for purpose directly through
// the repository, the state PromoteNow operates on.
func seedPendingKey(t *testing.T, s *wiringScenario, purpose string) {
	t.Helper()
	now := time.Now().UTC()
	key := &pki.SigningKey{
		ID:         "kid-pending-" + purpose,
		Purpose:    purpose,
		Algorithm:  pki.AlgorithmEd25519,
		SignerName: "local",
		KeyRef:     "keyref-pending-" + purpose,
		Status:     pki.SigningKeyStatusPending,
		PublicKey:  []byte{0x01, 0x02, 0x03},
		NotBefore:  now,
		NotAfter:   now.Add(24 * time.Hour),
	}
	if err := pki.NewSigningKeyRepository(s.db).Create(context.Background(), key); err != nil {
		t.Fatalf("seed pending key: %v", err)
	}
}

// TestPropagationWindowConfigItem_ControlsPromotionEndToEnd is the
// end-to-end proof for pki.propagation_window: with the seam wired, an
// explicit row is the window PromoteNow enforces.
func TestPropagationWindowConfigItem_ControlsPromotionEndToEnd(t *testing.T) {
	const purpose = "wiring.rotation"

	t.Run("wired reader honors the row (an immediate promotion passes)", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigPropagationWindow: time.Nanosecond})
		seedPendingKey(t, s, purpose)
		kid, err := s.module.Service().PromoteNow(context.Background(), purpose, 0)
		if err != nil {
			t.Fatalf("PromoteNow with a 1ns configured window: %v; want the promotion to pass", err)
		}
		if kid == "" {
			t.Fatal("PromoteNow returned an empty kid")
		}
	})

	t.Run("no row keeps the construction-time default (the window still gates)", func(t *testing.T) {
		s := newWiringScenario(t, true, nil)
		seedPendingKey(t, s, purpose)
		if _, err := s.module.Service().PromoteNow(context.Background(), purpose, 0); !apperr.HasCode(err, pki.ErrPropagationWindowNotElapsed.Code) {
			t.Fatalf("PromoteNow without a row = %v; want ErrPropagationWindowNotElapsed (the default window)", err)
		}
	})

	t.Run("unwired reader ignores the row (the default window still gates)", func(t *testing.T) {
		s := newWiringScenario(t, false, map[string]any{pki.ConfigPropagationWindow: time.Nanosecond})
		seedPendingKey(t, s, purpose)
		if _, err := s.module.Service().PromoteNow(context.Background(), purpose, 0); !apperr.HasCode(err, pki.ErrPropagationWindowNotElapsed.Code) {
			t.Fatalf("PromoteNow with a row but no reader = %v; want ErrPropagationWindowNotElapsed (a row alone must not move the window)", err)
		}
	})

	t.Run("a per-call override still wins over the row", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigPropagationWindow: time.Nanosecond})
		seedPendingKey(t, s, purpose)
		if _, err := s.module.Service().PromoteNow(context.Background(), purpose, time.Hour); !apperr.HasCode(err, pki.ErrPropagationWindowNotElapsed.Code) {
			t.Fatalf("PromoteNow with an explicit one-hour window = %v; want ErrPropagationWindowNotElapsed", err)
		}
	})
}

// seedActiveKey writes one active signing key for purpose whose NotAfter
// sits at notAfter, the state the staging step of ScanExpiry operates on.
func seedActiveKey(t *testing.T, s *wiringScenario, purpose string, notAfter time.Time) {
	t.Helper()
	now := time.Now().UTC()
	key := &pki.SigningKey{
		ID:         "kid-active-" + purpose,
		Purpose:    purpose,
		Algorithm:  pki.AlgorithmEd25519,
		SignerName: "local",
		KeyRef:     "keyref-active-" + purpose,
		Status:     pki.SigningKeyStatusActive,
		PublicKey:  []byte{0x01, 0x02, 0x03},
		NotBefore:  now.Add(-time.Hour),
		NotAfter:   notAfter,
	}
	if err := pki.NewSigningKeyRepository(s.db).Create(context.Background(), key); err != nil {
		t.Fatalf("seed active key: %v", err)
	}
}

// TestRenewalLeadTimeConfigItem_ControlsStagingEndToEnd is the end-to-end
// proof for pki.renewal_lead_time: with the seam wired, an explicit row is
// the lead time ScanExpiry's staging step applies.
func TestRenewalLeadTimeConfigItem_ControlsStagingEndToEnd(t *testing.T) {
	const purpose = "wiring.renewal"
	// The active key expires in an hour: staged under the 30-day default
	// lead time, not staged under a one-second one.
	nearing := func() time.Time { return time.Now().Add(time.Hour) }

	t.Run("wired reader honors the row (a 1s lead stages nothing)", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigRenewalLeadTime: time.Second})
		seedActiveKey(t, s, purpose, nearing())
		report, err := s.module.Service().ScanExpiry(context.Background(), pki.RotationConfig{})
		if err != nil {
			t.Fatalf("ScanExpiry: %v", err)
		}
		if len(report.Staged) != 0 {
			t.Fatalf("ScanExpiry staged %v; want nothing (the configured one-second lead does not reach a key expiring in an hour)", report.Staged)
		}
	})

	t.Run("no row keeps the default lead (the key is staged)", func(t *testing.T) {
		s := newWiringScenario(t, true, nil)
		seedActiveKey(t, s, purpose, nearing())
		report, err := s.module.Service().ScanExpiry(context.Background(), pki.RotationConfig{})
		if err != nil {
			t.Fatalf("ScanExpiry: %v", err)
		}
		if len(report.Staged) != 1 {
			t.Fatalf("ScanExpiry staged %v; want the purpose staged under the default lead time", report.Staged)
		}
	})

	t.Run("unwired reader ignores the row (the default lead stages)", func(t *testing.T) {
		s := newWiringScenario(t, false, map[string]any{pki.ConfigRenewalLeadTime: time.Second})
		seedActiveKey(t, s, purpose, nearing())
		report, err := s.module.Service().ScanExpiry(context.Background(), pki.RotationConfig{})
		if err != nil {
			t.Fatalf("ScanExpiry: %v", err)
		}
		if len(report.Staged) != 1 {
			t.Fatalf("ScanExpiry staged %v; want the default lead time to stage the purpose (a row alone must not move the lead)", report.Staged)
		}
	})
}

// createWiringRoot issues a root authority with a short explicit validity so
// the CRL cases have an authority to generate against.
func createWiringRoot(t *testing.T, s *wiringScenario) *pki.Authority {
	t.Helper()
	root, err := s.module.CA().CreateRootCA(context.Background(), pki.CAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}
	return root
}

// TestCRLValidityConfigItem_ControlsGeneratedCRLsEndToEnd is the end-to-end
// proof for pki.crl_validity: with the seam wired, an explicit row is the
// window every generated CRL claims, on both the direct GenerateCRL path
// and the batch RegenerateAllCRLs path that passes its own zero.
func TestCRLValidityConfigItem_ControlsGeneratedCRLsEndToEnd(t *testing.T) {
	const rowValidity = 2 * time.Hour

	t.Run("GenerateCRL with a zero validity honors the row", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigCRLValidity: rowValidity})
		root := createWiringRoot(t, s)
		authority, err := s.module.CA().GenerateCRL(context.Background(), root.ID, 0)
		if err != nil {
			t.Fatalf("GenerateCRL: %v", err)
		}
		if authority.CRLIssuedAt == nil || authority.CRLNextUpdate == nil {
			t.Fatal("GenerateCRL returned no CRL timestamps")
		}
		if got := authority.CRLNextUpdate.Sub(*authority.CRLIssuedAt); (got - rowValidity).Abs() > validityTolerance {
			t.Fatalf("CRL window = %v; want %v (the declared row)", got, rowValidity)
		}
	})

	t.Run("RegenerateAllCRLs resolves through the row on its internal path", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigCRLValidity: rowValidity})
		root := createWiringRoot(t, s)
		regenerated, err := s.module.CA().RegenerateAllCRLs(context.Background())
		if err != nil {
			t.Fatalf("RegenerateAllCRLs: %v", err)
		}
		if len(regenerated) != 1 || regenerated[0] != root.ID {
			t.Fatalf("RegenerateAllCRLs regenerated %v; want [%s]", regenerated, root.ID)
		}
		stored, err := pki.NewAuthorityRepository(s.db).FindByID(context.Background(), root.ID)
		if err != nil {
			t.Fatalf("re-read the authority: %v", err)
		}
		if stored.CRLIssuedAt == nil || stored.CRLNextUpdate == nil {
			t.Fatal("the stored authority has no CRL timestamps")
		}
		if got := stored.CRLNextUpdate.Sub(*stored.CRLIssuedAt); (got - rowValidity).Abs() > validityTolerance {
			t.Fatalf("stored CRL window = %v; want %v (the declared row)", got, rowValidity)
		}
	})

	t.Run("no row keeps DefaultCRLValidity", func(t *testing.T) {
		s := newWiringScenario(t, true, nil)
		root := createWiringRoot(t, s)
		authority, err := s.module.CA().GenerateCRL(context.Background(), root.ID, 0)
		if err != nil {
			t.Fatalf("GenerateCRL: %v", err)
		}
		if got := authority.CRLNextUpdate.Sub(*authority.CRLIssuedAt); (got - pki.DefaultCRLValidity).Abs() > validityTolerance {
			t.Fatalf("CRL window = %v; want DefaultCRLValidity %v", got, pki.DefaultCRLValidity)
		}
	})

	t.Run("an explicit validity argument still wins", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigCRLValidity: rowValidity})
		root := createWiringRoot(t, s)
		authority, err := s.module.CA().GenerateCRL(context.Background(), root.ID, time.Minute)
		if err != nil {
			t.Fatalf("GenerateCRL: %v", err)
		}
		if got := authority.CRLNextUpdate.Sub(*authority.CRLIssuedAt); (got - time.Minute).Abs() > validityTolerance {
			t.Fatalf("CRL window = %v; want the caller's own one minute", got)
		}
	})
}

// TestCRLDistributionPointConfigItem_DefaultsNewAuthoritiesEndToEnd is the
// end-to-end proof for pki.crl_distribution_point: with the seam wired, an
// explicit row fills a NEW authority's empty CAParams value, a caller value
// still wins, and existing authority rows are never rewritten.
func TestCRLDistributionPointConfigItem_DefaultsNewAuthoritiesEndToEnd(t *testing.T) {
	const rowPoint = "https://ca.example.test/crl/root.crl"

	t.Run("an empty CAParams value takes the row", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigCRLDistributionPoint: rowPoint})
		root := createWiringRoot(t, s)
		if root.CRLDistributionPoint != rowPoint {
			t.Fatalf("authority CRLDistributionPoint = %q; want the declared row %q", root.CRLDistributionPoint, rowPoint)
		}
	})

	t.Run("a caller value wins over the row", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigCRLDistributionPoint: rowPoint})
		root, err := s.module.CA().CreateRootCA(context.Background(), pki.CAParams{
			Subject:              pkix.Name{CommonName: "speed Root CA"},
			NotAfter:             time.Now().Add(24 * time.Hour),
			CRLDistributionPoint: "https://caller.example.test/crl.crl",
		})
		if err != nil {
			t.Fatalf("CreateRootCA: %v", err)
		}
		if root.CRLDistributionPoint != "https://caller.example.test/crl.crl" {
			t.Fatalf("authority CRLDistributionPoint = %q; want the caller's own value", root.CRLDistributionPoint)
		}
	})

	t.Run("an unwired reader leaves the value empty", func(t *testing.T) {
		s := newWiringScenario(t, false, map[string]any{pki.ConfigCRLDistributionPoint: rowPoint})
		root := createWiringRoot(t, s)
		if root.CRLDistributionPoint != "" {
			t.Fatalf("authority CRLDistributionPoint = %q; want empty (a row alone must not fill it)", root.CRLDistributionPoint)
		}
	})

	t.Run("existing rows are not rewritten by a later row change", func(t *testing.T) {
		s := newWiringScenario(t, true, map[string]any{pki.ConfigCRLDistributionPoint: rowPoint})
		root := createWiringRoot(t, s)
		s.setSystemRow(t, pki.ConfigCRLDistributionPoint, "https://ca.example.test/crl/rotated.crl")

		stored, err := pki.NewAuthorityRepository(s.db).FindByID(context.Background(), root.ID)
		if err != nil {
			t.Fatalf("re-read the authority: %v", err)
		}
		if stored.CRLDistributionPoint != rowPoint {
			t.Fatalf("stored CRLDistributionPoint = %q; want the creation-time %q (a row change must not rewrite existing authorities)", stored.CRLDistributionPoint, rowPoint)
		}
	})
}
