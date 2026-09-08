package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/integration/api"
)

// fixedSubject is a SubjectResolver that answers the same way for every
// request, standing in for the authn-backed resolver a real host wires --
// the identical test double org's own handler_test.go uses for its
// structurally-identical seam.
type fixedSubject struct {
	userID string
	ok     bool
}

func (f fixedSubject) Subject(_ *http.Request) (string, bool) { return f.userID, f.ok }

// compile-time check that the test double satisfies the seam.
var _ SubjectResolver = fixedSubject{}

// newTestHandler builds a Handler over a freshly Registered-and-Attached
// Module, mirroring newTestRegistry's own "the same arrangement every other
// file in this package uses" convention. Unlike org's identical helper --
// which builds Handler directly from concrete services NewModule already
// built -- this one must go through Register and Attach first, since this
// module's own Service is built in Attach, not NewModule (see module.go's
// "Register-time wiring, Attach-time Service" doc comment): m.handler is
// nil, and every request would answer this module's own "ran before
// Attach" internal error, without it.
func newTestHandler(t *testing.T, subject SubjectResolver, opts ...Option) (*Handler, *Module) {
	t.Helper()
	allOpts := append([]Option{WithSubjectResolver(subject)}, opts...)
	m := NewModule(newTestDB(t), allOpts...)
	reg := newTestRegistry(t)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := m.Attach(reg); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return m.handler, m
}

// doRequest sends req through h and returns the recorded response. req's
// context is always overridden with ctx, so the tenant (or its deliberate
// absence) is explicit at every call site rather than hidden in a shared
// helper default -- the identical shape org's own doRequest test helper
// uses.
func doRequest(h *Handler, ctx context.Context, method, path string, body any) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// assertErrorCode fails t unless rec's body is an IntegrationError carrying
// code, at wantStatus.
func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, code string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, wantStatus, rec.Body.String())
	}
	var got api.IntegrationError
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response body %q: %v", rec.Body.String(), err)
	}
	if got.Code == nil {
		t.Fatalf("error code = <nil>, want %q", code)
	}
	if *got.Code != code {
		t.Fatalf("error code = %q, want %q", *got.Code, code)
	}
}

// assertAuditGapSuccess decodes rec's success body in ONE pass and requires
// it to answer wantStatus with NO error-envelope field (neither code nor
// params -- the invariant that a created credential never rides in an
// error envelope, because error responses flow into logs, tickets and bug
// reports that success bodies do not) and the response's own
// auditRecordMissing field true, returning the raw body so the caller can
// assert its credential material sits in its normal field.
func assertAuditGapSuccess(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) map[string]any {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, wantStatus, rec.Body.String())
	}
	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode response body %q: %v", rec.Body.String(), err)
	}
	for _, forbidden := range []string{"code", "params"} {
		if _, present := raw[forbidden]; present {
			t.Fatalf("the %d answer is an error envelope (carries %q): %v -- a created credential must never ride in an error envelope", wantStatus, forbidden, raw)
		}
	}
	if gap, _ := raw["auditRecordMissing"].(bool); !gap {
		t.Fatalf("auditRecordMissing = %v, want true (body %q)", raw["auditRecordMissing"], rec.Body.String())
	}
	return raw
}

// TestHandler_IntegrationCreateAPIKey_AuditFailure_AnswersCreatedWithAuditRecordMissing
// is the HTTP half of the audit-failure material-retention contract (the
// Service level is pinned in service_test.go's
// TestService_Create_AuditFailureAfterCommit_ReturnsKeyWithError): when the
// key row committed but its audit record failed, the handler answers the
// operation's ORDINARY success -- the key in its normal field, never in an
// error envelope's params -- plus the response's auditRecordMissing field
// true, so the caller persists the one-time key material AND knows the
// audit record is missing. Error responses flow into logs, tickets and bug
// reports, a distribution channel no credential may ride in, so moving the
// credential into an error envelope's params -- a 500 with the key in
// params.created_api_key.key -- is exactly the shape this test refuses:
// the refusal must leave the key in its normal field on a 201.
func TestHandler_IntegrationCreateAPIKey_AuditFailure_AnswersCreatedWithAuditRecordMissing(t *testing.T) {
	h, m := newTestHandler(t, fixedSubject{userID: "user-1", ok: true})
	m.service.bus = errBus{}

	rec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys", map[string]any{})
	created := assertAuditGapSuccess(t, rec, http.StatusCreated)
	if rawKey, _ := created["key"].(string); rawKey == "" {
		t.Fatalf("the success answer has no key material in its key field: %v", created)
	}
}

