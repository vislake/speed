package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/integration/api"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// fakeQueue is a minimal jobs.Queue recording every Enqueue call, for tests
// that need to observe what handleDomainEvent submits without running a
// real jobs.StandaloneQueue worker pool.
type fakeQueue struct {
	tasks []jobs.Task
}

func (q *fakeQueue) Enqueue(_ context.Context, task jobs.Task, _ ...jobs.EnqueueOption) (jobs.JobID, error) {
	q.tasks = append(q.tasks, task)
	return jobs.JobID("fake-job"), nil
}

func (q *fakeQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) {
	return nil, errors.New("fakeQueue.Get is not implemented")
}

func (q *fakeQueue) Cancel(context.Context, jobs.JobID) error { return nil }

var _ jobs.Queue = (*fakeQueue)(nil)

// createTestSubscription creates one active webhook subscription for
// testTenant subscribed to testMapping.PublicType, returning the subscription
// id and its raw signing secret.
func createTestSubscription(t *testing.T, svc *Service, url string) (id, secret string) {
	t.Helper()
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL:        url,
		EventTypes: []string{testMapping.PublicType},
		CreatedBy:  "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	return created.ID, created.Secret
}

func TestService_handleDomainEvent_UnmappedType_Ignored(t *testing.T) {
	_, svc := newWebhookTestService(t)
	if err := svc.handleDomainEvent(ctxFor(testTenant), pkgcore.Event{Type: "no.such.mapping"}); err != nil {
		t.Errorf("handleDomainEvent = %v, want nil (unmapped types are silently ignored)", err)
	}
}

func TestService_handleDomainEvent_NoTenant_Skipped(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	createTestSubscription(t, svc, "https://example.com/hook")

	if err := svc.handleDomainEvent(context.Background(), pkgcore.Event{Type: testMapping.InternalType}); err != nil {
		t.Errorf("handleDomainEvent = %v, want nil", err)
	}
	if len(fq.tasks) != 0 {
		t.Errorf("len(fq.tasks) = %d, want 0 (no tenant, nothing to fan out to)", len(fq.tasks))
	}
}

func TestService_handleDomainEvent_NoMatchingSubscription_NoOp(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	// No subscription created at all.
	if err := svc.handleDomainEvent(ctxFor(testTenant), pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant}); err != nil {
		t.Errorf("handleDomainEvent = %v, want nil", err)
	}
	if len(fq.tasks) != 0 {
		t.Errorf("len(fq.tasks) = %d, want 0", len(fq.tasks))
	}
}

func TestService_handleDomainEvent_InactiveSubscription_NeverFannedOut(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	id, _ := createTestSubscription(t, svc, "https://example.com/hook")
	inactive := false
	if _, err := svc.UpdateWebhookSubscription(ctxFor(testTenant), UpdateWebhookSubscriptionInput{ID: id, Active: &inactive}); err != nil {
		t.Fatalf("UpdateWebhookSubscription: %v", err)
	}

	if err := svc.handleDomainEvent(ctxFor(testTenant), pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant}); err != nil {
		t.Errorf("handleDomainEvent = %v, want nil", err)
	}
	if len(fq.tasks) != 0 {
		t.Errorf("len(fq.tasks) = %d, want 0 (inactive subscription must never be fanned out to)", len(fq.tasks))
	}
}

func TestService_handleDomainEvent_CreatesDeliveryRowAndEnqueuesJob(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")

	if err := svc.handleDomainEvent(ctxFor(testTenant), pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant}); err != nil {
		t.Fatalf("handleDomainEvent: %v", err)
	}

	if len(fq.tasks) != 1 {
		t.Fatalf("len(fq.tasks) = %d, want 1", len(fq.tasks))
	}
	task := fq.tasks[0]
	if task.Type != jobTypeWebhookDeliver {
		t.Errorf("task.Type = %q, want %q", task.Type, jobTypeWebhookDeliver)
	}
	if string(task.TenantID) != string(testTenant) {
		t.Errorf("task.TenantID = %q, want %q", task.TenantID, testTenant)
	}
	if task.IdempotencyKey == "" {
		t.Error("task.IdempotencyKey is empty")
	}

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("len(deliveries) = %d, want 1", len(deliveries))
	}
	if deliveries[0].Status != DeliveryStatusPending {
		t.Errorf("Status = %q, want %q", deliveries[0].Status, DeliveryStatusPending)
	}
	if deliveries[0].EventType != testMapping.PublicType || deliveries[0].EventVersion != testMapping.PublicVersion {
		t.Errorf("EventType/EventVersion = %s/%s, want %s/%s",
			deliveries[0].EventType, deliveries[0].EventVersion, testMapping.PublicType, testMapping.PublicVersion)
	}
}

