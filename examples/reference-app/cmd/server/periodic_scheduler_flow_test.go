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
//     passes plus a survivor whose own deadline -- settled at create,
//     since an upload declaring no retention now expires at the module's
//     default lifetime ceiling -- is still months away, and closes with
//     both still served and listed -- expiry alone removes nothing.
//     Boot 2 runs with
//     the normal gate and the flow tests' injected cadence
//     (periodicFlowTickInterval); its very first
//     tick enqueues tenant-acme's first expiry sweep in a fresh
//     expirySweepWindowSize window (boot 1 never resolved any sweep key,
//     because its scheduler never ran -- and the windowed key means boot
//     2's later same-window ticks collapse into that first job rather
//     than piling up), the worker drains the task into
//     storage.LifecycleService's real sweep, and the sweep runs the real
//     delete protocol. The expired object's row is gone -- observed
//     through a second-connection storage.ObjectRepository, the same
//     "buildServer hands out neither its *gorm.DB nor a module service"
//     second connection server_test.go's audit test opens -- its bytes are
//     gone on the filesystem under cfg.ObjectStoreRoot, its metadata and
//     content answer 404, and the list holds only the survivor, whose own
//     row, bytes, reads and deadline are untouched.
//
//   - The compliance retention-sweep leg is the trigger-half twin of the
//     storage leg above: TestBuildServer_PeriodicScheduler_RetentionSweep_
//     ReapsExpiredSoftDeletedNote proves the host-scheduled retention sweep
//     really hard-deletes an expired soft-deleted note's row end to end --
//     the half that had no schedule point until the host wiring this test
//     proves.
//     Same two-boot shape, same reason: the sweep key must stay
//     unresolved for boot 1 so boot 2's first tick demonstrably schedules
//     the sweep. Boot 1 runs with the worker and scheduler disabled,
//     hosts a live note and one soft-deleted with deleted_at backdated
//     45 days (past the sweep's 30-day default window -- no product API
//     ages deleted_at; the backdate goes through the same second-connection
//     reach compliance_flow_test.go's softDeleteAndBackdate uses), and
//     closes with both rows physically present -- age alone removes
//     nothing. Boot 2 runs the normal gate with the injected cadence; its
//     very first tick enqueues tenant-acme's first retention sweep in a
//     fresh retentionSweepWindowSize window (alongside the expiry sweep --
//     both wired mechanisms share the one tick, per periodic_scheduler.go),
//     the worker drains the task into
//     compliance's real retentionSweepHandler, and SweepTenant runs the
//     notes participant's real HardDelete: the expired note's physical row
//     is gone -- observed through the same second connection -- while the
//     live note's row is untouched. The test FAILS when retention
//     scheduling is unwired: no tick enqueues compliance's task and
//     every soft-deleted row past its window is kept indefinitely.
//
//   - The self-registered-clinic leg pins the scheduler's tenant
//     universe reaching beyond cfg.HostTenants:
//     TestBuildServer_PeriodicScheduler_ExpiryAndRetentionSweeps_
//     ReachSelfRegisteredClinicTenant proves BOTH wired mechanisms reach
//     the rows of a tenant no configuration ever names -- a clinic the
//     real self-service flow provisioned at runtime, whose tenant id is
//     derived from its registrant's user id (self_service.go's
//     clinicTenantOf) and can therefore never appear in cfg.HostTenants
//     (self_service.go's own comment: a clinic id "can never collide with
//     a configured host tenant's id"). Same two-boot shape as the two
//     legs above: boot 1 registers the clinic owner through the real
//     register route -- whose synchronous provisioning creates the
//     clinic's org root, the org.node.created event that lands the
//     clinic in go/admin's tenant ledger, the discovery half of the
//     scheduler's universe -- hosts a completed object whose retention
//     deadline passes and a soft-deleted note backdated past the
//     retention window, all under the clinic tenant, and closes with
//     every row and byte present; boot 2's first tick must enqueue the
//     clinic's own expiry and retention sweeps -- the ledger names the
//     clinic, so the tenant-universe fix covers it -- and the wait
//     converges on the expired object's row and bytes gone and the
//     expired note's physical row gone while the live note survives. The
//     test FAILS when the scheduler's universe is only cfg.HostTenants
//     where every self-registered
//     clinic's expired objects and soft-deleted notes age past their
//     deadlines with no sweep ever reaching them.
//
// What remains honestly limited:
//
//   - The sweep keys are window-scoped (go/storage/cleanup.go's
//     expirySweepIdempotencyKey and go/compliance/retention.go's
//     retentionSweepIdempotencyKey each name their enqueue's
//     window-start), so on this app's StandaloneQueue -- which holds a
//     resolved key forever (go/jobs) -- each tenant is swept at most once
//     per window, not once per tick. That is why the two-boot shape above
//     must exist at all (boot
//     2's first tick must find no resolved key for its window), and why
//     a host that needs tighter reaping bounds must enqueue more often
//     than the module's window constant or shrink that constant. The
//     pre-window design -- one sweep per database file ever, with a
//     dead-lettered sweep poisoning its tenant forever -- and the dated
//     records of it are closed: the windowed keying and its proofs live
//     in the modules (go/storage's sweep_window_test.go,
//     go/compliance's retention_sweep_window_test.go, each against a real
//     StandaloneQueue).
//
//   - pki's signing-key expiry scan is window-scoped like the sweeps --
//     go/pki/job.go's expiryScanIdempotencyKey names its enqueue's
//     window-start, so on this app's StandaloneQueue the scan runs at
//     most once per DefaultExpiryScanWindow hour, and the rotation flow
//     test compresses that window below its own tick cadence through
//     cfg.PKIExpiryScanWindow so each test tick runs a real scan. The
//     resulting key rotation is proven end to end in
//     periodic_pki_scan_flow_test.go, and the window semantics themselves
//     in go/pki's enqueue_window_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/admin"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/storage"

	"gorm.io/gorm"
)