// TestHandler_IntegrationRotateAPIKey_PartialFailure_AnswersRotatedWithAuditRecordMissing
// is the HTTP half of Rotate's partial-failure contract: when a leg after
// the replacement key's creation fails -- the predecessor's revocation
// failing, or (deterministically here) the replacement's own audit record
// failing -- the handler answers the operation's ORDINARY success with the
// replacement key in its normal field (shown exactly once; a caller that
// does not receive it can never learn the credential of a live key this
// very call created) and auditRecordMissing true, so a caller never
// assumes the predecessor is revoked. Error responses flow into logs,
// tickets and bug reports, a distribution channel no credential may ride
// in, so a refusal shape that moved the replacement credential into an
// error envelope's params is exactly what this test refuses.
// (The revoke-leg partial itself is raced at the Service level in
// TestService_Rotate_RevokeFails_ReportsErrorWithNewKeyStillCreated; both
// partial legs reach this identical handler branch.)
func TestHandler_IntegrationRotateAPIKey_PartialFailure_AnswersRotatedWithAuditRecordMissing(t *testing.T) {
	h, m := newTestHandler(t, fixedSubject{userID: "user-1", ok: true})

	// A real key to rotate, created while the audit bus is healthy.
	createRec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys", map[string]any{})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %q", createRec.Code, createRec.Body.String())
	}
	var created api.IntegrationCreatedAPIKey
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	// Now the post-creation audit leg fails deterministically: Rotate's
	// internal Create commits the replacement and then fails its audit
	// record, so Rotate returns (replacement, error) -- exactly the shape
	// the revoke-leg partial also produces.
	m.service.bus = errBus{}
	rec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys/"+*created.ID+"/rotate", nil)
	createdKey := assertAuditGapSuccess(t, rec, http.StatusOK)
	if rawKey, _ := createdKey["key"].(string); rawKey == "" {
		t.Fatalf("the rotated answer has no key material in its key field: %v", createdKey)
	}
	if rawID, _ := createdKey["id"].(string); rawID == "" || rawID == *created.ID {
		t.Errorf("rotated id = %q, want a fresh id different from the predecessor %q", rawID, *created.ID)
	}
}

// TestHandler_IntegrationCreateWebhookSubscription_AuditFailure_AnswersCreatedWithAuditRecordMissing
// is the webhook twin of the create-key audit-gap surface: when the
// subscription row committed but its audit record failed, the handler
// answers the operation's ORDINARY success -- the raw signing secret in its
// normal field, never in an error envelope's params -- plus
// auditRecordMissing true. The secret is shown exactly once and never
// reproduced, so the caller persists it from the success response and knows
// the audit record is missing.
func TestHandler_IntegrationCreateWebhookSubscription_AuditFailure_AnswersCreatedWithAuditRecordMissing(t *testing.T) {
	// The webhook secret column needs its encrypting serializer registered
	// before any WebhookSubscription row is written (the plain newTestDB
	// that newTestHandler opens does not register it -- newWebhookTestDB
	// does, and this test reuses newTestHandler for its composed handler).
	cipher, err := dbkit.NewCipher(testWebhookCipherKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	dbkit.RegisterEncryptedSerializer(WebhookSecretSerializerName, cipher)

	h, m := newTestHandler(t, fixedSubject{userID: "user-1", ok: true},
		WithEventMapping(testMapping), WithWebhookURLValidator(alwaysAllowURL))
	m.service.bus = errBus{}

	rec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/webhooks", map[string]any{
		"url": "https://example.com/hook", "eventTypes": []string{"test.thing.happened"},
	})
	created := assertAuditGapSuccess(t, rec, http.StatusCreated)
	if secret, _ := created["secret"].(string); secret == "" {
		t.Fatalf("the success answer has no secret material in its secret field: %v", created)
	}
}

// lifecycleSecondMapping is a second EventMapping the lifecycle test below
// registers alongside testMapping (webhook_service_test.go) so its PATCH
// leg can replace a subscription's eventTypes with a genuinely different
// registered public type: testMapping alone declares one public type, so
// any replacement list would have to equal the stored one and the HTTP
// leg could never observe the replacement. The mapping's transform never
// runs in that test -- no delivery is driven there -- and mirrors
// testMapping's own shape.
var lifecycleSecondMapping = EventMapping{
	InternalType:  "test.second.happened",
	PublicType:    "test.second.happened",
	PublicVersion: "v1",
	Transform: func(_ context.Context, evt pkgcore.Event) (json.RawMessage, error) {
		return json.RawMessage(`{"seen":true}`), nil
	},
}

