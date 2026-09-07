package main

// periodic_scheduler_flow_test.go is the flow-test companion of
// periodic_scheduler.go: it proves the host's periodic-task scheduler
// really drives a wired jobs mechanism through the composed stack. The
// scheduler shares one tick cadence with the queue worker (the same
// cfg.DisableQueueWorker gate starts and stops both, per
// periodic_scheduler.go's own doc comment), so a real server built with a
// tuned cfg.PeriodicTaskInterval performs real enqueues on real ticks
// and the app's real standalone queue drains them into the wired modules'
// handlers -- nothing here is called in-process, enqueued by hand or
// otherwise driven around the scheduler.
//
// What this file proves:
//
//   - The storage expiry-sweep leg is a host-wired removal e2e:
//     TestBuildServer_PeriodicScheduler_ExpirySweep_RemovesExpiredObject
//     boots the real composed server twice over one SQLite file and one
//     object-store directory (the directory the APP_OBJECT_STORE_ROOT
//     injection pins -- without it, each boot's store would be a throwaway
//     temp directory and boot 2 could not remove boot 1's bytes). Boot 1
//     runs with the queue worker and the scheduler disabled (the same
//     gate), hosts a completed object whose retention deadline then
//     passes plus a no-deadline survivor, and closes with both still
//     served and listed -- expiry alone removes nothing. Boot 2 runs with
//     the normal gate and the flow tests' injected cadence
//     (periodicFlowTickInterval); its very first
//     tick enqueues tenant-acme's FIRST-ever expiry sweep (boot 1 never
//     resolved the sweep's deterministic per-tenant idempotency key,
//     because its scheduler never ran), the worker drains the task into
//     storage.LifecycleService's real sweep, and the sweep runs the real
//     delete protocol. The expired object's row is gone -- observed
//     through a second-connection storage.ObjectRepository, the same
//     "buildServer hands out neither its *gorm.DB nor a module service"
//     second connection server_test.go's audit test opens -- its bytes are
//     gone on the filesystem under cfg.ObjectStoreRoot, its metadata and
//     content answer 404, and the list holds only the survivor, whose own
//     row, bytes and reads are untouched. The test is shaped so it FAILS
//     under the one-shot semantics it replaces: had boot 1 let a tick's
//     sweep run (with nothing to delete), the key would be resolved
//     forever on this queue and no later sweep could ever execute.
//
//   - The compliance retention-sweep leg is the trigger-half twin of the
//     storage leg above: TestBuildServer_PeriodicScheduler_RetentionSweep_
//     ReapsExpiredSoftDeletedNote proves the host-scheduled retention sweep
//     really hard-deletes an expired soft-deleted note's row end to end --
//     the half go/compliance/AGENTS.md's Known limitations records as
//     having no schedule point until the host wiring this test proves.
//     Same two-boot shape, same reason: StandaloneQueue's permanent
//     idempotency gives each database file exactly one retention sweep per
//     tenant (the first-ever one), so boot 1 must leave the key
//     unresolved. Boot 1 runs with the worker and scheduler disabled,
//     hosts a live note and one soft-deleted with deleted_at backdated
//     45 days (past the sweep's 30-day default window -- no product API
//     ages deleted_at; the backdate goes through the same second-connection
//     reach compliance_flow_test.go's softDeleteAndBackdate uses), and
//     closes with both rows physically present -- age alone removes
//     nothing. Boot 2 runs the normal gate with the injected cadence; its
//     very first tick enqueues tenant-acme's FIRST-ever retention sweep
//     (alongside the expiry sweep -- both wired mechanisms share the one
//     tick, per periodic_scheduler.go), the worker drains the task into
//     compliance's real retentionSweepHandler, and SweepTenant runs the
//     notes participant's real HardDelete: the expired note's physical row
//     is gone -- observed through the same second connection -- while the
//     live note's row is untouched. The test FAILS when retention
//     scheduling is unwired -- the state that predates this round, where
//     no tick enqueued compliance's task and every soft-deleted row past
//     its window was kept indefinitely.
//
// What remains honestly limited:
//
//   - StandaloneQueue holds a resolved idempotency key forever (go/jobs:
//     succeeded rows are never deleted), so on this queue each tenant gets
//     exactly one expiry sweep per database file -- the first-ever one,
//     whenever the host's scheduler first enqueues it, which is why the
//     two-boot shape above must exist at all. An object whose retention
//     deadline passes after that one sweep has run is never reaped on
//     this queue; the bounded-idempotency distributed queue schedules a
//     fresh sweep every tick, which is the design intent
//     go/storage/cleanup.go's idempotency-key doc records. Module
//     semantics (go/jobs, go/storage) are unchanged by this round; the
//     residual limitation and its dated record live in this file's sweep
//     test, in periodic_scheduler.go's doc comment, and in
//     go/storage/AGENTS.md.
//
//   - pki's signing-key expiry scan IS genuinely periodic on the
//     standalone queue -- EnqueueExpiryScan carries no idempotency key,
//     each tick is its own independent occurrence, and the scan's guarded
//     status-updated state machine makes overlapping scans safe. The
//     resulting key rotation is proven end to end in
//     periodic_pki_scan_flow_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/storage"
)

