package flowtests

// smilesim_settlement_signal_flow_test.go drives the queue's terminal
// signal (jobs.job.terminal) end to end through the composed stack: a
// simulation's credit reservation settles and its completion notification
// goes out with NO call to the job-status route at all -- no client poll,
// and far inside the reconciliation sweep's five-minute default interval.
// The publisher is the real jobs.StandaloneQueue, built by BuildServer with
// jobs.WithEventBus(bus); the subscriber is the assembly-time subscription
// (internal/app/smilesim_terminal.go) around internal/smilesim's
// Service.OnJobTerminal. Each test polls only the OBSERVED SIDE EFFECT --
// the tenant's billing balance through a second database connection, and
// the console SMS the notification delivery renders -- never the job.
//
// Both tests are also the second leg's regression guard: the job-status
// route stays installed and still settles/notifies through the same latch,
// so TestSmileSimulation_CompletionNotifiesTheNamedRecipient and
// TestSmileSimulation_SufficientCredits_DebitsBalance (smilesim_flow_test.go,
// billing_credit_flow_test.go) keep exercising the poll-driven path
// unchanged.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

// TestSmileSimulation_TerminalSignalSettlesWithoutAnyPoll proves the
// settlement half: after the simulate request is accepted, this test never
// reads the job-status route, and the tenant's balance must still move from
// the reservation (Reserved) to the confirmed spend (Available debited by
// CreditsPerSimulation) on its own -- the terminal signal settling it the
// moment the job finishes.
func TestSmileSimulation_TerminalSignalSettlesWithoutAnyPoll(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildSmileSimTestServer(t, imgServer)

	const tenantID pkgcore.TenantID = "tenant-acme"
	token := registerAndAuthenticate(t, srv, cfg, tenantID, "smilesim-signal-owner")

	before := creditBalanceFor(t, cfg, tenantID)
	if before.Available < smilesim.CreditsPerSimulation {
		t.Fatalf("tenant-acme's balance before simulating = %+v, want Available >= %d (seedDemoCredits should have granted it at boot)", before, smilesim.CreditsPerSimulation)
	}

	photo := jpegWithExif(t)
	completedPhoto := uploadAndComplete(t, srv, token, photo, "")
	if completedPhoto.State != "completed" {
		t.Fatalf("photo state = %q, want completed", completedPhoto.State)
	}

	// No recipient: the settlement leg is the only thing this test
	// observes.
	simulateBody, err := json.Marshal(map[string]string{"photo_object_id": completedPhoto.ID})
	if err != nil {
		t.Fatalf("marshal simulate request: %v", err)
	}
	resp := smileSimRequest(t, srv, http.MethodPost, smileSimulatePath, token, simulateBody)
	var simulateOut struct {
		JobID string `json:"job_id"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&simulateOut); decodeErr != nil {
		resp.Body.Close()
		t.Fatalf("decode simulate response: %v", decodeErr)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST %s status = %d, want %d", smileSimulatePath, resp.StatusCode, http.StatusAccepted)
	}

	// From here on NOTHING reads the job -- the completion has to reach the
	// ledger through the queue's terminal signal alone.
	eventually(t, 5*time.Second, "the confirmed credit reservation with no job-status poll", func() bool {
		bal := creditBalanceFor(t, cfg, tenantID)
		return bal.Available == before.Available-smilesim.CreditsPerSimulation && bal.Reserved == before.Reserved
	})
}

// TestSmileSimulation_CompletionNotifiesWithoutAnyPoll proves the
// notification half: a simulate request naming a recipient gets that
// recipient's SMS with no job-status poll anywhere in the test, so the
// publish can only have been driven by the terminal signal. Exactly one
// SMS must land -- the shared latch still makes the notification at most
// once per job.
func TestSmileSimulation_CompletionNotifiesWithoutAnyPoll(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)

	// Built directly, rather than through buildSmileSimTestServer, so
	// cfg.SMSOutput can be set to a capturing buffer before the one
	// BuildServer call -- the console SMS sender this app wires reads
	// cfg.SMSOutput exactly once, at construction time.
	cfg := testConfig(t)
	cfg.AIGatewayImageBaseURL = imgServer.URL
	cfg.AIGatewayImageAPIKey = "sk-test-smilesim-signal-notify"
	sms := &lockedBuffer{}
	cfg.SMSOutput = sms
	// The named recipient must be an active member of the caller's tenant
	// -- the simulate surface's own recipient gate -- so the fixture is
	// granted membership in tenant-acme, the tenant this test's caller
	// signs into.
	cfg.Memberships.Grant(app.DemoSmileSimRecipientUserID, "tenant-acme")

	handler, cleanup, _, err := app.BuildServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "smilesim-signal-notify-owner")

	photo := jpegWithExif(t)
	completedPhoto := uploadAndComplete(t, srv, token, photo, "")
	if completedPhoto.State != "completed" {
		t.Fatalf("photo state = %q, want completed", completedPhoto.State)
	}

	simulateBody, err := json.Marshal(map[string]string{
		"photo_object_id":   completedPhoto.ID,
		"recipient_user_id": app.DemoSmileSimRecipientUserID,
	})
	if err != nil {
		t.Fatalf("marshal simulate request: %v", err)
	}
	resp := smileSimRequest(t, srv, http.MethodPost, smileSimulatePath, token, simulateBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST %s status = %d, want %d", smileSimulatePath, resp.StatusCode, http.StatusAccepted)
	}

	// No poll of the job-status route anywhere: the SMS below can only
	// have been triggered by the terminal signal's publish.
	eventually(t, 6*time.Second, "the simulation-ready SMS with no job-status poll", func() bool {
		return len(smsLinesTo(sms, "+8613800138099")) == 1
	})
}
