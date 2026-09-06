package main

// webhook_crud_flow_test.go is go/integration's round-7
// webhook-subscription-CRUD surface mandatory-first-consumer proof
// (go/integration/AGENTS.md's round-7 record names this the compensating
// obligation the round carried until a real host wired it end to end). It
// drives the module's own spec-generated HTTP surface --
// go/integration/api/openapi.yaml's seven operations under
// /api/v1/integration/webhooks, mounted through server.go's integrationModule
// wiring and the same generic mountModuleRoutes loop every other module's
// fragment uses, gated by demo_subject.go's guardIntegrationRoute with its
// round-7 sub-path dispatch choosing integration:webhook:read for reads and
// integration:webhook:manage for everything else -- through the composed
// HTTP stack: create (capturing the raw signing secret, shown exactly once),
// list (secret-free, verified at the raw-JSON level), update (a partial
// PATCH: pause with active=false, resume with active=true, a present-but-
// empty eventTypes refused), delete (the mark-delete round 3 shipped, hidden
// from every read), restore (always landing paused) and the recent-deliveries
// listing against a REAL delivery made through round 2's pipeline
// (webhook_flow_test.go's own receiver rig).
//
// It follows storage_flow_test.go's and org_flow_test.go's wire-shape
// discipline: responses are decoded into structs that mirror the JSON on
// the wire -- camelCase field names, exactly as api/openapi.yaml declares
// them -- never into go/integration/api's spec-generated types, so the
// assertions bind to the actual response contract rather than to the
// generator's Go shapes, and this file never imports go/integration itself.
// The create-operation wire struct (webhookSubscriptionResponse), the
// subscription-creation helper and the delivery-triggering rig all live in
// webhook_flow_test.go -- this file reuses createWebhookSubscription,
// buildWebhookFlowTestServer, newWebhookReceiver and triggerOrgMemberJoined
// rather than duplicating any of them, and adds only the CRUD-specific
// request/assert/decode helpers below.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// webhookSubscriptionListRow mirrors one row of the spec surface's list
// response (api.IntegrationWebhookSubscriptionSummary from
// api/openapi.yaml's IntegrationWebhookSubscriptionSummary schema): every
// field of the stored subscription except the signing secret. It
// deliberately has no Secret field at all -- decoding through a struct that
// does not declare it could never prove its absence on the wire, which is
// why the lifecycle test below additionally decodes the list response
// through a raw map for that one assertion.
type webhookSubscriptionListRow struct {
	ID         string   `json:"id"`
	URL        string   `json:"url"`
	EventTypes []string `json:"eventTypes"`
	Active     bool     `json:"active"`
	CreatedBy  string   `json:"createdBy"`
	CreatedAt  string   `json:"createdAt"`
	UpdatedAt  string   `json:"updatedAt"`
}

// webhookSubscriptionListResponse is the wire shape of the spec surface's
// list response envelope.
type webhookSubscriptionListResponse struct {
	WebhookSubscriptions []webhookSubscriptionListRow `json:"webhookSubscriptions"`
}

// webhookDeliveryRow mirrors one row of the spec surface's deliveries
// listing (api.IntegrationWebhookDeliverySummary): every field of the
// stored delivery except the raw payload bytes, which are an implementation
// detail of the send pipeline. The three nullable timestamps and the
// nullable lastStatusCode decode as pointers, exactly as they serialize.
type webhookDeliveryRow struct {
	ID             string  `json:"id"`
	SubscriptionID string  `json:"subscriptionId"`
	EventType      string  `json:"eventType"`
	EventVersion   string  `json:"eventVersion"`
	Status         string  `json:"status"`
	Attempts       int     `json:"attempts"`
	LastStatusCode *int    `json:"lastStatusCode"`
	LastError      string  `json:"lastError"`
	LastAttemptAt  *string `json:"lastAttemptAt"`
	DeliveredAt    *string `json:"deliveredAt"`
	CreatedAt      string  `json:"createdAt"`
}

