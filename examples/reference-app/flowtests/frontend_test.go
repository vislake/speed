package flowtests

// Tests for internal/app/frontend.go's built-frontend serving: the httptest regressions
// of the acceptance blocker they discharge (an anonymous GET / answering
// the app's page instead of a tenancy refusal, a built asset answering its
// bytes, an unknown non-API path answering the SPA fallback, API behavior
// byte-identical), plus the serving rules internal/app/frontend.go wires and
// pkgcore/spa's package doc comment pins: opt-in via cfg.WebDistDir, asset
// misses as real 404s, the cache policy split, HEAD support, and
// path-traversal confinement to the configured directory.
//
// The fixture dist directory mirrors the real vite build output shape:
// examples/reference-app/web/index.html (whose body carries the
// <div id="root"></div> mount point and whose production build rewrites
// the module script to a hashed /assets/<name> reference -- no vite
// `base` override exists in that app's vite.config.ts, so the built
// index.html references its bundles at the root-absolute /assets/* paths
// this handler serves). The suite drives the real composed handler through
// BuildServer, never a mock of the wiring.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/apptest"

	"github.com/vislake/speed/examples/reference-app/internal/app"

	obs "github.com/vislake/speed/go/observability"
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

// serveFrontendRequest drives one request through BuildServer's real
// composed handler with cfg.WebDistDir set to a fixture dist, returning
// the recorder.
func serveFrontendRequest(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	cfg := apptest.ServerConfig(t)
	cfg.WebDistDir = writeFrontendFixture(t)
	handler, cleanup, _, err := app.BuildServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
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

// TestFrontend_GETRoot_ServesIndexHTML: an anonymous GET / must answer
// the app's page (the html
// carrying the mount point the bootstrap mounts into), never the tenancy
// refusal (403 tenancy.tenant_unresolved) an unintercepted unknown path
// gets.
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
		// The social-binding callback convention a real deployment serves:
		// a provider's redirect lands on a real path, and the page that
		// handles the exchange is index.html.
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
// their current behavior" property: with the frontend enabled, the
// answers the API surface gives are byte-for-byte unchanged (the tenancy
// refusal for an anonymous unknown API path,
// the healthz "ok", and the /assets miss policy that never touches the
// API).
func TestFrontend_APIBehaviorByteIdentical(t *testing.T) {
	// Anonymous unknown API path: refused by the tenancy middleware with
	// the exact JSON body tenancyUnresolvedBody -- the frontend must never
	// intercept /api.
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
	rec = serveFrontendRequest(t, http.MethodGet, obs.HealthzPath)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("GET /healthz = %d %q, want 200 %q", rec.Code, rec.Body, "ok")
	}

	// A POST to an unknown non-API path is not a page navigation and the
	// frontend serves GET/HEAD only: it falls through to the composed
	// chain unchanged.
	rec = serveFrontendRequest(t, http.MethodPost, "/some/deep/link")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /some/deep/link status = %d, want the unchanged 403; body = %s", rec.Code, rec.Body)
	}
}

