//go:build integration

// Package referenceapp_test holds the reference app's integration tier, in
// the same physically separate, "integration"-tagged shape go/dbkit,
// go/jobs and go/pkgcore use (package name mirroring theirs: the module's
// own root has no base package, so the external test package stands alone
// exactly as dbkit_test does in go/dbkit/integration_test): a plain
// "go test ./..." never compiles or runs anything in this directory; it is
// invoked explicitly with "go test -tags=integration ./...", and it
// requires a working Docker daemon -- there is no skip-on-missing-Docker
// fallback, matching the other modules' integration tiers.
package referenceapp_test

// TestServer_RealRedisEventBusComposition_NotesAuditEventCrossesProcesses
// is this round's proof that the SPEED_REDIS_ADDR composition really works
// end to end: a standalone-deployment-mode reference app whose events
// cross a real Redis server.
//
// The app is booted as a REAL SUBPROCESS, not imported: the module's
// wiring lives in cmd/server's package main, which Go refuses to import
// ("is a program, not an importable package"), and the integration
// directory must stay physically separate from the unit suite -- so the
// test builds the actual binary with "go build ./cmd/server" and runs it
// on a real TCP port, exercising main -> configFromEnv -> buildServer in
// the child exactly as a production launch would, healthz-polls it, POSTs
// a note through its real HTTP stack, and stops it with SIGTERM, asserting
// the graceful-shutdown exit code.
//
// Authentication is as real as a subprocess can make it. The note request
// carries an access token minted below through authn's public Signer API,
// signed with the same committed dev key seed cmd/server derives its
// signing key from (server.go's devSigningKeySeed), so the child's real
// Verifier accepts it exactly as it accepts one a sign-in issued -- expiry,
// issuer and Ed25519 signature all checked, none of it faked. A
// register-then-login round trip is deliberately not attempted, because it
// cannot succeed against the production wiring this test boots: the app's
// demoMemberships store starts empty (its own doc comment in server.go
// records that seed accounts wait for authn+org+billing together), so a
// freshly registered account has no tenant membership and login/password
// fails closed. The tenant the request acts inside comes from the token's
// claim, never from a Host header; rbac's demo gate still reads the acting
// user from the X-Demo-User header, exactly as the unit suite's
// createNoteAs does (demo_subject.go's demoSubjectResolver).
//
// The observer half lives in THIS process: a second RedisEventBus
// instance, subscribed to the audit.event.recorded stream through a
// consumer group warmed up (with marker events) before the app boots, so
// it genuinely consumes the events the app's own bus appends to the same
// Redis streams. Cross-process delivery is the whole point: the observer's
// Event.Payload arrives JSON-reconstructed, and the test asserts on that
// reconstructed shape (capitalized map keys, envelope TenantID), which an
// in-process test could never exercise. A third bus instance publishes
// the warm-up markers, mirroring pkgcore's own integration-tier pattern
// (go/pkgcore/integration_test/redis_eventbus_test.go, whose helpers this
// file copies line for line by the same cross-module convention that
// pkgcore's container lifecycle follows go/jobs's).
//
// The notes-module audit event is the payload of choice because the app
// proves the audit trail twice over: once here, as the event observed
// crossing Redis, and once in the app's own SQLite file, read back through
// a second dbkit connection the way the unit suite's
// TestBuildServer_NoteCreate_PersistsAuditEvent does -- the app's own
// audit persister runs synchronously on the publishing side, so the row
// exists the moment the POST answers 201.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so this file's own dbkit.Open call (the second connection reading the
	// audit row back) has a driver to build from -- this test binary is a
	// separate package from cmd/server, so server.go's own blank import
	// does not reach it.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/pkgcore"
	eventbusredis "github.com/vislake/speed/go/pkgcore/eventbus/redis"
)

// startRedisClient starts a disposable Redis 7 container and returns a
// go-redis client connected to it, both already cleaned up via t.Cleanup
// on test completion (pass or fail). A copy of
// go/pkgcore/integration_test/redis_container_test.go's helper of the same
// name, which this test could not import.
func startRedisClient(t *testing.T, ctx context.Context) *redis.Client {
	t.Helper()

	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("start redis testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate redis testcontainer: %v", terminateErr)
		}
	})

	uri, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis testcontainer connection string: %v", err)
	}
	options, err := redis.ParseURL(uri)
	if err != nil {
		t.Fatalf("redis.ParseURL(%q): %v", uri, err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { client.Close() })
	return client
}

