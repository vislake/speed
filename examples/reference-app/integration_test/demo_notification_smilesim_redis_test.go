//go:build integration

// This file is the Docker-backed regression for the smilesim completion
// subscription's cross-instance wire shape: internal/app/demo_notification.go's
// smilesim.EventSimulationCompleted subscription reads its payload through
// a probe (simulationCompletedFieldsFromPayload) rather than a naked type
// assertion, because pkgcore/eventbus/redis's EventBus always JSON
// round-trips a payload before a subscriber on a different bus instance
// sees it, and "a subscriber in the same process" is not the same thing
// as "the bus instance that published" -- a genuinely distributed
// deployment's normal shape, where every replica subscribes on its own
// instance and the bus fans a publish out to all of them (see the
// subscription's own probe and its doc comment).
//
// It lives in package referenceapp_test alongside
// redis_eventbus_composition_test.go (same build tag, same
// "go test -tags=integration ./..." invocation, no skip-on-missing-Docker
// fallback) and reuses that file's startRedisClient/moduleRoot/apiClient
// and this directory's own freePort/scrubbedEnviron/bootReplica/
// stopGracefully/replica helpers (distributed_mode_test.go) rather than
// duplicating them.
//
// # Why this test does not drive the real AI-image pipeline
//
// The positive, in-process proof that a REAL smile-simulation job's
// completion publishes smilesim.EventSimulationCompleted and reaches this
// exact subscription exists: flowtests/smilesim_flow_test.go's
// TestSmileSimulation_CompletionNotifiesTheNamedRecipient drives the whole
// pipeline (storage upload, go/ai-gateway's async image job against a
// scripted OpenAI-compatible endpoint, NotifyOnCompletion's publish) over
// the in-process EventBus and asserts the resulting SMS. That test cannot
// exercise the cross-instance shape this file targets, precisely because
// the in-process bus delivers the publisher's own Go value unchanged to a
// same-process subscriber -- there is no JSON round-trip to get wrong.
// Reproducing that same pipeline against a genuinely cross-instance
// subscriber would need a second full reference-app replica wired to the
// same fake AI-image vendor. Booting one at a fake vendor is possible --
// ConfigFromEnv reads APP_AI_GATEWAY_IMAGE_BASE_URL and
// APP_AI_GATEWAY_IMAGE_API_KEY into ServerConfig.AIGatewayImageBaseURL
// and AIGatewayImageAPIKey (those env vars' doc comment in
// internal/app/server.go) -- but such a replica's pipeline half would add
// no coverage this file needs: the smilesim journey that ends in this
// publish is already driven end to end in-process by
// flowtests/smilesim_flow_test.go, and the only thing a separate bus
// instance changes is the delivery, which is exactly the shape under
// test.
//
// So this test does the one thing that actually isolates the shape: it
// plays the role of "a completed simulation job" by
// publishing a REAL smilesim.SimulationCompletedPayload-shaped
// pkgcore.Event directly onto the shared Redis stream, from an
// independent *eventbusredis.EventBus instance this test owns -- exactly
// the shape internal/smilesim.Service.NotifyOnCompletion publishes, just
// from a bus instance that is not the subprocess's own, which is what
// makes the delivery genuinely cross-instance (pkgcore/eventbus/redis's
// deliverRemote path, JSON-decoded to map[string]any) rather than the
// local synchronous delivery flowtests/smilesim_flow_test.go's own in-process test
// exercises. The one real reference-app subprocess this test boots runs
// the actual, unmodified wireDemoNotification subscription -- the exact
// code under test -- and its own console SMS sender's stdout is the
// externally observable proof of whether the subscription actually
// dispatched.
package referenceapp_test

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
	"github.com/vislake/speed/go/pkgcore"
	eventbusredis "github.com/vislake/speed/go/pkgcore/eventbus/redis"
)

// The reference app's own constants, hardcoded here with a note that they
// must match internal/app/demo_notification.go's real declarations --
// mirroring this directory's own established convention
// (redis_eventbus_composition_test.go's acmeTenantID/demoOwnerEmail doc
// comment).
//
// smilesimDemoRecipientUserID is internal/app/demo_notification.go's
// DemoSmileSimRecipientUserID, and smilesimDemoRecipientPhone is that same
// id's entry in DemoUserAddresses -- a static table the resolver reads
// with no user account or membership needed, which is why this test never
// registers or signs in as anyone: RecipientClassUser dispatch only needs
// a recipient id the resolver recognizes.
const (
	smilesimDemoRecipientUserID = "user-smilesim-recipient-1"
	smilesimDemoRecipientPhone  = "+8613800138099"
	// smilesimSMSLinePrefix is go/notification's console SMS sender's own
	// wire format (go/notification/sms.go: "SMS to %s: %s\n"), which the
	// child's default stdout-backed sender (cfg.SMSOutput unset) writes to
	// its own process stdout -- the signal this test polls for.
	smilesimSMSLinePrefix = "SMS to " + smilesimDemoRecipientPhone + ":"
	// smilesimUnreadablePayloadLogSubstring is the constant warning message
	// wireDemoNotification's smilesim subscription logs for a payload
	// simulationCompletedFieldsFromPayload cannot read -- a
	// version-independent "the child's
	// consumer group for this event type is live and consuming" signal this
	// test's warm-up step polls for (mirroring
	// redis_eventbus_composition_test.go's own warmUp, adapted to a
	// subprocess's log text instead of an in-process recorder).
	smilesimUnreadablePayloadLogSubstring = "demo notification glue dropped a smilesim.simulation_completed event with an unreadable payload"
)

