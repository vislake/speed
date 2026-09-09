package main

// apikey_flow_test.go is go/integration's API-key surface's
// mandatory-first-consumer proof. It drives the module's own
// spec-generated HTTP surface --
// internal/app/server.go's integrationModule wiring, mounted through the generic
// mountModuleRoutes loop exactly like every other module's fragment, gated
// by internal/app/demo_subject.go's guardIntegrationRoute -- through the composed
// HTTP stack: create (capturing the plaintext key, shown exactly once),
// list (confirming it never reappears), rotate (confirming the predecessor
// is revoked and the replacement works) and revoke.
//
// It follows storage_flow_test.go's and org_flow_test.go's wire-shape
// discipline: responses are decoded into structs that mirror the JSON on
// the wire, never into go/integration/api's spec-generated types, so the
// assertions bind to the actual response contract rather than to the
// generator's Go shapes, and this file never imports go/integration itself.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
)

// testCreatedAPIKey is the wire shape of IntegrationCreatedAPIKey --
// Service.Create's (and Service.Rotate's) result, the one and only place
// the raw key value is ever available.
type testCreatedAPIKey struct {
	ID        string   `json:"id"`
	Key       string   `json:"key"`
	Prefix    string   `json:"prefix"`
	Scopes    []string `json:"scopes"`
	CreatedBy string   `json:"createdBy"`
	ExpiresAt string   `json:"expiresAt"`
}

// testAPIKeySummary is the wire shape of one integration_listAPIKeys row.
// It deliberately has no Key or Hash field at all -- decoding through a
// struct that does not declare them could never prove their absence on the
// wire, which is why TestBuildServer_APIKeyFlow below additionally decodes
// the list response through a raw map for that one assertion.
type testAPIKeySummary struct {
	ID          string   `json:"id"`
	Prefix      string   `json:"prefix"`
	Scopes      []string `json:"scopes"`
	CreatedBy   string   `json:"createdBy"`
	CreatedAt   string   `json:"createdAt"`
	ExpiresAt   string   `json:"expiresAt"`
	LastUsedAt  *string  `json:"lastUsedAt"`
	RevokedAt   *string  `json:"revokedAt"`
	Revoked     bool     `json:"revoked"`
	Expired     bool     `json:"expired"`
	CreatorLeft bool     `json:"creatorLeft"`
}

type testListAPIKeysResponse struct {
	APIKeys []testAPIKeySummary `json:"apiKeys"`
}

// apikeyRequest issues method against path (already the full
// "/api/v1/integration/apikeys..." route) on srv as the acting user, in the
// tenant the given bearer token resolves -- the identical shape
// storageRequest and notesRequestAs both use. A non-empty user additionally
// sends X-Demo-User-Id (DemoNotesCreatorUserID, this app's one shared
// creator-attribution identity): integration_createAPIKey attributes the
// new key's CreatedBy through integration.SubjectResolver
// (DemoOrgSubjectResolver in internal/app/server.go, the identical seam instance org's
// and notification's own caller-scoped endpoints already share), which
// reads that same header. list, rotate and revoke never read it at all --
// see integration.SubjectResolver's own doc comment for why only Create
// needs a caller identity.
func apikeyRequest(t *testing.T, srv *httptest.Server, method, path, token, user string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if user != "" {
		req.Header.Set(app.DemoUserHeader, user)
		req.Header.Set(app.DemoOrgUserHeader, app.DemoNotesCreatorUserID)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s (user=%q): %v", method, path, user, err)
	}
	return resp
}

// decodeCreatedAPIKey reads resp, requires its status to be wantStatus, and
// decodes its body as a testCreatedAPIKey wire shape.
func decodeCreatedAPIKey(t *testing.T, resp *http.Response, wantStatus int, what string) testCreatedAPIKey {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, wantStatus, body)
	}
	var out testCreatedAPIKey
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	return out
}

