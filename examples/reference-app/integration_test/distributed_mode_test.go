//go:build integration

// This file is the reference app's distributed-mode integration tier: the
// positive and negative halves of a real the assembly under the
// distributed deployment mode. It
// lives in package referenceapp_test alongside
// redis_eventbus_composition_test.go (same build tag, same
// "go test -tags=integration ./..." invocation, no skip-on-missing-Docker
// fallback) and reuses several of that file's helpers directly
// (eventually, moduleRoot, apiClient, demoAccessToken,
// testNote/testListNotesResponse, and the acmeTenantID/demoUserHdr/
// demoOwner/demoOwnerEmail/demoUsersPassword constants) rather than
// duplicating them -- the Redis client comes from go/pkgcore/redistest.
//
// TWO real server processes, not one: both built from the SAME "go build
// ./cmd/server" binary, both run with APP_DEPLOYMENT_MODE=distributed,
// both pointed at the SAME real Redis, the SAME real RustFS bucket, the SAME
// real SMTP catcher, and -- necessarily, see the deviation note below --
// the SAME SQLite file.
//
// # How the positive proof crosses a process boundary
//
// A note created on replica A must not be observable on replica B through
// the shared SQLite file alone. internal/app/server.go wires
// jobs.NewStandaloneQueue(db) UNCONDITIONALLY, regardless of deployment
// mode -- there is no Redis-backed jobs queue anywhere in this app -- and
// both replicas share one SQLite file (see the deviation note below for
// why). notification.DeliveryService.Dispatch only ENQUEUES a job; the
// actual in_app_messages row write happens later, inside WHICHEVER
// replica's StandaloneQueue worker polls and claims that row from the
// shared jobs table. A proof that created a note on A and read the inbox
// row through B's REST endpoint would therefore never require the
// EventBus or KVStore to cross a process boundary at all -- two fully
// isolated, unconnected per-replica Redis instances would not make such
// a test fail.
//
// This file closes that gap by making replica B's own worker
// STRUCTURALLY incapable of ever processing a job: replica B boots with
// APP_DISABLE_QUEUE_WORKER=true (internal/app/server.go's cfg.DisableQueueWorker),
// which skips standaloneQueue.Start entirely on that replica -- no
// dispatcher, no worker goroutines, ever, on B, no matter how long it
// runs. With B's worker disabled, replica A is the ONLY process that can
// ever claim and execute the delivery job, which means
// notification.announceInbox's EventInboxCreated publish always
// originates from A's own process. The module's Hub
// (reg.Events.Subscribe(EventInboxCreated, m.hub.HandleEvent), go/
// notification/module.go) is subscribed independently on EVERY replica,
// including B, regardless of whether that replica's queue worker runs --
// so this test opens replica B's own GET /api/v1/notifications/stream
// (the module's SSE inbox announcement, handler.go's handleStream) BEFORE
// creating the note, and asserts the announcement frame arrives there.
// Because B can never self-produce that announcement (it never executes
// the job that would publish it), the ONLY way the frame reaches B's own
// SSE connection is if the real, Redis-backed EventBus actually delivered
// EventInboxCreated across the process boundary from A to B -- a shared
// SQLite file explains none of this, since the stream frame is the bus
// payload itself (message_id/tenant_id/recipient_user_id/type_key), never
// a database read.
//
// The "kv" seam gets its own, independent proof: the file drives two
// wrong-password
// login attempts deliberately: one against replica A (which records a
// failure and opens authn's own 30-second progressive-lockout window,
// go/authn/ratelimit.go's RecordLoginFailure/loginLockoutBase -- state
// kept in the shared pkgcore.KVStore seam, never in the SQLite file both
// replicas share), then immediately a second against replica B for the
// SAME account. Replica B can only answer authn.account_locked if its own
// authn.loginLocked read the SAME lockout row replica A just wrote a
// moment earlier through a genuinely shared KVStore; a KVStore that failed
// to cross replicas would leave replica B with no lockout state at all, so
// its attempt would still answer the ordinary authn.invalid_credentials a
// wrong password always gets on a fresh account.
//
// # A recorded deviation from a real Postgres container
//
// The two replicas do not share a real PostgreSQL container; both use one
// SQLite file at APP_DB_PATH. The reason is a fact about the live tree,
// not a shortcut: internal/app/server.go
// hard-codes dbkit.DialectSQLite (its own blank import of
// go/dbkit/dialect/sqlite is the only dialect driver this app links), with
// no environment variable or option anywhere in this app's wiring that
// selects a different dialect. Adding one would be a second-dialect-axis
// redesign this app deliberately does not attempt, so this file does not
// add it. The axes this file proves (deployment mode x infrastructure
// seam composition) are orthogonal to which SQL dialect the app's own
// database uses, and proving them needs no dialect change at all. SQLite tolerates
// more than one process holding the same file open (unlike, say, an
// exclusive advisory lock would), serializing writers rather than refusing
// a second opener, so two processes against one file is not itself broken
// -- but it is also not how a genuine multi-replica production deployment
// would be shaped, and this file's own design (below) deliberately funnels
// every WRITE through replica A and keeps replica B to reads (and now, the
// SSE stream) only, so the two processes' writes are never actually
// concurrent, which is the SQLite-specific accommodation this shared-file
// topology needs that a real distributed database would not. This is also
// exactly why the positive proof above cannot rely on the shared file for
// its EventBus/KVStore claims -- the same sharing that makes the topology
// workable at all is why APP_DISABLE_QUEUE_WORKER plus the SSE/lockout
// assertions exist: they make the crossing visible without touching the
// SQLite topology itself.
//
// # A recorded fact about the demo identity layer under two replicas
//
// A demo account's membership is org's own memberships row -- the
// signInMemberships store (internal/app/sign_in_memberships.go),
// which authn's WithMembershipReader reads to decide "does this user
// belong to this tenant", answers customer-tenant questions from that
// table, live -- and org's rows live in the SHARED database, so an
// account the boot-time seed registered and placed during replica A's
// boot (APP_DEMO_USERS_PASSWORD, internal/app/demo/demo_users.go) is visible to
// replica B's
// OWN, separate reader instance too: a fresh, successful login against
// replica B for that account would resolve its membership from the same
// rows A's seed wrote. This file's scenario is nevertheless shaped the
// way it is for the two reasons below:
//
//   - The one SUCCESSFUL, token-minting login happens exactly once,
//     against replica A, and that one access token is reused against
//     replica B for every subsequent request (the SSE stream included).
//     This keeps the proof single-writer where it can be -- the demo seed
//     already made replica A the boot-time writer this file's shared-
//     SQLite topology staggers -- and token verification does not consult
//     the membership reader at all: it is a stateless check against the
//     Ed25519 material go/pki's LocalSigner persists in the SAME shared
//     database, so the token replica A mints is genuinely verified by
//     replica B's own independent authn.Middleware, which is itself a
//     real cross-replica proof (a shared signing key via the shared
//     database, never a shared process).
//
//   - The two WRONG-password attempts the "kv" proof drives never reach
//     the membership-resolution step at all: authn's own login sequence
//     (go/authn/service.go's login) checks the rate limiter/lockout FIRST,
//     then resolves the account and verifies the password, and only
//     reaches tenant/membership resolution for a request whose password
//     was actually correct. A wrong password against either replica fails
//     at the password check (or, for the second attempt, at the lockout
//     check before the password is ever touched), never at membership --
//     so the membership store is simply never on the path this proof
//     exercises.
//
// # Why no Postgres/S3/SMTP touch the CORE assertion, and why they are still real
//
// The chosen proof (register once at boot, log in once, create one note,
// observe its announcement on the OTHER replica's SSE stream) exercises
// "eventbus" and "kv" directly, as described above. It does not
// synchronously touch "objectstore" or "mailer" during either replica's
// BOOT (no boot-time seeding step reads or writes an object or sends a
// mail), so in principle a fake, never-dialed S3 endpoint and SMTP relay
// would let the assembly's capability validation pass just as well,
// since that validation checks only the DECLARED capability bits of a
// resolved implementation, never its reachability
// (objectstore/s3.NewObjectStore and pkgcore.NewSMTPMailer both dial
// nothing at construction -- their own doc comments say so). This file
// uses REAL RustFS and a REAL SMTP catcher anyway, deliberately: declaring
// a capability this app never actually exercises would be a weaker proof,
// and the note-created notification
// type's DefaultChannels ("in_app", "email", "sms" --
// examples/reference-app/internal/notes/module.go) means the SAME note
// creation that drives the "eventbus"/"kv" proof ALSO drives a real
// delivery attempt over the "mailer" seam (the creator has an email
// address in internal/app/demo/demo_notification.go's DemoUserAddresses; no
// phone, so SMS is
// skipped, an ordinary no-address outcome, never a failure) -- so this file
// verifies that real send too, as a bonus assertion over Mailpit's own HTTP
// API, rather than leaving "mailer" a capability declared but never really
// proven to work. That delivery, too, only ever runs on replica A now
// (APP_DISABLE_QUEUE_WORKER on B), which is consistent with everything
// above rather than an accident of this particular assertion.
package referenceapp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/pkgcore/redistest"
	"github.com/vislake/speed/go/pkgcore/rustfstest"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// mailhogImage is a real SMTP catcher: Mailpit exposes a plaintext SMTP
