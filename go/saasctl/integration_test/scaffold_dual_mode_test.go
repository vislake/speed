//go:build integration

// This file is the scaffold-verify pipeline's real dual-mode boot gate: the
// roadmap M4 acceptance (docs/internal/15-roadmap.md's M4 row: a project
// `saasctl new` and `create-saas-app` generate must boot successfully in
// both the standalone and distributed deployment modes with one command
// each) applied to the Go half of that story, go/saasctl/AGENTS.md's own
// "the scaffold-verify workflow's M4 dual-mode boot gate" limitation
// discharged for one selection. It follows
// the SAME real end-to-end procedure this module's own AGENTS.md Testing
// section already documents for the B4 milestone gate (new -> tidy -> build
// -> boot -> smoke), extended with a second boot under
// APP_DEPLOYMENT_MODE=distributed against real Docker-backed
// infrastructure -- the identical Redis/RustFS/mailpit trio and image pins
// examples/reference-app/integration_test/distributed_mode_test.go already
// uses, so every Docker-backed tier in this repository exercises the same
// server behavior.
//
// # Why one selection, not all five
//
// Running the full generate/tidy(network)/build/boot/smoke cycle TWICE
// (once per deployment mode) against all five legal selections would run
// five real `go mod tidy` network round-trips and five container-backed
// boots in one CI job -- a multiplying cost for a property (the general
// infrastructure-seam wiring in cmd/server/config.go and the per-selection
// kernelOptions block in each server.go) that is IDENTICAL prose across
// every selection, copied deliberately so the bootstrap contract never
// changes with the selection (config.go's own orgIndexKeyEnv doc comment
// states this precedent already). This file proves it once, against
// authn+org+rbac -- the richest selection, the one whose server.go also
// exercises authn's "SMS sender" seam's three-way conditional-injection
// switch (config.go's smsGatewayURLEnv doc comment) that a selection with
// no authn module cannot touch at all. The other four selections'
// standalone-mode boot was proven once, by the B4 procedure's own real
// materialize-tidy-build-boot cycle recorded in AGENTS.md; their
// materialization is pinned byte-for-byte on every PR by this SAME
// module's offline unit suite (internal/new's golden byte-identity tests
// -- deliberately a pin of the materializer against the committed assets
// and nothing more: it is not a tidy/build proof, and not a freshness
// check on the go.mod goldens, which nothing automatic re-verifies
// today). Their own dual-mode CI coverage remains deferred, recorded as
// such in AGENTS.md's Known limitations, exactly the same
// "representative subset, explicit reason" shape root CLAUDE.md's
// Reference App section already blesses for go/pki's X.509 layer and
// go/integration's two rounds.
//
// # What this does NOT prove
//
// This proves ONE process satisfies Kernel.Bootstrap's capability
// validation under the distributed deployment mode and answers real HTTP
// requests over real Redis/RustFS/SMTP -- the identical "one binary, real
// infrastructure, distributed topology" property
// examples/reference-app/docker-compose.distributed.yml's own header
// records for the reference app. It does NOT run two replicas converging
// through the bus (distributed_mode_test.go's own job, for the reference
// app; saasctl ships no such multi-replica proof and this file does not
// invent one) -- a generated project's own cross-replica proof, if a
// consumer wants one, is the consumer's to write against their own
// business modules, the same way the reference app's is against notes and
// notification.
package saasctl_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vislake/speed/go/saasctl/internal/db"
	newcmd "github.com/vislake/speed/go/saasctl/internal/new"
)

// rustfsImage and mailpitImage pin the exact same images
// examples/reference-app/integration_test/distributed_mode_test.go uses, so
// this tier and that one exercise identical server behavior; redisImage
// mirrors that file's own Redis pin too. Keeping all three pins in sync
// across the two files is a matter of code review, the same way this
// repository already keeps several other Docker-backed tiers' pins in
// step deliberately (this file's own header has the full reasoning for why
// one shared pin set matters here).
const (
	redisImage   = "redis:7-alpine"
	rustfsImage  = "rustfs/rustfs:1.0.0-rc.5"
	mailpitImage = "axllent/mailpit:v1.31"
)