// assertAPIKeyError reads resp and requires it to be the API-key surface's
// structured error: status wantStatus and the envelope's code exactly
// wantCode -- the identical shape assertStorageError checks against
// storage's own surface.
func assertAPIKeyError(t *testing.T, resp *http.Response, wantStatus int, wantCode, what string) {
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

const apikeyBasePath = "/api/v1/integration/apikeys"

// TestBuildServer_APIKeyFlow_CreateListRotateRevoke_EndToEnd drives the
// API-key lifecycle through the composed HTTP stack: create (the raw key
// is available exactly once, right here), list (it never reappears,
// checked at the raw-JSON level so a struct that simply omitted the field
// could not hide a real regression), rotate (the predecessor is revoked and
// the replacement works and differs) and revoke.
func TestBuildServer_APIKeyFlow_CreateListRotateRevoke_EndToEnd(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	// The token signs a real account into tenant-acme; the demo user header
	// then names which seeded demo grant the rbac gate decides the request
	// against (internal/app/demo_subject.go's seedDemoGrants) -- the owner role carries
	// every permission any module declared, integration:apikey:* included.
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "apikey-flow")

	// Create with an empty body: every field of the request schema is
	// optional, and no host-wired PermissionLister is needed for a key
	// requesting zero scopes (see integration.Service.Create's own doc
	// comment) -- this app's own integration.NewModule wiring never wires
	// one, so a zero-scope key request is the only legal shape end to
	// end.
	createResp := apikeyRequest(t, srv, http.MethodPost, apikeyBasePath, acmeToken, app.DemoOwnerUserID)
	created := decodeCreatedAPIKey(t, createResp, http.StatusCreated, "create")
	if created.ID == "" {
		t.Fatal("created key carries no id")
	}
	if created.Key == "" {
		t.Fatal("created key carries no raw key -- the one and only place it is ever available")
	}
	if created.CreatedBy != app.DemoNotesCreatorUserID {
		t.Fatalf("created.createdBy = %q, want %q (from integration.SubjectResolver, never a request field)", created.CreatedBy, app.DemoNotesCreatorUserID)
	}

	// List: exactly the one key just created, and -- decoded through a raw
	// map, not testAPIKeySummary, so an unexpected field cannot hide behind
	// a struct that simply never declared it -- with no key or hash
	// anywhere on the wire.
	listResp := apikeyRequest(t, srv, http.MethodGet, apikeyBasePath, acmeToken, app.DemoOwnerUserID)
	listBody, err := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if err != nil {
		t.Fatalf("read list response: %v", err)
	}
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list: status = %d, want %d; body = %s", listResp.StatusCode, http.StatusOK, listBody)
	}
	var rawList struct {
		APIKeys []map[string]any `json:"apiKeys"`
	}
	if err := json.Unmarshal(listBody, &rawList); err != nil {
		t.Fatalf("decoding list %s: %v", listBody, err)
	}
	if len(rawList.APIKeys) != 1 {
		t.Fatalf("list = %d keys, want exactly 1; body = %s", len(rawList.APIKeys), listBody)
	}
	row := rawList.APIKeys[0]
	if got, _ := row["id"].(string); got != created.ID {
		t.Fatalf("listed row id = %q, want %q", got, created.ID)
	}
	for _, forbidden := range []string{"key", "hash"} {
		if _, present := row[forbidden]; present {
			t.Errorf("listed row carries %q, want it absent entirely: %v", forbidden, row)
		}
	}

	// Rotate: a brand-new id and key, the predecessor's id NEVER reappears
	// as the new one, and rotating the (now revoked) predecessor again
	// reports the already-revoked conflict rather than a further
	// replacement.
	rotateResp := apikeyRequest(t, srv, http.MethodPost, apikeyBasePath+"/"+created.ID+"/rotate", acmeToken, app.DemoOwnerUserID)
	rotated := decodeCreatedAPIKey(t, rotateResp, http.StatusOK, "rotate")
	if rotated.ID == created.ID {
		t.Fatal("rotated id equals the predecessor's -- Rotate must create-new-then-revoke-old, never rewrite in place")
	}
	if rotated.Key == "" || rotated.Key == created.Key {
		t.Fatal("rotated key must be a fresh raw value, never empty and never the predecessor's")
	}
	assertAPIKeyError(t,
		apikeyRequest(t, srv, http.MethodPost, apikeyBasePath+"/"+created.ID+"/rotate", acmeToken, app.DemoOwnerUserID),
		http.StatusConflict, "integration.key_already_revoked", "rotate the already-revoked predecessor")

	// The predecessor is revoked, not deleted: integration_listAPIKeys
	// still lists the row (predecessor + replacement both present below),
	// the difference living in the Revoked flag, not the row's presence.
	postRotateListResp := apikeyRequest(t, srv, http.MethodGet, apikeyBasePath, acmeToken, app.DemoOwnerUserID)
	postRotateList := decodeListAPIKeys(t, postRotateListResp, http.StatusOK, "list after rotate")
	if len(postRotateList.APIKeys) != 2 {
		t.Fatalf("list after rotate = %d keys, want exactly 2 (predecessor + replacement); %+v", len(postRotateList.APIKeys), postRotateList.APIKeys)
	}
	var predecessorRevoked, replacementLive bool
	for _, row := range postRotateList.APIKeys {
		switch row.ID {
		case created.ID:
			predecessorRevoked = row.Revoked
		case rotated.ID:
			replacementLive = !row.Revoked
		}
	}
	if !predecessorRevoked {
		t.Error("the predecessor does not list as revoked after rotate")
	}
	if !replacementLive {
		t.Error("the replacement does not list as live after rotate")
	}

	// Revoke the replacement. A second revoke reports the already-revoked
	// conflict; revoking an id that never existed at all reports not-found
	// -- the two refusals are distinct codes, never conflated.
	revokeResp := apikeyRequest(t, srv, http.MethodDelete, apikeyBasePath+"/"+rotated.ID, acmeToken, app.DemoOwnerUserID)
	revokeResp.Body.Close()
	if revokeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: status = %d, want %d", revokeResp.StatusCode, http.StatusNoContent)
	}
	assertAPIKeyError(t,
		apikeyRequest(t, srv, http.MethodDelete, apikeyBasePath+"/"+rotated.ID, acmeToken, app.DemoOwnerUserID),
		http.StatusConflict, "integration.key_already_revoked", "revoke the already-revoked replacement")
	assertAPIKeyError(t,
		apikeyRequest(t, srv, http.MethodDelete, apikeyBasePath+"/no-such-id", acmeToken, app.DemoOwnerUserID),
		http.StatusNotFound, "integration.key_not_found", "revoke an id that never existed")
}