// listener (port 1025, no auth required) and an HTTP API (port 8025) that
// lists every message it has caught, which is what this file's bonus
// assertion reads back from -- the same role MailHog itself is best known
// for (and this file's identifiers keep that familiar name), but MailHog
// upstream ships no arm64 image and has had no tagged release in years;
// Mailpit is its actively maintained, wire-compatible-enough successor
// (multi-arch, including arm64, verified empirically on Apple Silicon)
// and CI's own ubuntu-latest amd64 runners pull the
// identical image. No testcontainers-go module exists for either, unlike
// Redis/Postgres above and, since the RustFS swap, RustFS as well -- this
// is an ordinary generic container the same way any image without a
// dedicated module is run.
const mailhogImage = "axllent/mailpit:v1.31"

// distributedNoteCreatorUserID mirrors internal/app/demo/demo_subject.go's
// DemoNotesCreatorUserID byte for byte. It is the user id notes' own
// creator-subject resolver assigns when the X-Demo-User-Id header names
// it, and the ONLY demo user DemoUserAddresses maps to a real address
// ("user-creator-1@demo.example"), which is why this file's note is always
// created under this header.
const distributedNoteCreatorUserID = "user-creator-1"

// distributedNoteCreatorEmail is internal/app/demo/demo_notification.go's
// DemoUserAddresses entry for distributedNoteCreatorUserID, copied byte
// for byte -- the address this file's bonus MailHog assertion looks for.
const distributedNoteCreatorEmail = "user-creator-1@demo.example"

