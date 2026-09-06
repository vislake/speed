package main

// periodic_pki_scan_flow_test.go proves that the reference app's host-side
// periodic-task scheduler (periodic_scheduler.go) really drives go/pki's
// signing-key expiry scan through the composed stack: real ticks at the
// flow tests' cfg.PeriodicTaskInterval (periodicFlowTickInterval, one
// second) enqueue Service.EnqueueExpiryScan tasks, the app's real
// standalone queue drains them into pki's expiryScanHandler, and each run
// of ScanExpiry advances the signing-key state machine over the real
// pki_signing_keys rows.
//
// The mechanism is genuinely periodic on this app's queue, unlike
// storage's expiry sweep. The sweep's deterministic per-tenant idempotency
// key collides with StandaloneQueue's permanent idempotency, so each
// tenant gets exactly one sweep per database file -- the first-ever one
// (periodic_scheduler_flow_test.go's header records that residual
// limitation, and its two-boot test proves that one sweep really removes
// an expired object's row and bytes when an expired object exists).
// EnqueueExpiryScan carries NO idempotency key -- each tick is its own
// independent occurrence, and the scan's guarded, status-checked state
// transitions make overlapping scans safe -- so this test can observe a
// real rotation complete: the purpose's boot key staged over by a
// successor and demoted to retiring.
//
// What the test drives, and what it only watches:
//
//   - The boot key is created by the REAL bootstrap path: this app's authn
//     Signer calls pki.Service.EnsurePurpose lazily on its first token
//     issue, so the test's own registerAndAuthenticate sign-in is what
//     creates the purpose's first active key, synchronously, through the
//     real wire. Before that sign-in the purpose has no key at all -- the
//     test asserts that first, so the "boot" key's identity is never
//     guessed.
//   - The rotation is driven ONLY by the scheduled scans: nothing in this
//     test calls ScanExpiry, EnqueueExpiryScan, or any pki service method,
//     and the observer connection never writes. The stage (a pending
//     successor key) and the promotion (pending -> active while the boot
//     key -> retiring) can only be produced by expiryScanHandler runs that
//     real ticks enqueued and the real queue worker drained.
//   - The test's only timing nudge is the same test-override serverConfig
//     fields every flow test uses: cfg.PKIPropagationWindow and
//     cfg.PKIRenewalLeadTime (buildServer applies pki.WithPropagationWindow
//     / pki.WithRenewalLeadTime for non-zero values). PKIRenewalLeadTime is
//     set LONGER than the key validity EnsurePurpose grants (one year,
//     go/pki's defaultKeyValidity), so the boot key is "nearing expiry"
//     from the moment it exists and the first scan tick after the sign-in
//     stages its replacement; PKIPropagationWindow is 400ms instead of the
//     150s default, so a scan tick past that window promotes the staged
//     key. The rotation math itself is real: the scan reads the real key
//     rows' NotAfter/RetiringOverlap columns and the real clock; only the
//     policy constants are compressed, exactly like the compressed cadence
//     of periodicFlowTickInterval itself.
//   - Observation is the same second-connection reach the audit flow test
//     uses (TestBuildServer_NoteCreate_PersistsAuditEvent): buildServer
//     hands out neither its *gorm.DB nor module services, so a second
//     dbkit.Open connection to the same SQLite file is the only way a test
//     can read pki_signing_keys, through the module's own exported
//     SigningKeyRepository. Conditions are polled, never slept on, so the
//     test has no wall-clock race: the poll can only be satisfied by rows
//     the real scans wrote.
//
// The end state asserted -- exactly one active key, a different kid than
// the boot key, the boot key retiring with a RetiringAt -- is reached
// within seconds of real ticks. The boot key's retirement (retiring ->
// retired) is deliberately NOT asserted: RetireDueRetiring releases a key
// only after RetiringAt + RetiringOverlap, and RetiringOverlap is the
// maxCredentialLifetime authn declared at bootstrap -- the app's access-
// token TTL (go/authn's DefaultAccessTokenTTL, fifteen minutes) -- so a
// retirement cannot legitimately occur inside this test's seconds. The
// test closes instead with a wire smoke over the rotated purpose: a fresh
// sign-in (whose token the successor key must have signed) and the
// pre-rotation token (which the retiring boot key must still verify)
// both answer 200 through the real middleware chain.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pki"
)