// eventRecorder accumulates what one bus instance's handlers saw, for
// later count and content assertions. Handlers run on different goroutines
// (the local publish path and the reader goroutine both invoke them), so
// every access goes through the mutex. Copied from pkgcore's integration
// tier, where it carries the same shape.
type eventRecorder struct {
	mu   sync.Mutex
	evts []pkgcore.Event
}

func (r *eventRecorder) handler() func(context.Context, pkgcore.Event) error {
	return func(_ context.Context, evt pkgcore.Event) error {
		r.mu.Lock()
		r.evts = append(r.evts, evt)
		r.mu.Unlock()
		return nil
	}
}

func (r *eventRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.evts)
}

func (r *eventRecorder) at(i int) pkgcore.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.evts[i]
}

func (r *eventRecorder) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evts = nil
}

// eventually polls cond until it holds or the deadline passes, failing the
// test in the latter case. Cross-process delivery is asynchronous: the
// reader goroutine of a RedisEventBus wakes up at most every 500ms to take
// new entries off the stream, so remote delivery of an already-committed
// event lands well inside this five-second window. Copied from pkgcore's
// integration tier.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// warmUp proves that receiver's consumer group on the eventType stream
// exists and its reader goroutine is actually consuming, by publishing
// marker events from publisher until one of them reaches receiver.
// Subscribe starts the reader asynchronously -- the group is created on a
// background goroutine and starts at the live end of the stream -- so a
// test that publishes a real event immediately after Subscribe could race
// the group's creation and lose it. Once receiver reports a marker, the
// wait for one full read block (600ms) lets any other marker that was
// already appended drain out, so clearing the recorder afterwards leaves
// counts that only the test's own events move. Copied from
// go/pkgcore/eventbus/redis's own integration tier.
func warmUp(t *testing.T, publisher *eventbusredis.EventBus, receiver *eventRecorder, eventType string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for seq := 1; ; seq++ {
		if err := publisher.Publish(context.Background(), pkgcore.Event{
			Type:     eventType,
			TenantID: pkgcore.TenantID("warmup"),
			Payload:  map[string]any{"seq": seq},
		}); err != nil {
			t.Fatalf("warm-up publish %d: %v", seq, err)
		}
		for waited := 0; receiver.count() == 0 && waited < 500; waited += 25 {
			time.Sleep(25 * time.Millisecond)
		}
		if receiver.count() > 0 {
			time.Sleep(600 * time.Millisecond) // one full read block: drain stragglers
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("warm-up: no marker event reached the receiver within 5s")
		}
	}
}

// moduleRoot resolves the module root (the directory holding go.mod) from
// this file's own compile-time path: the integration_test directory sits
// directly under it, and the child "go build" needs the module as its
// working directory so the module's go.mod -- with its replace directives
// for the go/ modules -- governs the build.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to locate this test file")
	}
	return filepath.Dir(filepath.Dir(file))
}

// testNote and testListNotesResponse mirror the response shapes of notes'
// HTTP API. They are declared here rather than shared because the module's
// API structs live in internal/notes, which only cmd/server's package main
// may import; server_test.go's copies are just as local.
type testNote struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type testListNotesResponse struct {
	Notes []testNote `json:"notes"`
}

// The reference app's demo constants, hardcoded here with their meaning
// spelled out (cmd/server owns the real declarations; these must match
// them).
//
// acmeTenantID is the tenant the note is created in and read back under.
// The token's tenant claim -- not a Host header -- is what selects it
// (authn.NewPrincipalResolver), mirroring the unit suite, where the bearer
// token is "the ONLY thing that selects the tenant" (server_test.go's
// createNoteAs doc).
//
// demoUserHdr/demoOwner name WHO is acting for rbac's demo gate:
// demoSubjectResolver (cmd/server/demo_subject.go) reads the acting user
// from the header -- a placeholder for the access token's subject claim
// that the demo wiring keeps while authn supplies the tenant half -- so
// these requests carry both the token and the header, exactly like the
// unit suite's createNoteAs does.
//
// devSigningKeySeed and devSigningKeyID mirror cmd/server/server.go's
// devSigningKeySeed and the kid its devSigningKeySet() signs under. They
// are that file's committed DEV key material -- a recognizable constant
// for zero-setup standalone development, never a secret (server.go's own
// doc comment on the seed) -- reused here so the child's real Verifier
// accepts the token demoAccessToken mints. The two copies MUST stay in
// step: a mismatch makes the child answer 401 to the first notes request
// and fails this test loudly, which is the intended coupling.
const (
	acmeTenantID = "tenant-acme"
	demoUserHdr  = "X-Demo-User"
	demoOwner    = "demo-owner"
	// devSigningKeyID is the kid of the reference app's committed dev
	// signing key (cmd/server's devSigningKeySet).
	devSigningKeyID = "reference-app-dev"
)

