package flowtests

// sharing_flow_test.go is go/sharing's mandatory-first-consumer proof,
// covering both HTTP surfaces the module ships: the public,
// unauthenticated access route and the owner-facing create/list/
// get/revoke/access-log routes. Every operation drives the real composed
// stack buildTestServer wires (internal/app/server.go's tenancy allowlist,
// internal/app/demo_subject.go's routePublic gate for the access route and its
// sharingPermissionFor gate for the owner-facing ones, sharing.Handler, a
// real SQLite database), the same two-layer standard every other module's
// own flow test in this file's package already meets.
//
// Create, list, get, revoke and the access log all go through the real
// owner-facing HTTP routes (sharing.PathShares).

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"

	"github.com/vislake/speed/go/sharing"
)

// sharingTestJPEG returns a small, decodable JPEG -- storage's default
// media-type allowlist (defaultAllowedTypes, go/storage/module.go) accepts
// only image/jpeg and image/png, so this module's own flow test needs a
// genuine image, not an arbitrary byte string, to get past storage's own
// declare/complete gate before sharing_createShare ever runs.
func sharingTestJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 16; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 10), G: uint8(y * 10), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

// sharingAccessRequest issues an unauthenticated GET against srv's public
// sharing.PathAccess route -- no Authorization header, no demo user header,
// exactly the shape a genuinely anonymous visitor's request takes.
func sharingAccessRequest(t *testing.T, srv *httptest.Server, token, password string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+sharing.PathAccess+"?token="+token, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if password != "" {
		req.Header.Set(sharing.HeaderSharePassword, password)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", sharing.PathAccess, err)
	}
	return resp
}

// The wire shapes this file decodes, by field name rather than by importing
// go/sharing/api's generated types -- the same "assert on the wire shape,
// not the generator's Go types" posture server_test.go's testNote and
// org_flow_test.go's orgNode already take for their own modules.
type testSharingShare struct {
	ID                string `json:"id"`
	ResourceRef       string `json:"resourceRef"`
	MaxViews          *int   `json:"maxViews"`
	ViewCount         int    `json:"viewCount"`
	PasswordProtected bool   `json:"passwordProtected"`
	Sensitive         bool   `json:"sensitive"`
	RevokedAt         string `json:"revokedAt"`
	CreatedAt         string `json:"createdAt"`
}

type testCreateShareResponse struct {
	Share testSharingShare `json:"share"`
	Token string           `json:"token"`
}

type testListSharesResponse struct {
	Shares []testSharingShare `json:"shares"`
}

type testAccessLogEntry struct {
	Outcome string `json:"outcome"`
}

type testListAccessLogResponse struct {
	Entries []testAccessLogEntry `json:"entries"`
}

// decodeSharingBody reads resp, requires its status to be wantStatus, and
// decodes its body as out -- the shared decode step every wire-shape helper
// below runs, mirroring storage_flow_test.go's own decodeStorageObject.
func decodeSharingBody(t *testing.T, resp *http.Response, wantStatus int, what string, out any) {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, wantStatus, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
}

