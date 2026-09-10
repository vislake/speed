package flowtests

// webhook_flow_test.go is go/integration's outbound-webhook DELIVERY
// surface mandatory-first-consumer proof: it drives a real webhook
// subscription through the composed HTTP stack -- the REAL
// org.member.joined domain event org's own invite/accept flow publishes
// (the identical flow org_flow_test.go itself drives),
// go/integration's real HMAC signing, and its real
// jobs.StandaloneQueue-backed delivery pipeline -- against two real,
// non-fake receiver processes this test controls.
//
// A subscription is created through the module's spec-generated
// webhook-subscription-CRUD surface (go/integration/api/openapi.yaml,
// mounted under /api/v1/integration): each test below subscribes through
// the integration_createWebhookSubscription operation -- the POST
// internal/app/webhooks.go's createWebhookSubscription helper issues, mounted through
// the same generic mountModuleRoutes loop every other module's fragment
// uses, gated by internal/app/demo_subject.go's guardIntegrationRoute. The whole CRUD
// + recent-deliveries surface, driven end to end, lives in
// webhook_crud_flow_test.go; this file's job is the DELIVERY side, which
// no CRUD surface exercises.
//
// # Why this needs cfg.WebhookURLValidator/cfg.WebhookHTTPClient at all
//
// go/integration's SSRF protection (go/integration/ssrf.go) genuinely
// refuses loopback, private, link-local and CGNAT addresses, both at
// subscription-creation time and again at every delivery attempt's dial
// time. Every receiver process this repository's test infrastructure can
// stand up offline is exactly one of those addresses: an httptest.Server
// listens on loopback, and a sibling Docker container reached the way this
// repository's other Docker-backed integration tiers reach theirs (a
// testcontainers-mapped port, or a container-to-container address on a
// Docker bridge network) is either loopback or RFC 1918 private space
// either way -- there is no address a receiver this test controls could
// bind to that the module's real, unmodified SSRF check would accept. The
// two ServerConfig fields are a deliberate, additive, test-only escape
// hatch for exactly this situation (see their own doc comments on
// ServerConfig, and integration.WithWebhookURLValidator's own doc comment
// in go/integration/module.go, for the full argument, including why this
// never weakens ANY other Module's or ANY production composition's SSRF
// enforcement): every other test in this package, and every production
// boot, leaves both fields at their nil zero value and gets go/integration's
// real, strict default behavior unchanged.
import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/examples/reference-app/internal/app"

	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/pkgcore"
)

// recordedWebhookDelivery is one HTTP delivery attempt webhookReceiver
// observed, carrying exactly what a real receiver would see: the raw body
// and the three signing headers webhook_signature.go stamps.
type recordedWebhookDelivery struct {
	body      []byte
	signature string
	timestamp string
	webhookID string
}

// webhookReceiver is a real HTTP server this test controls -- never a mock
// of go/integration's own delivery code, only of the THIRD PARTY a real
// tenant would point a subscription at. It answers every request with a
// fixed status code and records each one it receives, so a test can assert
// on exactly what was sent rather than merely that something arrived.
type webhookReceiver struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recordedWebhookDelivery
}

// newWebhookReceiver starts a receiver that answers every request with
// statusCode.
func newWebhookReceiver(t *testing.T, statusCode int) *webhookReceiver {
	t.Helper()
	r := &webhookReceiver{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.requests = append(r.requests, recordedWebhookDelivery{
			body:      body,
			signature: req.Header.Get(integration.HeaderWebhookSignature),
			timestamp: req.Header.Get(integration.HeaderWebhookTimestamp),
			webhookID: req.Header.Get(integration.HeaderWebhookID),
		})
		r.mu.Unlock()
		w.WriteHeader(statusCode)
	}))
	t.Cleanup(r.Close)
	return r
}

// count reports how many requests this receiver has recorded so far.
func (r *webhookReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// last returns the most recently recorded request, failing the test if none
// arrived yet.
func (r *webhookReceiver) last(t *testing.T) recordedWebhookDelivery {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		t.Fatal("webhookReceiver: no request was recorded")
	}
	return r.requests[len(r.requests)-1]
}

