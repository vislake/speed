//go:build integration

// This file is the reference app's distributed-mode regression for the
// boot-time demo-user seed's register-budget behavior
// (internal/app/demo_users.go):
// repeated seeded restarts within authn's register quota window must not
// trip the register rate limit.
//
// The mechanism, and why only this tier can prove it: authn limits
// registrations per client IP (go/authn/ratelimit.go's
// limitRegisterByIP), and the seed's register POSTs travel in-process with
// no client address, so every seeded boot debits the one shared
// "no-address" bucket of whatever KVStore authn's rate guard sits on.
// Under the standalone deployment mode that KVStore is per-boot memory --
// a restart starts from a fresh budget, so the defect is unobservable
// there. Under the distributed mode the KVStore is the shared Redis both
// replicas compose, so the budget survives restarts: a seed that POSTed
// four registrations per boot (the three demo accounts plus the
// platform-staff account, conflict answers included -- the guard runs
// before the already-registered check) would be refused on the third
// startup within the one-hour window with authn.rate_limited and the boot
// would fail.
//
// This test reboots ONE real replica process three times against the SAME
// Redis KVStore and the SAME SQLite file with APP_DEMO_USERS_PASSWORD set
// -- three startups within the quota window -- asserting that every boot
// reaches /healthz and that the
// seeded owner can still sign in on each boot. The seed asks
// authn.Service.SearchUsers whether each account exists BEFORE POSTing
// the register route
// (authn/demoseed's Register), so boots two and three register nothing at
// all and never touch the public register budget, whatever deployment
// mode and whatever KVStore state the budget lives in.
//
// The env composition mirrors TestServer_DistributedMode_TwoReplicas_...'s
// sharedEnv exactly: one real Redis, one real RustFS bucket, one real SMTP
// catcher, the never-dialed SMS gateway URL, and one shared SQLite file
// (the same deliberate deviation from a second dialect that file's package
// doc comment records -- both replicas here are one replica, restarted).
package referenceapp_test

import (
	"bytes"
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestServer_DistributedMode_RepeatedSeededRestarts_DoNotTripTheRegisterBudget(t *testing.T) {
	ctx := context.Background()

	redisClient := startRedisClient(t, ctx)
	redisAddr := redisClient.Options().Addr
	s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey := startRustfsStore(t, ctx)
	smtpAddr, _ := mailhogEndpoints(t, ctx)
	smtpHost, smtpPort, err := net.SplitHostPort(smtpAddr)
	if err != nil {
		t.Fatalf("split mailhog smtp address %q: %v", smtpAddr, err)
	}

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "reference-app-server")
	buildOut, buildErr := new(bytes.Buffer), new(bytes.Buffer)
	build := exec.Command("go", "build", "-o", bin, "./cmd/server")
	build.Dir = moduleRoot(t)
	build.Stdout, build.Stderr = buildOut, buildErr
	if buildRunErr := build.Run(); buildRunErr != nil {
		t.Fatalf("go build ./cmd/server: %v\nstdout: %s\nstderr: %s", buildRunErr, buildOut.String(), buildErr.String())
	}

	// One SQLite file and one Redis budget shared across all three boots:
	// the honest image of a deployment restarting three times within the
	// register quota window.
	dbPath := filepath.Join(tmp, "reference-app.db")
	baseEnv := scrubbedEnviron()
	env := append(append([]string(nil), baseEnv...),
		"APP_DEPLOYMENT_MODE=distributed",
		"APP_CONFIG_KEY=",
		"APP_DB_PATH="+dbPath,
		"APP_REDIS_ADDR="+redisAddr,
		"APP_S3_ENDPOINT="+s3Endpoint,
		"APP_S3_BUCKET="+s3Bucket,
		"APP_S3_ACCESS_KEY="+s3AccessKey,
		"APP_S3_SECRET_KEY="+s3SecretKey,
		"APP_SMTP_HOST="+smtpHost,
		"APP_SMTP_PORT="+smtpPort,
		// Never dialed: this test never drives the phone-login flow, and
		// authn's wiring-time validation only checks that a sender is
		// PRESENT under the distributed deployment mode.
		"APP_SMS_GATEWAY_URL=http://127.0.0.1:1/sms",
		"APP_DEMO_USERS_PASSWORD="+demoUsersPassword,
	)

	httpClient := apiClient()
	for boot := 1; boot <= 3; boot++ {
		port := freePort(t)
		replica := bootReplica(t, bin, port, append(append([]string(nil), env...), "PORT="+strconv.Itoa(port)))

		// The seeded owner can sign in on every boot: boot one through the
		// registrations it just made, boots two and three through the
		// grants the seed re-asserted under the ids a previous boot
		// assigned (demoseed's already-exists path).
		token := demoAccessToken(t, httpClient, replica.baseURL, pkgcore.TenantID(acmeTenantID))
		if token == "" {
			t.Fatalf("boot %d: demo owner sign-in returned no access token", boot)
		}

		stopGracefully(t, replica)
		t.Logf("boot %d of 3 seeded and served cleanly", boot)
	}
}