// TestScaffoldNewProject_AuthnOrgRbac_BootsInBothDeploymentModes is this
// file's one test: materialize the authn+org+rbac selection for real
// (internal/new.Run, in-process -- no separately built saasctl binary
// needed, since this test lives in the same module and can call the
// command packages directly), `go mod tidy` it against the real network
// (exactly the B4 procedure), build its cmd/server, run `saasctl db
// migrate` against it (internal/db.Run, in-process) under the standalone
// default, boot the built binary and smoke it, then repeat the migrate step
// and the boot under APP_DEPLOYMENT_MODE=distributed with real
// Redis/RustFS/SMTP infrastructure this test starts itself.
func TestScaffoldNewProject_AuthnOrgRbac_BootsInBothDeploymentModes(t *testing.T) {
	speedRoot, err := newcmd.ResolveSpeedRoot("")
	if err != nil {
		t.Fatalf("resolve the speed checkout this test runs inside of: %v", err)
	}

	target := filepath.Join(t.TempDir(), "scaffold-app")
	if code := newcmd.Run([]string{"--speed-root", speedRoot, "--with", "authn,org,rbac", target}, os.Stdout, os.Stderr); code != 0 {
		t.Fatalf("saasctl new exited %d, want 0", code)
	}

	// Genuine `go mod tidy` against the real network -- the same real
	// proof AGENTS.md's Testing section records for the B4 milestone gate,
	// never faked or skipped here.
	runTool(t, target, "go", "mod", "tidy")

	binPath := filepath.Join(target, "server")
	runTool(t, target, "go", "build", "-o", binPath, "./cmd/server")

	modPath := filepath.Join(target, "go.mod")

	// Standalone leg first: `saasctl db migrate` (internal/db.Run,
	// in-process) against a fresh database with no special environment,
	// then boot the built binary and smoke it -- the identical shape
	// AGENTS.md's B4 procedure already proves for every legal selection.
	standaloneDB := filepath.Join(target, "standalone.db")
	runDBMigrate(t, modPath, standaloneDB, nil)
	smokeBoot(t, binPath, standaloneDB, nil)

	// Distributed leg: real Redis, RustFS (S3-compatible) and Mailpit
	// containers, the identical trio and image pins
	// distributed_mode_test.go already uses, started fresh for this test
	// alone (t.Cleanup terminates each). APP_SMS_GATEWAY_URL is set to a
	// deliberately unreachable address -- authn's distributed-mode
	// validation requires the "SMS sender" seam to be WIRED (a non-empty
	// URL), never that it is reachable, and this test never drives the
	// phone-login flow that would dial it (authn.NewHTTPSMSSender's own
	// construction dials nothing either) -- the identical convention
	// distributed_mode_test.go's own comment on this exact value and
	// docker-compose.distributed.yml both already use.
	ctx := context.Background()
	redisAddr := startRedis(t, ctx)
	s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey := startRustfsStore(t, ctx, "scaffold-verify")
	smtpAddr := startMailpit(t, ctx)

	distributedEnv := []string{
		"APP_DEPLOYMENT_MODE=distributed",
		"APP_REDIS_ADDR=" + redisAddr,
		"APP_S3_ENDPOINT=" + s3Endpoint,
		"APP_S3_BUCKET=" + s3Bucket,
		"APP_S3_ACCESS_KEY=" + s3AccessKey,
		"APP_S3_SECRET_KEY=" + s3SecretKey,
		"APP_S3_USE_SSL=false",
		"APP_SMTP_HOST=" + smtpHost(smtpAddr),
		"APP_SMTP_PORT=" + smtpPort(smtpAddr),
		"APP_SMS_GATEWAY_URL=http://127.0.0.1:1/sms",
	}

	// db migrate is the round's own relaxed-refusal proof: an earlier
	// version of this command refused any deployment mode but standalone
	// (migrate.go's own migrate function doc comment has the full
	// argument for why that refusal's premise did not hold); this call
	// proves the corrected behavior against a REAL distributed-mode
	// environment, not merely the offline unit fixture
	// internal/db/migrate_test.go's own
	// TestMigrateDistributedModeAppliesTheIdenticalSQLiteSchema pins.
	distributedDB := filepath.Join(target, "distributed.db")
	runDBMigrateWithEnv(t, modPath, distributedDB, distributedEnv)
	smokeBoot(t, binPath, distributedDB, distributedEnv)
}