// periodicFlowTickInterval is the cadence the scheduler flow tests inject
// through cfg.PeriodicTaskInterval: a real tick every second, far faster
// than the production default (defaultPeriodicTaskSchedulerInterval) so a
// mechanism's scheduled work lands within test time, and far slower than a
// spin so a pass never depends on tick-counting races.
//
// One second is a deliberate floor, not an arbitrary "fast": every tick
// writes to the app's SQLite file (the sweep's per-tenant enqueue merging
// into the resolved job, the pki scan's fresh enqueue), while the queue's
// own connection pool writes the same file to claim and complete the job
// the tick enqueued. A read-then-write job such as the expiry sweep must
// upgrade its transaction's shared lock to a write lock mid-flight, and
// that upgrade fails IMMEDIATELY with SQLITE_BUSY whenever another
// connection happens to hold the write lock -- SQLite's busy_timeout does
// not cover the upgrade path (see go/dbkit/dialect/sqlite's
// withDefaultPragmas doc comment), so the sweep has only the queue's
// bounded retry budget to absorb such collisions: four attempts, then the
// job is dead-lettered forever. The cadence must keep per-tick writes
// sparse enough that four consecutive collisions are unreachable under
// -race, where every handler window is wide: 150ms measured out at
// roughly one dead-lettered sweep per six -race runs, and 1s puts the
// write duty cycle an order of magnitude lower. One second is also all
// the pki rotation leg needs -- staging lands on the first tick after the
// sign-in and promotion on the next one, about two seconds apart -- so a
// single shared constant keeps both legs honest.
const periodicFlowTickInterval = time.Second

// buildPeriodicTestServer is buildTestServer with a tuning hook: it starts
// from the same testConfig(buildTestServer uses), lets the caller adjust
// the serverConfig (the scheduler flow tests inject the
// periodicFlowTickInterval cadence here -- the test-override field
// buildServer reads, exactly like the Mailer and DisableQueueWorker fields
// other tests tune), and then builds the exact composed server
// buildTestServer builds,
// periodic-task scheduler included. The *compliance.Module buildTestServer
// returns is discarded, like every HTTP-driven flow test does.
func buildPeriodicTestServer(t *testing.T, tune func(cfg *serverConfig)) (*httptest.Server, serverConfig) {
	t.Helper()

	cfg := testConfig(t)
	if tune != nil {
		tune(&cfg)
	}
	handler, cleanup, _, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, cfg
}

// periodicSweepTestConfig returns a serverConfig for the two-boot sweep
// test: testConfig's per-test temp files are replaced with fixed paths the
// test itself owns (one t.TempDir each), so both boots of the sweep test
// share one SQLite database file and one object-store directory. cfg is
// returned by value; each boot copies it and tunes the one field that boot
// needs (boot 1 disables the worker-and-scheduler pair, boot 2 sets the
// injected periodicFlowTickInterval cadence), keeping everything else --
// database file,
// object-store root, HostTenants, the demo membership store -- identical
// across the restart the test simulates.
func periodicSweepTestConfig(t *testing.T) serverConfig {
	t.Helper()
	cfg := testConfig(t)
	cfg.SQLitePath = filepath.Join(t.TempDir(), "periodic-sweep.db")
	cfg.ObjectStoreRoot = filepath.Join(t.TempDir(), "objectstore")
	return cfg
}

// periodicFlowServer is a built composed server whose teardown the test
// controls explicitly: close() shuts the HTTP listener down and runs
// buildServer's cleanup to completion, releasing the queue, the scheduler
// and the database connections. A two-boot test must finish one boot
// completely -- schema applied, connections closed, files released --
// before building the next one over the same database and object-store
// files, so the second boot's teardown cannot simply be left to t.Cleanup
// ordering.
type periodicFlowServer struct {
	srv     *httptest.Server
	cleanup func() error

	closeOnce sync.Once
	closeErr  error
}