// all returns a snapshot of every request recorded so far.
func (r *webhookReceiver) all() []recordedWebhookDelivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedWebhookDelivery, len(r.requests))
	copy(out, r.requests)
	return out
}

// verifyWebhookSignature independently re-derives webhook_signature.go's
// HMAC-SHA256-over-"<timestamp>.<body>" scheme from secret and compares it
// to req's own signature header -- this test's own implementation, never a
// call into go/integration's unexported signing code, so a bug that broke
// signing without breaking this comparison's own logic would still be
// caught.
func verifyWebhookSignature(t *testing.T, req recordedWebhookDelivery, secret string) {
	t.Helper()
	if req.timestamp == "" {
		t.Fatal("recorded delivery carries no timestamp header")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(req.timestamp))
	mac.Write([]byte("."))
	mac.Write(req.body)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	if req.signature != want {
		t.Fatalf("signature = %q, want %q (independently re-derived from the subscription's own secret over timestamp %q and the exact recorded body)",
			req.signature, want, req.timestamp)
	}
	if req.webhookID == "" {
		t.Error("recorded delivery carries no webhook-id header")
	}
}

// webhookSubscriptionResponse mirrors the wire shape of the spec surface's
// create response (api.IntegrationCreatedWebhookSubscription, from
// api/openapi.yaml's IntegrationCreatedWebhookSubscription schema): every
// field of Service.CreateWebhookSubscription's result, camelCase on the
// wire exactly as the fragment declares it, decoded through json tags that
// bind this file's assertions to the actual response contract -- the same
// wire-shape discipline apikey_flow_test.go's own test structs follow, and
// like that file this one never imports go/integration/api either.
type webhookSubscriptionResponse struct {
	ID         string   `json:"id"`
	URL        string   `json:"url"`
	EventTypes []string `json:"eventTypes"`
	Secret     string   `json:"secret"`
	Active     bool     `json:"active"`
	CreatedBy  string   `json:"createdBy"`
	CreatedAt  string   `json:"createdAt"`
}

// webhookBasePath is the spec surface's webhook-subscription mount, the
// path createWebhookSubscription POSTs to.
const webhookBasePath = "/api/v1/integration/webhooks"

// buildWebhookFlowTestServer wires BuildServer's real output exactly like
// buildOrgTestServer (org_flow_test.go) -- a capturingMailer stands in for
// the console mailer so this file can recover an invitation's token the
// same way org_flow_test.go does -- plus the two webhook-only test
// overrides this file's own header comment explains. client is shared by
// every subscription this test creates; a plain client with no SSRF guard
// can reach any httptest.Server this test starts, receiver or otherwise.
func buildWebhookFlowTestServer(t *testing.T, client *http.Client) (*httptest.Server, app.ServerConfig, *capturingMailer) {
	t.Helper()

	cfg := testConfig(t)
	mailer := &capturingMailer{}
	cfg.Mailer = mailer
	cfg.WebhookURLValidator = func(context.Context, string) error { return nil }
	cfg.WebhookHTTPClient = client

	handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, cfg, mailer
}

// createWebhookSubscription drives the spec surface's create operation
// (POST /api/v1/integration/webhooks, webhookBasePath) as DemoOwnerUserID
// -- whose built-in owner role carries integration.PermissionWebhookManage,
// the permission internal/app/demo_subject.go's guardIntegrationRoute dispatches a
// non-GET request under /webhooks to (the router-level gate
// extended from method-only dispatch to sub-path dispatch precisely so the
// API-key and webhook permission pairs each gate their own half of the
// shared /api/v1/integration mount). The X-Demo-User-Id header names
// DemoNotesCreatorUserID, this app's one shared creator-attribution
// identity: integration_createWebhookSubscription attributes the new
// subscription's CreatedBy through integration.SubjectResolver
// (DemoOrgSubjectResolverFor in internal/app/server.go -- the identical seam instance
// integration_createAPIKey already reads), which reads that same
// header; a create without it is refused 401 integration.subject_unresolved,
// never attributed to a default user. token selects the tenant the
// subscription is created in, exactly like every other permission-gated
// route in this app (storage_flow_test.go's identical token+X-Demo-User
// pairing).
func createWebhookSubscription(t *testing.T, srv *httptest.Server, token, url string, eventTypes []string) webhookSubscriptionResponse {
	t.Helper()

	body, err := json.Marshal(map[string]any{"url": url, "eventTypes": eventTypes})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+webhookBasePath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(app.DemoUserHeader, app.DemoOwnerUserID)
	req.Header.Set(app.DemoOrgUserHeader, app.DemoNotesCreatorUserID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", webhookBasePath, err)
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("read response body: %v", readErr)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s status = %d, want %d; body = %s", webhookBasePath, resp.StatusCode, http.StatusCreated, respBody)
	}
	var out webhookSubscriptionResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("decode response %s: %v", respBody, err)
	}
	if out.Secret == "" {
		t.Fatal("created subscription carries no secret")
	}
	if out.CreatedBy != app.DemoNotesCreatorUserID {
		t.Fatalf("created.createdBy = %q, want %q (from integration.SubjectResolver, never a request field)", out.CreatedBy, app.DemoNotesCreatorUserID)
	}
	return out
}