// demoUserIDHeader is internal/app/server.go's DemoOrgUserHeader copied byte
// for byte: notes' own creator-subject resolver AND notification's own
// subject resolver both read the acting user from this header, distinct
// from demoUserHdr ("X-Demo-User", redis_eventbus_composition_test.go's own
// constant), which is rbac's demo permission gate's header. A note-creating
// request in this file carries both: demoUserHdr so rbac's gate sees an
// authorized actor (demoOwner, granted the built-in owner role by
// SeedDemoGrants), and demoUserIDHeader so the note (and the notification
// dispatched back to its creator) is attributed to
// distributedNoteCreatorUserID. The stream request below carries only
// demoUserIDHeader: notification mounts no permission gate of its own
// (handler.go's own doc comment), so a caller only needs to be identified,
// never additionally authorized.
const demoUserIDHeader = "X-Demo-User-Id"

// distributedNoteCreatedTypeKey is
// examples/reference-app/internal/notes/module.go's EventNoteCreated
// copied byte for byte -- the type key notification's inbox-created
// announcement (InboxCreatedPayload.TypeKey) carries for the note this
// file creates. Copied rather than imported for the same reason this
// file's other constants above are: consistency with its neighbors, which
// copy cmd/server identifiers this file structurally cannot import.
const distributedNoteCreatedTypeKey = "notes.note.created"

// notifMessages mirrors flowtests/notification_flow_test.go's own wire shape for
// GET /api/v1/notifications/messages (this file cannot import package
// main, so it is declared again here, field-named after the same JSON the
// module's generated handler serves).
type notifMessages struct {
	Items []struct {
		ID     string         `json:"id"`
		Params map[string]any `json:"params"`
	} `json:"items"`
}

// startRustfsStore starts a disposable RustFS container and creates a fresh
// bucket on it, returning the endpoint (host:port, no scheme -- what
// objectstore/s3.Config.Endpoint and this file's APP_S3_ENDPOINT both
// want), the bucket name and the credentials. The container comes from
// go/pkgcore/rustfstest; the raw configuration is what this file passes to
// two SUBPROCESSES as environment variables, rather than constructing a
// pkgcore.ObjectStore directly the way a module-level test does.
func startRustfsStore(t *testing.T, ctx context.Context) (endpoint, bucket, accessKey, secretKey string) {
	t.Helper()

	const bucketName = "reference-app-distributed"
	endpoint = rustfstest.Start(t, ctx)
	rustfstest.CreateBucket(t, ctx, rustfstest.NewClient(t, endpoint), bucketName)
	return endpoint, bucketName, rustfstest.AccessKey, rustfstest.SecretKey
}

// mailhogEndpoints starts a disposable MailHog container and returns the
// SMTP host:port a Mailer sends through and the base URL of its HTTP API,
// which this file's bonus assertion reads captured messages back from.
func mailhogEndpoints(t *testing.T, ctx context.Context) (smtpAddr, apiBaseURL string) {
	t.Helper()

	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        mailhogImage,
			ExposedPorts: []string{"1025/tcp", "8025/tcp"},
			WaitingFor:   wait.ForListeningPort("1025/tcp"),
		},
		Started: true,
	}
	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		t.Fatalf("start mailhog testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate mailhog testcontainer: %v", terminateErr)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("mailhog testcontainer host: %v", err)
	}
	smtpPort, err := container.MappedPort(ctx, "1025/tcp")
	if err != nil {
		t.Fatalf("mailhog testcontainer smtp port: %v", err)
	}
	apiPort, err := container.MappedPort(ctx, "8025/tcp")
	if err != nil {
		t.Fatalf("mailhog testcontainer api port: %v", err)
	}
	return net.JoinHostPort(host, smtpPort.Port()), "http://" + net.JoinHostPort(host, apiPort.Port())
}

// freePort takes and immediately releases one free TCP port, mirroring
// redis_eventbus_composition_test.go's own inline port-picking logic --
// copied here as a helper since this file needs it twice.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if closeErr := listener.Close(); closeErr != nil {
		t.Fatalf("release the probe listener: %v", closeErr)
	}
	return port
}