// close tears the server down exactly once and returns the cleanup error,
// if any. Later calls (the t.Cleanup registered by
// buildPeriodicFlowServer) return the same error without re-running the
// teardown.
func (s *periodicFlowServer) close() error {
	s.closeOnce.Do(func() {
		s.srv.Close()
		s.closeErr = s.cleanup()
	})
	return s.closeErr
}

// buildPeriodicFlowServer builds the exact composed server buildServer
// produces for cfg -- which the caller has already tuned for this boot --
// behind an httptest.Server, and registers close() as a t.Cleanup safety
// net so a test that fails before reaching its own explicit close() still
// tears the boot down.
func buildPeriodicFlowServer(t *testing.T, cfg serverConfig) *periodicFlowServer {
	t.Helper()

	handler, cleanup, _, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	server := &periodicFlowServer{
		srv:     httptest.NewServer(handler),
		cleanup: cleanup,
	}
	t.Cleanup(func() {
		if err := server.close(); err != nil {
			t.Errorf("periodic-flow server cleanup: %v", err)
		}
	})
	return server
}

// waitUntil blocks until time.Now() reaches mark, sleeping in short
// steps, and fails the test if mark is not reached within a 15-second
// ceiling. It is the bounded form of a sleep: every caller uses it to wait
// for a time that is guaranteed to arrive (the tests never wait on a race
// -- see each call site), so the ceiling exists only to turn a broken
// assumption into a named failure instead of a hang.
func waitUntil(t *testing.T, mark time.Time, what string) {
	t.Helper()
	ceiling := time.Now().Add(15 * time.Second)
	for time.Now().Before(mark) {
		if time.Now().After(ceiling) {
			t.Fatalf("timed out waiting until %s (mark %s passed the 15s ceiling)", what, mark)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// originalObjectPath returns the filesystem path under an injected
// APP_OBJECT_STORE_ROOT directory where the local object store keeps one
// object's original bytes. It mirrors the grammar go/storage's key.go
// derives and pkgcore's localObjectStore realizes on disk --
// root/<tenantID>/<objectID>/original -- so the byte assertions observe
// the store's real layout without importing the store's internals.
func originalObjectPath(root string, objectID string) string {
	return filepath.Join(root, "tenant-acme", objectID, "original")
}

// declareUploadWithExpiry declares an upload exactly like declareUpload,
// plus an absolute retention deadline: the expiresAt the spec's
// StorageCreateObjectRequest carries (RFC 3339, e.g.
// time.Now().Add(d).Format(time.RFC3339)). The returned descriptor is the
// uploading object of the 201 response.
func declareUploadWithExpiry(t *testing.T, srv *httptest.Server, token, user string, declaredSize int64, declaredType, expiresAt string) testStorageObject {
	t.Helper()

	payload, err := json.Marshal(map[string]any{
		"declaredSize": declaredSize,
		"declaredType": declaredType,
		"expiresAt":    expiresAt,
	})
	if err != nil {
		t.Fatalf("marshal declaration: %v", err)
	}
	resp := storageRequest(t, srv, http.MethodPost, "/api/v1/storage/objects",
		token, user, "application/json", bytes.NewReader(payload))
	return decodeStorageObject(t, resp, http.StatusCreated, "POST /api/v1/storage/objects with an expiry")
}

// observeExpirySweepEnd takes one full snapshot of the state the
// first-ever expiry sweep must have produced and reports whether every
// terminal property already holds. It reads raw HTTP statuses and raw row
// and file probes -- no decode helper, whose status mismatch would kill
// the test -- because a not-yet-converged snapshot is an intermediate
// observation (the delete protocol's mark-then-remove runs for
// milliseconds), not a failure. The returned detail names the first
// property that does not yet hold, for the caller's deadline report.
func observeExpirySweepEnd(t *testing.T, srv *httptest.Server, token string, expiringID, survivorID string, objects *storage.ObjectRepository, acmeCtx context.Context, expiringPath, survivorPath string) (bool, string) {
	t.Helper()

	expiringMetadata := storageRequest(t, srv, http.MethodGet,
		"/api/v1/storage/objects/"+expiringID, token, demoOwnerUserID, "", nil)
	expiringMetadata.Body.Close()
	if expiringMetadata.StatusCode != http.StatusNotFound {
		return false, "expired object's metadata GET = " + http.StatusText(expiringMetadata.StatusCode) + ", want 404"
	}

	expiringContent := storageRequest(t, srv, http.MethodGet,
		"/api/v1/storage/objects/"+expiringID+"/content", token, demoOwnerUserID, "", nil)
	expiringContent.Body.Close()
	if expiringContent.StatusCode != http.StatusNotFound {
		return false, "expired object's content GET = " + http.StatusText(expiringContent.StatusCode) + ", want 404"
	}

	survivorMetadata := storageRequest(t, srv, http.MethodGet,
		"/api/v1/storage/objects/"+survivorID, token, demoOwnerUserID, "", nil)
	survivorMetadata.Body.Close()
	if survivorMetadata.StatusCode != http.StatusOK {
		return false, "survivor's metadata GET = " + http.StatusText(survivorMetadata.StatusCode) + ", want 200"
	}

	listResp := storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects?limit=10",
		token, demoOwnerUserID, "", nil)
	var listed testStorageListResponse
	if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
		listResp.Body.Close()
		return false, "list response does not decode: " + err.Error()
	}
	listResp.Body.Close()
	if len(listed.Objects) != 1 || listed.Objects[0].ID != survivorID {
		return false, "list = " + objectIDs(listed.Objects) + ", want only the survivor"
	}

	if _, err := objects.FindByID(acmeCtx, expiringID); !errIsRecordNotFound(err) {
		return false, "expired object's row still exists (FindByID err = " + errString(err) + ")"
	}
	survivorRow, err := objects.FindByID(acmeCtx, survivorID)
	if err != nil || survivorRow == nil {
		return false, "survivor's row is gone (FindByID err = " + errString(err) + ")"
	}

	_, err = os.Stat(expiringPath)
	if !os.IsNotExist(err) {
		return false, "expired object's bytes still exist under the object-store root (" + errString(err) + ")"
	}
	if _, err := os.Stat(survivorPath); err != nil {
		return false, "survivor's bytes are gone from the object-store root (" + errString(err) + ")"
	}
	return true, ""
}

// errIsRecordNotFound reports whether err is dbkit's record-not-found
// answer. Repository methods return dbkit.ErrRecordNotFound.WithParam
// ("id", ...) -- a derived *Error, never the sentinel pointer itself -- so
// the match compares apperr codes (apperr.As plus the code constant), the
// idiom go/storage's own tests use; errors.Is would compare *Error
// pointers and never match a derived value.
func errIsRecordNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == dbkit.ErrRecordNotFound.Code
}