// TestHandler_WebhookSubscriptionLifecycle_OverHTTP drives the webhook half
// of this module's fragment through one subscription's full life over the
// wire: create (the 201 whose body is the one and only place the raw signing
// secret is shown), list (whose summaries must never re-expose it), a
// partial PATCH replacing URL and event types while leaving Active alone,
// the mark-delete (204), the restore (204) and the forced pause every
// restore lands even when the deleted subscription was active at the moment
// of deletion -- the property Service.RestoreWebhookSubscription's own doc
// comment argues about, here proven through the same route a host's HTTP
// client calls. The Service-level mechanics behind each leg are pinned in
// webhook_service_test.go; this test's own role is the fragment surface:
// the statuses and bodies the spec declares, answered by the composed
// handler exactly as a caller would see them.
func TestHandler_WebhookSubscriptionLifecycle_OverHTTP(t *testing.T) {
	// The webhook secret column needs its encrypting serializer registered
	// before any WebhookSubscription row is written -- newWebhookTestDB's
	// own requirement, repeated here because newTestHandler opens the plain
	// newTestDB (registration is a keyed no-op replacement, per that same
	// comment).
	cipher, err := dbkit.NewCipher(testWebhookCipherKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	dbkit.RegisterEncryptedSerializer(WebhookSecretSerializerName, cipher)

	h, _ := newTestHandler(t, fixedSubject{userID: "user-1", ok: true},
		WithEventMapping(testMapping, lifecycleSecondMapping), WithWebhookURLValidator(alwaysAllowURL))

	// create: 201 with the raw secret in its normal field.
	rec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/webhooks", map[string]any{
		"url": "https://example.com/hook", "eventTypes": []string{"test.thing.happened"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %q", rec.Code, rec.Body.String())
	}
	var created api.IntegrationCreatedWebhookSubscription
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == nil || *created.ID == "" {
		t.Fatal("create response has no id")
	}
	if created.Secret == nil || *created.Secret == "" {
		t.Fatal("create response has no secret -- the create body is the one place the raw signing secret is shown")
	}
	if created.Active == nil || !*created.Active {
		t.Fatal("a freshly created subscription must start active")
	}
	if created.CreatedBy == nil || *created.CreatedBy != "user-1" {
		t.Errorf("createdBy = %v, want %q (from SubjectResolver, never a request field)", created.CreatedBy, "user-1")
	}

	// list: 200 with exactly that one row, and no secret anywhere on the
	// wire -- IntegrationWebhookSubscriptionSummary has no Secret field at
	// all, and a generic map decode is what proves the bytes omit one.
	rec = doRequest(h, ctxFor(testTenant), http.MethodGet, "/api/v1/integration/webhooks", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %q", rec.Code, rec.Body.String())
	}
	var listRaw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&listRaw); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	listItems, _ := listRaw["webhookSubscriptions"].([]any)
	if len(listItems) != 1 {
		t.Fatalf("len(webhookSubscriptions) = %d, want 1 (body %q)", len(listItems), rec.Body.String())
	}
	row, _ := listItems[0].(map[string]any)
	if _, present := row["secret"]; present {
		t.Errorf("listed row carries %q, want it absent entirely: %v", "secret", row)
	}
	if row["url"] != "https://example.com/hook" || row["active"] != true || row["id"] != *created.ID {
		t.Errorf("listed row = %v, want the created subscription's id, url and active state", row)
	}

	// Partial PATCH: URL and eventTypes replaced -- the subscription moves
	// onto the second registered public type -- while Active, absent from
	// the request (nil means no change), stays true. The replaced
	// eventTypes list in the response is the HTTP leg's own proof that a
	// present eventTypes in a partial PATCH replaces the stored selection
	// rather than being ignored (the service-level pin lives in
	// webhook_service_test.go).
	rec = doRequest(h, ctxFor(testTenant), http.MethodPatch, "/api/v1/integration/webhooks/"+*created.ID, map[string]any{
		"url": "https://example.com/hook-v2", "eventTypes": []string{"test.second.happened"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body %q", rec.Code, rec.Body.String())
	}
	var updated map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if updated["url"] != "https://example.com/hook-v2" {
		t.Errorf("updated url = %v, want %q", updated["url"], "https://example.com/hook-v2")
	}
	updatedTypes, _ := updated["eventTypes"].([]any)
	if len(updatedTypes) != 1 || updatedTypes[0] != "test.second.happened" {
		t.Errorf("updated eventTypes = %v, want the replaced [test.second.happened] -- a present eventTypes in a partial PATCH must replace the stored selection", updated["eventTypes"])
	}
	if updated["active"] != true {
		t.Errorf("updated active = %v, want true -- a field absent from a partial PATCH must leave the stored value unchanged", updated["active"])
	}

	// delete while the subscription is ACTIVE (the url PATCH never paused
	// it): 204, and the list answers empty afterwards.
	rec = doRequest(h, ctxFor(testTenant), http.MethodDelete, "/api/v1/integration/webhooks/"+*created.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body %q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 response carries a body: %q", rec.Body.String())
	}
	rec = doRequest(h, ctxFor(testTenant), http.MethodGet, "/api/v1/integration/webhooks", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list-after-delete status = %d, body %q", rec.Code, rec.Body.String())
	}
	var emptyRaw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&emptyRaw); err != nil {
		t.Fatalf("decode list-after-delete response: %v", err)
	}
	if emptyItems, _ := emptyRaw["webhookSubscriptions"].([]any); len(emptyItems) != 0 {
		t.Fatalf("len(webhookSubscriptions) after delete = %d, want 0 (body %q)", len(emptyItems), rec.Body.String())
	}

	// restore: 204, and the subscription is listable again -- paused, never
	// silently resumed to the URL nobody has looked at since the deletion.
	rec = doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/webhooks/"+*created.ID+"/restore", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("restore status = %d, body %q", rec.Code, rec.Body.String())
	}
	rec = doRequest(h, ctxFor(testTenant), http.MethodGet, "/api/v1/integration/webhooks", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list-after-restore status = %d, body %q", rec.Code, rec.Body.String())
	}
	var restoredRaw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&restoredRaw); err != nil {
		t.Fatalf("decode list-after-restore response: %v", err)
	}
	restoredItems, _ := restoredRaw["webhookSubscriptions"].([]any)
	if len(restoredItems) != 1 {
		t.Fatalf("len(webhookSubscriptions) after restore = %d, want 1 (body %q)", len(restoredItems), rec.Body.String())
	}
	restored, _ := restoredItems[0].(map[string]any)
	if restored["url"] != "https://example.com/hook-v2" {
		t.Errorf("restored url = %v, want the pre-delete %q intact", restored["url"], "https://example.com/hook-v2")
	}
	if restored["active"] != false {
		t.Errorf("restored active = %v, want false -- a restore must land the subscription paused even though it was active at deletion", restored["active"])
	}
}

