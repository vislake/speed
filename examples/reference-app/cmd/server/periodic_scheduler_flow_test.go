package main

// periodic_scheduler_flow_test.go is the flow-test companion of
// periodic_scheduler.go: it proves the host's periodic-task scheduler
// really drives a wired jobs mechanism through the composed stack. The
// scheduler shares one tick cadence with the queue worker (the same
// cfg.DisableQueueWorker gate starts and stops both, per
// periodic_scheduler.go's own doc comment), so a real server built with a
// sub-second cfg.PeriodicTaskInterval performs real enqueues on real ticks
// and the app's real standalone queue drains them into the wired modules'
// handlers -- nothing here is called in-process, enqueued by hand or
// otherwise driven around the scheduler.
//
// What this file proves, and what it honestly cannot:
//
//   - The storage expiry-sweep leg is a KNOWN-LIMITATION PIN, not a
//     removal e2e. The sweep's deterministic per-tenant idempotency key
//     (go/storage's cleanup.go, designed to collapse concurrent replica
//     enqueues into one in-flight sweep under the asynq queue's bounded
//     idempotency) collides with StandaloneQueue's PERMANENT idempotency
//     (go/jobs: a resolved key is held forever, succeeded rows are never
//     deleted). On this app's standalone queue the result is that exactly
//     one sweep per tenant ever executes -- the first tick's, always
//     before any object exists -- and every later tick's duplicate enqueue
//     merges into that completed job instead of scheduling another run. A
//     standalone host therefore cannot expire anything through this
//     mechanism. Fixing it requires changing go/jobs' or go/storage's
//     module semantics, which is outside this round's scope; the discovery
//     is dated and recorded in go/storage/AGENTS.md, and
//     TestBuildServer_PeriodicScheduler_ExpirySweep_StandaloneOneShot
//     below pins the standalone behaviour honestly -- the expired object
//     stays served and listed. The round that fixes the semantics will see
//     this test fail (the object will have vanished) and must flip it back
//     into the removal e2e it replaced.
//
//   - pki's signing-key expiry scan IS genuinely periodic on the
//     standalone queue -- EnqueueExpiryScan carries no idempotency key,
//     each tick is its own independent occurrence, and the scan's guarded
//     status-updated state machine makes overlapping scans safe. The
//     resulting key rotation is proven end to end in
//     periodic_pki_scan_flow_test.go.
//
// The second proof makes this file's wiring claim -- the scheduler really
// enqueues on real ticks and the standalone worker really drains into the
// modules' handlers -- complete even though the storage leg can only pin a
// limitation.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// periodicFlowTickInterval is the cadence the scheduler flow tests inject
// through cfg.PeriodicTaskInterval: a real tick every 150ms, far faster
// than the production default (defaultPeriodicTaskSchedulerInterval) so a
// mechanism's scheduled work lands within test time, and far slower than a
// spin so a pass never depends on tick-counting races.
const periodicFlowTickInterval = 150 * time.Millisecond

// buildPeriodicTestServer is buildTestServer with a tuning hook: it starts
// from the same testConfig(buildTestServer uses), lets the caller adjust
// the serverConfig (the scheduler flow tests inject a fast
// cfg.PeriodicTaskInterval here -- the test-override field buildServer
// reads, exactly like the Mailer and DisableQueueWorker fields other tests
// tune), and then builds the exact composed server buildTestServer builds,
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

