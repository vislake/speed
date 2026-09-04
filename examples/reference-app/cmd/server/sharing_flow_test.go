package main

// sharing_flow_test.go is go/sharing's round-2 mandatory-first-consumer
// proof: it drives the module's one public HTTP route through the real
// composed stack buildTestServer wires (server.go's tenancy allowlist,
// demo_subject.go's routePublic gate, sharing.Handler, a real SQLite
// database), the same two-layer standard every other module's own flow
// test in this file's package already meets.
//
// Create and Revoke stay Service-level this round (go/sharing/AGENTS.md
// records why: no owner-facing HTTP surface exists yet, only the public
// access route does), so this file mints and revokes the share through a
// second sharing.Module built over a second connection to the exact same
// SQLite file the running server itself opened -- the identical
// "buildServer hands out neither its *gorm.DB nor a module's own service,
// so a second connection is the only reach a test has into storage"
// pattern server_test.go's TestBuildServer_NoteCreate_PersistsAuditEvent
// and public_config_test.go's buildSeededTestServer already use for their
// own tables. The share row this second module writes lands in the same
// file the running server's own sharingModule (server.go) reads from, so
// a real HTTP GET against the running server sees it.

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/sharing"
)

// sharingTestJPEG returns a small, decodable JPEG -- storage's default
// media-type allowlist (defaultAllowedTypes, go/storage/module.go) accepts
// only image/jpeg and image/png, so this module's own flow test needs a
// genuine image, not an arbitrary byte string, to get past storage's own
// declare/complete gate before sharing.Service.Create ever runs.
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

// secondSharingService opens a second connection to the same SQLite file
// the running test server itself opened (cfg.SQLitePath) and returns a
// sharing.Service backed by it, bootstrapped through a throwaway registry
// of its own -- distinct from the running server's real registry, but
// writing into the exact same tables, since dbkit.MigrationRegistry.Apply
// already ran the migrations once, through the running server's own boot.
func secondSharingService(t *testing.T, cfg serverConfig) *sharing.Service {
	t.Helper()

	db, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: cfg.SQLitePath})
	if err != nil {
		t.Fatalf("open second connection to %q: %v", cfg.SQLitePath, err)
	}
	t.Cleanup(func() {
		sqlDB, dbErr := db.DB()
		if dbErr != nil {
			t.Errorf("second connection handle: %v", dbErr)
			return
		}
		if closeErr := sqlDB.Close(); closeErr != nil {
			t.Errorf("close second connection: %v", closeErr)
		}
	})

	module := sharing.NewModule(db)
	if _, bootErr := pkgcore.NewKernel().Bootstrap(context.Background(), module); bootErr != nil {
		t.Fatalf("bootstrap second sharing module: %v", bootErr)
	}
	return module.Service()
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

// TestBuildServer_SharingFlow_CreateAccessRevoke_EndToEnd is this round's
// full create/access/revoke life cycle through the real composed HTTP
// stack: create a share for a real go/storage object, access it as an
// unauthenticated visitor would (no token, no session, no demo header),
// revoke it, and observe the very next access refused -- the identical
// shape go/sharing's own example_test.go proves at the Service level,
// proven here end to end through real HTTP for the first time.
func TestBuildServer_SharingFlow_CreateAccessRevoke_EndToEnd(t *testing.T) {
	srv, cfg := buildTestServer(t)
	const tenantID = "tenant-acme"
	acmeToken := registerAndAuthenticate(t, srv, cfg, tenantID, "sharing-flow")

	content := sharingTestJPEG(t)
	completed := uploadAndComplete(t, srv, acmeToken, content, sha256Hex(content))

	svc := secondSharingService(t, cfg)
	tenantCtx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID(tenantID))
	created, err := svc.Create(tenantCtx, sharing.CreateParams{ResourceRef: completed.ID})
	if err != nil {
		t.Fatalf("sharing Create: %v", err)
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

	// Revoke, then the very next access is refused -- rule 3
	// (docs/internal/07-platform-services.md's "revocation takes effect
	// immediately" rule).
	if revokeErr := svc.Revoke(tenantCtx, created.Share.ID); revokeErr != nil {
		t.Fatalf("sharing Revoke: %v", revokeErr)
	}
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
	srv, _ := buildTestServer(t)
	resp := sharingAccessRequest(t, srv, "a-token-nobody-ever-issued", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s, want 404 -- an unrecognized token must never surface tenancy.tenant_unresolved", resp.StatusCode, body)
	}
}