// triggerOrgMemberJoined runs a real invite-then-accept cycle through the
// composed HTTP stack -- the exact flow org_flow_test.go's own
// TestOrgFlow_... test drives, reusing its helpers (orgRequest, orgNode,
// orgInvitation, orgMembership, tokenFromMail) -- and returns the resulting
// membership. This IS org's real org.member.joined event: MemberService's
// own Accept path (go/org/invite.go) publishes that event with exactly
// these three ids (MembershipID, UserID, NodeID) once the HTTP call below
// returns, which is what orgMemberJoinedWebhookMapping (internal/app/webhooks.go) maps
// onto the public payload a subscription receives.
func triggerOrgMemberJoined(t *testing.T, srv *httptest.Server, cfg app.ServerConfig, mailer *capturingMailer, tenant pkgcore.TenantID, namePrefix, inviteeEmail string) orgMembership {
	t.Helper()

	// The name prefix is display text ("Webhook Success") and not an email
	// local part, and the register route's canonical-form gate (dbkit's
	// NormalizeEmail) refuses an address whose local part holds a space -- so
	// the mailbox name is spelled without one, while the display names and
	// demo-user ids below keep the human-readable prefix.
	mailbox := strings.ReplaceAll(namePrefix, " ", "-")
	inviterToken := registerAndAuthenticate(t, srv, cfg, tenant, mailbox+"-inviter")
	inviteeToken := registerAndAuthenticate(t, srv, cfg, tenant, mailbox+"-invitee")

	var root orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", inviterToken, "",
		map[string]string{"name": namePrefix + " Group", "kind": "group"}, &root)

	var invitation orgInvitation
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations", inviterToken, "user-"+namePrefix+"-inviter",
		map[string]string{"email": inviteeEmail, "nodeId": root.ID}, &invitation)

	mail := mailer.last(t)
	token := tokenFromMail(t, mail)

	var membership orgMembership
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations/accept", inviteeToken, "user-"+namePrefix+"-invitee",
		map[string]string{"token": token}, &membership)
	return membership
}