// TestHandler_WebhookBodies_EmptyAndMalformed_RefusedInvalidRequestBody pins
// the required-body half of the webhook surface: unlike apikeys' create
// (whose only optional body decodes through decodeOptionalJSON), the webhook
// request schemas declare every field required, so an empty body is a
// malformed request refused with the plain ErrInvalidRequestBody and a body
// that is not JSON at all is refused with the same code carrying its decode
// cause -- never a 5xx, and never a half-decoded request reaching Service.
// The identical guard on update's own body is exercised too.
func TestHandler_WebhookBodies_EmptyAndMalformed_RefusedInvalidRequestBody(t *testing.T) {
	cipher, err := dbkit.NewCipher(testWebhookCipherKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	dbkit.RegisterEncryptedSerializer(WebhookSecretSerializerName, cipher)
	h, _ := newTestHandler(t, fixedSubject{userID: "user-1", ok: true},
		WithEventMapping(testMapping), WithWebhookURLValidator(alwaysAllowURL))

	// Empty create body (io.EOF): refused before any Service call.
	rec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/webhooks", nil)
	assertErrorCode(t, rec, http.StatusBadRequest, ErrInvalidRequestBody.Code)

	// Create body that is not JSON: the same code, with its decode cause.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/integration/webhooks", bytes.NewBufferString("{not json")).
		WithContext(ctxFor(testTenant))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	assertErrorCode(t, rec2, http.StatusBadRequest, ErrInvalidRequestBody.Code)

	// Empty PATCH body: update's own required-body decode refuses before the
	// subscription id is even looked up (the id here does not exist, and the
	// answer must still be the body error, never a not-found).
	rec = doRequest(h, ctxFor(testTenant), http.MethodPatch, "/api/v1/integration/webhooks/no-such-id", nil)
	assertErrorCode(t, rec, http.StatusBadRequest, ErrInvalidRequestBody.Code)
}

// TestHandler_RequestWithoutTenantContext_RefusedInternalError proves the
// handler never guesses a tenant: a request whose context carries none is
// refused before any Service call with this module's coded internal error
// -- the same fail-closed answer tenancy.Middleware would have replaced
// with its own refusal long before this handler in a composed host (see
// mustTenant's doc comment for why that path is normally unreachable and
// still handled rather than assumed away).
func TestHandler_RequestWithoutTenantContext_RefusedInternalError(t *testing.T) {
	h, _ := newTestHandler(t, fixedSubject{userID: "user-1", ok: true})

	for _, path := range []string{
		"/api/v1/integration/apikeys",
		"/api/v1/integration/webhooks",
	} {
		rec := doRequest(h, context.Background(), http.MethodGet, path, nil)
		assertErrorCode(t, rec, http.StatusInternalServerError, ErrInternal.Code)
	}
}

func TestHandler_IntegrationCreateAPIKey_EmptyBody_IssuesScopelessKey(t *testing.T) {
	h, _ := newTestHandler(t, fixedSubject{userID: "user-1", ok: true})

	rec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	var got api.IntegrationCreatedAPIKey
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID == nil || *got.ID == "" {
		t.Error("id is empty")
	}
	if got.Key == nil || *got.Key == "" {
		t.Error("key is empty -- the raw value must be returned exactly once")
	}
	if got.CreatedBy == nil || *got.CreatedBy != "user-1" {
		t.Errorf("createdBy = %v, want %q (from SubjectResolver, never a request field)", got.CreatedBy, "user-1")
	}
}

func TestHandler_IntegrationCreateAPIKey_WithScopes_ValidatedAgainstLister(t *testing.T) {
	h, _ := newTestHandler(t, fixedSubject{userID: "user-1", ok: true}, WithPermissionLister(alwaysHeld("notes:read")))

	rec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys",
		api.IntegrationCreateAPIKeyRequest{Scopes: &[]string{"notes:read"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}

	rec = doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys",
		api.IntegrationCreateAPIKeyRequest{Scopes: &[]string{"notes:write"}})
	assertErrorCode(t, rec, http.StatusForbidden, ErrScopeNotHeldByCreator.Code)
}

func TestHandler_IntegrationCreateAPIKey_NoSubjectResolver_Unresolved(t *testing.T) {
	h, _ := newTestHandler(t, nil)

	rec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys", nil)
	assertErrorCode(t, rec, http.StatusUnauthorized, ErrSubjectUnresolved.Code)
}

func TestHandler_IntegrationCreateAPIKey_ResolverReportsNotOK_Unresolved(t *testing.T) {
	h, _ := newTestHandler(t, fixedSubject{ok: false})

	rec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys", nil)
	assertErrorCode(t, rec, http.StatusUnauthorized, ErrSubjectUnresolved.Code)
}

func TestHandler_IntegrationCreateAPIKey_MalformedBody_InvalidRequestBody(t *testing.T) {
	h, _ := newTestHandler(t, fixedSubject{userID: "user-1", ok: true})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/integration/apikeys", bytes.NewBufferString("{not json")).
		WithContext(ctxFor(testTenant))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertErrorCode(t, rec, http.StatusBadRequest, ErrInvalidRequestBody.Code)
}

func TestHandler_IntegrationListAPIKeys_NeverExposesRawKeyOrHash(t *testing.T) {
	h, _ := newTestHandler(t, fixedSubject{userID: "user-1", ok: true})
	doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys", nil)

	rec := doRequest(h, ctxFor(testTenant), http.MethodGet, "/api/v1/integration/apikeys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}

	// The response is decoded through the raw map, not
	// api.IntegrationListAPIKeysResponse/IntegrationAPIKeySummary: those
	// generated types have no Key or Hash field at all, so decoding
	// through them could never prove the wire body omits one -- only a
	// generic map decode can.
	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	items, _ := raw["apiKeys"].([]any)
	if len(items) != 1 {
		t.Fatalf("len(apiKeys) = %d, want 1 (body %q)", len(items), rec.Body.String())
	}
	row, _ := items[0].(map[string]any)
	for _, forbidden := range []string{"key", "hash"} {
		if _, present := row[forbidden]; present {
			t.Errorf("listed row carries %q, want it absent entirely: %v", forbidden, row)
		}
	}
	if _, present := row["prefix"]; !present {
		t.Error("listed row is missing prefix, the display-safe field List does expose")
	}
}

func TestHandler_IntegrationRotateAPIKey_NotFound(t *testing.T) {
	h, _ := newTestHandler(t, fixedSubject{userID: "user-1", ok: true})

	rec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys/no-such-id/rotate", nil)
	assertErrorCode(t, rec, http.StatusNotFound, ErrKeyNotFound.Code)
}

func TestHandler_IntegrationRotateAPIKey_Success_NewIDDiffersFromPredecessor(t *testing.T) {
	h, _ := newTestHandler(t, fixedSubject{userID: "user-1", ok: true})

	createRec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys", nil)
	var created api.IntegrationCreatedAPIKey
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	rotateRec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys/"+*created.ID+"/rotate", nil)
	if rotateRec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rotateRec.Code, rotateRec.Body.String())
	}
	var rotated api.IntegrationCreatedAPIKey
	if err := json.NewDecoder(rotateRec.Body).Decode(&rotated); err != nil {
		t.Fatalf("decode rotate response: %v", err)
	}
	if rotated.ID == nil || *rotated.ID == *created.ID {
		t.Errorf("rotated id = %v, want a value different from the predecessor %q", rotated.ID, *created.ID)
	}
	if rotated.Key == nil || *rotated.Key == *created.Key {
		t.Error("rotated key must be a fresh raw value, never the predecessor's")
	}

	// The predecessor is now revoked -- rotating it again reports the
	// already-revoked conflict rather than issuing a further replacement.
	rec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys/"+*created.ID+"/rotate", nil)
	assertErrorCode(t, rec, http.StatusConflict, ErrKeyAlreadyRevoked.Code)
}

func TestHandler_IntegrationRevokeAPIKey_Success_NoContent(t *testing.T) {
	h, _ := newTestHandler(t, fixedSubject{userID: "user-1", ok: true})

	createRec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys", nil)
	var created api.IntegrationCreatedAPIKey
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	rec := doRequest(h, ctxFor(testTenant), http.MethodDelete, "/api/v1/integration/apikeys/"+*created.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 response carries a body: %q", rec.Body.String())
	}
}

func TestHandler_IntegrationRevokeAPIKey_AlreadyRevoked_Conflict(t *testing.T) {
	h, _ := newTestHandler(t, fixedSubject{userID: "user-1", ok: true})

	createRec := doRequest(h, ctxFor(testTenant), http.MethodPost, "/api/v1/integration/apikeys", nil)
	var created api.IntegrationCreatedAPIKey
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	doRequest(h, ctxFor(testTenant), http.MethodDelete, "/api/v1/integration/apikeys/"+*created.ID, nil)

	rec := doRequest(h, ctxFor(testTenant), http.MethodDelete, "/api/v1/integration/apikeys/"+*created.ID, nil)
	assertErrorCode(t, rec, http.StatusConflict, ErrKeyAlreadyRevoked.Code)
}

func TestHandler_IntegrationRevokeAPIKey_NotFound(t *testing.T) {
	h, _ := newTestHandler(t, fixedSubject{userID: "user-1", ok: true})

	rec := doRequest(h, ctxFor(testTenant), http.MethodDelete, "/api/v1/integration/apikeys/no-such-id", nil)
	assertErrorCode(t, rec, http.StatusNotFound, ErrKeyNotFound.Code)
}

// TestHandler_ServedBeforeAttach_WritesInternalError proves handler.go's own
// forwarding-wrapper guard: a Handler built (as Register does) before
// Attach has produced a Service answers a coded internal error rather than
// panicking on a nil Service. Reaching this state through the exported API
// needs building the Handler directly, bypassing newTestHandler's own
// Register-then-Attach sequence.
func TestHandler_ServedBeforeAttach_WritesInternalError(t *testing.T) {
	m := &Module{}
	h := NewHandler(m, fixedSubject{userID: "user-1", ok: true})

	rec := doRequest(h, ctxFor(testTenant), http.MethodGet, "/api/v1/integration/apikeys", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
}

func TestHandler_ImplementsServerInterface(t *testing.T) {
	var _ api.ServerInterface = (*Handler)(nil)
}
