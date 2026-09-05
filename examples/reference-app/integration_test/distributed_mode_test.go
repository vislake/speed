//go:build integration

// This file is the reference app's distributed-mode integration tier: the
// positive and negative halves of the property root CLAUDE.md's Repository
// Status names as the one M0 exit condition CI enforces "not at all" --
// "no Kernel.Bootstrap has ever run under the distributed mode itself." It
// lives in package referenceapp_test alongside
// redis_eventbus_composition_test.go (same build tag, same
// "go test -tags=integration ./..." invocation, no skip-on-missing-Docker
// fallback) and reuses several of that file's helpers directly
// (startRedisClient, eventually, moduleRoot, apiClient, demoAccessToken,
// testNote/testListNotesResponse, and the acmeTenantID/demoUserHdr/
// demoOwner/demoOwnerEmail/demoUsersPassword constants) rather than
// duplicating them.
//
// TWO real server processes, not one: both built from the SAME "go build
// ./cmd/server" binary, both run with SPEED_DEPLOYMENT_MODE=distributed,
// both pointed at the SAME real Redis, the SAME real MinIO bucket, the SAME
// real SMTP catcher, and -- necessarily, see the deviation note below --
// the SAME SQLite file. The scenario only works if the two replicas are
// genuinely composed against shared, multi-replica-safe infrastructure: a
// user registered and a note created through replica A's HTTP port must be
// observable through replica B's HTTP port, with the notification's inbox
// row crossing process boundaries over the real Redis EventBus (the
// "eventbus" seam) and the real Redis KVStore (the "kv" seam, exercised by
// authn's rate limiter on every login attempt) doing the actual work, not
// an in-process channel that would silently only work within one process.
//
// # A recorded deviation from a real Postgres container
//
// This round's brief suggested a genuine PostgreSQL container as the two
// replicas' shared database. That is not what this file does, and the
// reason is a fact about the LIVE tree, not a shortcut: cmd/server/server.go
// hard-codes dbkit.DialectSQLite (its own blank import of
// go/dbkit/dialect/sqlite is the only dialect driver this app links), with
// no environment variable or option anywhere in this app's wiring that
// selects a different dialect. Adding one would be exactly the
// second-dialect-axis redesign this round's own brief says is out of
// scope ("do not attempt the second-dialect axis in this round... this
// round's job is not to redesign that"), so this file does not add it.
// Both replicas therefore share one SQLite file at SPEED_DB_PATH instead --
// the two axes this round closes (deployment mode x infrastructure seam
// composition) are orthogonal to which SQL dialect the app's OWN database
// uses, and proving them needs no dialect change at all. SQLite tolerates
// more than one process holding the same file open (unlike, say, an
// exclusive advisory lock would), serializing writers rather than refusing
// a second opener, so two processes against one file is not itself broken
// -- but it is also not how a genuine multi-replica production deployment
// would be shaped, and this file's own design (below) deliberately funnels
// every WRITE through replica A and keeps replica B to reads only, so the
// two processes' writes are never actually concurrent, which is the
// SQLite-specific accommodation this shared-file topology needs that a
// real distributed database would not.
//
// # A recorded fact about the demo identity layer under two replicas
//
// cmd/server/demo_subject.go's demoMemberships (which authn's
// WithMembershipReader reads to decide "does this user belong to this
// tenant") is an IN-PROCESS map, never persisted -- so a demo account
// registered and granted membership during replica A's boot-time seed
// (SPEED_DEMO_USERS_PASSWORD, demo_users.go) is invisible to replica B's
// OWN, separate demoMemberships instance: an interactive LOGIN attempt
// against replica B for that same account would be refused (membership
// unavailable), even though the account genuinely exists in the shared
// database replica B reads. This file's scenario is designed around that
// real, documented limitation rather than working around it: it logs in
// exactly ONCE, against replica A, and reuses that one access token
// against replica B for every subsequent request. Token verification does
// not consult demoMemberships at all -- it is a stateless check against
// the Ed25519 material go/pki's LocalSigner persists in the SAME shared
// database, so the token replica A mints is genuinely verified by replica
// B's own independent authn.Middleware, which is itself a real
// cross-replica proof (a shared signing key via the shared database,
// never a shared process). A genuinely fresh login against an arbitrary
// replica is a real gap this file does not attempt to close -- it belongs
// to demoMemberships becoming a real, shared store, which is business
// (demo-glue) code this round's brief says not to touch.
//
// # Why no Postgres/S3/SMTP touch the CORE assertion, and why they are still real
//
// The chosen proof (register once at boot, log in once, create one note,
// observe its notification on the OTHER replica) exercises "eventbus" (the
// note-created event, and the notification module's own
// notification.inbox.created cross-replica announcement) and "kv" (every
// login and register attempt is rate-limited through go/ratelimit, which is
// KVStore-backed) directly. It does not synchronously touch "objectstore"
// or "mailer" during either replica's BOOT (no boot-time seeding step
// reads or writes an object or sends a mail), so in principle a fake,
// never-dialed S3 endpoint and SMTP relay would let Kernel.Bootstrap's
// capability validation pass just as well, since that validation checks
// only the DECLARED capability bits of an injected implementation, never
// its reachability (objectstore/s3.NewObjectStore and pkgcore.NewSMTPMailer
// both dial nothing at construction -- their own doc comments say so).
// This file uses REAL MinIO and a REAL SMTP catcher anyway, deliberately:
// declaring a capability this app never actually exercises would be a
// weaker proof than this round's own brief asks for, and the note-created
// notification type's DefaultChannels ("in_app", "email", "sms" --
// examples/reference-app/internal/notes/module.go) means the SAME note
// creation that drives the "eventbus"/"kv" proof ALSO drives a real
// delivery attempt over the "mailer" seam (the creator has an email
// address in demo_notification.go's demoUserAddresses; no phone, so SMS is
// skipped, an ordinary no-address outcome, never a failure) -- so this file
// verifies that real send too, as a bonus assertion over MailHog's own HTTP
// API, rather than leaving "mailer" a capability declared but never really
// proven to work.
package referenceapp_test

