package flowtests

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/apptest"
	"github.com/vislake/speed/examples/reference-app/internal/testutil"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
)

// This file drives the case domain routes
// (internal/app/cases.go) through the real composed HTTP stack
// apptest.BuildServer builds -- the authn+tenancy middleware chain, the real
// registration/sign-in surface, and a real temp-file SQLite database whose
// cases tables the boot's own EnsureSchema created -- not a mock of any of
// it. The tenant comes from the bearer token alone (apptest.RegisterAndAuthenticate's
// own doc comment explains why Host plays no part); where a request needs
// a creator (the create route only -- the clinic-wide list reads no
// creator), the attribution comes from the X-Demo-User-Id header
// (DemoNotesSubjectResolver, the same attribution seam notes' create
// handler uses), or from the verified Principal when no header rides
// along.

// testCaseError is the {code, params} envelope every refusal answers.
type testCaseError struct {
	Code string `json:"code"`
}

// assertCasesError reads resp and requires the structured code it answers
// (never merely "some 4xx/5xx"), the same envelope shape
// testutil.AssertPermissionDenied asserts against for the rbac gate.
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
// uploaded photos, sees it on the clinic's case list, and reads its
// detail back with the photos in attachment order -- the exact queries
// the case web UI will make.
func TestCasesFlow_CreateListDetail_Journey(t *testing.T) {
	srv, cfg, _ := apptest.BuildServer(t)
	acmeToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-journey")

	created := testutil.CreateCaseAs(t, srv, acmeToken, demo.DemoNotesCreatorUserID, testutil.CaseCreateBody{
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
	if created.CreatorUserID != demo.DemoNotesCreatorUserID {
		t.Fatalf("CreatorUserID = %q, want the X-Demo-User-Id attribution %q", created.CreatorUserID, demo.DemoNotesCreatorUserID)
	}
	if len(created.Photos) != 2 || created.Photos[0].ObjectID != "photo-front-acme" || created.Photos[1].ObjectID != "photo-smile-acme" {
		t.Fatalf("create answer photos = %+v, want both in request order", created.Photos)
	}

	listResp := testutil.CasesRequestAs(t, srv, http.MethodGet, testutil.CasesPath, acmeToken, demo.DemoNotesCreatorUserID, nil)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", testutil.CasesPath, listResp.StatusCode)
	}
	var list struct {
		Cases []testutil.TestCase `json:"cases"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Cases) != 1 || list.Cases[0].ID != created.ID {
		t.Fatalf("list = %+v, want exactly the created case", list.Cases)
	}

	detailResp := testutil.CasesRequestAs(t, srv, http.MethodGet, caseDetailPathPrefix+created.ID, acmeToken, demo.DemoNotesCreatorUserID, nil)
	defer detailResp.Body.Close()
	detail, raw := testutil.DecodeCasesResponse(t, detailResp)
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
	missingResp := testutil.CasesRequestAs(t, srv, http.MethodGet, caseDetailPathPrefix+"no-such-case", acmeToken, demo.DemoNotesCreatorUserID, nil)
	assertCasesError(t, missingResp, http.StatusNotFound, "cases.not_found", "GET detail of an unknown case")
}

// TestCasesFlow_CrossTenant_Invisible pins tenant isolation through the
// composed stack: a second tenant's account sees neither the case list
// nor the detail of the first tenant's case -- and the detail answers the
// same coded not-found an unknown id answers, never "exists but not
// yours".
func TestCasesFlow_CrossTenant_Invisible(t *testing.T) {
	srv, cfg, _ := apptest.BuildServer(t)
	acmeToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-acme-owner")
	globexToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-globex", "cases-globex-owner")

	created := testutil.CreateCaseAs(t, srv, acmeToken, demo.DemoNotesCreatorUserID, testutil.CaseCreateBody{
		PatientName: "Acme Private",
	})

	listResp := testutil.CasesRequestAs(t, srv, http.MethodGet, testutil.CasesPath, globexToken, demo.DemoNotesCreatorUserID, nil)
	defer listResp.Body.Close()
	var list struct {
		Cases []testutil.TestCase `json:"cases"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Cases) != 0 {
		t.Fatalf("globex list = %+v, want empty (acme's case must be invisible)", list.Cases)
	}

	detailResp := testutil.CasesRequestAs(t, srv, http.MethodGet, caseDetailPathPrefix+created.ID, globexToken, demo.DemoNotesCreatorUserID, nil)
	assertCasesError(t, detailResp, http.StatusNotFound, "cases.not_found", "GET detail of another tenant's case")
}

// TestCasesFlow_ListIsClinicWide pins the clinic-wide list semantic through
// the composed stack: two creators in ONE tenant each see BOTH cases --
// a case one colleague opened is visible to another, the property the
// product's acceptance chain names -- whatever creator header the
// request carries (the header is an attribution seam for create, never a
// list key).
func TestCasesFlow_ListIsClinicWide(t *testing.T) {
	srv, cfg, _ := apptest.BuildServer(t)
	acmeToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-two-creators")

	_ = testutil.CreateCaseAs(t, srv, acmeToken, "user-creator-1", testutil.CaseCreateBody{PatientName: "Creator One's Case"})
	_ = testutil.CreateCaseAs(t, srv, acmeToken, "user-creator-2", testutil.CaseCreateBody{PatientName: "Creator Two's Case"})

	for _, tc := range []struct {
		creator   string
		wantNames []string
		wantCount int
	}{
		{creator: "user-creator-1", wantNames: []string{"Creator Two's Case", "Creator One's Case"}, wantCount: 2},
		{creator: "user-creator-2", wantNames: []string{"Creator Two's Case", "Creator One's Case"}, wantCount: 2},
	} {
		listResp := testutil.CasesRequestAs(t, srv, http.MethodGet, testutil.CasesPath, acmeToken, tc.creator, nil)
		var list struct {
			Cases []testutil.TestCase `json:"cases"`
		}
		if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
			listResp.Body.Close()
			t.Fatalf("decode list response: %v", err)
		}
		listResp.Body.Close()
		if len(list.Cases) != tc.wantCount {
			t.Fatalf("list as %q = %+v, want %d cases (both creators' rows, newest first)", tc.creator, list.Cases, tc.wantCount)
		}
		for i, want := range tc.wantNames {
			if list.Cases[i].PatientName != want {
				t.Fatalf("list as %q = %+v, want names [%s] in order", tc.creator, list.Cases, tc.wantNames)
			}
		}
	}
}