// warmUpChildSmileSimSubscription proves the child's consumer group for
// smilesim.EventSimulationCompleted exists and its reader goroutine is
// actually consuming, the same way redis_eventbus_composition_test.go's own
// warmUp does for its in-process observer: Subscribe starts the reader
// asynchronously (the group is created on a background goroutine, at the
// stream's live end), so publishing the real test event immediately after
// the child answers /healthz could race the group's creation and lose it
// to the no-catch-up contract every bus instance in this codebase shares.
// The marker payload -- a map missing both fields
// simulationCompletedFieldsFromPayload requires -- is rejected identically
// by the probe, so this
// warm-up's own pass/fail is not itself the shape under test; only the
// real event published afterward is.
func warmUpChildSmileSimSubscription(t *testing.T, publisher *eventbusredis.EventBus, childLogs func() string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for seq := 1; ; seq++ {
		if err := publisher.Publish(context.Background(), pkgcore.Event{
			Type:     smilesim.EventSimulationCompleted,
			TenantID: pkgcore.TenantID("warmup"),
			Payload:  map[string]any{"seq": seq},
		}); err != nil {
			t.Fatalf("warm-up publish %d: %v", seq, err)
		}
		if strings.Contains(childLogs(), smilesimUnreadablePayloadLogSubstring) {
			time.Sleep(600 * time.Millisecond) // one full read block: drain stragglers
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("warm-up: the child never logged the unreadable-payload warning within 5s\n%s", childLogs())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestServer_RealRedisEventBusComposition_SmileSimCompletionCrossesProcesses
// proves that the smilesim completion subscription survives
// a genuinely cross-instance delivery of smilesim.EventSimulationCompleted
// over a real Redis EventBus -- the composition
// TestSmileSimulation_CompletionNotifiesTheNamedRecipient's in-process,
// same-bus-instance test structurally cannot reach (see this file's own
// package doc comment).
//
// One real reference-app subprocess is booted in standalone deployment
// mode with APP_REDIS_ADDR set -- a real, MultiReplicaSafe-capable
// EventBus composed in a single-process topology, exactly
// redis_eventbus_composition_test.go's own composition (a single binary
// talking to real infrastructure is the ordinary shape of a
// small-customer production install). A second,
// independent *eventbusredis.EventBus instance -- this test's own
// "publisher", sharing the same Redis server but never the subprocess's
// bus instance -- publishes a real smilesim.SimulationCompletedPayload
// event. Because the publisher's instance id can never equal the child's,
// pkgcore/eventbus/redis's own delivery contract guarantees the child
// receives this event through deliverRemote's JSON-decoded map[string]any
// path, never the local, type-preserving path Publish's own synchronous
// handler loop takes for an event the SAME instance published.
func TestServer_RealRedisEventBusComposition_SmileSimCompletionCrossesProcesses(t *testing.T) {
	ctx := context.Background()

	client := startRedisClient(t, ctx)
	redisAddr := client.Options().Addr

	publisher := eventbusredis.NewEventBus(client)
	t.Cleanup(publisher.Close)

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "reference-app-server")
	buildOut, buildErr := new(bytes.Buffer), new(bytes.Buffer)
	build := exec.Command("go", "build", "-o", bin, "./cmd/server")
	build.Dir = moduleRoot(t)
	build.Stdout, build.Stderr = buildOut, buildErr
	if err := build.Run(); err != nil {
		t.Fatalf("go build ./cmd/server: %v\nstdout: %s\nstderr: %s", err, buildOut.String(), buildErr.String())
	}

	dbPath := filepath.Join(tmp, "reference-app.db")
	port := freePort(t)
	env := append(append([]string(nil), scrubbedEnviron()...),
		"APP_DEPLOYMENT_MODE=standalone",
		"PORT="+strconv.Itoa(port),
		"APP_DB_PATH="+dbPath,
		"APP_CONFIG_KEY=",
		"APP_REDIS_ADDR="+redisAddr,
	)
	child := bootReplica(t, bin, port, env)

	// Prove the child's own consumer group for this event type is live
	// before publishing the real event -- see warmUpChildSmileSimSubscription's
	// own doc comment for why this is not itself the regression under test.
	warmUpChildSmileSimSubscription(t, publisher, child.logs)

	// The real test event: a successful simulation completion naming the
	// demo recipient, published from a bus instance that is NOT the
	// child's own -- the exact cross-instance delivery under test. A
	// naked evt.Payload.(smilesim.SimulationCompletedPayload) type
	// assertion would fail against the JSON-decoded map[string]any this
	// publish produces on the child's side, logging only a warning and
	// never dispatching -- no SMS ever appears in the child's own stdout;
	// simulationCompletedFieldsFromPayload reads the decoded map
	// correctly and the subscription dispatches, exactly as it does for a
	// same-instance publish.
	if err := publisher.Publish(ctx, pkgcore.Event{
		Type:     smilesim.EventSimulationCompleted,
		TenantID: pkgcore.TenantID(acmeTenantID),
		Payload: smilesim.SimulationCompletedPayload{
			ImageJobID:      "job-redis-cross-instance-proof",
			TenantID:        string(acmeTenantID),
			RecipientUserID: smilesimDemoRecipientUserID,
			Succeeded:       true,
			OutputObjectID:  "obj-redis-cross-instance-proof",
		},
	}); err != nil {
		t.Fatalf("publish the real smilesim.EventSimulationCompleted event: %v", err)
	}

	eventually(t, "the demo.simulation_ready SMS to appear in the child's own stdout", func() bool {
		return strings.Contains(child.logs(), smilesimSMSLinePrefix)
	})
	if !strings.Contains(child.logs(), smilesimSMSLinePrefix) {
		t.Fatalf("child never logged an SMS to %s after the cross-instance completion event\n%s",
			smilesimDemoRecipientPhone, child.logs())
	}

	stopGracefully(t, child)
}