// TestService_handleDomainEvent_Redelivery_SameOccurrence_IsIdempotent
// proves the fan-out dedupe still merges what it can recognize as ONE
// occurrence: an at-least-once redelivered domain event observed at the
// SAME occurrence instant (the same Type, tenant, payload and
// subscription-boundary clock reading) creates exactly one WebhookDelivery
// row, never two. The clock is pinned deliberately: under a live clock two
// arrivals always read different instants, and a redelivery observed at a
// LATER instant is treated as a distinct occurrence -- see
// deriveWebhookDeliveryKey's doc comment for why the module can only merge
// what it can recognize, and TestService_handleDomainEvent_DistinctOccurrences_SameBody_TwoDeliveries
// for the other half of that contract.
func TestService_handleDomainEvent_Redelivery_SameOccurrence_IsIdempotent(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")
	svc.now = func() time.Time { return fixedNow }

	evt := pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant}
	if err := svc.handleDomainEvent(ctxFor(testTenant), evt); err != nil {
		t.Fatalf("handleDomainEvent (1): %v", err)
	}
	if err := svc.handleDomainEvent(ctxFor(testTenant), evt); err != nil {
		t.Fatalf("handleDomainEvent (2, redelivery): %v", err)
	}

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("len(deliveries) = %d after two same-instant identical events, want 1", len(deliveries))
	}
}

// TestService_handleDomainEvent_DistinctOccurrences_SameBody_TwoDeliveries
// is the regression test for the delivery-key occurrence marker: two
// genuinely distinct occurrences of one event type whose public bodies are
// byte-identical -- the same member removed and later re-added, when the
// mapping's payload names only the member; testMapping's own transform
// renders the same {"seen":true} body for every event -- must produce two
// deliveries. Before the fix the key was derived from the body alone, so
// the second occurrence probed the first's settled delivery row and was
// silently dropped: a delivery the receiver never got and this module
// never retried. Each call's subscription-boundary clock reading differs
// (the clock is advanced deterministically between the two calls), so each
// occurrence derives its own key and fans out on its own.
func TestService_handleDomainEvent_DistinctOccurrences_SameBody_TwoDeliveries(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")

	at := fixedNow
	svc.now = func() time.Time { return at }
	defer func() { svc.now = nil }()

	evt := pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant}
	if err := svc.handleDomainEvent(ctxFor(testTenant), evt); err != nil {
		t.Fatalf("handleDomainEvent (occurrence 1): %v", err)
	}
	at = at.Add(time.Minute) // a genuinely later occurrence of the same event
	if err := svc.handleDomainEvent(ctxFor(testTenant), evt); err != nil {
		t.Fatalf("handleDomainEvent (occurrence 2): %v", err)
	}

	if len(fq.tasks) != 2 {
		t.Errorf("len(fq.tasks) = %d after two distinct occurrences, want 2 (one delivery job per occurrence)", len(fq.tasks))
	}
	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("len(deliveries) = %d after two distinct occurrences of one body-identical event, want 2", len(deliveries))
	}
}