// TestCasesFlow_PrincipalAttribution pins the no-demo-header path: a real
// signed-in account acting through its access token alone is attributed
// through the verified Principal (DemoNotesSubjectResolver's fallback --
// the browser-shaped caller), and its case is visible on the clinic-wide
// list to every attribution source -- the case's recorded creator is the
// principal's id, but the creator column never hides or splits the list,
// so a header-attributed colleague's read of the same tenant answers the
// same rows.
func TestCasesFlow_PrincipalAttribution(t *testing.T) {
	srv, cfg, _ := apptest.BuildServer(t)
	acmeToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-principal")

	created := testutil.CreateCaseAs(t, srv, acmeToken, "", testutil.CaseCreateBody{PatientName: "Principal's Case"})
	if created.CreatorUserID == "" || created.CreatorUserID == demo.DemoNotesCreatorUserID {
		t.Fatalf("CreatorUserID = %q, want the account's own principal user id, distinct from the demo creator", created.CreatorUserID)
	}

	for _, tc := range []struct {
		what    string
		creator string
	}{
		{what: "headerless read", creator: ""},
		{what: "demo-creator header read", creator: demo.DemoNotesCreatorUserID},
	} {
		listResp := testutil.CasesRequestAs(t, srv, http.MethodGet, testutil.CasesPath, acmeToken, tc.creator, nil)
		var list struct {
			Cases []testutil.TestCase `json:"cases"`
		}
		if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
			listResp.Body.Close()
			t.Fatalf("decode list response: %v", err)
		}
		listResp.Body.Close()
		if len(list.Cases) != 1 || list.Cases[0].ID != created.ID {
			t.Fatalf("%s = %+v, want the principal-created case (the clinic list is one list, whatever creator header rides along)", tc.what, list.Cases)
		}
	}
}