// runTool runs name with args inside dir, failing the test with its
// combined output on a non-zero exit -- the shared shape both the `go mod
// tidy` and `go build` steps use.
func runTool(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s (in %s): %v\n%s", name, strings.Join(args, " "), dir, err, out)
	}
}

// runDBMigrate runs `saasctl db migrate` in-process (internal/db.Run)
// against modPath with APP_DB_PATH set to dbPath and every other
// bootstrap variable cleared, so the standalone leg's migrate call is
// insulated from whatever the test process's own environment happens to
// hold.
func runDBMigrate(t *testing.T, modPath, dbPath string, extraEnv []string) {
	t.Helper()
	runDBMigrateWithEnv(t, modPath, dbPath, extraEnv)
}

// runDBMigrateWithEnv sets APP_DB_PATH (and every key=value pair in
// extraEnv) on the TEST PROCESS's own environment via t.Setenv -- internal/
// db.Run reads os.LookupEnv directly, being the same in-process call the
// real `saasctl db migrate` CLI dispatches to -- runs migrate, and fails
// the test on a non-zero exit.
func runDBMigrateWithEnv(t *testing.T, modPath, dbPath string, extraEnv []string) {
	t.Helper()
	t.Setenv("APP_DB_PATH", dbPath)
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	for _, kv := range extraEnv {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed env entry %q, want key=value", kv)
		}
		t.Setenv(key, value)
	}
	var stdout, stderr strings.Builder
	if code := db.Run([]string{"migrate", modPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("saasctl db migrate exited %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
}

// smokeBoot boots binPath as a subprocess with APP_DB_PATH=dbPath plus
// extraEnv, waits for /healthz to answer 200, issues one real smoke request
// against /api/config/public and one against the authn register endpoint
// (proving the composed authn+tenancy chain actually answers, not merely
// that the process is listening), then stops it gracefully with SIGTERM and
// requires a clean exit -- mirroring examples/reference-app/integration_test/
// distributed_mode_test.go's own bootReplica/stopGracefully shape,
// specialized to one process rather than two replicas.
func smokeBoot(t *testing.T, binPath, dbPath string, extraEnv []string) {
	t.Helper()

	port := freePort(t)
	env := append([]string{
		"APP_DB_PATH=" + dbPath,
		"PORT=" + strconv.Itoa(port),
	}, extraEnv...)

	cmd := exec.Command(binPath)
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", binPath, err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			_ = cmd.Process.Kill()
			<-exited
		}
	})

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	httpClient := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	healthy := false
	for time.Now().Before(deadline) {
		resp, getErr := httpClient.Get(baseURL + "/healthz")
		if getErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		select {
		case <-exited:
			t.Fatalf("%s exited during boot\nstdout:\n%s\nstderr:\n%s", binPath, stdout.String(), stderr.String())
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !healthy {
		t.Fatalf("%s never answered GET /healthz within 30s\nstdout:\n%s\nstderr:\n%s", binPath, stdout.String(), stderr.String())
	}

	resp, err := httpClient.Get(baseURL + "/api/config/public")
	if err != nil {
		t.Fatalf("GET /api/config/public: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/config/public = %d, want 200\nstdout:\n%s\nstderr:\n%s", resp.StatusCode, stdout.String(), stderr.String())
	}

	registerBody := strings.NewReader(`{"email":"scaffold-verify@example.com","password":"CorrectHorseBatteryStaple1!"}`)
	resp, err = httpClient.Post(baseURL+"/api/v1/authn/register", "application/json", registerBody)
	if err != nil {
		t.Fatalf("POST /api/v1/authn/register: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("POST /api/v1/authn/register = %d, want 201\nstdout:\n%s\nstderr:\n%s", resp.StatusCode, stdout.String(), stderr.String())
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt %s: %v", binPath, err)
	}
	select {
	case <-exited:
		if code := cmd.ProcessState.ExitCode(); code != 0 {
			t.Errorf("%s exited with code %d after a graceful shutdown signal, want 0\nstdout:\n%s\nstderr:\n%s", binPath, code, stdout.String(), stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Errorf("%s did not exit within 15s of a graceful shutdown signal\nstdout:\n%s\nstderr:\n%s", binPath, stdout.String(), stderr.String())
	}
}

// freePort asks the OS for a free TCP port by binding to :0 and closing
// immediately -- the same "let the kernel pick" pattern used anywhere else
// in this repository a test needs an ephemeral listen port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// startRedis starts a disposable Redis container and returns its
// "host:port" address, exactly the shape APP_REDIS_ADDR wants.
func startRedis(t *testing.T, ctx context.Context) string {
	t.Helper()
	req := testcontainers.ContainerRequest{
		Image:        redisImage,
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForListeningPort("6379/tcp"),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start redis testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate redis testcontainer: %v", terminateErr)
		}
	})
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("redis testcontainer host: %v", err)
	}
	mappedPort, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("redis testcontainer mapped port: %v", err)
	}
	return net.JoinHostPort(host, mappedPort.Port())
}

