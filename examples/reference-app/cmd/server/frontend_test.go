package main

// Tests for frontend.go's built-frontend serving: the httptest regressions
// of the acceptance blocker they discharge (an anonymous GET / answering
// the app's page instead of a tenancy refusal, a built asset answering its
// bytes, an unknown non-API path answering the SPA fallback, API behavior
// byte-identical), plus the serving rules frontend.go's package doc
// comment pins: opt-in via cfg.WebDistDir, asset misses as real 404s, the
// cache policy split, HEAD support, and path-traversal confinement to the
// configured directory.
//
// The fixture dist directory mirrors the real vite build output shape:
// examples/reference-app/web/index.html (whose body carries the
// <div id="root"></div> mount point and whose production build rewrites
// the module script to a hashed /assets/<name> reference -- no vite
// `base` override exists in that app's vite.config.ts, so the built
// index.html references its bundles at the root-absolute /assets/* paths
// this handler serves). The suite drives the real composed handler through
// buildServer, never a mock of the wiring.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureIndexHTML is the fixture index.html: the same mount-point shape
// as the real page, with the built script reference vite emits (hashed
// name under /assets/, root-absolute -- vite's default base "/").
const fixtureIndexHTML = `<!doctype html>
<html lang="zh-CN">
  <head>
    <meta charset="UTF-8" />
    <title>speed reference app</title>
  </head>
  <body>
    <div id="root"></div>
    <script type="module" crossorigin src="/assets/index-DeaDBeef.js"></script>
  </body>
</html>
`

// fixtureAssetName is the hashed bundle file the fixture index.html
// references.
const fixtureAssetName = "index-DeaDBeef.js"

// fixtureAssetBody is the fixture bundle's bytes.
var fixtureAssetBody = []byte("console.log('fixture asset');\n")

// tenancyUnresolvedBody is the exact JSON body tenancy.Middleware answers
// for a request it refuses (observed from the real composed handler):
// pinning the byte-identical claim for API paths means comparing against
// this literal, not against a looser status-code check.
const tenancyUnresolvedBody = "{\"code\":\"tenancy.tenant_unresolved\"}\n"

// writeFrontendFixture writes a minimal real-shaped dist directory into a
// fresh t.TempDir() and returns its path.
func writeFrontendFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(fixtureIndexHTML), 0o644); err != nil {
		t.Fatalf("write fixture index.html: %v", err)
	}
	assets := filepath.Join(dir, "assets")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		t.Fatalf("mkdir fixture assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(assets, fixtureAssetName), fixtureAssetBody, 0o644); err != nil {
		t.Fatalf("write fixture asset: %v", err)
	}
	return dir
}