// webhookDeliveriesResponse is the wire shape of the spec surface's
// deliveries listing envelope.
type webhookDeliveriesResponse struct {
	Deliveries []webhookDeliveryRow `json:"deliveries"`
}

// webhookCRUDRequest issues method against path (already the full
// "/api/v1/integration/webhooks..." route) on srv as the acting user, in the
// tenant the given bearer token resolves -- the identical shape
// apikeyRequest, storageRequest and notesRequestAs all use. A non-empty user
// additionally sends X-Demo-User-Id (demoNotesCreatorUserID, this app's one
// shared creator-attribution identity): integration_createWebhookSubscription
// attributes the new subscription's CreatedBy through
// integration.SubjectResolver (demoOrgSubjectResolver in server.go), which
// reads that same header -- an empty user sends neither demo header, the
// no-identity-at-all shape the permission-gate test below drives. body, when
// non-nil, is sent as the JSON request body with the application/json
// content type the spec-generated request schemas require.
func webhookCRUDRequest(t *testing.T, srv *httptest.Server, method, path, token, user string, body []byte) *http.Response {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if user != "" {
		req.Header.Set(demoUserHeader, user)
		req.Header.Set(demoOrgUserHeader, demoNotesCreatorUserID)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s (user=%q): %v", method, path, user, err)
	}
	return resp
}

// assertWebhookError reads resp and requires it to be the webhook surface's
// structured error: status wantStatus and the envelope's code exactly
// wantCode -- the identical shape assertAPIKeyError checks against the
// API-key surface's own structured errors, since both surfaces share the
// spec-generated {code, params} envelope.
func assertWebhookError(t *testing.T, resp *http.Response, wantStatus int, wantCode, what string) {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, wantStatus, body)
	}
	var decoded struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	if decoded.Code != wantCode {
		t.Fatalf("%s: error code = %q, want %q; body = %s", what, decoded.Code, wantCode, body)
	}
}

// decodeWebhookSubscriptionList reads resp, requires its status to be
// wantStatus, and decodes its body as a webhookSubscriptionListResponse
// wire shape.
func decodeWebhookSubscriptionList(t *testing.T, resp *http.Response, wantStatus int, what string) webhookSubscriptionListResponse {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, wantStatus, body)
	}
	var out webhookSubscriptionListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	return out
}

// decodeWebhookSubscriptionCreated reads resp, requires its status to be
// wantStatus, and decodes its body as the spec surface's create-response
// wire shape -- the one response carrying the raw signing secret
// (webhook_flow_test.go's webhookSubscriptionResponse, the shape
// createWebhookSubscription itself decodes).
func decodeWebhookSubscriptionCreated(t *testing.T, resp *http.Response, wantStatus int, what string) webhookSubscriptionResponse {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, wantStatus, body)
	}
	var out webhookSubscriptionResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	return out
}

// decodeWebhookSubscriptionUpdate reads resp, requires its status to be
// wantStatus, and decodes its body as the spec surface's update-response
// wire shape: the full summary (id, url, eventTypes, active, createdBy,
// createdAt, updatedAt), whose fields are exactly
// webhookSubscriptionListRow's -- the summary schema the list and the
// update operations share.
func decodeWebhookSubscriptionUpdate(t *testing.T, resp *http.Response, wantStatus int, what string) webhookSubscriptionListRow {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, wantStatus, body)
	}
	var out webhookSubscriptionListRow
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	return out
}

// decodeWebhookDeliveries reads resp, requires its status to be wantStatus,
// and decodes its body as a webhookDeliveriesResponse wire shape.
func decodeWebhookDeliveries(t *testing.T, resp *http.Response, wantStatus int, what string) webhookDeliveriesResponse {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, wantStatus, body)
	}
	var out webhookDeliveriesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	return out
}