// decodeListAPIKeys reads resp, requires its status to be wantStatus, and
// decodes its body as a testListAPIKeysResponse wire shape.
func decodeListAPIKeys(t *testing.T, resp *http.Response, wantStatus int, what string) testListAPIKeysResponse {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, wantStatus, body)
	}
	var out testListAPIKeysResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	return out
}

// TestBuildServer_APIKeyPermissionGate_EnforcesTheAPIKeyPermissions is the
// integration mirror of storage_flow_test.go's own permission-gate test:
// the read-only demo user holds notes:read and nothing else -- deliberately
// no integration:apikey:* permission at all -- so both the read and manage
// directions of this module's gate must refuse it, while the owner passes.
// This is also the proof that guardIntegrationRoute
// (internal/app/demo_subject.go) genuinely evaluates a real permission rather than the
// generic gate silently passing everything through: a router bug that
// let every request past would make this test the one that fails.
func TestBuildServer_APIKeyPermissionGate_EnforcesTheAPIKeyPermissions(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "apikey-gate")

	// The reader may neither list nor create: both directions of the
	// integration:apikey:read / integration:apikey:manage gate are closed.
	assertAPIKeyError(t,
		apikeyRequest(t, srv, http.MethodGet, apikeyBasePath, acmeToken, app.DemoReaderUserID),
		http.StatusForbidden, "rbac.permission_denied", "reader list")
	assertAPIKeyError(t,
		apikeyRequest(t, srv, http.MethodPost, apikeyBasePath, acmeToken, app.DemoReaderUserID),
		http.StatusForbidden, "rbac.permission_denied", "reader create")

	// The owner, holding every permission any module declared, passes both.
	ownerCreateResp := apikeyRequest(t, srv, http.MethodPost, apikeyBasePath, acmeToken, app.DemoOwnerUserID)
	created := decodeCreatedAPIKey(t, ownerCreateResp, http.StatusCreated, "owner create")
	if created.ID == "" {
		t.Fatal("owner create returned no id")
	}
	ownerListResp := apikeyRequest(t, srv, http.MethodGet, apikeyBasePath, acmeToken, app.DemoOwnerUserID)
	ownerListResp.Body.Close()
	if ownerListResp.StatusCode != http.StatusOK {
		t.Fatalf("owner list: status = %d, want %d", ownerListResp.StatusCode, http.StatusOK)
	}

	// A request with no identity at all (no demo header, and the token
	// alone names a tenant but no rbac Subject) is refused the same way --
	// there being no built-in role for a request the gate cannot attribute
	// to anyone, this always denies rather than defaulting to any grant.
	assertAPIKeyError(t,
		apikeyRequest(t, srv, http.MethodGet, apikeyBasePath, acmeToken, ""),
		http.StatusForbidden, "rbac.permission_denied", "no identity at all")
}