// scrubbedEnviron returns the ambient environment with every APP_* and
// PORT variable removed, mirroring redis_eventbus_composition_test.go's own
// inline scrub -- pulled out as a helper since this file builds more than
// one child's environment from it.
func scrubbedEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		if strings.HasPrefix(kv[:eq], "APP_") || kv[:eq] == "PORT" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// syncBuffer is a bytes.Buffer safe for concurrent use. cmd.Start wires it
// up as the subprocess's Stdout/Stderr, so an internal exec goroutine
// copies the child's output into it for as long as the child runs, while
// replica.logs() reads it back from the test goroutine -- concurrently,
// whenever a test polls logs() before the child exits (e.g. this
// directory's warmUpChildSmileSimSubscription and its eventually-based SMS
// wait). A plain bytes.Buffer is not safe for that concurrent read/write
// (caught by the race detector against
// TestServer_RealRedisEventBusComposition_SmileSimCompletionCrossesProcesses),
// so every access goes through the mutex here instead.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// replica is one booted reference-app child process.
type replica struct {
	cmd     *exec.Cmd
	stdout  *syncBuffer
	stderr  *syncBuffer
	baseURL string
	exited  chan struct{}
}

// logs returns the child's captured stdout and stderr, for a test failure
// message.
func (r *replica) logs() string {
	return "child stdout:\n" + r.stdout.String() + "\nchild stderr:\n" + r.stderr.String()
}

// TestReplicaLogs_ConcurrentWriteAndRead_NoRace reproduces, without Docker
// or a real reference-app binary, the exact race warmUpChildSmileSimSubscription
// (demo_notification_smilesim_redis_test.go) hits against a live child: a
// subprocess still writing to its captured stdout on os/exec's own
// background copy goroutine, while this test's own goroutine concurrently
// calls replica.logs (String) in a polling loop before the child has
// exited. syncBuffer exists because a plain *bytes.Buffer made `go test
// -race` fail this test with "DATA RACE" on every run.
func TestReplicaLogs_ConcurrentWriteAndRead_NoRace(t *testing.T) {
	cmd := exec.Command("sh", "-c", "i=0; while [ $i -lt 200 ]; do echo \"line $i\"; i=$((i+1)); done")
	r := &replica{
		cmd:    cmd,
		stdout: new(syncBuffer),
		stderr: new(syncBuffer),
		exited: make(chan struct{}),
	}
	cmd.Stdout, cmd.Stderr = r.stdout, r.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start subprocess: %v", err)
	}
	go func() {
		_ = cmd.Wait()
		close(r.exited)
	}()

	// Read concurrently with the still-running child's write goroutine,
	// exactly like warmUpChildSmileSimSubscription's polling loop does
	// against a real replica.
	for {
		_ = r.logs()
		select {
		case <-r.exited:
			return
		default:
		}
	}
}

// bootReplica builds bin (already built by the caller) as a subprocess with
// env, waits up to 30s for its /healthz to answer 200, and returns it ready
// to receive requests. A child that exits during boot, or never answers
// healthz in time, fails the test immediately -- mirroring
// redis_eventbus_composition_test.go's own boot-wait loop, generalized to
// run twice.
func bootReplica(t *testing.T, bin string, port int, env []string) *replica {
	t.Helper()

	cmd := exec.Command(bin)
	cmd.Env = env
	r := &replica{
		cmd:     cmd,
		stdout:  new(syncBuffer),
		stderr:  new(syncBuffer),
		baseURL: "http://127.0.0.1:" + strconv.Itoa(port),
		exited:  make(chan struct{}),
	}
	cmd.Stdout, cmd.Stderr = r.stdout, r.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start reference-app subprocess on port %d: %v", port, err)
	}
	go func() {
		_ = cmd.Wait()
		close(r.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-r.exited:
		default:
			_ = cmd.Process.Kill()
			<-r.exited
		}
	})

	httpClient := apiClient()
	healthzURL := r.baseURL + "/healthz"
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, getErr := httpClient.Get(healthzURL)
		if getErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return r
			}
		}
		select {
		case <-r.exited:
			t.Fatalf("child on port %d exited during boot: %v\n%s", port, cmd.ProcessState, r.logs())
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("child on port %d never answered GET %s within 30s\n%s", port, healthzURL, r.logs())
	return nil
}

// stopGracefully SIGTERMs r and requires a clean exit within 15s, mirroring
// redis_eventbus_composition_test.go's own shutdown assertion.
func stopGracefully(t *testing.T, r *replica) {
	t.Helper()
	if err := r.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM the child: %v", err)
	}
	select {
	case <-r.exited:
		if code := r.cmd.ProcessState.ExitCode(); code != 0 {
			t.Fatalf("child exited with code %d after SIGTERM, want 0\n%s", code, r.logs())
		}
		if !strings.Contains(r.stdout.String(), "server stopped cleanly") {
			t.Fatalf("child log does not record the clean stop\n%s", r.logs())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("child did not exit within 15s of SIGTERM\n%s", r.logs())
	}
}

