package testutil

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
)

// NotesRequestAs issues method against /api/v1/notes with the given bearer
// token (empty means no Authorization header at all) and acting user,
// returning the raw response for the caller to assert on. An empty user
// sends no demo user header at all, which is how a request with a
// resolvable tenant (the token) but no identity is expressed.
//
// A non-empty user additionally sends X-Demo-User-Id (DemoNotesCreatorUserID):
// notes' create handler attributes the note through its own SubjectResolver
// (DemoNotesSubjectResolver in internal/app/server.go), which reads that header first and
// falls back to the verified Principal when no demo header is present -- so
// the requests that carry a demo user carry the creator header the demo
// flows were built around, while a token-only request (demo_users_test.go,
// the seeded accounts acting as real users) is attributed through the
// Principal instead.
func NotesRequestAs(t *testing.T, srv *httptest.Server, method, token, user string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+"/api/v1/notes", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if user != "" {
		req.Header.Set(demo.DemoUserHeader, user)
		req.Header.Set(demo.DemoOrgUserHeader, demo.DemoNotesCreatorUserID)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s /api/v1/notes (user=%q): %v", method, user, err)
	}
	return resp
}

// AssertPermissionDenied reads resp and requires it to be rbac's 403 with
// the structured code the client resolves against the module's locale
// files -- not merely "some 4xx", which tenancy's own fail-closed 403 would
// also satisfy.
func AssertPermissionDenied(t *testing.T, resp *http.Response, what string) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, http.StatusForbidden, body)
	}
	var decoded struct {
		Code string `json:"code"`
	}
	if err = json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	if decoded.Code != "rbac.permission_denied" {
		t.Fatalf("%s: error code = %q, want %q; body = %s", what, decoded.Code, "rbac.permission_denied", body)
	}
}

// CasesRequestAs issues method against path (a full path under the case
// surface, e.g. /api/v1/cases) with the given bearer token (empty means no
// Authorization header at all) and creator (empty means no X-Demo-User-Id
// header, which is how a request attributed through the verified Principal
// is expressed), returning the raw response.
func CasesRequestAs(t *testing.T, srv *httptest.Server, method, path, token, creator string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if creator != "" {
		req.Header.Set(demo.DemoOrgUserHeader, creator)
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

// CreateCaseAs POSTs a case body to srv authenticated as token and
// attributed to creator, asserting the 201 and returning the decoded case
// (photos included). t.Fatal on any deviation -- this is the happy-path
// helper every journey leg builds on.
func CreateCaseAs(t *testing.T, srv *httptest.Server, token, creator string, body CaseCreateBody) TestCase {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal case body: %v", err)
	}
	resp := CasesRequestAs(t, srv, http.MethodPost, CasesPath, token, creator, bytes.NewReader(raw))
	defer func() { _ = resp.Body.Close() }()

	decoded, respBody := DecodeCasesResponse(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s (creator=%q) status = %d, want %d; body = %s",
			CasesPath, creator, resp.StatusCode, http.StatusCreated, respBody)
	}
	return decoded
}

// DecodeCasesResponse decodes resp's JSON body into a TestCase and also
// returns the raw body bytes for failure messages.
func DecodeCasesResponse(t *testing.T, resp *http.Response) (TestCase, string) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	var decoded TestCase
	if len(body) > 0 {
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("decoding %s: %v", body, err)
		}
	}
	return decoded, string(body)
}

// NotifRequest issues method against srv.URL+path with a bearer token, the
// acting subject (the X-Demo-User-Id header DemoOrgSubjectResolverFor reads;
// empty omits it) and an optional JSON body, and requires the response to
// carry wantStatus, decoding it into out (nil to skip decoding, for empty
// responses like the 204s and the demo route's 202). The envelope of every
// refusal decodes into a NotifErrorBody through the same out slot.
func NotifRequest(t *testing.T, srv *httptest.Server, method, path, token, subjectUserID string, body any, wantStatus int, out any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if subjectUserID != "" {
		req.Header.Set(demo.DemoOrgUserHeader, subjectUserID)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != wantStatus {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s status = %d, want %d; body = %s", method, path, resp.StatusCode, wantStatus, respBody)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s %s response: %v", method, path, err)
		}
	}
}

// NotifError issues a request whose response must carry the given error
// status, and returns the decoded envelope so the caller can pin the code.
func NotifError(t *testing.T, srv *httptest.Server, method, path, token, subjectUserID string, body any, wantStatus int) NotifErrorBody {
	t.Helper()
	var env NotifErrorBody
	NotifRequest(t, srv, method, path, token, subjectUserID, body, wantStatus, &env)
	if env.Code == nil {
		t.Fatalf("%s %s: error response carried no code", method, path)
	}
	return env
}

type TestNote struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type TestListNotesResponse struct {
	Notes []TestNote `json:"notes"`
}

// TestCase is the response shape a case's create/list/detail answers
// share (the spec fragment's CasesCase, encoded by toCasesCase in
// internal/app/cases.go), decoded enough for the callers' assertions.
type TestCase struct {
	ID            string `json:"id"`
	PatientName   string `json:"patient_name"`
	PatientRef    string `json:"patient_ref"`
	CreatorUserID string `json:"creator_user_id"`
	Photos        []struct {
		ObjectID string `json:"object_id"`
	} `json:"photos"`
}

// CaseCreateBody is the wire body the suites' helpers send to POST
// /api/v1/cases.
type CaseCreateBody struct {
	PatientName    string   `json:"patient_name"`
	PatientRef     string   `json:"patient_ref"`
	PhotoObjectIDs []string `json:"photo_object_ids"`
}

type NotifErrorBody struct {
	Code *string `json:"code"`
}

// CasesPath is the cases fragment list-and-create route, mirrored from the
// fixture so suites in either test package can name the wire path.
const CasesPath = "/api/v1/cases"