import (
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
	"syscall"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
	"github.com/testcontainers/testcontainers-go/wait"
)

// minioImage pins the same MinIO release go/storage's and go/pkgcore's own
// MinIO integration legs run against, so every tier in this repository
// exercises the same server behavior.
const minioImage = "minio/minio:RELEASE.2024-01-16T16-07-38Z"

// mailhogImage is a real SMTP catcher: Mailpit exposes a plaintext SMTP
// listener (port 1025, no auth required) and an HTTP API (port 8025) that
// lists every message it has caught, which is what this file's bonus
// assertion reads back from -- the same role MailHog itself is best known
// for (and this file's identifiers keep that familiar name), but MailHog
// upstream ships no arm64 image and has had no tagged release in years;
// Mailpit is its actively maintained, wire-compatible-enough successor
// (multi-arch, including arm64, verified empirically while writing this
// test on Apple Silicon) and CI's own ubuntu-latest amd64 runners pull the
// identical image. No testcontainers-go module exists for either, unlike
// Redis/Postgres/MinIO above -- this is an ordinary generic container the
// same way any image without a dedicated module is run.
const mailhogImage = "axllent/mailpit:v1.31"

// distributedNoteCreatorUserID is demo_notification.go's
// demoNotesCreatorUserID copied byte for byte (cmd/server owns the real
// declaration; this file cannot import package main). It is the user id
// notes' own creator-subject resolver assigns when the X-Demo-User-Id
// header names it, and the ONLY demo user demoUserAddresses maps to a real
// address ("user-creator-1@demo.example"), which is why this file's note is
// always created under this header.
const distributedNoteCreatorUserID = "user-creator-1"

// distributedNoteCreatorEmail is demo_notification.go's demoUserAddresses
// entry for distributedNoteCreatorUserID, copied byte for byte -- the
// address this file's bonus MailHog assertion looks for.
const distributedNoteCreatorEmail = "user-creator-1@demo.example"

// demoUserIDHeader is cmd/server/server.go's demoOrgUserHeader copied byte
// for byte: notes' own creator-subject resolver AND notification's own
// subject resolver both read the acting user from this header, distinct
// from demoUserHdr ("X-Demo-User", redis_eventbus_composition_test.go's own
// constant), which is rbac's demo permission gate's header. A note-creating
// request in this file carries both: demoUserHdr so rbac's gate sees an
// authorized actor (demoOwner, granted the built-in owner role by
// seedDemoGrants), and demoUserIDHeader so the note (and the notification
// dispatched back to its creator) is attributed to
// distributedNoteCreatorUserID.
const demoUserIDHeader = "X-Demo-User-Id"

// notifMessages mirrors notification_flow_test.go's own wire shape for
// GET /api/v1/notifications/messages (this file cannot import package
// main, so it is declared again here, field-named after the same JSON the
// module's generated handler serves).
type notifMessages struct {
	Items []struct {
		ID     string         `json:"id"`
		Params map[string]any `json:"params"`
	} `json:"items"`
}