// TestCasesFlow_ValidationAndConflicts_OverHTTP pins the coded refusals
// the case create form renders: patient-name validation, duplicate
// object ids within one request, and the conflict a reused photo object
// answers.
func TestCasesFlow_ValidationAndConflicts_OverHTTP(t *testing.T) {
	srv, cfg, _ := apptest.BuildServer(t)
	acmeToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-conflicts")

	// An empty patient name is refused before anything is inserted.
	emptyName := testutil.CasesRequestAs(t, srv, http.MethodPost, testutil.CasesPath, acmeToken, demo.DemoNotesCreatorUserID,
		bytes.NewReader([]byte(`{"patient_name":"   "}`)))
	assertCasesError(t, emptyName, http.StatusBadRequest, "cases.patient_name_required", "create with an empty patient name")

	// The same object twice in one request is a client bug, refused
	// outright.
	dupBody := testutil.CasesRequestAs(t, srv, http.MethodPost, testutil.CasesPath, acmeToken, demo.DemoNotesCreatorUserID,
		bytes.NewReader([]byte(`{"patient_name":"Dup","photo_object_ids":["photo-a","photo-a"]}`)))
	assertCasesError(t, dupBody, http.StatusBadRequest, "cases.duplicate_photo_object", "create with a duplicated photo")

	_ = testutil.CreateCaseAs(t, srv, acmeToken, demo.DemoNotesCreatorUserID, testutil.CaseCreateBody{
		PatientName:    "First Owner",
		PhotoObjectIDs: []string{"photo-a"},
	})

	// Reusing an attached photo for a second case is the coded conflict
	// the sequential double-create answers.
	reuse := testutil.CasesRequestAs(t, srv, http.MethodPost, testutil.CasesPath, acmeToken, demo.DemoNotesCreatorUserID,
		bytes.NewReader([]byte(`{"patient_name":"Second Owner","photo_object_ids":["photo-a"]}`)))
	assertCasesError(t, reuse, http.StatusConflict, "cases.photo_already_attached", "create reusing an attached photo")
}

// TestCasesFlow_Anonymous_Refused pins the middleware-chain fail-closed
// default for the new surface: a request with no valid token at all is
// refused before any handler runs.
func TestCasesFlow_Anonymous_Refused(t *testing.T) {
	srv, _, _ := apptest.BuildServer(t)

	resp := testutil.CasesRequestAs(t, srv, http.MethodGet, testutil.CasesPath, "", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("anonymous GET %s status = %d, want 403 (tenancy's fail-closed default)", testutil.CasesPath, resp.StatusCode)
	}
}

// TestCasesFlow_OversizedBody_RefusedWithInvalidRequestBody pins the
// MaxBytesReader bound internal/app/cases.go applies
// (see casesMaxRequestBodyBytes), mirroring the identical regressions
// internal/notes/handler_test.go's own oversized-body test and
// go/authn/handler_test.go's TestHandler_Register_OversizedBody_RefusedWithInvalidRequestBody
// pin: an arbitrarily large create-case body must not be read in full --
// unbounded buffering of an attacker's payload before any validation has
// run -- nor land its content in the database. The body below is valid
// JSON whose SIZE alone exceeds the byte bound: everything after the
// padding is a perfectly legal create-case request, so the ONLY thing that
// can refuse it is the body bound, and the refusal must surface as the
// catalogued invalid-request-body code rather than a successful case
// creation: an unbounded decoder would accept the whole body.
func TestCasesFlow_OversizedBody_RefusedWithInvalidRequestBody(t *testing.T) {
	srv, cfg, _ := apptest.BuildServer(t)
	acmeToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-oversized")

	// One byte over the bound -- casesMaxRequestBodyBytes's value, 1<<16,
	// written as a literal so this test also compiles against a handler that
	// handler, which defined no constant -- spent on JSON-leading
	// whitespace (legal, and skipped by the decoder), so the payload that
	// follows -- a valid create-case request -- is what an unbounded
	// reader would have accepted and stored.
	var body strings.Builder
	body.WriteString(strings.Repeat(" ", (1<<16)+1))
	body.WriteString(`{"patient_name":"Anna Meyer"}`)

	resp := testutil.CasesRequestAs(t, srv, http.MethodPost, testutil.CasesPath, acmeToken, demo.DemoNotesCreatorUserID, strings.NewReader(body.String()))
	assertCasesError(t, resp, http.StatusBadRequest, "cases.invalid_request_body", "create with an oversized body")

	// Nothing was created: the same creator can still create a case
	// afterwards.
	_ = testutil.CreateCaseAs(t, srv, acmeToken, demo.DemoNotesCreatorUserID, testutil.CaseCreateBody{
		PatientName: "After the refused oversized body",
	})
}