// devSigningKeySeed is cmd/server/server.go's devSigningKeySeed, copied
// byte for byte -- the 0x20..0x3f ascending sequence, deliberately the
// same shape of recognizable non-secret as devConfigKey's 0x00..0x1f.
var devSigningKeySeed = []byte{
	0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27,
	0x28, 0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f,
	0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37,
	0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f,
}

// demoSessionID is the session the demo access token claims, named after
// the demo owner like every demo identity in this app. The session row
// itself does not exist and does not need to: authn.Middleware only
// consults the session store when a host wires a RevocationChecker, and
// cmd/server's chain deliberately does not (revocation takes effect on the
// refresh path, the natural-expiry mode -- go/authn/middleware.go's
// WithRevocationChecker doc). If a future wiring adds the checker, this
// test fails loudly on the first request and must mint a real session
// instead.
const demoSessionID = "demo-owner-session"

// apiClient is the test's own HTTP client for talking to the subprocess's
// real TCP listener. A fresh client per test, with a bounded timeout so a
// wedged child fails the test instead of hanging it.
func apiClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}

// demoAccessToken mints a real access token for the demo owner acting in
// tenant, through authn's public Signer API with the key material the
// child derives from the same dev seed (see devSigningKeySeed above). The
// derivation mirrors cmd/server's devSigningKeySet() line for line --
// ed25519.NewKeyFromSeed, then a TokenKey under devSigningKeyID -- so the
// child's KeySet holds the verifying half of exactly the key that signed
// here.
//
// This is not a fake credential: the child verifies the token's Ed25519
// signature, issuer and expiry through the same Verifier that checks every
// Authorization header it receives. What is skipped is the sign-in round
// trip that would normally precede minting one, and why it is skipped is
// the test doc's authentication paragraph -- the production wiring this
// test boots contains no account that could complete it.
func demoAccessToken(t *testing.T, tenant pkgcore.TenantID) string {
	t.Helper()

	priv := ed25519.NewKeyFromSeed(devSigningKeySeed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("derive the demo signing key's public half")
	}
	keys, err := authn.NewKeySet(authn.TokenKey{ID: devSigningKeyID, Private: priv, Public: pub})
	if err != nil {
		t.Fatalf("build the demo key set: %v", err)
	}
	signer, err := authn.NewSigner(keys)
	if err != nil {
		t.Fatalf("build the demo token signer: %v", err)
	}
	token, _, err := signer.Issue(authn.Principal{
		UserID:    demoOwner,
		SessionID: demoSessionID,
		TenantID:  tenant,
	})
	if err != nil {
		t.Fatalf("issue the demo access token: %v", err)
	}
	return token
}