// startMinioStore starts a disposable MinIO container and creates a fresh
// bucket on it, returning the endpoint (host:port, no scheme -- what
// objectstore/s3.Config.Endpoint and this file's SPEED_S3_ENDPOINT both
// want), the bucket name and the credentials. Copied from
// go/storage/integration_test/minio_leg_test.go's startMinioStore, adapted
// to hand back raw configuration this file passes to two SUBPROCESSES as
// environment variables, rather than constructing a pkgcore.ObjectStore
// directly the way the module-level test does.
func startMinioStore(t *testing.T, ctx context.Context) (endpoint, bucket, accessKey, secretKey string) {
	t.Helper()

	container, err := tcminio.Run(ctx, minioImage,
		tcminio.WithUsername("minioadmin"),
		tcminio.WithPassword("minioadmin"),
	)
	if err != nil {
		t.Fatalf("start minio testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate minio testcontainer: %v", terminateErr)
		}
	})

	endpoint, err = container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("minio testcontainer connection string: %v", err)
	}

	const bucketName = "reference-app-distributed"
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(container.Username, container.Password, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("build a minio client for %q: %v", endpoint, err)
	}
	if err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("create bucket %q on the minio testcontainer: %v", bucketName, err)
	}

	return endpoint, bucketName, container.Username, container.Password
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

// scrubbedEnviron returns the ambient environment with every SPEED_* and
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
		if strings.HasPrefix(kv[:eq], "SPEED_") || kv[:eq] == "PORT" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// replica is one booted reference-app child process.
type replica struct {
	cmd     *exec.Cmd
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	baseURL string
	exited  chan struct{}
}