func TestService_handleDeliveryJob_Success_SignsAndDelivers(t *testing.T) {
	var gotBody []byte
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, svc := newWebhookTestService(t, WithWebhookHTTPClient(srv.Client()))
	subID, secret := createTestSubscription(t, svc, srv.URL)

	delivery := createPendingDelivery(t, svc, subID)

	result, err := svc.handleDeliveryJob(ctxFor(testTenant), deliveryJob(delivery.ID, subID))
	if err != nil {
		t.Fatalf("handleDeliveryJob: %v", err)
	}
	_ = result

	if gotBody == nil {
		t.Fatal("the webhook receiver never got a request")
	}
	if string(gotBody) != string(delivery.Payload) {
		t.Errorf("body = %s, want %s", gotBody, delivery.Payload)
	}
	if gotHeaders.Get(HeaderWebhookID) != delivery.ID {
		t.Errorf("%s = %q, want %q", HeaderWebhookID, gotHeaders.Get(HeaderWebhookID), delivery.ID)
	}
	ts, err := strconv.ParseInt(gotHeaders.Get(HeaderWebhookTimestamp), 10, 64)
	if err != nil {
		t.Fatalf("parse %s: %v", HeaderWebhookTimestamp, err)
	}
	wantSig := signWebhookPayload(secret, ts, gotBody)
	if gotHeaders.Get(HeaderWebhookSignature) != wantSig {
		t.Errorf("%s = %q, want %q", HeaderWebhookSignature, gotHeaders.Get(HeaderWebhookSignature), wantSig)
	}

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusDelivered {
		t.Fatalf("deliveries = %+v, want exactly one Delivered row", deliveries)
	}
	if deliveries[0].DeliveredAt == nil {
		t.Error("DeliveredAt is nil after a successful delivery")
	}
	if deliveries[0].LastStatusCode == nil || *deliveries[0].LastStatusCode != http.StatusOK {
		t.Errorf("LastStatusCode = %v, want 200", deliveries[0].LastStatusCode)
	}
}

func TestService_handleDeliveryJob_ReceiverError_MarksFailedAndRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	_, svc := newWebhookTestService(t, WithWebhookHTTPClient(srv.Client()))
	subID, _ := createTestSubscription(t, svc, srv.URL)
	delivery := createPendingDelivery(t, svc, subID)

	_, err := svc.handleDeliveryJob(ctxFor(testTenant), deliveryJob(delivery.ID, subID))
	if err == nil {
		t.Fatal("handleDeliveryJob = nil error, want a retryable failure for a 500 response")
	}

	deliveries, listErr := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if listErr != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", listErr)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusFailed {
		t.Fatalf("deliveries = %+v, want exactly one Failed row", deliveries)
	}
	if deliveries[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", deliveries[0].Attempts)
	}
	if deliveries[0].LastError == "" {
		t.Error("LastError is empty after a failed attempt")
	}
}

// TestHandler_IntegrationListWebhookDeliveries_BlockedDial_LastErrorNamesNoResolvedIP
// is the delivery-log half of the dial-time SSRF oracle fix (the creation-
// time half is pinned by ssrf_test.go's own asymmetry tests, and the
// dial-time refusal mechanism itself by
// TestNewSafeHTTPClient_RefusesLoopbackAtDialTime): when a delivery's
// dial-time re-check refuses a hostname that resolves to
// a blocked address, the refusal text persisted into the delivery row and
// served back to the tenant through the delivery-log API must name NO
// resolved address. The resolved internal IP is information the tenant
// does not have -- for a hostname resolvable only inside the platform's own
// network it is exactly the answer an internal-DNS reconnaissance oracle
// would give, the same disclosure errors.go's own ErrWebhookURLBlocked
// comment already rules out of the creation-time answer -- and the detail
// belongs in the server-side log, never in LastError. Before this P0 was
// closed the dial-time refusal text carried the resolved address shaped
// "host -> 10.x.y.z", and the delivery-log API returned it to the tenant on
// every read. (Failing before: LastError over the delivery-log API carries
// the resolved loopback address of "localhost"; passing after: it names no
// IP at all.)
func TestHandler_IntegrationListWebhookDeliveries_BlockedDial_LastErrorNamesNoResolvedIP(t *testing.T) {
	cipher, err := dbkit.NewCipher(testWebhookCipherKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	dbkit.RegisterEncryptedSerializer(WebhookSecretSerializerName, cipher)

	// A real composed handler over the default (guarded) webhook HTTP
	// client: WithWebhookURLValidator(alwaysAllowURL) lets the subscription
	// below be created for a hostname the production creation-time check
	// would refuse, so the DIAL-time re-check is the only gate left -- the
	// exact DNS-rebinding shape (a URL that validated at creation, whose
	// destination is refused when the delivery actually connects).
	h, m := newTestHandler(t, fixedSubject{userID: "user-1", ok: true},
		WithEventMapping(testMapping), WithWebhookURLValidator(alwaysAllowURL))

	// localhost resolves to a loopback address through the real resolver on
	// any standard system (hosts file, no network) -- the deterministic
	// stand-in for "internal-name -> 10.x.y.z". The port is never dialed:
	// every resolved candidate is blocked before any connection is made.
	rec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/webhooks", map[string]any{
		"url": "http://localhost:1/hook", "eventTypes": []string{"test.thing.happened"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %q", rec.Code, rec.Body.String())
	}
	var created api.IntegrationCreatedWebhookSubscription
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	// One real delivery attempt through the guarded transport: the dial is
	// refused as blocked, the row settles Failed with LastError.
	svc := m.service
	delivery := createPendingDelivery(t, svc, *created.ID)
	if _, err := svc.handleDeliveryJob(ctxFor(testTenant), deliveryJob(delivery.ID, *created.ID)); err == nil {
		t.Fatal("handleDeliveryJob = nil error, want the blocked-dial refusal to fail the attempt")
	}

	// Read the failure back through the delivery-log API -- the channel the
	// tenant admin actually sees.
	rec = doRequest(h, ctxFor(testTenant), http.MethodGet, "/api/v1/integration/webhooks/"+*created.ID+"/deliveries", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("deliveries status = %d, body %q", rec.Code, rec.Body.String())
	}
	var list api.IntegrationListWebhookDeliveriesResponse
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode deliveries response: %v", err)
	}
	if list.Deliveries == nil || len(*list.Deliveries) != 1 {
		t.Fatalf("deliveries = %+v, want exactly one row (body %q)", list.Deliveries, rec.Body.String())
	}
	row := (*list.Deliveries)[0]
	if row.LastError == nil {
		t.Fatal("LastError is nil over the delivery-log API after a failed attempt")
	}
	if !strings.Contains(*row.LastError, "blocked") {
		t.Fatalf("LastError = %q, want the blocked-destination refusal to be identifiable as such", *row.LastError)
	}
	for _, tok := range strings.Fields(*row.LastError) {
		if net.ParseIP(tok) != nil {
			t.Fatalf("LastError over the delivery-log API names the resolved address %q: %q -- an internal-DNS reconnaissance oracle for the tenant; the detail belongs in the server-side log, never in LastError", tok, *row.LastError)
		}
	}
}