// objectIDs renders a list's object ids for a snapshot detail line.
func objectIDs(objects []testStorageObject) string {
	ids := make([]string, len(objects))
	for i, o := range objects {
		ids[i] = o.ID
	}
	if len(ids) == 0 {
		return "[]"
	}
	return "[" + ids[0] + " ...]"
}

// errString renders err for a snapshot detail line, "nil" when there is
// none.
func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

// TestBuildServer_PeriodicScheduler_ExpirySweep_RemovesExpiredObject is
// the storage expiry-sweep leg of this file's wiring proof: it proves the
// host-wired mechanism really removes an expired object's rows and bytes
// end to end -- the removal e2e the file header describes, over two real
// boots of the composed server sharing one SQLite file and one
// object-store directory. See the header for why the two-boot shape is
// required (StandaloneQueue's permanent idempotency gives each database
// file exactly one sweep per tenant: the first-ever one) and for the
// residual limitation that shape leaves recorded.
//
// Boot 1 runs with cfg.DisableQueueWorker set, the same gate that guards
// both the worker and the scheduler (periodic_scheduler.go's doc comment),
// so no sweep key is ever resolved for tenant-acme in this database file
// while boot 1 is up -- the queue never starts, so its schema is never
// even created (go/jobs's StandaloneQueue applies it inside Start), which
// is why the thumbnail-derive enqueues the completions attempt are logged
// and dropped in boot 1: another honest record of the gate, and it leaves
// boot 2's tenant slots entirely free for the sweep. Boot 1 hosts a
// completed object whose retention deadline passes before it closes, plus
// a no-deadline survivor, and asserts both are still served and listed
// once the deadline is behind them: expiry alone removes nothing.
//
// Boot 2 starts the same server over the same files with the normal gate
// and the injected periodicFlowTickInterval cadence. Its first tick
// enqueues tenant-acme's first-ever expiry sweep; the worker drains it
// into the
// real LifecycleService sweep, which finds the expired completed object
// and runs the real delete protocol. The test waits -- bounded, with no
// wall-clock race: nothing in the wait depends on a tick, only on the
// terminal state the sweep must produce -- until the object's metadata and
// content answer 404, the list holds only the survivor, the object's row
// is absent from the database (observed through a second connection, the
// same reach server_test.go's audit test uses), and its bytes are absent
// under cfg.ObjectStoreRoot, then asserts the survivor's row, bytes and
// reads are all untouched. If the one-shot semantics this test replaces
// were still in force, no sweep would ever run in boot 2 and the wait
// would fail.
func TestBuildServer_PeriodicScheduler_ExpirySweep_RemovesExpiredObject(t *testing.T) {
	cfg := periodicSweepTestConfig(t)
	jpegBytes := jpegWithExif(t)

	// ------------------------------------------------------------------
	// Boot 1: host the expiring object and the survivor, with neither the
	// queue worker nor the scheduler running (the same DisableQueueWorker
	// gate), and let the deadline pass. Because no scheduler runs, the
	// sweep's per-tenant idempotency key stays unresolved in this database
	// file -- the precondition boot 2's first-ever sweep depends on.
	// ------------------------------------------------------------------
	boot1Cfg := cfg
	boot1Cfg.DisableQueueWorker = true
	boot1 := buildPeriodicFlowServer(t, boot1Cfg)
	boot1Token := registerAndAuthenticate(t, boot1.srv, cfg, "tenant-acme", "periodic-sweep-boot-1")

	// The expiring object. RFC 3339 carries whole seconds, so a deadline
	// declared three seconds out lands 2-3s after the create request --
	// comfortably past the completion below, and short enough that boot 1
	// does not idle long before closing.
	expiresAt := time.Now().Add(3 * time.Second).Format(time.RFC3339)
	declared := declareUploadWithExpiry(t, boot1.srv, boot1Token, demoOwnerUserID,
		int64(len(jpegBytes)), "image/jpeg", expiresAt)
	uploadBytes(t, boot1.srv, boot1Token, demoOwnerUserID, declared.ID, jpegBytes)
	expiring := completeObject(t, boot1.srv, boot1Token, demoOwnerUserID, declared.ID)
	if expiring.ExpiresAt == "" {
		t.Fatalf("completed object lost its retention deadline: %+v", expiring)
	}
	expiryTime, err := time.Parse(time.RFC3339, expiring.ExpiresAt)
	if err != nil {
		t.Fatalf("completed object's expiresAt %q is not RFC 3339: %v", expiring.ExpiresAt, err)
	}

	// The live survivor -- no expiresAt on the wire at all. Each completion
	// attempts to enqueue a thumbnail-derive task; with the worker gated
	// off, the queue's schema was never created (go/jobs applies it inside
	// Start), so those enqueues fail and storage logs the failure as a
	// warning, exactly as its wiring contract allows -- completion itself
	// is never held hostage to the queue. That leaves boot 2 with no
	// derivative backlog, so the first tick's sweep is claimed without
	// competing for tenant-acme's worker slots.
	survivor := uploadAndComplete(t, boot1.srv, boot1Token, jpegBytes, "")
	if survivor.ExpiresAt != "" {
		t.Fatalf("survivor carries a retention deadline it was never declared with: %+v", survivor)
	}

	// Wait the deadline out. Nothing in boot 1 can react to it -- no
	// worker, no scheduler -- so once the clock is past the deadline the
	// expired-but-present state is terminal and boot 1 can close.
	waitUntil(t, expiryTime.Add(500*time.Millisecond), "the expiry deadline to pass in boot 1")

	// The state handed to boot 2: both objects are still served and
	// listed. Expiry alone removes nothing; deletion happens only when the
	// wired sweep runs.
	for _, obj := range []testStorageObject{expiring, survivor} {
		resp := storageRequest(t, boot1.srv, http.MethodGet, "/api/v1/storage/objects/"+obj.ID,
			boot1Token, demoOwnerUserID, "", nil)
		got := decodeStorageObject(t, resp, http.StatusOK, "GET after the deadline passed, before boot 2")
		if got.ID != obj.ID {
			t.Fatalf("GET %s answered object %s", obj.ID, got.ID)
		}
	}
	resp := storageRequest(t, boot1.srv, http.MethodGet,
		"/api/v1/storage/objects/"+expiring.ID+"/content", boot1Token, demoOwnerUserID, "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET content of the expired object before boot 2 = %d, want 200", resp.StatusCode)
	}
	listResp := storageRequest(t, boot1.srv, http.MethodGet, "/api/v1/storage/objects?limit=10",
		boot1Token, demoOwnerUserID, "", nil)
	before := decodeList(t, listResp, "GET list after the deadline passed, before boot 2")
	if len(before.Objects) != 2 {
		t.Fatalf("pre-boot-2 list = %+v, want exactly the two completed objects", before.Objects)
	}

	if closeErr := boot1.close(); closeErr != nil {
		t.Fatalf("closing boot 1: %v", closeErr)
	}

	// ------------------------------------------------------------------
	// Boot 2: the restart. Same database file, same object-store
	// directory, normal gate, the injected cadence. The scheduler's very
	// first tick enqueues tenant-acme's first-ever expiry sweep, the
	// worker drains it, and the sweep finds the object boot 1 let expire.
	// ------------------------------------------------------------------
	boot2Cfg := cfg
	boot2Cfg.PeriodicTaskInterval = periodicFlowTickInterval
	boot2 := buildPeriodicFlowServer(t, boot2Cfg)
	boot2Token := registerAndAuthenticate(t, boot2.srv, cfg, "tenant-acme", "periodic-sweep-boot-2")

	// The second-connection observer: buildServer hands out neither its
	// *gorm.DB nor a module service, so a second dbkit.Open connection to
	// the same SQLite file -- the identical pattern server_test.go's audit
	// test uses -- is the only reach a test has into the objects table,
	// and the row-absence half of the removal assertion must go through a
	// real storage.ObjectRepository, never raw gorm.
	observerDB, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: cfg.SQLitePath})
	if err != nil {
		t.Fatalf("open second connection to %q: %v", cfg.SQLitePath, err)
	}
	t.Cleanup(func() {
		sqlDB, dbErr := observerDB.DB()
		if dbErr != nil {
			t.Errorf("observer connection: %v", dbErr)
			return
		}
		if closeErr := sqlDB.Close(); closeErr != nil {
			t.Errorf("closing observer connection: %v", closeErr)
		}
	})
	objects := storage.NewObjectRepository(observerDB)
	acmeCtx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))
	expiringBytesPath := originalObjectPath(cfg.ObjectStoreRoot, expiring.ID)
	survivorBytesPath := originalObjectPath(cfg.ObjectStoreRoot, survivor.ID)

	// Wait for the sweep's terminal state. Each observation runs the full
	// property set, so the wait cannot exit on a transient mid-protocol
	// snapshot (metadata 404 while the bytes are not yet removed, say):
	// every terminal property must hold in one and the same observation.
	// Observations are paced ~5x per second rather than hammered: each one
	// takes read locks on the SQLite file (the HTTP reads on the server's
	// connections plus this test's observer connection), and a
	// near-continuous read stream under -race slowed the sweep's own
	// writes into SQLITE_BUSY failures -- the queue's designed answer is
	// an exponential retry backoff, which the ceiling below must leave
	// room for. The ceiling turns a sweep that never runs -- the failure
	// mode this test exists to catch -- into a named failure: a broken
	// mechanism can never converge, however long the wait, so a generous
	// ceiling costs the test none of its discriminating power; it only
	// bounds how long a healthy-but-contended sweep may take to land.
	convergenceDeadline := time.Now().Add(90 * time.Second)
	for {
		converged, detail := observeExpirySweepEnd(t, boot2.srv, boot2Token,
			expiring.ID, survivor.ID, objects, acmeCtx, expiringBytesPath, survivorBytesPath)
		if converged {
			break
		}
		if time.Now().After(convergenceDeadline) {
			t.Fatalf("the first-ever expiry sweep never reached its terminal state within 90s; last observation: %s", detail)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The strict post-conditions, asserted once more with the decode
	// helpers now that the state is terminal, so each property's failure
	// names itself in the test output.
	expiringMetadata := storageRequest(t, boot2.srv, http.MethodGet,
		"/api/v1/storage/objects/"+expiring.ID, boot2Token, demoOwnerUserID, "", nil)
	assertStorageError(t, expiringMetadata, http.StatusNotFound, "storage.object_not_found",
		"GET metadata of the sweep-expired object after boot 2")
	expiringContent := storageRequest(t, boot2.srv, http.MethodGet,
		"/api/v1/storage/objects/"+expiring.ID+"/content", boot2Token, demoOwnerUserID, "", nil)
	assertStorageError(t, expiringContent, http.StatusNotFound, "storage.object_not_found",
		"GET content of the sweep-expired object after boot 2")

	survivorMetadata := storageRequest(t, boot2.srv, http.MethodGet,
		"/api/v1/storage/objects/"+survivor.ID, boot2Token, demoOwnerUserID, "", nil)
	survivorAfter := decodeStorageObject(t, survivorMetadata, http.StatusOK,
		"GET the survivor after the sweep")
	if survivorAfter.ID != survivor.ID {
		t.Fatalf("GET survivor answered object %s", survivorAfter.ID)
	}
	survivorContent := storageRequest(t, boot2.srv, http.MethodGet,
		"/api/v1/storage/objects/"+survivor.ID+"/content", boot2Token, demoOwnerUserID, "", nil)
	survivorContent.Body.Close()
	if survivorContent.StatusCode != http.StatusOK {
		t.Fatalf("GET content of the survivor after the sweep = %d, want 200", survivorContent.StatusCode)
	}
	listAfter := storageRequest(t, boot2.srv, http.MethodGet, "/api/v1/storage/objects?limit=10",
		boot2Token, demoOwnerUserID, "", nil)
	final := decodeList(t, listAfter, "GET list after the sweep")
	if len(final.Objects) != 1 || final.Objects[0].ID != survivor.ID {
		t.Fatalf("post-sweep list = %+v, want only the survivor", final.Objects)
	}

	if _, findErr := objects.FindByID(acmeCtx, expiring.ID); !errIsRecordNotFound(findErr) {
		t.Fatalf("the sweep-expired object's row is still present (FindByID err = %v)", findErr)
	}
	survivorRow, err := objects.FindByID(acmeCtx, survivor.ID)
	if err != nil || survivorRow == nil {
		t.Fatalf("the survivor's row is gone after the sweep (FindByID err = %v)", err)
	}

	if _, err := os.Stat(expiringBytesPath); !os.IsNotExist(err) {
		t.Fatalf("the sweep-expired object's bytes still exist under %q (err = %v)", expiringBytesPath, err)
	}
	if _, err := os.Stat(survivorBytesPath); err != nil {
		t.Fatalf("the survivor's bytes are gone from %q: %v", survivorBytesPath, err)
	}
}

// TestBuildServer_PeriodicScheduler_RetentionSweep_ReapsExpiredSoftDeletedNote
// is the compliance retention-sweep leg of this file's wiring proof: it
// proves the host-scheduled retention sweep really hard-deletes an expired
// soft-deleted note's row end to end -- the trigger-half proof
// go/compliance/AGENTS.md's "no schedule point" Known-limitations entry
// records the absence of -- over two real boots of the composed server
// sharing one SQLite file, the same two-boot shape the storage expiry-sweep
// leg above uses and for the same reason: StandaloneQueue's permanent
// idempotency gives each database file exactly one retention sweep per
// tenant, the first-ever one, so boot 1 must leave the key unresolved.
//
// Boot 1 runs with cfg.DisableQueueWorker set -- the same gate that guards
// the worker and the scheduler (periodic_scheduler.go's doc comment) -- so
// no retention-sweep key is ever resolved for tenant-acme in this database
// file while boot 1 is up. Boot 1 hosts a live note and one whose
// deleted_at is backdated 45 days (past the 30-day default retention
// window every sweep uses when no config service is wired --
// go/compliance/retention.go's defaultRetentionWindow; notes exposes no
// delete endpoint and no product API ages deleted_at, so the soft delete
// and backdate go through the second-connection reach compliance_flow_test.go's
// softDeleteAndBackdate uses), and asserts both rows are physically present
// as it closes: age alone removes nothing.
//
// Boot 2 starts the same server over the same file with the normal gate
// and the injected periodicFlowTickInterval cadence. Its first tick
// enqueues tenant-acme's first-ever retention sweep -- alongside the
// expiry sweep, since both wired mechanisms share the one tick, per
// periodic_scheduler.go; nothing in boot 2 needs a signed-in user, the
// sweep is tenant-scoped from the host's tenant map. The worker drains the
// task into compliance's real retentionSweepHandler, whose SweepTenant
// runs the notes participant's real HardDelete. The test waits -- bounded,
// with no wall-clock race: nothing in the wait depends on a tick, only on
// the terminal state the sweep must produce -- until the expired note's
// physical row is gone while the live note's row stays, both observed
// through the same second connection boot 1 already opened. If retention
// scheduling were still unwired from the host scheduler -- the state this
// test exists to catch -- boot 2's ticks would enqueue nothing for
// compliance and the wait would fail.
func TestBuildServer_PeriodicScheduler_RetentionSweep_ReapsExpiredSoftDeletedNote(t *testing.T) {
	cfg := periodicSweepTestConfig(t)

	// ------------------------------------------------------------------
	// Boot 1: host the note to expire and the live survivor, with neither
	// the queue worker nor the scheduler running (the same DisableQueueWorker
	// gate). Because no scheduler runs, the retention sweep's per-tenant
	// idempotency key stays unresolved in this database file -- the
	// precondition boot 2's first-ever sweep depends on.
	// ------------------------------------------------------------------
	boot1Cfg := cfg
	boot1Cfg.DisableQueueWorker = true
	boot1 := buildPeriodicFlowServer(t, boot1Cfg)
	boot1Token := registerAndAuthenticate(t, boot1.srv, cfg, "tenant-acme", "periodic-retention-boot-1")

	// The note to expire: created through the real notes HTTP surface, then
	// soft-deleted with deleted_at backdated 45 days -- past the 30-day
	// default retention window, so any retention sweep that ever runs will
	// reap it.
	expiredID := createNoteAs(t, boot1.srv, boot1Token, "note past its retention window")
	liveID := createNoteAs(t, boot1.srv, boot1Token, "note still inside its retention window")

	db := openSecondDB(t, cfg)
	softDeleteAndBackdate(t, db, "tenant-acme", expiredID, time.Now().Add(-45*24*time.Hour))

	// The state handed to boot 2: both rows are physically present. Age
	// alone removes nothing; removal happens only when the wired sweep runs.
	if got := physicalNoteCount(t, db, "tenant-acme", expiredID); got != 1 {
		t.Fatalf("pre-boot-2 expired note physical rows = %d, want 1", got)
	}
	if got := physicalNoteCount(t, db, "tenant-acme", liveID); got != 1 {
		t.Fatalf("pre-boot-2 live note physical rows = %d, want 1", got)
	}

	if closeErr := boot1.close(); closeErr != nil {
		t.Fatalf("closing boot 1: %v", closeErr)
	}

	// ------------------------------------------------------------------
	// Boot 2: the restart. Same database file, normal gate, the injected
	// cadence. The scheduler's very first tick enqueues tenant-acme's
	// first-ever retention sweep, the worker drains it into the wired
	// retentionSweepHandler, and SweepTenant reaps the note boot 1 left
	// expired.
	// ------------------------------------------------------------------
	boot2Cfg := cfg
	boot2Cfg.PeriodicTaskInterval = periodicFlowTickInterval
	buildPeriodicFlowServer(t, boot2Cfg)

	// Wait for the sweep's terminal state: the expired note's physical row
	// gone, the live note's row untouched. The state is monotone -- a row
	// HardDelete removed stays removed, and the live note is never a sweep
	// target -- so the wait cannot exit on a transient mid-protocol
	// snapshot. Observations are paced ~5x per second rather than hammered:
	// each count takes a read lock on the SQLite file, and a near-continuous
	// read stream under -race slowed the expiry sweep's own writes into
	// SQLITE_BUSY failures, the lesson this file's cadence comment records.
	// The ceiling turns a sweep that never runs -- the failure mode this
	// test exists to catch -- into a named failure: a host that never
	// enqueues the retention sweep can never converge, however long the
	// wait, so a generous ceiling costs the test none of its discriminating
	// power; it only bounds how long a healthy-but-contended sweep may take
	// to land.
	convergenceDeadline := time.Now().Add(90 * time.Second)
	for {
		expiredRows := physicalNoteCount(t, db, "tenant-acme", expiredID)
		liveRows := physicalNoteCount(t, db, "tenant-acme", liveID)
		if expiredRows == 0 && liveRows == 1 {
			break
		}
		if time.Now().After(convergenceDeadline) {
			t.Fatalf("the first-ever retention sweep never reaped the expired note within 90s (expired rows = %d, live rows = %d)",
				expiredRows, liveRows)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The strict post-conditions, asserted once more now that the state is
	// terminal, so each property's failure names itself in the test output.
	if got := physicalNoteCount(t, db, "tenant-acme", expiredID); got != 0 {
		t.Fatalf("the sweep-expired note still has %d physical row(s), want 0", got)
	}
	if got := physicalNoteCount(t, db, "tenant-acme", liveID); got != 1 {
		t.Fatalf("the live note physical rows = %d, want 1 (the sweep must not touch a fresh note)", got)
	}
}