// mailhogCapturedMessage polls the SMTP catcher's HTTP API (Mailpit's
// /api/v1/messages) for at least one message whose raw JSON mentions
// address, within a bounded window -- a loose, substring-based check
// rather than a strict decode of its message schema, deliberately: this is
// a bonus assertion (the "mailer" seam genuinely worked, not just declared
// MultiReplicaSafe), and a substring check is far less brittle against a
// third-party JSON shape this codebase does not own.
func mailhogCapturedMessage(t *testing.T, apiBaseURL, address string) bool {
	t.Helper()
	resp, err := apiClient().Get(apiBaseURL + "/api/v1/messages")
	if err != nil {
		t.Fatalf("GET mailhog messages: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read mailhog messages body: %v", err)
	}
	return strings.Contains(string(body), address)
}

// authnLoginAttempt POSTs one password-login attempt for demoOwnerEmail
// against baseURL and returns the response's status code and, when the
// body carries one, its JSON envelope's top-level "code" field (empty on a
// 200, which carries no such field). Unlike demoAccessToken, this helper
// does not require the attempt to succeed -- the "kv" cross-replica proof
// below needs to observe exactly which failure code comes back, not a
// token.
func authnLoginAttempt(t *testing.T, httpClient *http.Client, baseURL, password string) (status int, code string) {
	t.Helper()

	body, err := json.Marshal(map[string]string{
		"identifier": demoOwnerEmail,
		"password":   password,
		"tenant_id":  string(acmeTenantID),
	})
	if err != nil {
		t.Fatalf("marshal the login attempt body: %v", err)
	}
	resp, err := httpClient.Post(baseURL+"/api/v1/authn/login/password", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST the login attempt: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the login attempt response: %v", err)
	}
	var decoded struct {
		Code string `json:"code"`
	}
	// Best effort: a 200 body (access_token/refresh_token/...) carries no
	// "code" field at all, so a decode error there is expected and
	// harmless -- status alone already tells that case apart.
	_ = json.Unmarshal(respBody, &decoded)
	return resp.StatusCode, decoded.Code
}

// sseListener is one open GET /api/v1/notifications/stream connection,
// read in a background goroutine so this file can create the note that
// triggers the announcement afterward and wait for the frame on its own
// schedule -- the connection must be opened BEFORE the note is created,
// since the stream carries no replay (go/notification/handler.go's own
// doc comment on handleStream: "no replay and no resume").
type sseListener struct {
	messages chan notification.InboxCreatedPayload
	cancel   context.CancelFunc
}