// TestTruncateWebhookErrorText_MultibyteText_StaysValidUTF8AndCutsByRune is
// the regression test for the byte-truncation bug this fix closes:
// truncateWebhookErrorText used to cut with text[:webhookDeliveryErrorBudget],
// a BYTE cut that lands wherever byte 4000 of the text happens to fall --
// inside a multi-byte rune whenever the failure text carries one there.
// The stored result then held a split rune: invalid UTF-8 bytes that the
// last_error column (VARCHAR(4000) on both dialects) received. SQLite
// accepts and stores them (dirty data every later reader of the row --
// ListRecentWebhookDeliveries' JSON encoding included -- has to live with);
// PostgreSQL refuses the write outright (SQLSTATE 22021), so on the second
// dialect a receiver answering multi-byte text at the boundary permanently
// wedged the delivery record: every failure-path update of the row failed,
// and the retry horizon's dead-letter write failed the same way.
//
// The input below is 5000 three-byte runes: byte 4000 falls inside the
// 1334th one, so the pre-fix byte cut deterministically produces invalid
// UTF-8. The fix cuts by RUNE after sanitizing, mirroring go/sharing's
// truncateAccessLogValue (go/sharing/service.go) and go/authn's
// truncateClientField (go/authn/model.go): the result must be valid UTF-8
// and no longer than the budget in RUNES -- 5000 > 4000 runes, so the cut
// genuinely happens.
func TestTruncateWebhookErrorText_MultibyteText_StaysValidUTF8AndCutsByRune(t *testing.T) {
	text := strings.Repeat("界", 5000) // 5000 runes, 15000 bytes
	if len(text) <= webhookDeliveryErrorBudget {
		t.Fatal("test premise: the input must exceed the byte budget")
	}

	got := truncateWebhookErrorText(text)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateWebhookErrorText returned invalid UTF-8: %q (first 80 bytes: %q) -- a byte cut that splits a multi-byte rune must never reach the last_error column", got, got[:min(len(got), 80)])
	}
	if n := utf8.RuneCountInString(got); n > webhookDeliveryErrorBudget {
		t.Errorf("RuneCount = %d, want at most %d -- the cut must be by rune, not by byte", n, webhookDeliveryErrorBudget)
	}
}

