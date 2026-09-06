package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// This file drives product round P2b's case domain routes
// (cmd/server/cases.go) through the real composed HTTP stack
// buildTestServer builds -- the authn+tenancy middleware chain, the real
// registration/sign-in surface, and a real temp-file SQLite database whose
// cases tables the boot's own EnsureSchema created -- not a mock of any of
// it. The tenant comes from the bearer token alone (registerAndAuthenticate's
// own doc comment explains why Host plays no part); the acting creator
// comes from the X-Demo-User-Id header (demoNotesSubjectResolver, the same
// attribution seam notes' create handler uses), or from the verified
// Principal when no header rides along.

// caseCreateBody is the wire body this file's helpers send to POST
// /api/v1/cases.
type caseCreateBody struct {
	PatientName    string   `json:"patient_name"`
	PatientRef     string   `json:"patient_ref"`
	PhotoObjectIDs []string `json:"photo_object_ids"`
}

// testCase is the response shape a case's create/list/detail answers
// share (the spec fragment's CasesCase, encoded by toCasesCase in
// cases.go), decoded enough for this file's assertions.
type testCase struct {
	ID            string `json:"id"`
	PatientName   string `json:"patient_name"`
	PatientRef    string `json:"patient_ref"`
	CreatorUserID string `json:"creator_user_id"`
	Photos        []struct {
		ObjectID string `json:"object_id"`
	} `json:"photos"`
}

// testCaseError is the {code, params} envelope every refusal answers.
type testCaseError struct {
	Code string `json:"code"`
}

// casesRequestAs issues method against path (a full path under the case
// surface, e.g. /api/v1/cases) with the given bearer token (empty means no
// Authorization header at all) and creator (empty means no X-Demo-User-Id
// header, which is how a request attributed through the verified Principal
// is expressed), returning the raw response.
func casesRequestAs(t *testing.T, srv *httptest.Server, method, path, token, creator string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if creator != "" {
		req.Header.Set(demoOrgUserHeader, creator)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s (creator=%q): %v", method, path, creator, err)
	}
	return resp
}

// createCaseAs POSTs a case body to srv authenticated as token and
// attributed to creator, asserting the 201 and returning the decoded case
// (photos included). t.Fatal on any deviation -- this is the happy-path
// helper every journey leg builds on.
func createCaseAs(t *testing.T, srv *httptest.Server, token, creator string, body caseCreateBody) testCase {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal case body: %v", err)
	}
	resp := casesRequestAs(t, srv, http.MethodPost, casesPath, token, creator, bytes.NewReader(raw))
	defer resp.Body.Close()

	decoded, respBody := decodeCasesResponse(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s (creator=%q) status = %d, want %d; body = %s",
			casesPath, creator, resp.StatusCode, http.StatusCreated, respBody)
	}
	return decoded
}

// decodeCasesResponse decodes resp's JSON body into a testCase and also
// returns the raw body bytes for failure messages.
func decodeCasesResponse(t *testing.T, resp *http.Response) (testCase, string) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	var decoded testCase
	if len(body) > 0 {
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("decoding %s: %v", body, err)
		}
	}
	return decoded, string(body)
}

// assertCasesError reads resp and requires the structured code it answers
// (never merely "some 4xx/5xx"), the same envelope shape
// assertPermissionDenied asserts against for the rbac gate.
func assertCasesError(t *testing.T, resp *http.Response, wantStatus int, wantCode, what string) {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, wantStatus, body)
	}
	var decoded testCaseError
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	if decoded.Code != wantCode {
		t.Fatalf("%s: error code = %q, want %q; body = %s", what, decoded.Code, wantCode, body)
	}
}