// openInboxStream opens GET baseURL+"/api/v1/notifications/stream",
// identified as userIDHeader and authenticated by accessToken, and starts
// reading server-sent-event frames into the returned listener's buffered
// channel in the background. The listener (and its underlying connection)
// is torn down by t.Cleanup.
func openInboxStream(t *testing.T, baseURL, accessToken, userIDHeader string) *sseListener {
	t.Helper()

	// The request's own context, not apiClient()'s Client.Timeout, is what
	// bounds this connection: Client.Timeout covers the WHOLE round trip
	// including reading the body, which would truncate a deliberately
	// long-lived stream, so this listener uses a dedicated client with no
	// Client.Timeout at all.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/notifications/stream", nil)
	if err != nil {
		cancel()
		t.Fatalf("build the SSE stream request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set(demoUserIDHeader, userIDHeader)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET the SSE stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("GET the SSE stream status = %d, want %d; body = %s", resp.StatusCode, http.StatusOK, respBody)
	}

	l := &sseListener{
		messages: make(chan notification.InboxCreatedPayload, 8),
		cancel:   cancel,
	}
	go func() {
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok {
				// Every other line this endpoint sends ("event: message",
				// or the blank frame separator) is not a payload line.
				continue
			}
			var payload notification.InboxCreatedPayload
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				// The handler only ever marshals InboxCreatedPayload
				// (handler.go's own doc comment), so this is unreachable
				// in a running system; skip rather than fail the reader
				// goroutine over it.
				continue
			}
			select {
			case l.messages <- payload:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(l.cancel)
	return l
}

// TestServer_DistributedMode_TwoReplicas_NotificationCrossesRealInfrastructure
// is the positive proof: two real reference-app server processes,
// both booted under APP_DEPLOYMENT_MODE=distributed against the SAME
// real Redis, the SAME real RustFS bucket and the SAME real SMTP catcher --
// the composition internal/app/server.go builds,
// declaring MultiReplicaSafe|SurvivesRestart on every one of the four
// stateful seams the assembly validates. Replica B additionally boots
// with APP_DISABLE_QUEUE_WORKER=true, so it can never itself execute the
// delivery job the note-created event triggers -- see this file's own
// package doc comment for why that is what turns the assertions below into
// a genuine cross-process proof of the "eventbus" and "kv" seams, rather
// than something the two replicas' shared SQLite file could explain on its
// own.
func TestServer_DistributedMode_TwoReplicas_NotificationCrossesRealInfrastructure(t *testing.T) {
	ctx := context.Background()

	redisClient := redistest.Client(t, ctx)
	redisAddr := redisClient.Options().Addr
	s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey := startRustfsStore(t, ctx)
	smtpAddr, mailhogAPI := mailhogEndpoints(t, ctx)
	smtpHost, smtpPort, err := net.SplitHostPort(smtpAddr)
	if err != nil {
		t.Fatalf("split mailhog smtp address %q: %v", smtpAddr, err)
	}

	// Build the real binary once; both replicas run it.
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "reference-app-server")
	buildOut, buildErr := new(bytes.Buffer), new(bytes.Buffer)
	build := exec.Command("go", "build", "-o", bin, "./cmd/server")
	build.Dir = moduleRoot(t)
	build.Stdout, build.Stderr = buildOut, buildErr
	if buildRunErr := build.Run(); buildRunErr != nil {
		t.Fatalf("go build ./cmd/server: %v\nstdout: %s\nstderr: %s", buildRunErr, buildOut.String(), buildErr.String())
	}

	// One shared SQLite file -- see this file's package doc comment for
	// why this is the correct topology given the app's own current,
	// hard-coded SQLite dialect, and for why it is deliberately NOT what
	// this test's cross-replica claims rest on.
	dbPath := filepath.Join(tmp, "reference-app.db")
	baseEnv := scrubbedEnviron()
	sharedEnv := append(append([]string(nil), baseEnv...),
		"APP_DEPLOYMENT_MODE=distributed",
		"APP_CONFIG__CIPHER_KEY=",
		"APP_DB_PATH="+dbPath,
		"APP_REDIS_ADDR="+redisAddr,
		"APP_S3_ENDPOINT="+s3Endpoint,
		"APP_S3_BUCKET="+s3Bucket,
		"APP_S3_ACCESS_KEY="+s3AccessKey,
		"APP_S3_SECRET_KEY="+s3SecretKey,
		// The RustFS fixture is reached at a bare host:port, where the
		// bucket is addressable only path-style; naming the style pins the
		// variable's end-to-end path through the binary's own config
		// resolution into the store.
		"APP_S3_BUCKET_LOOKUP=path",
		"APP_SMTP_HOST="+smtpHost,
		"APP_SMTP_PORT="+smtpPort,
		// Never dialed: this test never drives the phone-login flow, and
		// authn's own wiring-time validation only checks that a sender is
		// PRESENT under the distributed deployment mode, not that it is
		// reachable (pkgcore.NewHTTPSMSSender's own construction dials
		// nothing either).
		"APP_SMS_GATEWAY_URL=http://127.0.0.1:1/sms",
		"APP_DEMO_USERS_PASSWORD="+demoUsersPassword,
	)

	// Replica A boots first and fully (its own boot-time SeedDemoGrants and
	// SeedDemoUsers steps, both real writes to the shared SQLite file, must
	// complete before replica B's own boot-time writes begin -- see this
	// file's package doc comment on why the two boots are staggered rather
	// than concurrent). A keeps its queue worker: it is the only replica
	// that will ever execute a job in this test.
	portA := freePort(t)
	envA := append(append([]string(nil), sharedEnv...), "PORT="+strconv.Itoa(portA))
	replicaA := bootReplica(t, bin, portA, envA)

	// Replica B's queue worker is structurally disabled -- see this file's
	// package doc comment for why that is the change that turns this
	// test's cross-replica assertions into a genuine proof.
	portB := freePort(t)
	envB := append(append([]string(nil), sharedEnv...),
		"PORT="+strconv.Itoa(portB),
		"APP_DISABLE_QUEUE_WORKER=true",
	)
	replicaB := bootReplica(t, bin, portB, envB)

	httpClient := apiClient()

	// One real, SUCCESSFUL login, against replica A only -- see this
	// file's package doc comment on why the proof keeps to one
	// token-minting login (single-writer topology, and a second login's
	// membership answer is not what this file exercises): every
	// subsequent request, including the ones against
	// replica B below, reuses this ONE token, and its verification on
	// replica B is itself a real cross-replica proof (the shared go/pki
	// signing key material, read from the shared database).
	accessToken := demoAccessToken(t, httpClient, replicaA.baseURL, acmeTenantID)

	// The "kv" cross-replica proof: two wrong-password login attempts,
	// deliberately against different replicas, for the state authn's own
	// progressive lockout keeps in the shared KVStore (never in the SQLite
	// file the note/notification chain below could otherwise explain
	// away) -- see this file's package doc comment for the full argument.
	wrongPassword := demoUsersPassword + "-wrong-on-purpose"
	statusA, codeA := authnLoginAttempt(t, httpClient, replicaA.baseURL, wrongPassword)
	if statusA != http.StatusUnauthorized || codeA != "authn.invalid_credentials" {
		t.Fatalf("first wrong-password attempt, against replica A: status = %d, code = %q, want %d / %q\n%s",
			statusA, codeA, http.StatusUnauthorized, "authn.invalid_credentials", replicaA.logs())
	}
	statusB, codeB := authnLoginAttempt(t, httpClient, replicaB.baseURL, wrongPassword)
	if statusB != http.StatusTooManyRequests || codeB != "authn.account_locked" {
		t.Fatalf("second wrong-password attempt, against replica B, inside the 30s lockout window replica A's attempt just opened: "+
			"status = %d, code = %q, want %d / %q -- this is exactly what an unshared KVStore would produce instead (an ordinary "+
			"authn.invalid_credentials, since replica B would see no lockout state of its own)\n%s",
			statusB, codeB, http.StatusTooManyRequests, "authn.account_locked", replicaB.logs())
	}

	// The "eventbus" cross-replica proof starts here: open replica B's own
	// SSE inbox stream BEFORE the note that will trigger an announcement
	// is created (no replay exists -- see the openInboxStream doc
	// comment).
	stream := openInboxStream(t, replicaB.baseURL, accessToken, distributedNoteCreatorUserID)

	// Create one note through replica A's real HTTP stack, attributed to
	// distributedNoteCreatorUserID -- notes.note.created's DefaultChannels
	// dispatch back to that same id over in_app and email (DemoNotesCreatorUserID
	// has an email address in DemoUserAddresses, no phone, so sms is
	// skipped as an ordinary no-address outcome).
	const noteText = "buy milk across two real distributed replicas"
	noteBody, err := json.Marshal(map[string]string{"text": noteText})
	if err != nil {
		t.Fatalf("marshal note body: %v", err)
	}
	notesURL := replicaA.baseURL + "/api/v1/notes"
	createReq, err := http.NewRequest(http.MethodPost, notesURL, bytes.NewReader(noteBody))
	if err != nil {
		t.Fatalf("build POST request: %v", err)
	}
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+accessToken)
	createReq.Header.Set(demoUserHdr, demoOwner)
	createReq.Header.Set(demoUserIDHeader, distributedNoteCreatorUserID)
	createResp, err := httpClient.Do(createReq)
	if err != nil {
		t.Fatalf("POST %s: %v", notesURL, err)
	}
	_ = createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/notes on replica A status = %d, want %d\n%s",
			createResp.StatusCode, http.StatusCreated, replicaA.logs())
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
		t.Fatalf("GET /api/v1/notes on replica A status = %d, want %d\n%s",
			listResp.StatusCode, http.StatusOK, replicaA.logs())
	}
	var listed testListNotesResponse
	if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	var noteID string
	for _, n := range listed.Notes {
		if n.Text == noteText {
			noteID = n.ID
			break
		}
	}
	if noteID == "" {
		t.Fatalf("no note with text %q in replica A's own listing: %+v", noteText, listed.Notes)
	}

	// The core assertion: the note-created event's resulting
	// notification.inbox.created announcement must arrive on REPLICA B's
	// own SSE stream. Replica B's queue worker is disabled, so it cannot
	// have produced this announcement itself -- the only path is a real
	// cross-process delivery over the "eventbus" seam internal/app/server.go's
	// wiring composes over Redis.
	var gotFrame bool
	deadline := time.After(20 * time.Second)
	for !gotFrame {
		select {
		case payload := <-stream.messages:
			if payload.RecipientUserID == distributedNoteCreatorUserID &&
				payload.TenantID == acmeTenantID &&
				payload.TypeKey == distributedNoteCreatedTypeKey {
				gotFrame = true
			}
			// An unrelated frame (there should not be one in this
			// scenario, but nothing here assumes there cannot be) is
			// simply not what we are waiting for; keep reading.
		case <-deadline:
			t.Fatalf("replica B's own SSE stream never announced the note-created delivery within 20s "+
				"(replica B cannot have produced this itself -- its queue worker never started)\nreplica A:\n%s\nreplica B:\n%s",
				replicaA.logs(), replicaB.logs())
		}
	}
	// Close the stream connection now rather than waiting for t.Cleanup:
	// stopGracefully below asks replica B's own http.Server to shut down
	// gracefully, which waits for every active connection to finish first,
	// and this long-lived SSE connection (handleStream blocks on
	// <-r.Context().Done()) would otherwise still be open, holding that
	// shutdown past its own deadline for no reason once this assertion is
	// done with it.
	stream.cancel()

	// A secondary correctness check, not a cross-replica proof by itself
	// (see this file's package doc comment): the delivered message is also
	// readable through replica B's own REST endpoint, over the shared
	// database.
	messagesURL := replicaB.baseURL + "/api/v1/notifications/messages"
	var foundViaREST bool
	testkit.EventuallyWithin(t, 5*time.Second, "the note-created inbox message to be readable through replica B's REST endpoint", func() bool {
		req, reqErr := http.NewRequest(http.MethodGet, messagesURL, nil)
		if reqErr != nil {
			t.Fatalf("build GET request: %v", reqErr)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set(demoUserIDHeader, distributedNoteCreatorUserID)
		resp, doErr := httpClient.Do(req)
		if doErr != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		var out notifMessages
		if decodeErr := json.NewDecoder(resp.Body).Decode(&out); decodeErr != nil {
			return false
		}
		for _, item := range out.Items {
			if item.Params["note_id"] == noteID {
				foundViaREST = true
				return true
			}
		}
		return false
	})
	if !foundViaREST {
		t.Fatalf("the note-created notification never became readable through replica B's own REST listing\nreplica A:\n%s\nreplica B:\n%s",
			replicaA.logs(), replicaB.logs())
	}

	// Bonus: the SAME delivery's email leg genuinely reached the real SMTP
	// catcher -- the "mailer" seam is not merely declared
	// MultiReplicaSafe|SurvivesRestart, it actually moved a message.
	testkit.EventuallyWithin(t, 5*time.Second, "the note-created delivery's email to reach MailHog", func() bool {
		return mailhogCapturedMessage(t, mailhogAPI, distributedNoteCreatorEmail)
	})

	stopGracefully(t, replicaB)
	stopGracefully(t, replicaA)
}