// TestWebhookFlow_RealSignedDelivery_EndToEnd is the positive
// proof: a demo tenant subscribes to org.member.joined, a real invite is
// accepted, and a REAL, independently-verifiable HMAC-signed HTTP POST
// lands at a receiver this test controls -- carrying the real membership's
// own ids, wrapped in the versioned public envelope, with the internal
// InvitationID deliberately left out (the
// "only the deliberately chosen fields are exposed" rule).
func TestWebhookFlow_RealSignedDelivery_EndToEnd(t *testing.T) {
	receiver := newWebhookReceiver(t, http.StatusOK)
	client := &http.Client{Timeout: 15 * time.Second}
	srv, cfg, mailer := buildWebhookFlowTestServer(t, client)

	const tenant = pkgcore.TenantID("tenant-acme")
	ownerToken := registerAndAuthenticate(t, srv, cfg, tenant, "webhook-owner-ok")
	sub := createWebhookSubscription(t, srv, ownerToken, receiver.URL, []string{"org.member.joined"})

	membership := triggerOrgMemberJoined(t, srv, cfg, mailer, tenant, "Webhook Success", "webhook-member-ok@example.com")

	eventually(t, 15*time.Second, "the webhook delivery", func() bool {
		return receiver.count() >= 1
	})
	if got := receiver.count(); got != 1 {
		t.Fatalf("receiver recorded %d requests, want exactly 1 (no duplicate fan-out for one event)", got)
	}

	delivered := receiver.last(t)
	verifyWebhookSignature(t, delivered, sub.Secret)

	var envelope struct {
		Event struct {
			Type    string `json:"type"`
			Version string `json:"version"`
		} `json:"event"`
		Data struct {
			MembershipID string `json:"membership_id"`
			UserID       string `json:"user_id"`
			NodeID       string `json:"node_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(delivered.body, &envelope); err != nil {
		t.Fatalf("decode delivered body %s: %v", delivered.body, err)
	}
	if envelope.Event.Type != "org.member.joined" || envelope.Event.Version != "v1" {
		t.Fatalf("envelope.event = %+v, want {org.member.joined v1}", envelope.Event)
	}
	if envelope.Data.MembershipID != membership.MembershipID || envelope.Data.UserID != membership.UserID || envelope.Data.NodeID != membership.NodeID {
		t.Fatalf("delivered data = %+v, want the real membership %+v", envelope.Data, membership)
	}

	// invitation_id is deliberately left out of the public schema
	// (orgMemberJoinedWebhookMapping's own doc comment, internal/app/webhooks.go) -- a
	// second, map-shaped decode of the same "data" field is what proves its
	// absence, since the typed decode above would simply ignore an unwanted
	// key rather than reveal one.
	var rawEnvelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(delivered.body, &rawEnvelope); err != nil {
		t.Fatalf("decode delivered body %s as raw data map: %v", delivered.body, err)
	}
	if _, present := rawEnvelope.Data["invitation_id"]; present {
		t.Error("delivered data carries invitation_id, which orgMemberJoinedWebhookMapping's own doc comment says must be left out of the public schema")
	}
}

// TestWebhookFlow_ReceiverNonSuccess_RetriesThenDeadLetters is the
// negative proof: a receiver that always answers a non-2xx status makes
// go/integration's own real retry/dead-letter state machine run to
// exhaustion -- webhookMaxRetries (go/integration/webhook_delivery.go) is 6,
// so exactly 1 initial attempt plus 6 retries (7 total) are ever sent, and
// no 8th attempt follows once jobs' own bounded retry horizon is spent.
// Every attempt observed is independently re-verified as a genuinely signed
// delivery, not merely counted.
func TestWebhookFlow_ReceiverNonSuccess_RetriesThenDeadLetters(t *testing.T) {
	receiver := newWebhookReceiver(t, http.StatusInternalServerError)
	client := &http.Client{Timeout: 15 * time.Second}
	srv, cfg, mailer := buildWebhookFlowTestServer(t, client)

	const tenant = pkgcore.TenantID("tenant-globex")
	ownerToken := registerAndAuthenticate(t, srv, cfg, tenant, "webhook-owner-dl")
	sub := createWebhookSubscription(t, srv, ownerToken, receiver.URL, []string{"org.member.joined"})

	triggerOrgMemberJoined(t, srv, cfg, mailer, tenant, "Webhook DeadLetter", "webhook-member-dl@example.com")

	// The exponential backoff jobs.StandaloneQueue applies between retries
	// (1s base, doubling) means the 7th and final attempt lands roughly
	// 1+2+4+8+16+32 = 63s after the first -- this window is generous enough
	// to absorb a slow CI worker on top of that.
	const wantAttempts = 7
	eventually(t, 90*time.Second, "the retry horizon to exhaust (7 total attempts)", func() bool {
		return receiver.count() >= wantAttempts
	})

	// Every attempt -- not only the first -- must be genuinely, correctly
	// signed: a retry resends the SAME stored Payload (webhook_delivery.go's
	// own doc comment on WebhookDelivery.Payload explains why), so its
	// signature is recomputed fresh against a possibly later timestamp each
	// time, never reused verbatim.
	for _, req := range receiver.all() {
		verifyWebhookSignature(t, req, sub.Secret)
	}

	// Retries genuinely stop once the horizon is spent -- the queue must
	// dead-letter, never retry forever.
	never(t, 12*time.Second, "an attempt beyond the retry horizon", func() bool {
		return receiver.count() > wantAttempts
	})
}