// TestCasesFlow_CreateListDetail_Journey is the case domain's composed
// happy path: a clinic staff member creates a case for a patient with two
// uploaded photos, sees it on the "my cases" list, and reads its detail
// back with the photos in attachment order -- the exact queries the P3 UI
// will make.
func TestCasesFlow_CreateListDetail_Journey(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-journey")

	created := createCaseAs(t, srv, acmeToken, demoNotesCreatorUserID, caseCreateBody{
		PatientName:    "Anna Meyer",
		PatientRef:     "CH-1001",
		PhotoObjectIDs: []string{"photo-front-acme", "photo-smile-acme"},
	})
	if created.ID == "" {
		t.Fatal("create answer carried no id")
	}
	if created.PatientName != "Anna Meyer" || created.PatientRef != "CH-1001" {
		t.Fatalf("create answer = %+v, want the submitted patient record echoed", created)
	}
	if created.CreatorUserID != demoNotesCreatorUserID {
		t.Fatalf("CreatorUserID = %q, want the X-Demo-User-Id attribution %q", created.CreatorUserID, demoNotesCreatorUserID)
	}
	if len(created.Photos) != 2 || created.Photos[0].ObjectID != "photo-front-acme" || created.Photos[1].ObjectID != "photo-smile-acme" {
		t.Fatalf("create answer photos = %+v, want both in request order", created.Photos)
	}

	listResp := casesRequestAs(t, srv, http.MethodGet, casesPath, acmeToken, demoNotesCreatorUserID, nil)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", casesPath, listResp.StatusCode)
	}
	var list struct {
		Cases []testCase `json:"cases"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Cases) != 1 || list.Cases[0].ID != created.ID {
		t.Fatalf("list = %+v, want exactly the created case", list.Cases)
	}

	detailResp := casesRequestAs(t, srv, http.MethodGet, caseDetailPathPrefix+created.ID, acmeToken, demoNotesCreatorUserID, nil)
	defer detailResp.Body.Close()
	detail, raw := decodeCasesResponse(t, detailResp)
	if detailResp.StatusCode != http.StatusOK {
		t.Fatalf("GET detail status = %d, want 200; body = %s", detailResp.StatusCode, raw)
	}
	if detail.ID != created.ID || detail.PatientName != "Anna Meyer" {
		t.Fatalf("detail = %+v, want the created case", detail)
	}
	if len(detail.Photos) != 2 || detail.Photos[0].ObjectID != "photo-front-acme" || detail.Photos[1].ObjectID != "photo-smile-acme" {
		t.Fatalf("detail photos = %+v, want both in attachment order", detail.Photos)
	}

	// A missing id is the coded not-found, indistinguishable from another
	// tenant's id (pinned over HTTP by the cross-tenant leg below).
	missingResp := casesRequestAs(t, srv, http.MethodGet, caseDetailPathPrefix+"no-such-case", acmeToken, demoNotesCreatorUserID, nil)
	assertCasesError(t, missingResp, http.StatusNotFound, "cases.not_found", "GET detail of an unknown case")
}

// TestCasesFlow_CrossTenant_Invisible pins tenant isolation through the
// composed stack: a second tenant's account sees neither the case list
// nor the detail of the first tenant's case -- and the detail answers the
// same coded not-found an unknown id answers, never "exists but not
// yours".
func TestCasesFlow_CrossTenant_Invisible(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-acme-owner")
	globexToken := registerAndAuthenticate(t, srv, cfg, "tenant-globex", "cases-globex-owner")

	created := createCaseAs(t, srv, acmeToken, demoNotesCreatorUserID, caseCreateBody{
		PatientName: "Acme Private",
	})

	listResp := casesRequestAs(t, srv, http.MethodGet, casesPath, globexToken, demoNotesCreatorUserID, nil)
	defer listResp.Body.Close()
	var list struct {
		Cases []testCase `json:"cases"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Cases) != 0 {
		t.Fatalf("globex list = %+v, want empty (acme's case must be invisible)", list.Cases)
	}

	detailResp := casesRequestAs(t, srv, http.MethodGet, caseDetailPathPrefix+created.ID, globexToken, demoNotesCreatorUserID, nil)
	assertCasesError(t, detailResp, http.StatusNotFound, "cases.not_found", "GET detail of another tenant's case")
}

// TestCasesFlow_ListIsMineOnly pins the "my cases" semantic through the
// composed stack: two creators in ONE tenant each see exactly their own
// cases, whatever the other creator does.
func TestCasesFlow_ListIsMineOnly(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-two-creators")

	_ = createCaseAs(t, srv, acmeToken, "user-creator-1", caseCreateBody{PatientName: "Creator One's Case"})
	_ = createCaseAs(t, srv, acmeToken, "user-creator-2", caseCreateBody{PatientName: "Creator Two's Case"})

	for _, tc := range []struct {
		creator   string
		wantName  string
		wantCount int
	}{
		{creator: "user-creator-1", wantName: "Creator One's Case", wantCount: 1},
		{creator: "user-creator-2", wantName: "Creator Two's Case", wantCount: 1},
	} {
		listResp := casesRequestAs(t, srv, http.MethodGet, casesPath, acmeToken, tc.creator, nil)
		var list struct {
			Cases []testCase `json:"cases"`
		}
		if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
			listResp.Body.Close()
			t.Fatalf("decode list response: %v", err)
		}
		listResp.Body.Close()
		if len(list.Cases) != tc.wantCount || list.Cases[0].PatientName != tc.wantName {
			t.Fatalf("list as %q = %+v, want exactly %q's own case (%q)", tc.creator, list.Cases, tc.creator, tc.wantName)
		}
	}
}