// pkiFlowPropagationWindow is the cfg.PKIPropagationWindow the rotation
// test injects: 400ms in place of go/pki's DefaultPropagationWindow (150s,
// five 30s cache TTLs), so a staged successor key becomes promotable
// within test time. The window is what stops one scan from both staging
// and promoting: a promotion requires now >= staged_at + window, which no
// single scan can satisfy for the key it just staged. With the one-second
// flow-tick cadence the window also sits well under a tick interval, so
// the tick after the staging one is always far enough past the staging
// scan -- promotion lands on the next tick, never on the staging one.
const pkiFlowPropagationWindow = 400 * time.Millisecond

// pkiFlowRenewalLeadTime is the cfg.PKIRenewalLeadTime the rotation test
// injects: 400 days in place of go/pki's DefaultRenewalLeadTime (30d),
// deliberately LONGER than the one-year validity EnsurePurpose grants a
// boot key (go/pki's defaultKeyValidity). StageDueRotations stages a
// replacement for every active key whose NotAfter is at or before
// now+renewalLeadTime, so with a 400-day lead a freshly bootstrapped
// one-year key is "nearing expiry" from its very first scan tick onward.
const pkiFlowRenewalLeadTime = 400 * 24 * time.Hour

// signingKeyRotationDeadline bounds the poll below. The happy path is fast
// (a stage on the first tick after the sign-in, a promotion on the next
// tick, which the one-second cadence puts well past the propagation
// window -- about two seconds of real ticks), so the deadline is generous
// purely against loaded CI machines; a rotation that has not landed within
// it means the scheduled scans are not advancing the state machine, which
// is exactly the failure this test exists to catch.
const signingKeyRotationDeadline = 20 * time.Second

// TestBuildServer_PeriodicScheduler_PKIExpiryScan_RotatesBootKey drives the
// full rotation described in this file's header: boot key via the real
// sign-in, successor staged and promoted by real scheduled scans, observed
// through a second connection to the app's own SQLite file.
func TestBuildServer_PeriodicScheduler_PKIExpiryScan_RotatesBootKey(t *testing.T) {
	srv, cfg := buildPeriodicTestServer(t, func(cfg *serverConfig) {
		cfg.PeriodicTaskInterval = periodicFlowTickInterval
		cfg.PKIPropagationWindow = pkiFlowPropagationWindow
		cfg.PKIRenewalLeadTime = pkiFlowRenewalLeadTime
	})

	// The observer: a second connection to the same SQLite file the
	// running server writes, read through pki's own repository. The same
	// reach TestBuildServer_NoteCreate_PersistsAuditEvent uses for the
	// audit table -- buildServer exposes neither its *gorm.DB nor any
	// module service, and pki_signing_keys is platform data whose
	// repository is a plain, tenant-unfiltered *gorm.DB.
	observerDB, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     cfg.SQLitePath,
	})
	if err != nil {
		t.Fatalf("open observer connection to %q: %v", cfg.SQLitePath, err)
	}
	t.Cleanup(func() {
		sqlDB, dbErr := observerDB.DB()
		if dbErr != nil {
			t.Errorf("observer connection handle: %v", dbErr)
			return
		}
		if closeErr := sqlDB.Close(); closeErr != nil {
			t.Errorf("close observer connection: %v", closeErr)
		}
	})
	keys := pki.NewSigningKeyRepository(observerDB)

	const purpose = authn.AccessTokenKeyPurpose

	// Step 1: at boot, before anyone has signed in, the purpose has no
	// signing key at all. EnsurePurpose runs lazily inside authn's first
	// token issue -- nothing at buildServer or in demo seeding signs a
	// token -- so this assert is what makes the next read's "boot key"
	// label exact: whatever the sign-in below creates is the key the
	// scheduler's scans will have to replace. If a future boot path starts
	// ensuring purposes eagerly, this assert fails and the test's premise
	// must be re-derived rather than silently re-labeled.
	if boot := activeSigningKey(t, keys, purpose); boot != nil {
		t.Fatalf("purpose %q already has an active signing key (%s) before the first sign-in -- EnsurePurpose ran outside a token issue", purpose, boot.ID)
	}

	// Step 2: the real sign-in. Login mints an access token, which runs
	// the Signer's one-time ensure: EnsurePurpose bootstraps the purpose's
	// first key synchronously -- active, one year of validity, a retiring
	// overlap of the app's access-token TTL. This token is signed by that
	// boot key.
	bootToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "periodic-pki")
	boot := activeSigningKey(t, keys, purpose)
	if boot == nil {
		t.Fatalf("after the first sign-in purpose %q still has no active signing key -- authn never bootstrapped it", purpose)
	}

	// Step 3: wait for the scheduled scans to rotate the boot key away.
	// The first tick after the sign-in stages a pending successor (the
	// 400-day lead makes the one-year boot key due immediately); the first
	// tick at least pkiFlowPropagationWindow after that promotes the
	// successor to active and demotes the boot key to retiring, in one
	// transaction. Nothing here requests any of it.
	successor := waitForSigningKeyRotation(t, keys, purpose, boot.ID, signingKeyRotationDeadline)

	// Step 4: the end state. Exactly one active key remains -- the
	// purpose's rotation replaced the boot key rather than adding a second
	// signer -- and the boot key sits in retiring: no longer selected as
	// ActiveSigner, but still verifiable until RetiringAt + its
	// RetiringOverlap (the access-token TTL authn declared at bootstrap,
	// fifteen minutes) passes, which this test's seconds cannot reach.
	if got := activeSigningKey(t, keys, purpose); got == nil || got.ID != successor.ID {
		t.Fatalf("after rotation active key = %+v, want the promoted successor %s", got, successor.ID)
	}
	bootRow, err := keys.FindByID(context.Background(), boot.ID)
	if err != nil {
		t.Fatalf("read boot key %s after rotation: %v", boot.ID, err)
	}
	if bootRow.Status != pki.SigningKeyStatusRetiring {
		t.Fatalf("boot key %s status after rotation = %q, want %q (retired would need %v to pass since RetiringAt -- out of this test's reach)",
			boot.ID, bootRow.Status, pki.SigningKeyStatusRetiring, bootRow.RetiringOverlap)
	}
	if bootRow.RetiringAt == nil {
		t.Fatalf("boot key %s is retiring but carries no RetiringAt", boot.ID)
	}

	// Step 5: the wire smoke over the rotated purpose. A fresh sign-in
	// mints its token under the successor -- the only active key -- and
	// the pre-rotation token was signed by the boot key, now retiring;
	// both must still verify end to end (authn's Verifier accepts the
	// active key and every retiring one). /api/v1/authn/me answers 200
	// only after authn.Middleware verified the bearer signature, so two
	// 200s are the composed proof that the rotation left this app's real
	// consumer fully served.
	postRotationToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "periodic-pki-post-rotation")
	client := srv.Client()
	for name, token := range map[string]string{
		"successor-signed (post-rotation sign-in)": postRotationToken,
		"boot-signed (pre-rotation sign-in)":       bootToken,
	} {
		resp := authnJSON(t, client, http.MethodGet, srv.URL+"/api/v1/authn/me", token, nil, new(map[string]any))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/v1/authn/me with the %s token = %d, want 200 after the rotation", name, resp.StatusCode)
		}
	}
}