// TestBuildServer_PeriodicScheduler_ExpirySweep_StandaloneOneShot pins the
// expiry sweep's behaviour on this app's standalone queue: exactly one
// sweep ever runs per tenant, on the scheduler's first tick, before any
// object exists -- so a completed object whose retention deadline passes
// many ticks later is still served and listed. That is a known limitation,
// not the intended behaviour: see the file header for the cross-module
// semantics collision behind it (StandaloneQueue's permanent idempotency
// against the sweep's deterministic per-tenant key) and why the fixing
// round must flip this test back into a removal e2e. Until then this test
// is the honest pin that keeps the limitation observable instead of letting
// a fake pass pretend the mechanism works.
//
// Nothing here is driven by hand: the server runs the real tick loop at
// periodicFlowTickInterval and the real standalone queue drains the real
// enqueues. The test only waits -- first for the one-and-only sweep to
// have run (before any object exists), then for the retention deadline to
// pass with further ticks behind it -- and reads the results through the
// wire. If the semantics are ever fixed, the expired object's GET answers
// 404 and this test fails, pointing at exactly what to flip.
func TestBuildServer_PeriodicScheduler_ExpirySweep_StandaloneOneShot(t *testing.T) {
	srv, cfg := buildPeriodicTestServer(t, func(cfg *serverConfig) {
		cfg.PeriodicTaskInterval = periodicFlowTickInterval
	})
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "periodic-sweep")

	jpegBytes := jpegWithExif(t)

	// The pin depends on the one-and-only sweep having run before any
	// object exists. The scheduler's first tick fires one interval after
	// buildServer started the loop, so let a couple of ticks pass first:
	// whatever that first sweep found (nothing -- this test's objects do
	// not exist yet), it is the only sweep that will ever run for this
	// tenant, because every later tick's enqueue of the same deterministic
	// key merges into its completed job.
	time.Sleep(2 * periodicFlowTickInterval)

	// Step 1: the object whose retention deadline passes while the
	// scheduler keeps ticking. RFC 3339 carries whole seconds, so the
	// deadline lands 4-5s after the create request -- the seconds in which
	// the completion below and the immediately-following reads happen.
	expiresAt := time.Now().Add(5 * time.Second).Format(time.RFC3339)
	declared := declareUploadWithExpiry(t, srv, acmeToken, demoOwnerUserID,
		int64(len(jpegBytes)), "image/jpeg", expiresAt)
	uploadBytes(t, srv, acmeToken, demoOwnerUserID, declared.ID, jpegBytes)
	expiring := completeObject(t, srv, acmeToken, demoOwnerUserID, declared.ID)
	if expiring.ExpiresAt == "" {
		t.Fatalf("completed object lost its retention deadline: %+v", expiring)
	}
	expiryTime, err := time.Parse(time.RFC3339, expiring.ExpiresAt)
	if err != nil {
		t.Fatalf("completed object's expiresAt %q is not RFC 3339: %v", expiring.ExpiresAt, err)
	}

	// Step 2: the live survivor -- no expiresAt on the wire at all.
	survivor := uploadAndComplete(t, srv, acmeToken, jpegBytes, "")
	if survivor.ExpiresAt != "" {
		t.Fatalf("survivor carries a retention deadline it was never declared with: %+v", survivor)
	}

	// Step 3: before the deadline passes, both objects are served and
	// listed -- the reads that would change if a sweep ever ran again.
	for _, obj := range []testStorageObject{expiring, survivor} {
		resp := storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects/"+obj.ID,
			acmeToken, demoOwnerUserID, "", nil)
		got := decodeStorageObject(t, resp, http.StatusOK, "GET before the deadline passes")
		if got.ID != obj.ID {
			t.Fatalf("GET %s answered object %s", obj.ID, got.ID)
		}
	}
	listResp := storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects?limit=10",
		acmeToken, demoOwnerUserID, "", nil)
	before := decodeList(t, listResp, "GET list before the deadline passes")
	if len(before.Objects) != 2 {
		t.Fatalf("pre-deadline list = %+v, want exactly the two completed objects", before.Objects)
	}

	// Step 4: wait out the retention deadline plus a clear margin of
	// further ticks -- several seconds in which a functioning periodic
	// sweep would have run again and again. Each of those ticks re-enqueues
	// the sweep's deterministic per-tenant key; on this queue each enqueue
	// merges into the first tick's completed job (see the file header).
	waitUntil := expiryTime.Add(time.Second + 4*periodicFlowTickInterval)
	for time.Now().Before(waitUntil) {
		time.Sleep(20 * time.Millisecond)
	}

	// Step 5: the pin. The expired object is still served, its content
	// still answers, and both objects still sit on the list. On a queue
	// whose idempotency is bounded (the distributed asynq queue's
	// retention windows release a resolved key), the tick after the
	// deadline would have scheduled a fresh sweep and this GET would have
	// answered 404 -- the removal e2e this test replaced. On
	// StandaloneQueue no sweep has run since the first tick, so both
	// objects survive. The test's final reads are 200s by design; the
	// failing round is the one that fixes the semantics, and this file's
	// header tells it exactly what to flip.
	for _, obj := range []testStorageObject{expiring, survivor} {
		resp := storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects/"+obj.ID,
			acmeToken, demoOwnerUserID, "", nil)
		got := decodeStorageObject(t, resp, http.StatusOK,
			"GET long after the retention deadline passed (one-shot pin)")
		if got.ID != obj.ID {
			t.Fatalf("GET %s answered object %s", obj.ID, got.ID)
		}
	}
	resp := storageRequest(t, srv, http.MethodGet,
		"/api/v1/storage/objects/"+expiring.ID+"/content", acmeToken, demoOwnerUserID, "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET content of the expired object = %d, want 200 (one-shot pin: no sweep ran since the first tick)",
			resp.StatusCode)
	}
	listResp = storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects?limit=10",
		acmeToken, demoOwnerUserID, "", nil)
	final := decodeList(t, listResp, "GET list long after the retention deadline passed")
	if len(final.Objects) != 2 {
		t.Fatalf("post-deadline list = %+v, want both objects still present (one-shot pin)", final.Objects)
	}
}