// TestCasesFlow_ColleagueSeesColleaguesCase is the clinic-wide-list
// regression in its cleanest form: two real signed-in accounts in ONE
// tenant; the first creates a case; the second's list must contain it.
// The property being pinned is that the list answers every case of the
// tenant -- never the caller's own cases only, which would leave the
// colleague's read empty (one patient, two charts).
func TestCasesFlow_ColleagueSeesColleaguesCase(t *testing.T) {
	srv, cfg, _ := apptest.BuildServer(t)
	aliceToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "colleague-alice")
	bobToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "colleague-bob")

	created := testutil.CreateCaseAs(t, srv, aliceToken, "", testutil.CaseCreateBody{PatientName: "Colleague Patient"})

	listResp := testutil.CasesRequestAs(t, srv, http.MethodGet, testutil.CasesPath, bobToken, "", nil)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("bob's GET %s status = %d, want 200", testutil.CasesPath, listResp.StatusCode)
	}
	var list struct {
		Cases []testutil.TestCase `json:"cases"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode bob's list response: %v", err)
	}
	if len(list.Cases) != 1 || list.Cases[0].ID != created.ID {
		t.Fatalf("bob's list = %+v, want Alice's case (a colleague in the same clinic must see it; pre-fix this list was creator-scoped and came back empty)", list.Cases)
	}
}

// TestCasesFlow_BlockA_ClinicJourney is the composed-stack form of the
// clinic-wide acceptance journey the e2e gate (core-journey.spec.ts)
// names: a clinic user creates a case with a real
// photo and sees it listed with the photo readable on the case, and a
// second user of the SAME clinic sees the first user's case in the list
// -- while a third
// user in ANOTHER tenant sees neither. The journey runs through the real
// composed HTTP stack with real signed-in accounts (no demo header, the
// browser shape): the photo travels through the cases upload op, the
// case through the cases fragment, and the photo's bytes come back
// through the content op.
func TestCasesFlow_BlockA_ClinicJourney(t *testing.T) {
	srv, cfg, _ := apptest.BuildServer(t)
	aliceToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "blocka-alice")
	bobToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "blocka-bob")
	globexToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-globex", "blocka-globex")

	// Alice uploads a real patient photo and creates the case with it in
	// one submission's worth of calls -- the pre-upload-then-create shape
	// the one-page web flow performs.
	photo := uploadPhotoAs(t, srv, aliceToken, base64.StdEncoding.EncodeToString(jpegWithExif(t)))
	created := testutil.CreateCaseAs(t, srv, aliceToken, "", testutil.CaseCreateBody{
		PatientName:    "Block A Patient",
		PhotoObjectIDs: []string{photo.ObjectID},
	})
	if len(created.Photos) != 1 || created.Photos[0].ObjectID != photo.ObjectID {
		t.Fatalf("created case photos = %+v, want exactly the uploaded photo", created.Photos)
	}

	// Alice sees the case on the list and reads the photo's bytes on the
	// case -- the photo is visible.
	listAs := func(t *testing.T, token, what string) []testutil.TestCase {
		t.Helper()
		resp := testutil.CasesRequestAs(t, srv, http.MethodGet, testutil.CasesPath, token, "", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: GET %s status = %d, want 200", what, testutil.CasesPath, resp.StatusCode)
		}
		var list struct {
			Cases []testutil.TestCase `json:"cases"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatalf("%s: decode list response: %v", what, err)
		}
		return list.Cases
	}
	aliceList := listAs(t, aliceToken, "alice's list")
	if len(aliceList) != 1 || aliceList[0].ID != created.ID || aliceList[0].PatientName != "Block A Patient" {
		t.Fatalf("alice's list = %+v, want exactly the created case", aliceList)
	}
	content := photoContentAs(t, srv, aliceToken, created.ID, photo.ObjectID)
	if content.MediaType != "image/jpeg" {
		t.Fatalf("photo media_type = %q, want image/jpeg", content.MediaType)
	}
	decoded, err := base64.StdEncoding.DecodeString(content.ContentBase64)
	if err != nil || len(decoded) == 0 {
		t.Fatalf("photo content base64 does not decode to bytes (err=%v)", err)
	}

	// Bob, a second user of the same clinic, sees Alice's case on the
	// clinic list and can open its photo. FAILS BEFORE the clinic-wide
	// list fix: the list answered the caller's own cases, Bob's came back
	// empty, and the patient would have met a colleague who could not
	// find their last case.
	bobList := listAs(t, bobToken, "bob's list")
	if len(bobList) != 1 || bobList[0].ID != created.ID {
		t.Fatalf("bob's list = %+v, want Alice's case (a colleague in the same clinic must see it)", bobList)
	}
	bobContent := photoContentAs(t, srv, bobToken, created.ID, photo.ObjectID)
	if bobContent.MediaType != "image/jpeg" || bobContent.ContentBase64 != content.ContentBase64 {
		t.Fatal("bob's photo read differs from Alice's, want the identical bytes")
	}

	// A third user in another tenant sees neither the case nor its photo:
	// the tenant boundary, not the creator column, is the scope.
	globexList := listAs(t, globexToken, "globex's list")
	if len(globexList) != 0 {
		t.Fatalf("globex's list = %+v, want empty (acme's case must stay invisible)", globexList)
	}
	foreign := testutil.CasesRequestAs(t, srv, http.MethodGet, casePhotoContentPath(created.ID, photo.ObjectID), globexToken, "", nil)
	assertCasesError(t, foreign, http.StatusNotFound, "cases.not_found", "another tenant's view of the case's photo")
}