// TestBuildServer_SharingFlow_CreateAccessRevoke_EndToEnd is this module's
// full owner-facing create/list/get/access/access-log/revoke life cycle
// through the real composed HTTP stack: create a share for a real
// go/storage object through sharing_createShare, confirm it through
// sharing_listShares and sharing_getShare, access it as an unauthenticated
// visitor would (no token, no session, no demo header), read the resulting
// entry back through sharing_listShareAccessLog, revoke it through
// sharing_revokeShare, and observe the very next access refused -- the
// identical shape go/sharing's own example_test.go proves at the Service
// level, proven here end to end through real HTTP.
func TestBuildServer_SharingFlow_CreateAccessRevoke_EndToEnd(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	const tenantID = "tenant-acme"
	acmeToken := registerAndAuthenticate(t, srv, cfg, tenantID, "sharing-flow")

	content := sharingTestJPEG(t)
	completed := uploadAndComplete(t, srv, acmeToken, content, sha256Hex(content))

	// Create, through the real owner-facing route -- gated on
	// sharing:create (internal/app/demo_subject.go's sharingPermissionFor), held by
	// demo-owner.
	createBody, err := json.Marshal(map[string]any{"resourceRef": completed.ID})
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	createResp := storageRequest(t, srv, http.MethodPost, sharing.PathShares,
		acmeToken, app.DemoOwnerUserID, "application/json", bytes.NewReader(createBody))
	var created testCreateShareResponse
	decodeSharingBody(t, createResp, http.StatusCreated, "create share", &created)
	if created.Token == "" {
		t.Fatalf("create response carries no token")
	}
	if created.Share.ID == "" {
		t.Fatalf("create response carries no share id")
	}

	// List: the created share appears, and the response never leaks the
	// bearer token (SharingShare carries no token field at all).
	listResp := storageRequest(t, srv, http.MethodGet, sharing.PathShares,
		acmeToken, app.DemoOwnerUserID, "", nil)
	var list testListSharesResponse
	decodeSharingBody(t, listResp, http.StatusOK, "list shares", &list)
	if len(list.Shares) != 1 || list.Shares[0].ID != created.Share.ID {
		t.Fatalf("list shares = %+v, want exactly the one created share (id %q)", list.Shares, created.Share.ID)
	}

	// Get: the owner reads back the same share by id.
	getResp := storageRequest(t, srv, http.MethodGet, sharing.PathShares+"/"+created.Share.ID,
		acmeToken, app.DemoOwnerUserID, "", nil)
	var got testSharingShare
	decodeSharingBody(t, getResp, http.StatusOK, "get share", &got)
	if got.ID != created.Share.ID {
		t.Fatalf("get share id = %q, want %q", got.ID, created.Share.ID)
	}
	if got.RevokedAt != "" {
		t.Fatalf("a freshly created share already carries revokedAt = %q", got.RevokedAt)
	}

	// An unauthenticated visitor reads the shared object's bytes back
	// through the running server's real, composed HTTP stack.
	resp := sharingAccessRequest(t, srv, created.Token, "")
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read access response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200; body = %s", sharing.PathAccess, resp.StatusCode, body)
	}
	if got := sha256Hex(body); got != completed.ChecksumSha256 {
		t.Fatalf("shared content checksum = %q, want the finalized object's own %q -- storage's completion pipeline may re-encode bytes, so this compares against ITS checksum, not the upload's raw bytes", got, completed.ChecksumSha256)
	}
	if _, _, decodeErr := image.Decode(bytes.NewReader(body)); decodeErr != nil {
		t.Fatalf("shared content is not a decodable image: %v", decodeErr)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q, want %q", got, "image/jpeg")
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}

	// The owner reads back the access log: one granted attempt, and the
	// design rule that access needs no login but must leave a trail.
	logResp := storageRequest(t, srv, http.MethodGet, sharing.PathShares+"/"+created.Share.ID+"/access-log",
		acmeToken, app.DemoOwnerUserID, "", nil)
	var accessLog testListAccessLogResponse
	decodeSharingBody(t, logResp, http.StatusOK, "list access log", &accessLog)
	if len(accessLog.Entries) != 1 {
		t.Fatalf("access log = %+v, want exactly 1 entry", accessLog.Entries)
	}
	if accessLog.Entries[0].Outcome != sharing.AccessOutcomeGranted {
		t.Errorf("access log entry outcome = %q, want %q", accessLog.Entries[0].Outcome, sharing.AccessOutcomeGranted)
	}

	// Revoke, through the real owner-facing route -- then the very next
	// access is refused: revocation takes effect immediately.
	revokeResp := storageRequest(t, srv, http.MethodPost, sharing.PathShares+"/"+created.Share.ID+"/revoke",
		acmeToken, app.DemoOwnerUserID, "", nil)
	var revoked testSharingShare
	decodeSharingBody(t, revokeResp, http.StatusOK, "revoke share", &revoked)
	if revoked.RevokedAt == "" {
		t.Fatalf("revoked share carries no revokedAt")
	}

	// Revoking again is idempotent -- Service.Revoke's own contract,
	// unchanged by this HTTP translation.
	revokeAgainResp := storageRequest(t, srv, http.MethodPost, sharing.PathShares+"/"+created.Share.ID+"/revoke",
		acmeToken, app.DemoOwnerUserID, "", nil)
	var revokedAgain testSharingShare
	decodeSharingBody(t, revokeAgainResp, http.StatusOK, "revoke share again", &revokedAgain)

	refused := sharingAccessRequest(t, srv, created.Token, "")
	refusedBody, err := io.ReadAll(refused.Body)
	refused.Body.Close()
	if err != nil {
		t.Fatalf("read refused response: %v", err)
	}
	if refused.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (after revoke): status = %d, want 404; body = %s", sharing.PathAccess, refused.StatusCode, refusedBody)
	}
	var decoded struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(refusedBody, &decoded); err != nil {
		t.Fatalf("decoding refused body %s: %v", refusedBody, err)
	}
	if decoded.Code != "sharing.not_accessible" {
		t.Errorf("refused code = %q, want %q", decoded.Code, "sharing.not_accessible")
	}
}