// TestCasesFlow_PrincipalAttribution pins the no-demo-header path: a real
// signed-in account acting through its access token alone is attributed
// through the verified Principal (demoNotesSubjectResolver's fallback --
// the browser-shaped caller), and its cases are visible to itself and to
// no one else's list.
func TestCasesFlow_PrincipalAttribution(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-principal")

	created := createCaseAs(t, srv, acmeToken, "", caseCreateBody{PatientName: "Principal's Case"})
	if created.CreatorUserID == "" || created.CreatorUserID == demoNotesCreatorUserID {
		t.Fatalf("CreatorUserID = %q, want the account's own principal user id, distinct from the demo creator", created.CreatorUserID)
	}

	listResp := casesRequestAs(t, srv, http.MethodGet, casesPath, acmeToken, "", nil)
	defer listResp.Body.Close()
	var list struct {
		Cases []testCase `json:"cases"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Cases) != 1 || list.Cases[0].ID != created.ID {
		t.Fatalf("headerless list = %+v, want exactly the principal-created case", list.Cases)
	}

	// The demo-header creator's own list is untouched by the principal's
	// case: the two attribution sources key separate "my cases" scopes.
	otherListResp := casesRequestAs(t, srv, http.MethodGet, casesPath, acmeToken, demoNotesCreatorUserID, nil)
	defer otherListResp.Body.Close()
	if err := json.NewDecoder(otherListResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Cases) != 0 {
		t.Fatalf("demo-creator list = %+v, want empty (the principal's case must not leak into it)", list.Cases)
	}
}

// TestCasesFlow_ValidationAndConflicts_OverHTTP pins the coded refusals
// the P3 create form will render: patient-name validation, duplicate
// object ids within one request, and the conflict a reused photo object
// answers.
func TestCasesFlow_ValidationAndConflicts_OverHTTP(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-conflicts")

	// An empty patient name is refused before anything is inserted.
	emptyName := casesRequestAs(t, srv, http.MethodPost, casesPath, acmeToken, demoNotesCreatorUserID,
		bytes.NewReader([]byte(`{"patient_name":"   "}`)))
	assertCasesError(t, emptyName, http.StatusBadRequest, "cases.patient_name_required", "create with an empty patient name")

	// The same object twice in one request is a client bug, refused
	// outright.
	dupBody := casesRequestAs(t, srv, http.MethodPost, casesPath, acmeToken, demoNotesCreatorUserID,
		bytes.NewReader([]byte(`{"patient_name":"Dup","photo_object_ids":["photo-a","photo-a"]}`)))
	assertCasesError(t, dupBody, http.StatusBadRequest, "cases.duplicate_photo_object", "create with a duplicated photo")

	_ = createCaseAs(t, srv, acmeToken, demoNotesCreatorUserID, caseCreateBody{
		PatientName:    "First Owner",
		PhotoObjectIDs: []string{"photo-a"},
	})

	// Reusing an attached photo for a second case is the coded conflict
	// the sequential double-create answers.
	reuse := casesRequestAs(t, srv, http.MethodPost, casesPath, acmeToken, demoNotesCreatorUserID,
		bytes.NewReader([]byte(`{"patient_name":"Second Owner","photo_object_ids":["photo-a"]}`)))
	assertCasesError(t, reuse, http.StatusConflict, "cases.photo_already_attached", "create reusing an attached photo")
}

// TestCasesFlow_Anonymous_Refused pins the middleware-chain fail-closed
// default for the new surface: a request with no valid token at all is
// refused before any handler runs.
func TestCasesFlow_Anonymous_Refused(t *testing.T) {
	srv, _, _ := buildTestServer(t)

	resp := casesRequestAs(t, srv, http.MethodGet, casesPath, "", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("anonymous GET %s status = %d, want 403 (tenancy's fail-closed default)", casesPath, resp.StatusCode)
	}
}