// bootFailureCase describes one way a distributed-mode boot is expected to
// fail closed.
type bootFailureCase struct {
	name       string
	extraEnv   []string
	wantSubstr []string
}

// TestServer_DistributedMode_IncompleteComposition_FailsClosedAtBoot is the
// negative proof, run through the REAL BINARY rather than an
// in-process BuildServer call (flowtests/server_test.go's own
// TestBuildServer_DistributedDeploymentMode_* tests already cover that
// in-process form) -- proving that an operator who requests the
// distributed deployment mode without genuinely composing every seam gets
// a real, non-zero process exit and a real, actionable log message, never
// a silent fall-through to a working-but-secretly-standalone composition.
// Needs no Docker and touches no network: both sub-cases fail before
// anything is dialed.
func TestServer_DistributedMode_IncompleteComposition_FailsClosedAtBoot(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "reference-app-server")
	buildOut, buildErr := new(bytes.Buffer), new(bytes.Buffer)
	build := exec.Command("go", "build", "-o", bin, "./cmd/server")
	build.Dir = moduleRoot(t)
	build.Stdout, build.Stderr = buildOut, buildErr
	if buildRunErr := build.Run(); buildRunErr != nil {
		t.Fatalf("go build ./cmd/server: %v\nstdout: %s\nstderr: %s", buildRunErr, buildOut.String(), buildErr.String())
	}

	cases := []bootFailureCase{
		{
			// Nothing configured at all -- the naive operator mistake.
			// The assembly's Prepare stage walks the selected components'
			// capabilities in the composition's order and runs before any
			// component construction, so THIS is the first thing that
			// fails closed: the in-process event bus a distributed
			// deployment may not select -- ahead of authn.NewModule's own
			// wiring-time SMS-sender validation.
			name:     "nothing configured",
			extraEnv: nil,
			wantSubstr: []string{
				"does not satisfy the deployment mode's required capability",
				"eventbus.memory", "MultiReplicaSafe", "distributed",
			},
		},
		{
			// The SMS seam alone satisfied (a fake, never-dialed gateway
			// URL), Redis/S3/SMTP left on the in-process defaults -- the
			// assembly's own capability validation is what fails, naming
			// the first component the composition selects that cannot run
			// distributed: the in-process memory event bus. The property is
			// the same as it always was: an incomplete distributed
			// composition still fails closed, and it is the component,
			// capability and mode naming that proves it.
			name:     "SMS sender present, every assembly-resolved seam left on its in-process default",
			extraEnv: []string{"APP_SMS_GATEWAY_URL=http://127.0.0.1:1/sms"},
			wantSubstr: []string{
				// The stable core of pkgcore.ErrCapabilityUnsatisfied's
				// own Error() text -- checked as this literal phrase, not
				// the Go identifier, since that identifier never appears
				// in the wrapped message a real operator actually sees,
				// and not the qualifiers the sentinel carries around the
				// core, which may be reworded at any time. Quote
				// characters are deliberately left out of every substring
				// below: the child's log line is one JSON-encoded string,
				// so a literal `"` in the underlying message is escaped
				// to `\"` on the wire, and checking for the unescaped
				// form here would be brittle against that encoding rather
				// than testing anything about the message itself.
				"does not satisfy the deployment mode's required capability",
				"eventbus.memory", "MultiReplicaSafe", "distributed",
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(tmp, fmt.Sprintf("reference-app-negative-%d.db", i))
			env := append(append([]string(nil), scrubbedEnviron()...),
				"APP_DEPLOYMENT_MODE=distributed",
				"APP_CONFIG__CIPHER_KEY=",
				"APP_DB_PATH="+dbPath,
				"PORT="+strconv.Itoa(freePort(t)),
			)
			env = append(env, tc.extraEnv...)

			cmd := exec.Command(bin)
			cmd.Env = env
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			runErr := cmd.Run()
			if runErr == nil {
				t.Fatalf("%s: process exited 0, want a non-zero exit (fail closed)\nstdout: %s\nstderr: %s",
					tc.name, stdout.String(), stderr.String())
			}
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) || exitErr.ExitCode() == 0 {
				t.Fatalf("%s: process error = %v, want a non-zero *exec.ExitError\nstdout: %s\nstderr: %s",
					tc.name, runErr, stdout.String(), stderr.String())
			}
			for _, want := range tc.wantSubstr {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("%s: child stdout does not mention %q\nstdout: %s\nstderr: %s",
						tc.name, want, stdout.String(), stderr.String())
				}
			}
		})
	}
}