// TestBuildServer_WebhookPermissionGate_EnforcesTheWebhookPermissions is
// this surface's mirror of apikey_flow_test.go's own permission-gate test:
// the read-only demo user holds notes:read and nothing else -- deliberately
// no integration:webhook:* permission at all -- so both the read and manage
// directions of guardIntegrationRoute's sub-path dispatch must refuse it,
// across every operation class of the fragment: list and the deliveries
// listing (dispatched to integration:webhook:read) and create, update,
// delete and restore (dispatched to integration:webhook:manage). This is
// also this round's own proof that the router-level gate genuinely evaluates
// a real permission per sub-path rather than method alone: a router bug that
// gated every /webhooks request on the API-key pair -- or passed everything
// through -- would make this test the one that fails.
func TestBuildServer_WebhookPermissionGate_EnforcesTheWebhookPermissions(t *testing.T) {
	client := &http.Client{Timeout: 15 * time.Second}
	srv, cfg, _ := buildWebhookFlowTestServer(t, client)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "webhook-gate")

	// The reader may neither read nor manage: both directions of the
	// integration:webhook:read / integration:webhook:manage gate are closed,
	// on every operation class the fragment ships.
	assertWebhookError(t,
		webhookCRUDRequest(t, srv, http.MethodGet, webhookBasePath, acmeToken, demoReaderUserID, nil),
		http.StatusForbidden, "rbac.permission_denied", "reader list")
	assertWebhookError(t,
		webhookCRUDRequest(t, srv, http.MethodGet, webhookBasePath+"/no-such-id/deliveries", acmeToken, demoReaderUserID, nil),
		http.StatusForbidden, "rbac.permission_denied", "reader deliveries listing")
	assertWebhookError(t,
		webhookCRUDRequest(t, srv, http.MethodPost, webhookBasePath, acmeToken, demoReaderUserID,
			[]byte(`{"url":"https://hooks.example.test/gate","eventTypes":["org.member.joined"]}`)),
		http.StatusForbidden, "rbac.permission_denied", "reader create")
	assertWebhookError(t,
		webhookCRUDRequest(t, srv, http.MethodPatch, webhookBasePath+"/no-such-id", acmeToken, demoReaderUserID,
			[]byte(`{"active":false}`)),
		http.StatusForbidden, "rbac.permission_denied", "reader update")
	assertWebhookError(t,
		webhookCRUDRequest(t, srv, http.MethodDelete, webhookBasePath+"/no-such-id", acmeToken, demoReaderUserID, nil),
		http.StatusForbidden, "rbac.permission_denied", "reader delete")
	assertWebhookError(t,
		webhookCRUDRequest(t, srv, http.MethodPost, webhookBasePath+"/no-such-id/restore", acmeToken, demoReaderUserID, nil),
		http.StatusForbidden, "rbac.permission_denied", "reader restore")

	// A request with no identity at all (no demo header, and the token
	// alone names a tenant but no rbac Subject) is refused the same way --
	// there being no built-in role for a request the gate cannot attribute
	// to anyone, this always denies rather than defaulting to any grant.
	assertWebhookError(t,
		webhookCRUDRequest(t, srv, http.MethodGet, webhookBasePath, acmeToken, "", nil),
		http.StatusForbidden, "rbac.permission_denied", "no identity at all")

	// The owner, holding every permission any module declared, passes both
	// directions -- the gate genuinely opens for the permission it names.
	ownerCreateResp := webhookCRUDRequest(t, srv, http.MethodPost, webhookBasePath, acmeToken, demoOwnerUserID,
		[]byte(`{"url":"https://hooks.example.test/gate","eventTypes":["org.member.joined"]}`))
	created := decodeWebhookSubscriptionCreated(t, ownerCreateResp, http.StatusCreated, "owner create")
	if created.ID == "" {
		t.Fatal("owner create returned no id")
	}
	ownerListResp := webhookCRUDRequest(t, srv, http.MethodGet, webhookBasePath, acmeToken, demoOwnerUserID, nil)
	ownerListResp.Body.Close()
	if ownerListResp.StatusCode != http.StatusOK {
		t.Fatalf("owner list: status = %d, want %d", ownerListResp.StatusCode, http.StatusOK)
	}
}