// TestBuildServer_SharingFlow_UnknownToken_Answers404 proves the public
// route is genuinely reachable with no Principal, no tenant and no demo
// header at all -- a request tenancy.Middleware would otherwise 403 before
// this route's own handler ever ran, on any path not on its allowlist --
// and answers a real refusal rather than the tenant-unresolved error.
func TestBuildServer_SharingFlow_UnknownToken_Answers404(t *testing.T) {
	srv, _, _ := buildTestServer(t)
	resp := sharingAccessRequest(t, srv, "a-token-nobody-ever-issued", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s, want 404 -- an unrecognized token must never surface tenancy.tenant_unresolved", resp.StatusCode, body)
	}
}

// TestBuildServer_SharingPermissionGate_EnforcesTheSharingPermissions is the
// sharing mirror of storage's own gate test
// (TestBuildServer_StoragePermissionGate_EnforcesTheStoragePermissions):
// the read-only demo user holds notes:read and nothing else -- deliberately
// no sharing permission at all -- so sharing's owner-facing routes must
// refuse it in all three directions (sharing:create for POST
// sharing.PathShares, sharing:read for GET sharing.PathShares, and
// sharing:revoke for POST the revoke sub-route), proving demoRouteGuards'
// entry for sharing.PathShares and sharingPermissionFor's own
// GET/POST-create/POST-revoke split are all wired for real, never left
// ungated the way the route is without this gate.
//
// The revoke case deliberately targets a share id nobody created
// ("share-does-not-exist"): sharingPermissionFor selects the permission
// from the request's method and path suffix alone, before the handler ever
// looks up the share, so a denied response here proves the permission gate
// itself refused the request rather than the handler's own not-found path
// coincidentally answering the same way. Only a principal that clears the
// gate (TestBuildServer_SharingFlow_CreateAccessRevoke_EndToEnd's
// demo-owner) may reach far enough to observe a real 404 for an unknown id.
func TestBuildServer_SharingPermissionGate_EnforcesTheSharingPermissions(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "sharing-gate")

	resp := storageRequest(t, srv, http.MethodPost, sharing.PathShares,
		acmeToken, app.DemoReaderUserID, "application/json", bytes.NewReader([]byte(`{"resourceRef":"ref-1"}`)))
	assertPermissionDenied(t, resp, "POST "+sharing.PathShares+" as the read-only demo user")

	resp = storageRequest(t, srv, http.MethodGet, sharing.PathShares,
		acmeToken, app.DemoReaderUserID, "", nil)
	assertPermissionDenied(t, resp, "GET "+sharing.PathShares+" as the read-only demo user")

	resp = storageRequest(t, srv, http.MethodPost, sharing.PathShares+"/share-does-not-exist/revoke",
		acmeToken, app.DemoReaderUserID, "", nil)
	assertPermissionDenied(t, resp, "POST "+sharing.PathShares+"/{shareId}/revoke as the read-only demo user")
}