func TestServer_RealRedisEventBusComposition_NotesAuditEventCrossesProcesses(t *testing.T) {
	ctx := context.Background()

	// One disposable real Redis server; three bus instances share it --
	// the observer and warmer here, and the app's own bus in the child
	// process, which connects to the same host:port through
	// SPEED_REDIS_ADDR.
	client := startRedisClient(t, ctx)
	redisAddr := client.Options().Addr // "host:port", reachable from this process

	// The observer: subscribed to the audit stream and provably consuming
	// BEFORE the app boots. The warmer publishes its markers; the app's
	// own consumer group is created later, at the stream's live end, so
	// the markers are history for it and never reach its audit persister
	// (pkgcore pins that no-catch-up contract in its own tier).
	observer := eventbusredis.NewEventBus(client)
	t.Cleanup(observer.Close)
	warmer := eventbusredis.NewEventBus(client)
	t.Cleanup(warmer.Close)
	recorder := &eventRecorder{}
	observer.Subscribe(audit.EventRecorded, recorder.handler())
	warmUp(t, warmer, recorder, audit.EventRecorded)
	recorder.clear()

	// Build the real binary and run it as a subprocess.
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "reference-app-server")
	buildOut, buildErr := new(bytes.Buffer), new(bytes.Buffer)
	build := exec.Command("go", "build", "-o", bin, "./cmd/server")
	build.Dir = moduleRoot(t)
	build.Stdout, build.Stderr = buildOut, buildErr
	if err := build.Run(); err != nil {
		t.Fatalf("go build ./cmd/server: %v\nstdout: %s\nstderr: %s", err, buildOut.String(), buildErr.String())
	}

	// A real TCP port for the child, taken and released so the listener
	// does not hold it while the child boots.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if closeErr := listener.Close(); closeErr != nil {
		t.Fatalf("release the probe listener: %v", closeErr)
	}

	// The child's environment: the ambient environment, scrubbed of every
	// SPEED_* variable and PORT (whose ambient values must not leak into
	// the subprocess), then the explicit configuration for this run.
	// SPEED_CONFIG_KEY="" selects the documented dev default key, and
	// SPEED_DB_PATH points at a fresh file in this test's own temp
	// directory, so the test can open a second connection to the same
	// SQLite file afterwards.
	var baseEnv []string
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		if strings.HasPrefix(kv[:eq], "SPEED_") || kv[:eq] == "PORT" {
			continue
		}
		baseEnv = append(baseEnv, kv)
	}
	dbPath := filepath.Join(tmp, "reference-app.db")
	env := append(baseEnv,
		"SPEED_DEPLOYMENT_MODE=standalone",
		"PORT="+strconv.Itoa(port),
		"SPEED_DB_PATH="+dbPath,
		"SPEED_CONFIG_KEY=",
		"SPEED_REDIS_ADDR="+redisAddr,
	)

	cmd := exec.Command(bin)
	cmd.Env = env
	var childOut, childErr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &childOut, &childErr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the reference-app subprocess: %v", err)
	}
	childExited := make(chan struct{})
	go func() {
		_ = cmd.Wait() // reaps the child; ProcessState is read after childExited closes
		close(childExited)
	}()
	childLogs := func() string {
		return "child stdout:\n" + childOut.String() + "\nchild stderr:\n" + childErr.String()
	}
	// A failed or interrupted test must never leave the child running: on
	// the graceful path the test body stops it first and this no-ops.
	t.Cleanup(func() {
		select {
		case <-childExited:
		default:
			_ = cmd.Process.Kill()
			<-childExited
		}
	})

	// Wait for the child to finish booting: buildServer (SQLite migrations,
	// module registration, kernel bootstrap with the injected Redis bus)
	// all happen before anything listens, so a 200 from /healthz means the
	// composition is genuinely up -- with the app's audit persister
	// subscribed to the SAME Redis streams the observer reads.
	httpClient := apiClient()
	healthzURL := "http://127.0.0.1:" + strconv.Itoa(port) + "/healthz"
	bootDeadline := time.Now().Add(30 * time.Second)
	healthy := false
	for time.Now().Before(bootDeadline) {
		resp, getErr := httpClient.Get(healthzURL)
		if getErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		select {
		case <-childExited:
			t.Fatalf("child exited during boot: %v\n%s", cmd.ProcessState, childLogs())
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !healthy {
		t.Fatalf("child never answered GET %s within 30s\n%s", healthzURL, childLogs())
	}

	// Drain-then-clear: any audit event the child's boot may have appended
	// to the stream (this app emits none -- audit.Emit runs only inside the
	// note-creation handler -- but the 600ms one-read-block wait keeps the
	// exactly-one assertion below immune to a straggler either way).
	time.Sleep(600 * time.Millisecond)
	recorder.clear()

	// Create one note through the child's real HTTP stack, authenticated
	// the way every protected request to this app must be: a dev-seeded
	// access token (demoAccessToken) whose tenant claim tenancy.Middleware
	// trusts, plus the X-Demo-User header rbac's demo gate still reads for
	// the acting user.
	body, err := json.Marshal(map[string]string{"text": "buy milk through a real redis bus"})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	notesURL := "http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/notes"
	accessToken := demoAccessToken(t, acmeTenantID)
	req, err := http.NewRequest(http.MethodPost, notesURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build POST request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set(demoUserHdr, demoOwner)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", notesURL, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/notes status = %d, want %d\n%s",
			resp.StatusCode, http.StatusCreated, childLogs())
	}

	listReq, err := http.NewRequest(http.MethodGet, notesURL, nil)
	if err != nil {
		t.Fatalf("build GET request: %v", err)
	}
	listReq.Header.Set("Authorization", "Bearer "+accessToken)
	listReq.Header.Set(demoUserHdr, demoOwner)
	listResp, err := httpClient.Do(listReq)
	if err != nil {
		t.Fatalf("GET %s: %v", notesURL, err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/notes status = %d, want %d\n%s",
			listResp.StatusCode, http.StatusOK, childLogs())
	}
	var listed testListNotesResponse
	if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listed.Notes) != 1 {
		t.Fatalf("notes after create = %+v, want exactly 1", listed.Notes)
	}
	noteID := listed.Notes[0].ID

	// The event must cross the real Redis server into THIS process -- the
	// observer's reader picks entries up within one 500ms read block, so
	// the five-second eventually window is ample.
	eventually(t, "the audit.event.recorded event to reach the observer over Redis", func() bool {
		return recorder.count() == 1
	})
	if n := recorder.count(); n != 1 {
		t.Fatalf("observer recorded %d audit events, want exactly 1", n)
	}
	evt := recorder.at(0)
	if evt.Type != audit.EventRecorded {
		t.Fatalf("observer event Type = %q, want %q", evt.Type, audit.EventRecorded)
	}
	if evt.TenantID != acmeTenantID {
		t.Fatalf("observer event TenantID = %q, want %q (envelope survived the wire)", evt.TenantID, acmeTenantID)
	}
	payload, ok := evt.Payload.(map[string]any)
	if !ok {
		t.Fatalf("observer event Payload = %T, want map[string]any (the JSON-reconstructed shape)", evt.Payload)
	}
	if action, _ := payload["Action"].(string); action != "notes.note.create" {
		t.Fatalf("observer payload Action = %v, want %q", payload["Action"], "notes.note.create")
	}
	resource, ok := payload["Resource"].(map[string]any)
	if !ok {
		t.Fatalf("observer payload Resource = %T, want the decoded map with capitalized keys", payload["Resource"])
	}
	if resType, _ := resource["Type"].(string); resType != "note" {
		t.Fatalf("observer payload Resource.Type = %v, want %q", resource["Type"], "note")
	}
	if resID, _ := resource["ID"].(string); resID != noteID {
		t.Fatalf("observer payload Resource.ID = %v, want the created note's id %q", resource["ID"], noteID)
	}
	if result, ok := payload["Result"].(map[string]any); ok {
		if success, _ := result["Success"].(bool); !success {
			t.Fatalf("observer payload Result.Success = %v, want true", result["Success"])
		}
	} else {
		t.Fatalf("observer payload Result = %T, want the decoded map with capitalized keys", payload["Result"])
	}

	// The app's own side of the audit trail: the row its persister wrote
	// into SQLite, read back through a second connection to the same file
	// exactly as the unit suite's audit test does -- the persister ran
	// synchronously on the publishing side inside the POST, so the row
	// exists now.
	auditDB, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dbPath})
	if err != nil {
		t.Fatalf("open second connection to %q: %v", dbPath, err)
	}
	t.Cleanup(func() {
		sqlDB, dbErr := auditDB.DB()
		if dbErr != nil {
			t.Errorf("second connection handle: %v", dbErr)
			return
		}
		if closeErr := sqlDB.Close(); closeErr != nil {
			t.Errorf("close second connection: %v", closeErr)
		}
	})

	events, err := audit.NewRepository(auditDB).ListByTenant(context.Background(), acmeTenantID)
	if err != nil {
		t.Fatalf("ListByTenant(%q): %v", acmeTenantID, err)
	}
	if len(events) != 1 {
		t.Fatalf("audit events for tenant %q = %+v, want exactly 1", acmeTenantID, events)
	}
	got := events[0]
	if got.Action != "notes.note.create" {
		t.Fatalf("AuditEvent.Action = %q, want %q", got.Action, "notes.note.create")
	}
	if got.Resource().Type != "note" || got.Resource().ID != noteID {
		t.Fatalf("AuditEvent.Resource() = %+v, want {note %q}", got.Resource(), noteID)
	}
	if !got.Result().Success {
		t.Fatalf("AuditEvent.Result().Success = %v, want true", got.Result().Success)
	}
	// Negative control, as in the unit suite: an unrelated tenant's read
	// must see none of acme's audit trail.
	globexEvents, err := audit.NewRepository(auditDB).ListByTenant(context.Background(), "tenant-globex")
	if err != nil {
		t.Fatalf("ListByTenant(%q): %v", "tenant-globex", err)
	}
	if len(globexEvents) != 0 {
		t.Fatalf("audit events for tenant %q = %+v, want none", "tenant-globex", globexEvents)
	}

	// Stop the child the way an operator would: SIGTERM, then the graceful
	// drain run() performs (bounded by its 10s shutdown timeout). Exit
	// code 0 and the "stopped cleanly" log prove the whole lifecycle --
	// including the cleanup that closes the injected Redis bus and its
	// client -- completed without error.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM the child: %v", err)
	}
	select {
	case <-childExited:
		if code := cmd.ProcessState.ExitCode(); code != 0 {
			t.Fatalf("child exited with code %d after SIGTERM, want 0\n%s", code, childLogs())
		}
		if !strings.Contains(childOut.String(), "server stopped cleanly") {
			t.Fatalf("child log does not record the clean stop\n%s", childLogs())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("child did not exit within 15s of SIGTERM\n%s", childLogs())
	}
}