// TestBuildServer_WebhookCRUD_CreateListUpdateDeleteRestore_EndToEnd drives
// the full subscription lifecycle through the composed HTTP stack: create
// (the raw signing secret is available exactly once, right here), list (it
// never reappears, checked at the raw-JSON level so a struct that simply
// omitted the field could not hide a real regression), update (a partial
// PATCH: a present-but-empty eventTypes refused 400, url replaced, pause
// with active=false), the collapsed not-found refusals (an unknown id, and
// restore against a subscription that is not currently deleted), delete
// (the mark-delete: the row vanishes from every read), restore (the row
// returns, always PAUSED regardless of the active it held when deleted)
// and the explicit re-activation PATCH that resumes delivery.
func TestBuildServer_WebhookCRUD_CreateListUpdateDeleteRestore_EndToEnd(t *testing.T) {
	client := &http.Client{Timeout: 15 * time.Second}
	srv, cfg, _ := buildWebhookFlowTestServer(t, client)
	ownerToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "webhook-crud")

	// Create: every response field of the spec's
	// IntegrationCreatedWebhookSubscription schema, with the secret shown
	// exactly once and the creator attributed through SubjectResolver, never
	// a request field.
	const createBody = `{"url":"https://hooks.example.test/crud","eventTypes":["org.member.joined"]}`
	createResp := webhookCRUDRequest(t, srv, http.MethodPost, webhookBasePath, ownerToken, demoOwnerUserID, []byte(createBody))
	created := decodeWebhookSubscriptionCreated(t, createResp, http.StatusCreated, "create")
	if created.ID == "" {
		t.Fatal("created subscription carries no id")
	}
	if created.URL != "https://hooks.example.test/crud" {
		t.Fatalf("created.url = %q, want the requested url", created.URL)
	}
	if len(created.EventTypes) != 1 || created.EventTypes[0] != "org.member.joined" {
		t.Fatalf("created.eventTypes = %v, want [org.member.joined]", created.EventTypes)
	}
	if created.Secret == "" {
		t.Fatal("created subscription carries no secret -- the one and only place it is ever available")
	}
	if !created.Active {
		t.Error("a freshly created subscription is not active -- delivery would never start")
	}
	if created.CreatedBy != demoNotesCreatorUserID {
		t.Fatalf("created.createdBy = %q, want %q (from integration.SubjectResolver, never a request field)", created.CreatedBy, demoNotesCreatorUserID)
	}
	if created.CreatedAt == "" {
		t.Error("created subscription carries no createdAt")
	}

	// List: exactly the one subscription just created, still carrying the
	// secret-free shape -- decoded through a raw map, not
	// webhookSubscriptionListRow, so an unexpected secret field cannot hide
	// behind a struct that simply never declared it.
	listResp := webhookCRUDRequest(t, srv, http.MethodGet, webhookBasePath, ownerToken, demoOwnerUserID, nil)
	listBody, err := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if err != nil {
		t.Fatalf("read list response: %v", err)
	}
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list: status = %d, want %d; body = %s", listResp.StatusCode, http.StatusOK, listBody)
	}
	var rawList struct {
		WebhookSubscriptions []map[string]any `json:"webhookSubscriptions"`
	}
	if err := json.Unmarshal(listBody, &rawList); err != nil {
		t.Fatalf("decoding list %s: %v", listBody, err)
	}
	if len(rawList.WebhookSubscriptions) != 1 {
		t.Fatalf("list = %d subscriptions, want exactly 1; body = %s", len(rawList.WebhookSubscriptions), listBody)
	}
	row := rawList.WebhookSubscriptions[0]
	if got, _ := row["id"].(string); got != created.ID {
		t.Fatalf("listed row id = %q, want %q", got, created.ID)
	}
	if _, present := row["secret"]; present {
		t.Errorf("listed row carries %q, want it absent entirely: %v", "secret", row)
	}
	if got, _ := row["updatedAt"].(string); got == "" {
		t.Error("listed row carries no updatedAt")
	}

	// A present-but-empty eventTypes is refused on update exactly as on
	// create -- there is no supported way to leave a subscription with zero
	// event types short of deleting it.
	assertWebhookError(t,
		webhookCRUDRequest(t, srv, http.MethodPatch, webhookBasePath+"/"+created.ID, ownerToken, demoOwnerUserID,
			[]byte(`{"eventTypes":[]}`)),
		http.StatusBadRequest, "integration.event_types_required", "update with empty eventTypes")

	// A partial PATCH replaces only the field it names: url here, leaving
	// eventTypes and the active gate untouched.
	updateURLResp := webhookCRUDRequest(t, srv, http.MethodPatch, webhookBasePath+"/"+created.ID, ownerToken, demoOwnerUserID,
		[]byte(`{"url":"https://hooks.example.test/crud/v2"}`))
	updatedURL := decodeWebhookSubscriptionUpdate(t, updateURLResp, http.StatusOK, "update url")
	if updatedURL.ID != created.ID {
		t.Fatalf("update answered id %q, want %q", updatedURL.ID, created.ID)
	}
	if updatedURL.URL != "https://hooks.example.test/crud/v2" {
		t.Fatalf("updated.url = %q, want the replaced url", updatedURL.URL)
	}
	if len(updatedURL.EventTypes) != 1 || updatedURL.EventTypes[0] != "org.member.joined" {
		t.Fatalf("updated.eventTypes = %v, want the untouched [org.member.joined]", updatedURL.EventTypes)
	}
	if !updatedURL.Active {
		t.Error("updating url alone flipped active -- a partial update must leave unnamed fields untouched")
	}

	// Pause with active=false -- the supported way to stop delivery without
	// deleting.
	pauseResp := webhookCRUDRequest(t, srv, http.MethodPatch, webhookBasePath+"/"+created.ID, ownerToken, demoOwnerUserID,
		[]byte(`{"active":false}`))
	paused := decodeWebhookSubscriptionUpdate(t, pauseResp, http.StatusOK, "pause")
	if paused.Active {
		t.Error("pause (active=false) answered active=true")
	}

	// An update naming an id that never existed reports the collapsed
	// not-found -- never a quiet success.
	assertWebhookError(t,
		webhookCRUDRequest(t, srv, http.MethodPatch, webhookBasePath+"/no-such-id", ownerToken, demoOwnerUserID,
			[]byte(`{"active":true}`)),
		http.StatusNotFound, "integration.webhook_subscription_not_found", "update an id that never existed")

	// Restore against a subscription that is not currently deleted reports
	// the SAME collapsed not-found -- a caller cannot tell "no such id" from
	// "exists but not deleted", the identical signal Service-level Restore
	// gives.
	assertWebhookError(t,
		webhookCRUDRequest(t, srv, http.MethodPost, webhookBasePath+"/"+created.ID+"/restore", ownerToken, demoOwnerUserID, nil),
		http.StatusNotFound, "integration.webhook_subscription_not_found", "restore a live subscription")

	// Delete: the mark-delete takes the subscription out of every read this
	// fragment performs...
	deleteResp := webhookCRUDRequest(t, srv, http.MethodDelete, webhookBasePath+"/"+created.ID, ownerToken, demoOwnerUserID, nil)
	deleteResp.Body.Close()
	if deleteResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want %d", deleteResp.StatusCode, http.StatusNoContent)
	}

	// ...including the list, and both update and a second delete against the
	// now-hidden id refuse with the same not-found.
	postDeleteList := decodeWebhookSubscriptionList(t,
		webhookCRUDRequest(t, srv, http.MethodGet, webhookBasePath, ownerToken, demoOwnerUserID, nil),
		http.StatusOK, "list after delete")
	if len(postDeleteList.WebhookSubscriptions) != 0 {
		t.Fatalf("list after delete = %d rows, want 0 (mark-deleted rows are hidden from every read): %+v", len(postDeleteList.WebhookSubscriptions), postDeleteList.WebhookSubscriptions)
	}
	assertWebhookError(t,
		webhookCRUDRequest(t, srv, http.MethodPatch, webhookBasePath+"/"+created.ID, ownerToken, demoOwnerUserID,
			[]byte(`{"active":true}`)),
		http.StatusNotFound, "integration.webhook_subscription_not_found", "update a deleted subscription")
	assertWebhookError(t,
		webhookCRUDRequest(t, srv, http.MethodDelete, webhookBasePath+"/"+created.ID, ownerToken, demoOwnerUserID, nil),
		http.StatusNotFound, "integration.webhook_subscription_not_found", "delete a deleted subscription")

	// Restore: the row returns, but ALWAYS paused (active=false) whatever
	// active held at deletion -- resuming automatic outbound delivery to a
	// URL nobody has looked at since is never implicit.
	restoreResp := webhookCRUDRequest(t, srv, http.MethodPost, webhookBasePath+"/"+created.ID+"/restore", ownerToken, demoOwnerUserID, nil)
	restoreResp.Body.Close()
	if restoreResp.StatusCode != http.StatusNoContent {
		t.Fatalf("restore: status = %d, want %d", restoreResp.StatusCode, http.StatusNoContent)
	}
	restored := decodeWebhookSubscriptionList(t,
		webhookCRUDRequest(t, srv, http.MethodGet, webhookBasePath, ownerToken, demoOwnerUserID, nil),
		http.StatusOK, "list after restore")
	if len(restored.WebhookSubscriptions) != 1 {
		t.Fatalf("list after restore = %d rows, want exactly 1: %+v", len(restored.WebhookSubscriptions), restored.WebhookSubscriptions)
	}
	if restored.WebhookSubscriptions[0].ID != created.ID {
		t.Fatalf("restored row id = %q, want %q", restored.WebhookSubscriptions[0].ID, created.ID)
	}
	if restored.WebhookSubscriptions[0].Active {
		t.Error("a restored subscription is active -- restore must always land paused, whatever active held at deletion")
	}

	// The explicit re-activation PATCH is the step that resumes delivery.
	reactivateResp := webhookCRUDRequest(t, srv, http.MethodPatch, webhookBasePath+"/"+created.ID, ownerToken, demoOwnerUserID,
		[]byte(`{"active":true}`))
	reactivated := decodeWebhookSubscriptionUpdate(t, reactivateResp, http.StatusOK, "re-activate")
	if !reactivated.Active {
		t.Error("re-activation (active=true) answered active=false")
	}
}