// TestTruncateWebhookErrorText_InvalidUTF8WithinBudget_IsSanitized is the
// second half of the same regression: failure text WITHIN the budget but
// carrying invalid UTF-8 bytes used to pass through untouched (the pre-fix
// cut only ever fired past 4000 bytes), storing the invalid bytes verbatim.
// A receiver's error text is untrusted free-form bytes -- a receiver may
// answer its own body in any encoding, and attemptDelivery echoes the raw
// snippet into the failure text -- so the sanitization must happen at the
// write boundary for short values too, the same hazard go/sharing's
// truncateAccessLogValue handles for a caller-controlled User-Agent or
// Referer. Invalid bytes are rendered as the Unicode replacement character,
// never silently dropped (mirroring sharing's documented policy), and the
// result must be valid UTF-8.
func TestTruncateWebhookErrorText_InvalidUTF8WithinBudget_IsSanitized(t *testing.T) {
	dirty := "receiver said: \xff\xfe\x80 boom" + strings.Repeat("界", 10)
	if utf8.ValidString(dirty) {
		t.Fatal("test premise: the input must carry invalid UTF-8 bytes")
	}

	got := truncateWebhookErrorText(dirty)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateWebhookErrorText returned invalid UTF-8 for an invalid input within the budget: %q -- invalid bytes must be sanitized even when no cut happens", got)
	}
	if !strings.Contains(got, "�") {
		t.Errorf("result %q contains no replacement character -- invalid bytes must be rendered as U+FFFD, never silently dropped", got)
	}
}

// TestService_handleDeliveryJob_ReceiverError_MultibyteBody_StoresValidLastError
// is the end-to-end regression for the truncation fix: a receiver answering
// a multi-byte body that crosses the error-text budget mid-rune must leave
// the delivery row's LastError valid UTF-8 of at most the budget in runes,
// exactly like any other failure text. The body is crafted so byte 4000 of
// the recorded failure text falls inside a three-byte rune, and the
// receiver's body itself also ends mid-rune at the 4096-byte snippet
// budget -- both places the pre-fix byte handling could store a split
// rune. Pre-fix the row's LastError held invalid bytes (accepted silently
// by SQLite, refused with SQLSTATE 22021 by PostgreSQL -- see
// TestTruncateWebhookErrorText_MultibyteText_StaysValidUTF8AndCutsByRune's
// doc comment); post-fix the stored value is clean on both dialects.
func TestService_handleDeliveryJob_ReceiverError_MultibyteBody_StoresValidLastError(t *testing.T) {
	// The failure text is "integration: webhook receiver answered 500: "
	// followed by the body snippet. The padding puts byte 4000 of that text
	// inside the second byte of a three-byte rune, so the pre-fix byte cut
	// splits a rune deterministically.
	prefix := fmt.Sprintf("integration: webhook receiver answered %d: ", http.StatusInternalServerError)
	pad := (webhookDeliveryErrorBudget - len(prefix) - 2) % 3
	if pad < 0 {
		pad += 3
	}
	body := strings.Repeat("x", pad) + strings.Repeat("界", 1400) // 4200+ bytes, snippet reads 4096

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	_, svc := newWebhookTestService(t, WithWebhookHTTPClient(srv.Client()))
	subID, _ := createTestSubscription(t, svc, srv.URL)
	delivery := createPendingDelivery(t, svc, subID)

	if _, err := svc.handleDeliveryJob(ctxFor(testTenant), deliveryJob(delivery.ID, subID)); err == nil {
		t.Fatal("handleDeliveryJob = nil error, want a retryable failure for a 500 response")
	}

	deliveries, listErr := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if listErr != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", listErr)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusFailed {
		t.Fatalf("deliveries = %+v, want exactly one Failed row", deliveries)
	}
	if !utf8.ValidString(deliveries[0].LastError) {
		t.Fatalf("the failure-path update stored invalid UTF-8 in last_error: %q (first 80 bytes: %q) -- a multi-byte receiver body truncated at the budget must never wedge the delivery record", deliveries[0].LastError, deliveries[0].LastError[:min(len(deliveries[0].LastError), 80)])
	}
	if n := utf8.RuneCountInString(deliveries[0].LastError); n > webhookDeliveryErrorBudget {
		t.Errorf("LastError RuneCount = %d, want at most %d", n, webhookDeliveryErrorBudget)
	}
}