// activeSigningKey reads the purpose's currently active signing key
// through the observer connection, returning nil when the purpose has none
// (the pre-bootstrap state). More than one active key is a corruption this
// test cannot proceed past -- the lifecycle's transitions are single-
// transaction flips, so two actives would mean the scan is broken, not
// that the state is merely mid-flight.
func activeSigningKey(t *testing.T, repo *pki.SigningKeyRepository, purpose string) *pki.SigningKey {
	t.Helper()

	actives, err := repo.ListByPurposeAndStatuses(context.Background(), purpose, pki.SigningKeyStatusActive)
	if err != nil {
		t.Fatalf("list active %q signing keys: %v", purpose, err)
	}
	if len(actives) == 0 {
		return nil
	}
	if len(actives) > 1 {
		t.Fatalf("purpose %q has %d active signing keys: %+v", purpose, len(actives), actives)
	}
	return &actives[0]
}

// waitForSigningKeyRotation polls the observer connection until purpose's
// active signing key is no longer bootID -- the only observable signature
// of a completed rotation -- failing after deadline. The poll replaces any
// sleep-based timing: the condition can be satisfied only by rows the
// scheduled expiry scans wrote (a promotion requires a prior staged key
// and runs inside expiryScanHandler), so a pass is real regardless of how
// loaded the machine is, and a deadline miss means the scans never
// advanced the state machine.
func waitForSigningKeyRotation(t *testing.T, repo *pki.SigningKeyRepository, purpose, bootID string, deadline time.Duration) pki.SigningKey {
	t.Helper()

	start := time.Now()
	for {
		actives, err := repo.ListByPurposeAndStatuses(context.Background(), purpose, pki.SigningKeyStatusActive)
		if err != nil {
			t.Fatalf("list active %q signing keys while waiting for rotation: %v", purpose, err)
		}
		if len(actives) == 1 && actives[0].ID != bootID {
			return actives[0]
		}
		if time.Since(start) > deadline {
			t.Fatalf("purpose %q still signs with the boot key %q after %v of real scheduled ticks -- the expiry scan never staged and promoted a successor (active rows: %+v)",
				purpose, bootID, deadline, actives)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