// periodicFlowTickInterval is the cadence the scheduler flow tests inject
// through cfg.PeriodicTaskInterval: a real tick every second, far faster
// than the production default (defaultPeriodicTaskSchedulerInterval) so a
// mechanism's scheduled work lands within test time, and far slower than a
// spin so a pass never depends on tick-counting races.
//
// One second is a deliberate floor, not an arbitrary "fast": every tick
// writes to the app's SQLite file (the sweep's per-tenant enqueue merging
// into the resolved job, the pki scan's fresh enqueue -- fresh because
// the rotation test compresses the scan window below the tick cadence,
// cfg.PKIExpiryScanWindow), while the queue's
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

// tenantObjectPath returns the filesystem path under an injected
// APP_OBJECT_STORE_ROOT directory where the local object store keeps one
// object's original bytes. It mirrors the grammar go/storage's key.go
// derives and pkgcore's localObjectStore realizes on disk --
// root/<tenantID>/<objectID>/original -- so the byte assertions observe
// the store's real layout without importing the store's internals.
// originalObjectPath below is the tenant-acme spelling of this helper.
func tenantObjectPath(root string, tenant pkgcore.TenantID, objectID string) string {
	return filepath.Join(root, string(tenant), objectID, "original")
}

// originalObjectPath returns tenantObjectPath for tenant-acme, the
// tenant the configured-universe legs host their objects under.
func originalObjectPath(root string, objectID string) string {
	return tenantObjectPath(root, pkgcore.TenantID("tenant-acme"), objectID)
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
// first expiry sweep must have produced and reports whether every
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
// required (the sweep keys are window-scoped, so boot 2's first tick must
// find a fresh window whose key no enqueue ever resolved) and for what
// the windowed keying leaves limited.
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
// a survivor whose own deadline still sits at the module's default
// lifetime ceiling -- 90 days under this host's no-option wiring, far
// beyond the seconds the test spans -- and asserts both are still served
// and listed once the expiring deadline is behind them: expiry alone
// removes nothing.
//
// Boot 2 starts the same server over the same files with the normal gate
// and the injected periodicFlowTickInterval cadence. Its first tick
// enqueues tenant-acme's first expiry sweep of a fresh window; the
// worker drains it
// into the
// real LifecycleService sweep, which finds the expired completed object
// and runs the real delete protocol. The test waits -- bounded, with no
// wall-clock race: nothing in the wait depends on a tick, only on the
// terminal state the sweep must produce -- until the object's metadata and
// content answer 404, the list holds only the survivor, the object's row
// is absent from the database (observed through a second connection, the
// same reach server_test.go's audit test uses), and its bytes are absent
// under cfg.ObjectStoreRoot, then asserts the survivor's row, bytes,
// reads and deadline are all untouched. This leg proves the first sweep
// of a fresh window really runs through the real host wiring; the
// periodicity of
// later windows (and a dead-lettered window's non-poisoning) is the
// modules' own real-queue proof (go/storage's sweep_window_test.go,
// go/compliance's retention_sweep_window_test.go), not this e2e's.
func TestBuildServer_PeriodicScheduler_ExpirySweep_RemovesExpiredObject(t *testing.T) {
	cfg := periodicSweepTestConfig(t)
	jpegBytes := jpegWithExif(t)

	// ------------------------------------------------------------------
	// Boot 1: host the expiring object and the survivor, with neither the
	// queue worker nor the scheduler running (the same DisableQueueWorker
	// gate), and let the deadline pass. Because no scheduler runs, no
	// sweep key is ever resolved in this database file -- the windowed key
	// is resolved only by an enqueue -- the precondition boot 2's first
	// sweep depends on.
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

	// storageDefaultObjectLifetime mirrors go/storage/module.go's
	// defaultMaxObjectLifetime -- the default ceiling in force for a host
	// that wires no WithMaxObjectLifetime, which this one is (server.go's
	// storage.NewModule call). The mirror is load-bearing: the survivor
	// below lives because its default deadline sits months away, so a
	// material change to the module's default -- or the host's adoption of
	// its own shorter ceiling -- must revisit this two-boot shape, not
	// pass it silently.
	const storageDefaultObjectLifetime = 90 * 24 * time.Hour

	// The survivor -- an ordinary upload declaring no retention at all.
	// The default-life rule (go/storage/object.go's CreateParams.Retention
	// doc) settles every row's expiry at create, so a no-request upload
	// now carries a deadline at the module's configured maximum lifetime:
	// storageDefaultObjectLifetime here, far beyond the seconds the test
	// spans. That far deadline is exactly what makes this object a
	// survivor -- the sweep spares it because its time has not come, never
	// because it has no time. Each completion attempts to enqueue a
	// thumbnail-derive task; with the worker gated off, the queue's schema
	// was never created (go/jobs applies it inside Start), so those
	// enqueues fail and storage logs the failure as a warning, exactly as
	// its wiring contract allows -- completion itself is never held
	// hostage to the queue. That leaves boot 2 with no derivative backlog,
	// so the first tick's sweep is claimed without competing for
	// tenant-acme's worker slots.
	survivor := uploadAndComplete(t, boot1.srv, boot1Token, jpegBytes, "")
	survivorDeadline, err := time.Parse(time.RFC3339Nano, survivor.ExpiresAt)
	if err != nil {
		t.Fatalf("survivor carries no expiry deadline: expiresAt %q is not RFC 3339 (%v); an upload declaring no retention must default to the module's lifetime ceiling",
			survivor.ExpiresAt, err)
	}
	if survivorDeadline.Before(time.Now().Add(storageDefaultObjectLifetime-time.Minute)) ||
		survivorDeadline.After(time.Now().Add(storageDefaultObjectLifetime+time.Minute)) {
		t.Fatalf("survivor's default-life deadline = %v, want roughly now + the module's default maximum lifetime (%v); the sweep must spare it because its time has not come",
			survivorDeadline, storageDefaultObjectLifetime)
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
	// first tick enqueues tenant-acme's first expiry sweep of a fresh
	// window, the
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
			t.Fatalf("the first expiry sweep never reached its terminal state within 90s; last observation: %s", detail)
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
	if survivorAfter.ExpiresAt != survivor.ExpiresAt {
		t.Fatalf("the sweep changed the survivor's deadline: %q before, %q after (a spared object's row is untouched)",
			survivor.ExpiresAt, survivorAfter.ExpiresAt)
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
// the compliance module's own "no schedule point" limitation entry
// records the absence of -- over two real boots of the composed server
// sharing one SQLite file, the same two-boot shape the storage expiry-sweep
// leg above uses and for the same reason: the sweep keys are
// window-scoped, so boot 1 must leave every window's key unresolved for
// boot 2's first tick to schedule the sweep.
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
// enqueues tenant-acme's first retention sweep of a fresh window --
// alongside the
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
	// gate). Because no scheduler runs, no retention-sweep key is ever
	// resolved in this database file -- the windowed key is resolved only
	// by an enqueue -- the precondition boot 2's first sweep depends on.
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
	// first retention sweep of a fresh window, the worker drains it into
	// the wired
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
			t.Fatalf("the first retention sweep never reaped the expired note within 90s (expired rows = %d, live rows = %d)",
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

// periodicClinicEmail is the account the self-registered-clinic leg
// registers. The local part is unique to this leg so a fresh database
// never collides with the demo or suite accounts; the clinic tenant is
// derived from the user id authn assigns (self_service.go's
// clinicTenantOf), never chosen by the test.
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
// durable discovery record periodic_scheduler.go's universe reads. The
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

	if _, err := objects.FindByID(clinicCtx, expiringID); !errIsRecordNotFound(err) {
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
// (self_service.go's wireSelfService subscription) creates the account's
// own clinic tenant -- derived from the registrant's user id
// (clinicTenantOf), so by construction absent from cfg.HostTenants --
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

	if _, findErr := objects.FindByID(pkgcore.WithTenant(context.Background(), clinic), expiring.ID); !errIsRecordNotFound(findErr) {
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