// startRustfsStore starts a disposable RustFS container and creates a
// fresh bucket named bucketPrefix on it, returning the endpoint
// (host:port, no scheme), the bucket name and the credentials -- the
// identical shape distributed_mode_test.go's own startRustfsStore uses,
// duplicated here (rather than shared) because that helper lives in the
// reference app's own module and this test cannot import it.
func startRustfsStore(t *testing.T, ctx context.Context, bucketPrefix string) (endpoint, bucket, accessKey, secretKey string) {
	t.Helper()

	const rustfsAccessKey = "rustfsadmin"
	const rustfsSecretKey = "rustfsadmin"
	req := testcontainers.ContainerRequest{
		Image:        rustfsImage,
		ExposedPorts: []string{"9000/tcp"},
		Env: map[string]string{
			"RUSTFS_ACCESS_KEY":     rustfsAccessKey,
			"RUSTFS_SECRET_KEY":     rustfsSecretKey,
			"RUSTFS_ADDRESS":        ":9000",
			"RUSTFS_CONSOLE_ENABLE": "false",
		},
		WaitingFor: wait.ForHTTP("/health").WithPort("9000/tcp").WithStartupTimeout(60 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start rustfs testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate rustfs testcontainer: %v", terminateErr)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("rustfs testcontainer host: %v", err)
	}
	mappedPort, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("rustfs testcontainer mapped port: %v", err)
	}
	endpoint = net.JoinHostPort(host, mappedPort.Port())

	bucketName := bucketPrefix
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(rustfsAccessKey, rustfsSecretKey, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("build a minio-go client for %q: %v", endpoint, err)
	}
	if err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("create bucket %q on the rustfs testcontainer: %v", bucketName, err)
	}

	return endpoint, bucketName, rustfsAccessKey, rustfsSecretKey
}

// startMailpit starts a disposable Mailpit container and returns its SMTP
// "host:port" address.
func startMailpit(t *testing.T, ctx context.Context) string {
	t.Helper()
	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        mailpitImage,
			ExposedPorts: []string{"1025/tcp"},
			WaitingFor:   wait.ForListeningPort("1025/tcp"),
		},
		Started: true,
	}
	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		t.Fatalf("start mailpit testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate mailpit testcontainer: %v", terminateErr)
		}
	})
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("mailpit testcontainer host: %v", err)
	}
	mappedPort, err := container.MappedPort(ctx, "1025/tcp")
	if err != nil {
		t.Fatalf("mailpit testcontainer mapped port: %v", err)
	}
	return net.JoinHostPort(host, mappedPort.Port())
}

// smtpHost and smtpPort split a "host:port" address into the two separate
// APP_SMTP_HOST/APP_SMTP_PORT values the generated project's
// configFromEnv wants.
func smtpHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func smtpPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}