func TestService_handleDeliveryJob_SubscriptionDeleted_TerminatesWithoutRetry(t *testing.T) {
	_, svc := newWebhookTestService(t)
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")
	delivery := createPendingDelivery(t, svc, subID)

	if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), subID); err != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", err)
	}

	_, err := svc.handleDeliveryJob(ctxFor(testTenant), deliveryJob(delivery.ID, subID))
	if err != nil {
		t.Fatalf("handleDeliveryJob = %v, want nil (a deleted subscription is terminal, not retried)", err)
	}

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusDeadLetter {
		t.Fatalf("deliveries = %+v, want exactly one DeadLetter row", deliveries)
	}
}

// TestService_handleDeliveryJob_SubscriptionLoadFailure_RetriesNotDeadLetters
// is the regression test for the delivery error-classification split in
// handleDeliveryJob's subscription lookup: ONLY a genuine record-not-found
// (the subscription was deleted after this delivery was enqueued) settles
// the delivery terminal with the "was deleted" diagnostic; every OTHER
// FindByID failure -- a transient store error, a secret whose ciphertext no
// longer decrypts -- must be returned so jobs retries it, and must leave
// the delivery row pending rather than dead-lettered with a diagnostic
// text blaming the subscription. Before the fix, any FindByID error at all
// dead-lettered the delivery as "webhook subscription no longer exists",
// which both lost a recoverable delivery and misdiagnosed it. The
// non-not-found failure is injected by pointing the subscription repository
// at a database whose connection pool is closed: every query fails with a
// driver error, deterministically, while the delivery row lives on the
// service's own healthy database.
func TestService_handleDeliveryJob_SubscriptionLoadFailure_RetriesNotDeadLetters(t *testing.T) {
	_, svc := newWebhookTestService(t)
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")
	delivery := createPendingDelivery(t, svc, subID)

	closedDB, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     filepath.Join(t.TempDir(), "closed.sqlite"),
	})
	if err != nil {
		t.Fatalf("open a closed-database stand-in: %v", err)
	}
	rawDB, dbErr := closedDB.DB()
	if dbErr != nil {
		t.Fatalf("underlying *sql.DB: %v", dbErr)
	}
	if closeErr := rawDB.Close(); closeErr != nil {
		t.Fatalf("close the underlying *sql.DB: %v", closeErr)
	}

	broken := &Service{
		deliveryRepo: svc.deliveryRepo,
		webhookRepo:  NewWebhookSubscriptionRepository(closedDB),
	}

	if _, jobErr := broken.handleDeliveryJob(ctxFor(testTenant), deliveryJob(delivery.ID, subID)); jobErr == nil {
		t.Fatal("handleDeliveryJob = nil error, want a retryable error: a non-not-found subscription load failure must be retried, never settled terminal")
	}

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusPending {
		t.Fatalf("deliveries = %+v, want exactly one untouched Pending row (the delivery must not have been dead-lettered)", deliveries)
	}
}