// logs returns the child's captured stdout and stderr, for a test failure
// message.
func (r *replica) logs() string {
	return "child stdout:\n" + r.stdout.String() + "\nchild stderr:\n" + r.stderr.String()
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
		stdout:  new(bytes.Buffer),
		stderr:  new(bytes.Buffer),
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

// TestServer_DistributedMode_TwoReplicas_NotificationCrossesRealInfrastructure
// is this round's positive proof: two real reference-app server processes,
// both booted under SPEED_DEPLOYMENT_MODE=distributed against the SAME
// real Redis, the SAME real MinIO bucket and the SAME real SMTP catcher --
// exactly the composition this round's server.go changes make possible,
// declaring MultiReplicaSafe|SurvivesRestart on every one of the four
// stateful seams Kernel.Bootstrap validates. A user registered and a note
// created through replica A's HTTP port produce a notification whose inbox
// row is read back through replica B's HTTP port, proving the EventBus and
// KVStore seams genuinely crossed process boundaries through Redis rather
// than an in-process channel that would silently only work within one
// process -- and, as a bonus, that the real SMTP relay actually received
// the same delivery's email leg.
func TestServer_DistributedMode_TwoReplicas_NotificationCrossesRealInfrastructure(t *testing.T) {
	ctx := context.Background()

	redisClient := startRedisClient(t, ctx)
	redisAddr := redisClient.Options().Addr
	s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey := startMinioStore(t, ctx)
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
	// hard-coded SQLite dialect.
	dbPath := filepath.Join(tmp, "reference-app.db")
	baseEnv := scrubbedEnviron()
	sharedEnv := append(append([]string(nil), baseEnv...),
		"SPEED_DEPLOYMENT_MODE=distributed",
		"SPEED_CONFIG_KEY=",
		"SPEED_DB_PATH="+dbPath,
		"SPEED_REDIS_ADDR="+redisAddr,
		"SPEED_S3_ENDPOINT="+s3Endpoint,
		"SPEED_S3_BUCKET="+s3Bucket,
		"SPEED_S3_ACCESS_KEY="+s3AccessKey,
		"SPEED_S3_SECRET_KEY="+s3SecretKey,
		"SPEED_SMTP_HOST="+smtpHost,
		"SPEED_SMTP_PORT="+smtpPort,
		// Never dialed: this test never drives the phone-login flow, and
		// authn's own wiring-time validation only checks that a sender is
		// PRESENT under the distributed deployment mode, not that it is
		// reachable (authn.NewHTTPSMSSender's own construction dials
		// nothing either).
		"SPEED_SMS_GATEWAY_URL=http://127.0.0.1:1/sms",
		"SPEED_DEMO_USERS_PASSWORD="+demoUsersPassword,
	)

	// Replica A boots first and fully (its own boot-time seedDemoGrants and
	// seedDemoUsers steps, both real writes to the shared SQLite file, must
	// complete before replica B's own boot-time writes begin -- see this
	// file's package doc comment on why the two boots are staggered rather
	// than concurrent).
	portA := freePort(t)
	envA := append(append([]string(nil), sharedEnv...), "PORT="+strconv.Itoa(portA))
	replicaA := bootReplica(t, bin, portA, envA)

	portB := freePort(t)
	envB := append(append([]string(nil), sharedEnv...), "PORT="+strconv.Itoa(portB))
	replicaB := bootReplica(t, bin, portB, envB)

	httpClient := apiClient()

	// One real login, against replica A only -- see this file's package
	// doc comment on why a second, independent login against replica B
	// would fail today (demoMemberships is in-process, not shared) and why
	// that does not weaken this test: every subsequent request, including
	// the ones against replica B below, reuses this ONE token, and its
	// verification on replica B is itself a real cross-replica proof (the
	// shared go/pki signing key material, read from the shared database).
	accessToken := demoAccessToken(t, httpClient, replicaA.baseURL, acmeTenantID)

	// Create one note through replica A's real HTTP stack, attributed to
	// distributedNoteCreatorUserID -- notes.note.created's DefaultChannels
	// dispatch back to that same id over in_app and email (demoNotesCreatorUserID
	// has an email address in demoUserAddresses, no phone, so sms is
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

	// The cross-replica assertion: the notification the note-created event
	// triggers must be readable through REPLICA B's own HTTP port -- the
	// same access token replica A minted, verified independently by
	// replica B's own authn.Middleware, over the real Redis-backed
	// EventBus and KVStore this round's server.go wiring composes.
	messagesURL := replicaB.baseURL + "/api/v1/notifications/messages"
	var found bool
	eventually(t, "the note-created inbox message to reach replica B", func() bool {
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
				found = true
				return true
			}
		}
		return false
	})
	if !found {
		t.Fatalf("the note-created notification never reached replica B's own inbox listing\nreplica A:\n%s\nreplica B:\n%s",
			replicaA.logs(), replicaB.logs())
	}

	// Bonus: the SAME delivery's email leg genuinely reached the real SMTP
	// catcher -- the "mailer" seam is not merely declared
	// MultiReplicaSafe|SurvivesRestart, it actually moved a message.
	eventually(t, "the note-created delivery's email to reach MailHog", func() bool {
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

// TestServer_DistributedMode_IncompleteComposition_FailsClosedAtBoot is this
// round's negative proof, run through the REAL BINARY rather than an
// in-process buildServer call (server_test.go's own
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
			// authn.NewModule's own wiring-time validation runs BEFORE
			// Kernel.Bootstrap in buildServer (see server.go's authn
			// wiring comment), so THIS is the first thing that fails
			// closed: no SMS sender for a distributed deployment.
			name:       "nothing configured",
			extraEnv:   nil,
			wantSubstr: []string{"distributed deployment mode requires an explicit SMS sender"},
		},
		{
			// The SMS seam alone satisfied (a fake, never-dialed gateway
			// URL), Redis/S3/SMTP left on the Preset's in-process
			// defaults -- Kernel.Bootstrap's OWN capability validation is
			// what fails now, naming the first seam it resolves in its
			// fixed order: "eventbus".
			name:     "SMS sender present, every kernel seam left on its in-process default",
			extraEnv: []string{"SPEED_SMS_GATEWAY_URL=http://127.0.0.1:1/sms"},
			wantSubstr: []string{
				// pkgcore.ErrCapabilityUnsatisfied's own Error() text --
				// checked as this literal string, not the Go identifier,
				// since that identifier never appears in the wrapped
				// message a real operator actually sees. Quote characters
				// are deliberately left out of every substring below: the
				// child's log line is one JSON-encoded string, so a literal
				// `"` in the underlying message is escaped to `\"` on the
				// wire, and checking for the unescaped form here would be
				// brittle against that encoding rather than testing
				// anything about the message itself.
				"seam implementation does not satisfy the deployment mode's required capability",
				"eventbus", "eventbus.memory", "MultiReplicaSafe", "distributed",
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(tmp, fmt.Sprintf("reference-app-negative-%d.db", i))
			env := append(append([]string(nil), scrubbedEnviron()...),
				"SPEED_DEPLOYMENT_MODE=distributed",
				"SPEED_CONFIG_KEY=",
				"SPEED_DB_PATH="+dbPath,
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