// TestBuildServer_WebhookCRUD_DeliveriesList_RecentDeliveredRow is the
// deliveries-listing half of this surface's consumer proof: after a REAL
// delivery through round 2's pipeline (an org member-join triggering one
// signed POST at the receiver this test controls, the identical rig
// webhook_flow_test.go's own delivery proofs drive), the fragment's recent-
// deliveries listing answers the delivery's row -- status delivered,
// attempts and lastStatusCode from the receiver's 200, the delivery and
// last-attempt timestamps set -- while a subscription id that names nothing
// answers an empty list rather than a 404: the no-enumeration signal the
// spec's own description of this operation pins, since telling "no such
// subscription" from "genuine subscription, zero deliveries" apart would let
// a caller probe another tenant's subscription ids.
func TestBuildServer_WebhookCRUD_DeliveriesList_RecentDeliveredRow(t *testing.T) {
	receiver := newWebhookReceiver(t, http.StatusOK)
	client := &http.Client{Timeout: 15 * time.Second}
	srv, cfg, mailer := buildWebhookFlowTestServer(t, client)

	const tenant = pkgcore.TenantID("tenant-acme")
	ownerToken := registerAndAuthenticate(t, srv, cfg, tenant, "webhook-deliveries")
	sub := createWebhookSubscription(t, srv, ownerToken, receiver.URL, []string{"org.member.joined"})

	triggerOrgMemberJoined(t, srv, cfg, mailer, tenant, "Webhook Deliveries", "webhook-deliveries-member@example.com")
	eventually(t, 15*time.Second, "the webhook delivery", func() bool {
		return receiver.count() >= 1
	})

	// The listing answers exactly one row -- the real delivery just made --
	// with every field of the delivered-row vocabulary set: the receiver's
	// 200 as lastStatusCode, deliveredAt and lastAttemptAt both populated,
	// and no payload bytes anywhere (the raw event body is an implementation
	// detail of the send pipeline, never exposed to subscription-management
	// callers).
	deliveries := decodeWebhookDeliveries(t,
		webhookCRUDRequest(t, srv, http.MethodGet, webhookBasePath+"/"+sub.ID+"/deliveries", ownerToken, demoOwnerUserID, nil),
		http.StatusOK, "deliveries listing")
	if len(deliveries.Deliveries) != 1 {
		t.Fatalf("deliveries = %d rows, want exactly 1 for one real delivery: %+v", len(deliveries.Deliveries), deliveries.Deliveries)
	}
	row := deliveries.Deliveries[0]
	if row.ID == "" {
		t.Error("delivered row carries no id")
	}
	if row.SubscriptionID != sub.ID {
		t.Fatalf("row.subscriptionId = %q, want %q", row.SubscriptionID, sub.ID)
	}
	if row.EventType != "org.member.joined" || row.EventVersion != "v1" {
		t.Fatalf("row.event = %s v%s, want org.member.joined v1", row.EventType, row.EventVersion)
	}
	if row.Status != "delivered" {
		t.Fatalf("row.status = %q, want %q", row.Status, "delivered")
	}
	if row.Attempts != 1 {
		t.Fatalf("row.attempts = %d, want 1 (the receiver's 200 answered the first attempt)", row.Attempts)
	}
	if row.LastStatusCode == nil || *row.LastStatusCode != http.StatusOK {
		t.Fatalf("row.lastStatusCode = %v, want 200", row.LastStatusCode)
	}
	if row.LastError != "" {
		t.Fatalf("row.lastError = %q, want empty for a delivered row", row.LastError)
	}
	if row.LastAttemptAt == nil || *row.LastAttemptAt == "" {
		t.Error("row.lastAttemptAt is unset on a delivered row")
	}
	if row.DeliveredAt == nil || *row.DeliveredAt == "" {
		t.Error("row.deliveredAt is unset on a delivered row")
	}
	if row.CreatedAt == "" {
		t.Error("row.createdAt is empty")
	}

	// An id that names no subscription answers an empty list -- the
	// operation deliberately never verifies its subscriptionId, so a caller
	// cannot distinguish "no such id" from "zero deliveries" (the
	// no-enumeration rule the spec's description of this operation states).
	unknown := decodeWebhookDeliveries(t,
		webhookCRUDRequest(t, srv, http.MethodGet, webhookBasePath+"/no-such-id/deliveries", ownerToken, demoOwnerUserID, nil),
		http.StatusOK, "deliveries listing for an unknown id")
	if len(unknown.Deliveries) != 0 {
		t.Fatalf("deliveries for an unknown id = %d rows, want 0 (no-enumeration): %+v", len(unknown.Deliveries), unknown.Deliveries)
	}
}
