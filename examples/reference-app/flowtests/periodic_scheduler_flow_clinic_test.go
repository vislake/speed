package flowtests

// periodic_scheduler_flow_clinic_test.go is the self-registered-clinic
// leg of the periodic-scheduler suite whose expiry and retention halves
// live in periodic_scheduler_flow_test.go. It pins the scheduler's tenant
// universe reaching beyond cfg.HostTenants:
// TestBuildServer_PeriodicScheduler_ExpiryAndRetentionSweeps_
// ReachSelfRegisteredClinicTenant proves BOTH wired mechanisms reach the
// rows of a tenant no configuration ever names -- a clinic the real
// self-service flow provisioned at runtime, whose tenant id is derived
// from its registrant's user id (internal/app/self_service.go's
// ClinicTenantOf) and can therefore never appear in cfg.HostTenants
// (internal/app/self_service.go's own comment: a clinic id can never
// collide with a configured host tenant's id). Same two-boot shape as the
// two sibling legs: boot 1 registers the clinic owner through the real
// register route -- whose synchronous provisioning creates the clinic's
// org root, the org.node.created event that lands the clinic in
// go/admin's tenant ledger, the discovery half of the scheduler's
// universe -- hosts a completed object whose retention deadline passes
// and a soft-deleted note backdated past the retention window, all under
// the clinic tenant, and closes with every row and byte present; boot
// 2's first tick must enqueue the clinic's own expiry and retention
// sweeps -- the ledger names the clinic, so the tenant-universe fix
// covers it -- and the wait converges on the expired object's row and
// bytes gone and the expired note's physical row gone while the live
// note survives. The test FAILS when the scheduler's universe is only
// cfg.HostTenants: every self-registered clinic's expired objects and
// soft-deleted notes would age past their deadlines with no sweep ever
// reaching them.
import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/admin"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/storage"
)

// periodicClinicEmail is the account the self-registered-clinic leg
// registers. The local part is unique to this leg so a fresh database
// never collides with the demo or suite accounts; the clinic tenant is
// derived from the user id authn assigns (internal/app/self_service.go's
// ClinicTenantOf), never chosen by the test.
const periodicClinicEmail = "periodic-clinic-founder@example.com"

// createClinicNote POSTs a note to the clinic tenant as its real owner:
// the bearer token alone, no demo headers, so the notes gate resolves
// the acting subject from the verified Principal (the clinic owner, whom
// the registration's provisioning granted the owner role) -- the same
// token-only browser shape the self-service journey tests drive. It
// returns the created note's id.
func createClinicNote(t *testing.T, srv *httptest.Server, token, text string) string {
	t.Helper()

	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		t.Fatalf("marshal note body: %v", err)
	}
	resp := notesRequestAs(t, srv, http.MethodPost, token, "", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/v1/notes as the clinic owner: status = %d, want %d; body = %s",
			resp.StatusCode, http.StatusCreated, raw)
	}
	var created testNote
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create-note response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("create-note response carried no id")
	}
	return created.ID
}

// ledgerRowCount counts the D3 tenant-ledger rows (go/admin's
// admin_tenants table) naming tenant in the shared database file -- the
// durable discovery record internal/app/periodic_scheduler.go's universe reads. The
// count goes through admin's own model over the second connection, the
// same second-connection reach physicalNoteCount uses for notes rows:
// admin_tenants is platform data (never dbkit.TenantScoped), so no
// tenant-scope plugin filter applies and the WHERE clause is the test's
// own primary-key literal.
func ledgerRowCount(t *testing.T, db *gorm.DB, tenant pkgcore.TenantID) int64 {
	t.Helper()

	var count int64
	if err := db.Model(&admin.Tenant{}).Where("tenant_id = ?", string(tenant)).Count(&count).Error; err != nil {
		t.Fatalf("count the admin_tenants row of %s: %v", tenant, err)
	}
	return count
}