// TestFrontend_Disabled_BehaviorsByteIdentical pins the opt-in property:
// a BuildServer boot with no WebDistDir must answer exactly as an
// unintercepted server does -- the frontend changes nothing unless an
// operator (or a test) configures a dist directory.
func TestFrontend_Disabled_BehaviorsByteIdentical(t *testing.T) {
	cfg := apptest.ServerConfig(t)
	handler, cleanup, _, err := app.BuildServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
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
	cfg := apptest.ServerConfig(t)
	dir := writeFrontendFixture(t)
	// A canary file outside the dist directory: no response below may
	// ever contain its bytes.
	canary := "CANARY-OUTSIDE-THE-DIST"
	cfg.WebDistDir = dir
	handler, cleanup, _, err := app.BuildServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
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

// TestFrontend_PathTraversal_NoEscapeShapeNamesOrReadsOutsideTheDist is the
// traversal battery behind the confinement rule, driven over the full shape
// family at the composed-server level: raw and percent-encoded dot-dot
// segments, multi-level ".." sequences, trailing-dot forms and dot-only
// paths, each answered while a canary file sitting one directory above the
// dist (a join that escaped the configured directory would land exactly on
// it) never leaks a byte. The suite's assertion is the confinement property
// itself -- no request can name or read a file outside the configured dist
// directory -- pinned over every shape, with the serving answer pinned per
// shape too: the root-pinned cleaning confines every escape attempt to a
// name inside the dist, which does not exist there (the SPA-fallback
// answer, index.html), is the dist's own index.html, or still sits under
// the cleaned /assets/ prefix (the asset-miss 404). A real hashed asset
// request closes the suite as the control: the same handler still serves
// the dist's genuine file when the path is a legitimate one.
func TestFrontend_PathTraversal_NoEscapeShapeNamesOrReadsOutsideTheDist(t *testing.T) {
	cfg := apptest.ServerConfig(t)
	dir := writeFrontendFixture(t)
	// A canary file outside the dist directory (the fixture's parent): any
	// join that escaped the configured directory would land on exactly this
	// file, so its bytes in a response are the leak signal.
	canary := "CANARY-OUTSIDE-THE-DIST"
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "canary.txt"), []byte(canary), 0o644); err != nil {
		t.Fatalf("write canary: %v", err)
	}
	cfg.WebDistDir = dir
	handler, cleanup, _, err := app.BuildServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	// wantPage and wantAssetMiss are the two confined answers an escape
	// shape can produce, each the serving rules' ordinary answer for the
	// CLEANED name, never an answer carrying an outside file: wantPage is
	// the root/fallback answer (index.html, status 200) for a cleaned name
	// that does not exist in the dist and is not under /assets/; wantAssetMiss
	// is the real 404 a missing file under the cleaned /assets/ prefix gets
	// (internal/app/frontend.go's broken-deploy signal). Which of the two a shape lands
	// on is a property of where its cleaned form sits, not of the escape
	// attempt -- the pin collapses every shape into the dist, and the dist's
	// own serving rules answer from there.
	const (
		wantPage = iota
		wantAssetMiss
	)
	// escapeTargets are the traversal shapes: dot-dot segments raw and in
	// their percent-encodings, single- and multi-level climbs, trailing-dot
	// and dot-only paths, and traversal disguised under the /assets/ prefix.
	escapeTargets := []struct {
		name   string
		target string
		want   int
	}{
		{"raw dot-dot from the root", "/../canary.txt", wantPage},
		{"encoded %2e%2e dot-dot", "/%2e%2e/canary.txt", wantPage},
		{"encoded %2f separator after dot-dot", "/..%2fcanary.txt", wantPage},
		{"fully encoded %2e%2e%2f climb", "/%2e%2e%2fcanary.txt", wantPage},
		{"dot-dot through the assets prefix", "/assets/../../canary.txt", wantPage},
		{"encoded climb through the assets prefix", "/assets/..%2f..%2fcanary.txt", wantPage},
		{"encoded segments through the assets prefix", "/assets/%2e%2e/%2e%2e/canary.txt", wantPage},
		{"single-level climb from a subdirectory", "/a/../canary.txt", wantPage},
		{"two-level climb from a nested path", "/a/b/../../canary.txt", wantPage},
		{"three-level climb from a deeper path", "/a/b/c/../../../canary.txt", wantPage},
		{"staggered climbs from a nested path", "/a/../b/../../canary.txt", wantPage},
		{"climb back through a named file", "/canary.txt/../../canary.txt", wantPage},
		{"encoded climb from a subdirectory", "/a/..%2f..%2fcanary.txt", wantPage},
		{"climb to the root from one level", "/a/../..", wantPage},
		{"climb to the root from two levels", "/a/b/../../..", wantPage},
		{"assets climb to the root", "/assets/..", wantPage},
		{"bare dot-dot", "/..", wantPage},
		{"trailing dot on the file name", "/canary.txt.", wantPage},
		{"encoded trailing dot on the file name", "/canary.txt%2e", wantPage},
		{"leading dot segment", "/./canary.txt", wantPage},
		{"dot segment under assets, staying under assets", "/assets/./canary.txt", wantAssetMiss},
		{"dot-only file name", "/...", wantPage},
		{"dot-dot with a trailing space", "/..%20/canary.txt", wantPage},
		{"staggered dot-dot segments", "/.././../canary.txt", wantPage},
	}
	for _, tt := range escapeTargets {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.target, nil))
			if strings.Contains(rec.Body.String(), canary) {
				t.Fatalf("GET %s leaked the canary file outside the dist: %s", tt.target, rec.Body)
			}
			if rec.Code >= http.StatusInternalServerError {
				t.Fatalf("GET %s status = %d, want no server error; body = %s", tt.target, rec.Code, rec.Body)
			}
			switch tt.want {
			case wantPage:
				// Confinement collapses the shape to a name inside the dist
				// that does not exist there, so the answer is the ordinary
				// unknown-path one: index.html with status 200, never a 404
				// for an escape (there is nothing to 404 over) and never a
				// file from outside the directory.
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s status = %d, want the confined 200 answer; body = %s", tt.target, rec.Code, rec.Body)
				}
				if body := rec.Body.String(); !strings.Contains(body, `id="root"`) {
					t.Fatalf("GET %s answered %q, want the app page (the confined answer); no outside file may ever appear", tt.target, body)
				}
			case wantAssetMiss:
				// The cleaned name still sits under /assets/, so the miss
				// answers with the serving rules' real 404 -- a broken-deploy
				// signal for an asset that does not exist, never index.html
				// and never an outside file.
				if rec.Code != http.StatusNotFound {
					t.Fatalf("GET %s status = %d, want the confined 404 (asset-shaped miss); body = %s", tt.target, rec.Code, rec.Body)
				}
				if body := rec.Body.String(); strings.Contains(body, `id="root"`) {
					t.Fatalf("GET %s answered index.html for an asset-shaped miss, want the real 404", tt.target)
				}
			}
		})
	}

	// Traversal disguised at the top of a real, in-dist name still only
	// ever opens the dist's own file: /assets/../index.html cleans to
	// /index.html, which exists inside the fixture -- the answer is the
	// fixture's own index.html bytes, never a sibling file.
	for _, target := range []string{"/assets/../index.html", "/../index.html", "/index.html"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200; body = %s", target, rec.Code, rec.Body)
		}
		if body := rec.Body.String(); !strings.Contains(body, `id="root"`) || strings.Contains(body, canary) {
			t.Fatalf("GET %s answered %q, want the dist's own index.html and never the canary", target, body)
		}
	}

	// Control: the legitimate hashed-asset request the dist genuinely holds
	// still serves its bytes through the same handler, so the battery's
	// refusals are not over-broad -- only paths that could name a file
	// outside the dist are confined.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/"+fixtureAssetName, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /assets/%s status = %d, want 200; body = %s", fixtureAssetName, rec.Code, rec.Body)
	}
	if got := rec.Body.Bytes(); string(got) != string(fixtureAssetBody) {
		t.Errorf("GET /assets/%s body = %q, want the fixture bytes %q", fixtureAssetName, got, fixtureAssetBody)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("GET /assets/%s Cache-Control = %q, want the immutable hashed-asset policy", fixtureAssetName, cc)
	}
}