// TestService_handleDeliveryJob_SubscriptionMarkDeleted_DeliveryRowUnaffected
// is this round's own proof for the mark-delete adoption: a WebhookDelivery
// enqueued before its subscription was mark-deleted settles exactly as
// TestService_handleDeliveryJob_SubscriptionDeleted_TerminatesWithoutRetry
// already proves for the identical scenario (that test is unaffected by
// this round precisely because it exercises the ordinary Service.
// DeleteWebhookSubscription call, which is now a mark-delete) -- and this
// test additionally reaches under the Service to confirm WHY: the
// subscription row still physically exists, mark-deleted, and the delivery
// row -- an id reference only, per this module's own no-cross-table-FK
// discipline (webhook_model.go's WebhookDelivery.SubscriptionID doc
// comment) -- is completely untouched by the subscription's own
// soft-delete columns, since WebhookDelivery does not implement
// dbkit.SoftDeletable at all.
func TestService_handleDeliveryJob_SubscriptionMarkDeleted_DeliveryRowUnaffected(t *testing.T) {
	m, svc := newWebhookTestService(t)
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")
	delivery := createPendingDelivery(t, svc, subID)

	if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), subID); err != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", err)
	}

	// The subscription row is mark-deleted, not gone.
	var subCount int64
	if err := dbkit.WithTenantSession(ctxFor(testTenant), m.db, func(tx *gorm.DB) error {
		return tx.Unscoped().Model(&WebhookSubscription{}).
			Where("id = ? AND deleted_at IS NOT NULL", subID).
			Count(&subCount).Error
	}); err != nil {
		t.Fatalf("counting the mark-deleted subscription row: %v", err)
	}
	if subCount != 1 {
		t.Fatalf("mark-deleted subscription row count = %d, want exactly 1", subCount)
	}

	// The delivery row is untouched: no deleted_at/deleted_by columns exist
	// on WebhookDelivery at all, and its own fields are exactly what
	// createPendingDelivery produced.
	var deliveryRow WebhookDelivery
	if err := dbkit.WithTenantSession(ctxFor(testTenant), m.db, func(tx *gorm.DB) error {
		return tx.Unscoped().Where("id = ?", delivery.ID).First(&deliveryRow).Error
	}); err != nil {
		t.Fatalf("reading the delivery row back: %v", err)
	}
	if deliveryRow.Status != DeliveryStatusPending {
		t.Errorf("delivery Status = %q, want unchanged %q", deliveryRow.Status, DeliveryStatusPending)
	}
	if deliveryRow.SubscriptionID != subID {
		t.Errorf("delivery SubscriptionID = %q, want unchanged %q", deliveryRow.SubscriptionID, subID)
	}

	// The already-enqueued job still settles terminal without retrying,
	// exactly as it did before this round -- handleDeliveryJob's own
	// FindByID lookup on the now mark-deleted subscription is hidden from
	// it by dbkit's soft-delete auto-scope plugin exactly as a physical
	// DELETE always hid it before.
	if _, err := svc.handleDeliveryJob(ctxFor(testTenant), deliveryJob(delivery.ID, subID)); err != nil {
		t.Fatalf("handleDeliveryJob = %v, want nil (a mark-deleted subscription is terminal, not retried)", err)
	}
	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusDeadLetter {
		t.Fatalf("deliveries = %+v, want exactly one DeadLetter row", deliveries)
	}
}

func TestService_handleDeliveryJob_SubscriptionInactive_TerminatesWithoutRetry(t *testing.T) {
	_, svc := newWebhookTestService(t)
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")
	delivery := createPendingDelivery(t, svc, subID)

	inactive := false
	if _, err := svc.UpdateWebhookSubscription(ctxFor(testTenant), UpdateWebhookSubscriptionInput{ID: subID, Active: &inactive}); err != nil {
		t.Fatalf("UpdateWebhookSubscription: %v", err)
	}

	_, err := svc.handleDeliveryJob(ctxFor(testTenant), deliveryJob(delivery.ID, subID))
	if err != nil {
		t.Fatalf("handleDeliveryJob = %v, want nil (a paused subscription is terminal, not retried)", err)
	}

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusDeadLetter {
		t.Fatalf("deliveries = %+v, want exactly one DeadLetter row", deliveries)
	}
}

func TestService_handleDeliveryJob_AlreadyDelivered_NoOp(t *testing.T) {
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, svc := newWebhookTestService(t, WithWebhookHTTPClient(srv.Client()))
	subID, _ := createTestSubscription(t, svc, srv.URL)
	delivery := createPendingDelivery(t, svc, subID)

	job := deliveryJob(delivery.ID, subID)
	if _, err := svc.handleDeliveryJob(ctxFor(testTenant), job); err != nil {
		t.Fatalf("first handleDeliveryJob: %v", err)
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("requestCount after the first attempt = %d, want 1", got)
	}

	// A retried job for an already-succeeded delivery must not re-send.
	if _, err := svc.handleDeliveryJob(ctxFor(testTenant), job); err != nil {
		t.Fatalf("second handleDeliveryJob: %v", err)
	}
	if got := requestCount.Load(); got != 1 {
		t.Errorf("requestCount after the second (replayed) attempt = %d, want still 1 -- an already-delivered delivery must not be re-sent", got)
	}
}

func TestService_onWebhookDeliveryDeadLetter_MarksDeadLetter(t *testing.T) {
	_, svc := newWebhookTestService(t)
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")
	delivery := createPendingDelivery(t, svc, subID)

	svc.onWebhookDeliveryDeadLetter(ctxFor(testTenant), deliveryJob(delivery.ID, subID), errors.New("exhausted"))

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusDeadLetter {
		t.Fatalf("deliveries = %+v, want exactly one DeadLetter row", deliveries)
	}
	if deliveries[0].LastError != "exhausted" {
		t.Errorf("LastError = %q, want %q", deliveries[0].LastError, "exhausted")
	}
}