// observeClinicSweepEnd takes one full snapshot of the state the first
// expiry sweep AND the first retention sweep of a self-registered
// clinic tenant must have produced, and reports whether every terminal
// property already holds -- observeExpirySweepEnd's shape extended
// across both wired mechanisms, since the clinic leg's point is that the
// one shared tick reaches the clinic tenant at all. It reads raw HTTP
// statuses and raw row and file probes -- no decode helper, whose status
// mismatch would kill the test -- because a not-yet-converged snapshot
// is an intermediate observation (the delete protocols run for
// milliseconds), not a failure. The returned detail names the first
// property that does not yet hold, for the caller's deadline report.
func observeClinicSweepEnd(t *testing.T, srv *httptest.Server, token string, expiringID, expiredNoteID, liveNoteID string, objects *storage.ObjectRepository, db *gorm.DB, clinic pkgcore.TenantID, expiringPath string) (bool, string) {
	t.Helper()

	clinicCtx := pkgcore.WithTenant(context.Background(), clinic)

	expiringMetadata := storageRequest(t, srv, http.MethodGet,
		"/api/v1/storage/objects/"+expiringID, token, "", "", nil)
	expiringMetadata.Body.Close()
	if expiringMetadata.StatusCode != http.StatusNotFound {
		return false, "expired object's metadata GET = " + http.StatusText(expiringMetadata.StatusCode) + ", want 404"
	}

	expiringContent := storageRequest(t, srv, http.MethodGet,
		"/api/v1/storage/objects/"+expiringID+"/content", token, "", "", nil)
	expiringContent.Body.Close()
	if expiringContent.StatusCode != http.StatusNotFound {
		return false, "expired object's content GET = " + http.StatusText(expiringContent.StatusCode) + ", want 404"
	}

	listResp := storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects?limit=10",
		token, "", "", nil)
	var listed testStorageListResponse
	if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
		listResp.Body.Close()
		return false, "list response does not decode: " + err.Error()
	}
	listResp.Body.Close()
	if len(listed.Objects) != 0 {
		return false, "list = " + objectIDs(listed.Objects) + ", want no objects left under the clinic"
	}

	if _, err := objects.FindByID(clinicCtx, expiringID); !dbkit.IsRecordNotFound(err) {
		return false, "expired object's row still exists (FindByID err = " + errString(err) + ")"
	}

	_, err := os.Stat(expiringPath)
	if !os.IsNotExist(err) {
		return false, "expired object's bytes still exist under the object-store root (" + errString(err) + ")"
	}

	expiredRows := physicalNoteCount(t, db, clinic, expiredNoteID)
	if expiredRows != 0 {
		return false, "the sweep-expired note still has " + strconv.FormatInt(expiredRows, 10) + " physical row(s), want 0"
	}
	if liveRows := physicalNoteCount(t, db, clinic, liveNoteID); liveRows != 1 {
		return false, "the live note physical rows = " + strconv.FormatInt(liveRows, 10) + ", want 1 (the sweep must not touch a fresh note)"
	}
	return true, ""
}