// serveFrontendRequest drives one request through buildServer's real
// composed handler with cfg.WebDistDir set to a fixture dist, returning
// the recorder.
func serveFrontendRequest(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	cfg := testConfig(t)
	cfg.WebDistDir = writeFrontendFixture(t)
	handler, cleanup, _, err := buildServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// TestFrontend_GETRoot_ServesIndexHTML is the acceptance blocker's first
// regression: an anonymous GET / must answer the app's page (the html
// carrying the mount point the bootstrap mounts into), never the tenancy
// refusal the path answered before this round (403
// tenancy.tenant_unresolved).
func TestFrontend_GETRoot_ServesIndexHTML(t *testing.T) {
	rec := serveFrontendRequest(t, http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200; body = %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(body, `id="root"`) {
		t.Errorf("GET / body does not carry the app mount point: %s", body)
	}
	if strings.Contains(body, "tenancy.tenant_unresolved") {
		t.Errorf("GET / body is a tenancy error, want the app page: %s", body)
	}
	// The page must reference its built assets the way the server serves
	// them (vite's default base "/"), or the sign-in page can never load.
	if !strings.Contains(body, `src="/assets/`+fixtureAssetName+`"`) {
		t.Errorf("GET / body does not reference the built asset path: %s", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("GET / Cache-Control = %q, want no-cache (index.html must never pin a stale deploy)", cc)
	}
}

// TestFrontend_GETBuiltAsset_ServesBytes is the acceptance blocker's
// second regression: a request for a built asset path answers 200 with
// the asset's own bytes, with the long-lived immutable cache header the
// hashed-name build makes safe.
func TestFrontend_GETBuiltAsset_ServesBytes(t *testing.T) {
	rec := serveFrontendRequest(t, http.MethodGet, "/assets/"+fixtureAssetName)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET asset status = %d, want 200; body = %s", rec.Code, rec.Body)
	}
	if got := rec.Body.Bytes(); string(got) != string(fixtureAssetBody) {
		t.Errorf("GET asset body = %q, want the fixture bytes %q", got, fixtureAssetBody)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("GET asset Content-Type = %q, want text/javascript", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("GET asset Cache-Control = %q, want the immutable hashed-asset policy", cc)
	}
}

// TestFrontend_UnknownNonAPIPath_ServesIndex is the acceptance blocker's
// third regression: an unknown non-API path answers the app's page (the
// hash-routed app's SPA fallback), not a middleware error.
func TestFrontend_UnknownNonAPIPath_ServesIndex(t *testing.T) {
	for _, target := range []string{
		"/some/deep/link",
		// The social-binding callback convention a real deployment serves
		// with its SPA fallback (web/README.md): a provider's redirect
		// lands on a real path, and the page that handles the exchange is
		// index.html.
		"/callback/social/demo-google",
	} {
		rec := serveFrontendRequest(t, http.MethodGet, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200; body = %s", target, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), `id="root"`) {
			t.Errorf("GET %s body is not the app page: %s", target, rec.Body)
		}
	}
}

// TestFrontend_APIBehaviorByteIdentical pins the "API paths keep exactly
// the current behavior" half of the round: with the frontend enabled, the
// answers the API surface gives are byte-for-byte the answers it gave
// before the round (the tenancy refusal for an anonymous unknown API path,
// the healthz "ok", and the /assets miss policy that never touches the
// API).
func TestFrontend_APIBehaviorByteIdentical(t *testing.T) {
	// Anonymous unknown API path: refused by the tenancy middleware with
	// the same exact JSON body the probe of the pre-round wiring answers
	// (tenancyUnresolvedBody) -- the frontend must never intercept /api.
	rec := serveFrontendRequest(t, http.MethodGet, "/api/v1/bogus-route")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/v1/bogus-route status = %d, want 403; body = %s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != tenancyUnresolvedBody {
		t.Errorf("GET /api/v1/bogus-route body = %q, want the byte-identical refusal %q", got, tenancyUnresolvedBody)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("GET /api/v1/bogus-route Content-Type = %q, want application/json", ct)
	}

	// /healthz stays the probe endpoint behind its own path, and the
	// frontend must not swallow it as an unknown path.
	rec = serveFrontendRequest(t, http.MethodGet, healthzPath)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("GET /healthz = %d %q, want 200 %q", rec.Code, rec.Body, "ok")
	}

	// A POST to an unknown non-API path is not a page navigation and the
	// frontend serves GET/HEAD only: it falls through to the composed
	// chain exactly as before.
	rec = serveFrontendRequest(t, http.MethodPost, "/some/deep/link")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /some/deep/link status = %d, want the unchanged 403; body = %s", rec.Code, rec.Body)
	}
}

// TestFrontend_Disabled_BehaviorsByteIdentical pins the opt-in property:
// a buildServer boot with no WebDistDir must answer exactly as the
// pre-round wiring did -- the frontend round changes nothing unless an
// operator (or a test) configures a dist directory.
func TestFrontend_Disabled_BehaviorsByteIdentical(t *testing.T) {
	cfg := testConfig(t)
	handler, cleanup, _, err := buildServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	for _, target := range []string{"/", "/assets/whatever.js", "/some/unknown/path"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s without WebDistDir status = %d, want the unchanged 403; body = %s", target, rec.Code, rec.Body)
		}
		if got := rec.Body.String(); got != tenancyUnresolvedBody {
			t.Errorf("GET %s without WebDistDir body = %q, want %q", target, got, tenancyUnresolvedBody)
		}
	}
}

// TestFrontend_AssetMiss_IsAReal404 pins the broken-deploy signal: a
// missing file under /assets/ answers 404, never index.html -- answering
// an html document for a script reference would hide a stale index.html
// behind a 200.
func TestFrontend_AssetMiss_IsAReal404(t *testing.T) {
	rec := serveFrontendRequest(t, http.MethodGet, "/assets/index-Gone.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing asset status = %d, want 404; body = %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `id="root"`) {
		t.Errorf("GET missing asset answered index.html, want a real 404")
	}
}

// TestFrontend_Head_AnswersHeadersOnly pins HEAD support on the static
// surface: the same headers as GET, no body.
func TestFrontend_Head_AnswersHeadersOnly(t *testing.T) {
	rec := serveFrontendRequest(t, http.MethodHead, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD / status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD / body = %q, want empty", rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("HEAD / Content-Type = %q, want text/html", ct)
	}
}

// TestFrontend_PathTraversal_StaysInsideTheDist pins the confinement
// rule: a path that tries to climb above the frontend directory with ".."
// segments (encoded or raw) can never serve a file from outside it -- the
// request either misses inside the directory (falling back to index.html
// or a 404) or falls through, but the response never carries a file the
// dist does not hold.
func TestFrontend_PathTraversal_StaysInsideTheDist(t *testing.T) {
	cfg := testConfig(t)
	dir := writeFrontendFixture(t)
	// A canary file outside the dist directory: no response below may
	// ever contain its bytes.
	canary := "CANARY-OUTSIDE-THE-DIST"
	cfg.WebDistDir = dir
	handler, cleanup, _, err := buildServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "canary.txt"), []byte(canary), 0o644); err != nil {
		t.Fatalf("write canary: %v", err)
	}
	for _, target := range []string{
		"/../canary.txt",
		"/assets/../../canary.txt",
		"/assets/..%2f..%2fcanary.txt",
		"/..%2fcanary.txt",
		"/%2e%2e/canary.txt",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if strings.Contains(rec.Body.String(), canary) {
			t.Fatalf("GET %s leaked the canary file outside the dist: %s", target, rec.Body)
		}
		// Whatever the answer is (index fallback for the cleaned paths
		// that miss inside the dist, a 404 for asset-shaped ones), it must
		// not be the canary's bytes and must not be a server error.
		if rec.Code >= http.StatusInternalServerError {
			t.Fatalf("GET %s status = %d, want no server error; body = %s", target, rec.Code, rec.Body)
		}
	}
}