func TestDeriveWebhookDeliveryKey_DeterministicAndDistinct(t *testing.T) {
	body := []byte(`{"a":1}`)
	occ := fixedNow
	k1 := deriveWebhookDeliveryKey("sub-1", "type.a", "v1", body, occ)
	k2 := deriveWebhookDeliveryKey("sub-1", "type.a", "v1", body, occ)
	if k1 != k2 {
		t.Error("deriveWebhookDeliveryKey is not deterministic for identical inputs")
	}
	if k3 := deriveWebhookDeliveryKey("sub-2", "type.a", "v1", body, occ); k3 == k1 {
		t.Error("a different subscription id produced the same key")
	}
	if k4 := deriveWebhookDeliveryKey("sub-1", "type.b", "v1", body, occ); k4 == k1 {
		t.Error("a different public type produced the same key")
	}
	if k5 := deriveWebhookDeliveryKey("sub-1", "type.a", "v1", body, occ.Add(time.Second)); k5 == k1 {
		t.Error("a different occurrence marker produced the same key (two distinct occurrences of a body-identical event must not collide)")
	}
}

// TestDefaultWebhookHTTPClient_SharedOnceBuiltWithSaneIdleTimeout pins the
// module-level delivery transport's shape: a Service built with no
// WithWebhookHTTPClient override delivers through a NON-nil client whose
// transport is the shared, once-built default (two separately Attach-ed
// Services hold the same client instance) carrying a sane, finite
// IdleConnTimeout -- so consecutive deliveries to one receiver reuse a
// connection instead of every attempt dialing its own fresh transport.
func TestDefaultWebhookHTTPClient_SharedOnceBuiltWithSaneIdleTimeout(t *testing.T) {
	_, svc1 := newWebhookTestService(t)
	if svc1.httpClient == nil {
		t.Fatal("a Service with no WithWebhookHTTPClient override has a nil httpClient -- delivery would build a fresh transport per attempt")
	}
	_, svc2 := newWebhookTestService(t)
	if svc2.httpClient != svc1.httpClient {
		t.Error("two Services do not share the same default delivery client (the transport is rebuilt per Service, not once)")
	}

	transport, ok := svc1.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default client transport = %T, want *http.Transport", svc1.httpClient.Transport)
	}
	if transport.IdleConnTimeout <= 0 {
		t.Errorf("IdleConnTimeout = %v, want a sane positive value (0 keeps idle connections open forever)", transport.IdleConnTimeout)
	}
	if transport.IdleConnTimeout > time.Hour {
		t.Errorf("IdleConnTimeout = %v, want a sane finite value", transport.IdleConnTimeout)
	}
	if transport.MaxIdleConnsPerHost <= 0 {
		t.Errorf("MaxIdleConnsPerHost = %d, want a positive value", transport.MaxIdleConnsPerHost)
	}
}

// createPendingDelivery enqueues one real delivery for subID by publishing
// testMapping's InternalType through handleDomainEvent (over a discarding
// fake queue, so the row is created without a real job being submitted),
// and returns the created row read back from the repository.
func createPendingDelivery(t *testing.T, svc *Service, subID string) WebhookDelivery {
	t.Helper()
	prevQueue := svc.queue
	svc.queue = &fakeQueue{}
	defer func() { svc.queue = prevQueue }()

	if err := svc.handleDomainEvent(ctxFor(testTenant), pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant}); err != nil {
		t.Fatalf("handleDomainEvent: %v", err)
	}
	rows, err := svc.deliveryRepo.ListRecentBySubscription(ctxFor(testTenant), subID, 1)
	if err != nil {
		t.Fatalf("ListRecentBySubscription: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	return rows[0]
}

// deliveryJob builds the jobs.Job handleDeliveryJob and
// onWebhookDeliveryDeadLetter expect: TenantID set (mirroring what a real
// worker rebuilds before calling Handle) and Payload the encoded
// webhookDeliveryJobPayload.
func deliveryJob(deliveryID, subscriptionID string) *jobs.Job {
	payload, _ := json.Marshal(webhookDeliveryJobPayload{SubscriptionID: subscriptionID, DeliveryID: deliveryID})
	return &jobs.Job{
		ID:       "job-1",
		Type:     jobTypeWebhookDeliver,
		TenantID: testTenant,
		Payload:  payload,
	}
}