// TestBuildServer_PeriodicScheduler_ExpiryAndRetentionSweeps_ReachSelfRegisteredClinicTenant
// is the self-registered-clinic leg of this file's wiring proof: it
// proves the host-scheduled expiry AND retention sweeps reach a tenant
// no configuration ever names -- the closure the file header
// describes -- over two real boots of the composed server sharing one
// SQLite file and one object-store directory, the same two-boot shape
// the two configured-universe legs above use and for the same reason
// (the sweep keys are window-scoped, so boot 2's first tick must find a
// fresh window whose key no enqueue ever resolved).
//
// Boot 1 runs with cfg.DisableQueueWorker set -- the same gate that
// guards the worker and the scheduler -- and hosts the clinic's rows
// through the REAL self-service flow: a fresh account registers through
// authn's real register route, whose synchronous provisioning
// (internal/app/self_service.go's wireSelfService subscription) creates the account's
// own clinic tenant -- derived from the registrant's user id
// (ClinicTenantOf), so by construction absent from cfg.HostTenants --
// with its org root, membership and owner grant. The clinic owner then
// hosts, under the clinic tenant and through the real HTTP surfaces, a
// completed object whose retention deadline passes before boot 1 closes,
// and two notes of which one is soft-deleted with deleted_at backdated
// 45 days (past the 30-day default retention window). The ledger premise
// is asserted in boot 1 itself: the clinic's org-root creation fired
// org's org.node.created event, so go/admin's event-driven lazy
// population (D3) recorded the clinic in the durable tenant ledger --
// the discovery half of the scheduler's universe the fix iterates. Boot
// 1 closes with every row and byte present: expiry and age alone remove
// nothing.
//
// Boot 2 starts the same server over the same files with the normal gate
// and the injected periodicFlowTickInterval cadence. Its first tick
// resolves the tenant universe -- the configured host tenants AND every
// tenant the D3 ledger names, this clinic among them -- and enqueues the
// clinic's first sweeps of fresh windows. The worker drains the clinic's
// expiry sweep into the real LifecycleService sweep (removing the
// expired object's row and bytes) and the clinic's retention sweep into
// compliance's real retentionSweepHandler (hard-deleting the expired
// note's physical row, through the notes participant). The test waits --
// bounded, with no wall-clock race, exactly like the two legs above --
// until every terminal property holds in one observation, then asserts
// the strict post-conditions once more with the decode helpers. If the
// scheduler's universe were still only cfg.HostTenants -- the wiring this
// state the file header records -- boot 2's ticks would never enqueue
// anything
// for the clinic and the wait would fail: a self-registered tenant's
// expired objects and soft-deleted notes past their retention windows
// would stay unreclaimed forever.
func TestBuildServer_PeriodicScheduler_ExpiryAndRetentionSweeps_ReachSelfRegisteredClinicTenant(t *testing.T) {
	cfg := periodicSweepTestConfig(t)
	jpegBytes := jpegWithExif(t)

	// ------------------------------------------------------------------
	// Boot 1: provision the clinic through the real self-service flow,
	// host its expired object and expired note, and let the deadline
	// pass -- with neither the queue worker nor the scheduler running
	// (the same DisableQueueWorker gate). Because no scheduler runs, no
	// sweep key is ever resolved for the clinic in this database file --
	// the precondition boot 2's first sweeps depend on.
	// ------------------------------------------------------------------
	boot1Cfg := cfg
	boot1Cfg.DisableQueueWorker = true
	boot1 := buildPeriodicFlowServer(t, boot1Cfg)

	// The clinic comes from the real self-service flow, never from any
	// configuration: registerFreshAccount goes through authn's real
	// register route, and the provisioning its user-created event fires
	// synchronously creates the clinic. The tenant id derivation is
	// spelled out here rather than reached through the production helper
	// so this leg keeps compiling (and failing with a clean assertion)
	// against the wiring this regression is measured on.
	userID := registerFreshAccount(t, boot1.srv, periodicClinicEmail, testPassword)
	clinic := pkgcore.TenantID("tenant-" + userID)
	status, code, token, signedInTenant := browserSignIn(t, boot1.srv, periodicClinicEmail, testPassword)
	if status != http.StatusOK {
		t.Fatalf("sign-in of the freshly registered clinic owner: status = %d, code = %q, want %d",
			status, code, http.StatusOK)
	}
	if signedInTenant != clinic {
		t.Fatalf("sign-in landed the clinic owner in tenant %q, want the clinic %q", signedInTenant, clinic)
	}

	// The D3 premise the fix rests on: the provisioning's org-root
	// creation fired org's real org.node.created event, and admin's
	// event-driven lazy population (tenant_service.go's
	// handleOrgNodeCreated) recorded the clinic in the durable tenant
	// ledger. A clinic absent from the ledger would make the discovery
	// half of the fixed universe itself broken, and this assertion would
	// fail before the sweeps ever got the chance to prove it.
	db := openSecondDB(t, cfg)
	if got := ledgerRowCount(t, db, clinic); got != 1 {
		t.Fatalf("admin_tenants rows naming the clinic = %d, want 1 (the self-registered clinic must enter the D3 tenant ledger when its org root is created)",
			got)
	}

	// The clinic's expiring object, hosted through the real storage HTTP
	// surface as the clinic owner -- the bearer token alone, no demo
	// headers, the browser shape. RFC 3339 carries whole seconds, so a
	// deadline declared three seconds out lands 2-3s after the create
	// request -- the same window the tenant-acme leg uses.
	expiresAt := time.Now().Add(3 * time.Second).Format(time.RFC3339)
	declared := declareUploadWithExpiry(t, boot1.srv, token, "",
		int64(len(jpegBytes)), "image/jpeg", expiresAt)
	uploadBytes(t, boot1.srv, token, "", declared.ID, jpegBytes)
	expiring := completeObject(t, boot1.srv, token, "", declared.ID)
	if expiring.ExpiresAt == "" {
		t.Fatalf("completed object lost its retention deadline: %+v", expiring)
	}
	expiryTime, err := time.Parse(time.RFC3339, expiring.ExpiresAt)
	if err != nil {
		t.Fatalf("completed object's expiresAt %q is not RFC 3339: %v", expiring.ExpiresAt, err)
	}

	// The clinic's notes: one to expire (soft-deleted with deleted_at
	// backdated 45 days -- past the 30-day default retention window, so
	// any retention sweep that ever runs will reap it) and the live
	// survivor, both created through the real notes HTTP surface as the
	// clinic owner. The soft delete and backdate go through the
	// second-connection reach compliance_flow_test.go's
	// softDeleteAndBackdate uses; no product API ages deleted_at.
	expiredNoteID := createClinicNote(t, boot1.srv, token, "clinic note past its retention window")
	liveNoteID := createClinicNote(t, boot1.srv, token, "clinic note still inside its retention window")
	softDeleteAndBackdate(t, db, clinic, expiredNoteID, time.Now().Add(-45*24*time.Hour))

	// Wait the deadline out. Nothing in boot 1 can react to it -- no
	// worker, no scheduler -- so once the clock is past the deadline the
	// expired-but-present state is terminal and boot 1 can close.
	waitUntil(t, expiryTime.Add(500*time.Millisecond), "the clinic object's expiry deadline to pass in boot 1")

	// The state handed to boot 2: the expired object is still served and
	// listed, and both note rows are physically present. Expiry and age
	// alone remove nothing; removal happens only when the wired sweeps
	// run.
	objectResp := storageRequest(t, boot1.srv, http.MethodGet, "/api/v1/storage/objects/"+expiring.ID,
		token, "", "", nil)
	objectResp.Body.Close()
	if objectResp.StatusCode != http.StatusOK {
		t.Fatalf("GET the clinic's expired object before boot 2 = %d, want 200", objectResp.StatusCode)
	}
	contentResp := storageRequest(t, boot1.srv, http.MethodGet,
		"/api/v1/storage/objects/"+expiring.ID+"/content", token, "", "", nil)
	contentResp.Body.Close()
	if contentResp.StatusCode != http.StatusOK {
		t.Fatalf("GET content of the clinic's expired object before boot 2 = %d, want 200", contentResp.StatusCode)
	}
	listResp := storageRequest(t, boot1.srv, http.MethodGet, "/api/v1/storage/objects?limit=10",
		token, "", "", nil)
	before := decodeList(t, listResp, "GET list after the deadline passed, before boot 2")
	if len(before.Objects) != 1 || before.Objects[0].ID != expiring.ID {
		t.Fatalf("pre-boot-2 clinic list = %+v, want exactly the one expired object", before.Objects)
	}
	if got := physicalNoteCount(t, db, clinic, expiredNoteID); got != 1 {
		t.Fatalf("pre-boot-2 expired note physical rows = %d, want 1", got)
	}
	if got := physicalNoteCount(t, db, clinic, liveNoteID); got != 1 {
		t.Fatalf("pre-boot-2 live note physical rows = %d, want 1", got)
	}

	if closeErr := boot1.close(); closeErr != nil {
		t.Fatalf("closing boot 1: %v", closeErr)
	}

	// ------------------------------------------------------------------
	// Boot 2: the restart. Same database file, same object-store
	// directory, normal gate, the injected cadence. The scheduler's very
	// first tick resolves the tenant universe -- the configured host
	// tenants AND the D3 tenant ledger, whose durable row names this
	// clinic -- and enqueues the clinic's first expiry and retention
	// sweeps of fresh windows; the worker drains them into the wired
	// mechanisms' real handlers, which find the rows boot 1 left expired.
	// ------------------------------------------------------------------
	boot2Cfg := cfg
	boot2Cfg.PeriodicTaskInterval = periodicFlowTickInterval
	boot2 := buildPeriodicFlowServer(t, boot2Cfg)

	// A fresh sign-in for boot 2's HTTP probes: the same account lands in
	// the same clinic across the restart (the org membership row
	// survives, the property self_service_test.go's restart leg pins).
	status, code, token, signedInTenant = browserSignIn(t, boot2.srv, periodicClinicEmail, testPassword)
	if status != http.StatusOK {
		t.Fatalf("boot-two sign-in of the clinic owner: status = %d, code = %q, want %d",
			status, code, http.StatusOK)
	}
	if signedInTenant != clinic {
		t.Fatalf("boot-two sign-in landed the clinic owner in tenant %q, want the clinic %q", signedInTenant, clinic)
	}

	// The second-connection observer -- the same handle boot 1 already
	// opened (the identical pattern the retention leg uses across its own
	// two boots): the row-absence halves of the removal assertions must
	// go through real repositories, never raw gorm.
	objects := storage.NewObjectRepository(db)
	clinicBytesPath := tenantObjectPath(cfg.ObjectStoreRoot, clinic, expiring.ID)

	// Wait for the sweeps' terminal state. Each observation runs the full
	// property set -- the expiry sweep's removals AND the retention
	// sweep's, both enqueued by the same ticks -- so the wait cannot exit
	// on a transient mid-protocol snapshot. Observations are paced ~5x
	// per second, the pacing lesson this file's cadence comment records.
	// The ceiling turns sweeps that never run -- the failure mode this
	// leg exists to catch -- into a named failure: a universe that never
	// names this clinic can never converge, however long the wait.
	convergenceDeadline := time.Now().Add(90 * time.Second)
	for {
		converged, detail := observeClinicSweepEnd(t, boot2.srv, token,
			expiring.ID, expiredNoteID, liveNoteID, objects, db, clinic, clinicBytesPath)
		if converged {
			break
		}
		if time.Now().After(convergenceDeadline) {
			t.Fatalf("the first sweeps never reached the self-registered clinic's rows within 90s; last observation: %s", detail)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The strict post-conditions, asserted once more with the decode
	// helpers now that the state is terminal, so each property's failure
	// names itself in the test output.
	expiringMetadata := storageRequest(t, boot2.srv, http.MethodGet,
		"/api/v1/storage/objects/"+expiring.ID, token, "", "", nil)
	assertStorageError(t, expiringMetadata, http.StatusNotFound, "storage.object_not_found",
		"GET metadata of the sweep-expired clinic object after boot 2")
	expiringContent := storageRequest(t, boot2.srv, http.MethodGet,
		"/api/v1/storage/objects/"+expiring.ID+"/content", token, "", "", nil)
	assertStorageError(t, expiringContent, http.StatusNotFound, "storage.object_not_found",
		"GET content of the sweep-expired clinic object after boot 2")

	listAfter := storageRequest(t, boot2.srv, http.MethodGet, "/api/v1/storage/objects?limit=10",
		token, "", "", nil)
	final := decodeList(t, listAfter, "GET list after the sweeps")
	if len(final.Objects) != 0 {
		t.Fatalf("post-sweep clinic list = %+v, want no objects left under the clinic", final.Objects)
	}

	if _, findErr := objects.FindByID(pkgcore.WithTenant(context.Background(), clinic), expiring.ID); !dbkit.IsRecordNotFound(findErr) {
		t.Fatalf("the sweep-expired clinic object's row is still present (FindByID err = %v)", findErr)
	}
	if _, err := os.Stat(clinicBytesPath); !os.IsNotExist(err) {
		t.Fatalf("the sweep-expired clinic object's bytes still exist under %q (err = %v)", clinicBytesPath, err)
	}

	if got := physicalNoteCount(t, db, clinic, expiredNoteID); got != 0 {
		t.Fatalf("the sweep-expired clinic note still has %d physical row(s), want 0", got)
	}
	if got := physicalNoteCount(t, db, clinic, liveNoteID); got != 1 {
		t.Fatalf("the live clinic note physical rows = %d, want 1 (the sweep must not touch a fresh note)", got)
	}
}
