package spa

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fixtureIndexHTML mirrors the shape a default vite build emits: the mount
// point the app's bootstrap renders into, plus the module script rewritten to
// a hashed, root-absolute /assets/<name> reference (vite's default base "/",
// with a content hash in every emitted name).
const fixtureIndexHTML = `<!doctype html>
<html lang="en">
  <head><title>spa fixture %s</title></head>
  <body>
    <div id="root"></div>
    <script type="module" src="/assets/index-F1xTur3.js"></script>
  </body>
</html>
`

// fixtureAssetName is the hashed bundle file the fixture index.html
// references; fixtureAssetBody its bytes.
const fixtureAssetName = "index-F1xTur3.js"

var fixtureAssetBody = []byte("console.log('fixture asset');\n")

// nextStub stands in for the wrapped handler: it records every request it
// receives and answers a status and body no frontend answer shares, so a
// test can tell passthrough from a frontend answer at a glance.
type nextStub struct {
	mu    sync.Mutex
	paths []string
}

func (n *nextStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	n.paths = append(n.paths, r.Method+" "+r.URL.Path)
	n.mu.Unlock()
	w.WriteHeader(http.StatusTeapot)
	_, _ = w.Write([]byte("wrapped handler"))
}

func (n *nextStub) requests() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.paths...)
}

// writeFrontendFixture writes a minimal real-shaped dist directory into dir
// (which the caller owns -- usually a fresh t.TempDir()): index.html carrying
// marker so two fixtures can be told apart, plus the hashed asset under
// assets/. It returns dir for convenient value-style use.
func writeFrontendFixture(t *testing.T, dir, marker string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir fixture assets: %v", err)
	}
	index := fmt.Sprintf(fixtureIndexHTML, marker)
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(index), 0o644); err != nil {
		t.Fatalf("write fixture index.html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", fixtureAssetName), fixtureAssetBody, 0o644); err != nil {
		t.Fatalf("write fixture asset: %v", err)
	}
	return dir
}

// do drives one request through h and returns the recorder.
func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// TestNew_GETRoot_ServesIndexHTML pins the entry answer: an anonymous GET /
// is answered with the app's page, its content type and the no-cache policy
// that keeps a redeploy from pinning a returning visitor to a stale
// index.html (and with it a stale asset set).
func TestNew_GETRoot_ServesIndexHTML(t *testing.T) {
	h := New(writeFrontendFixture(t, t.TempDir(), "root"), &nextStub{})

	rec := do(t, h, http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200; body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / Content-Type = %q, want text/html", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, `id="root"`) || !strings.Contains(body, "/assets/"+fixtureAssetName) {
		t.Errorf("GET / body is not the built page: %s", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != defaultCacheControl {
		t.Errorf("GET / Cache-Control = %q, want %q", cc, defaultCacheControl)
	}
}

// TestNew_GETBuiltAsset_ServesBytesWithImmutableCache pins the asset answer:
// a built asset path is served with its own bytes, its content type and the
// immutable cache policy the hashed name makes safe.
func TestNew_GETBuiltAsset_ServesBytesWithImmutableCache(t *testing.T) {
	h := New(writeFrontendFixture(t, t.TempDir(), "asset"), &nextStub{})

	rec := do(t, h, http.MethodGet, "/assets/"+fixtureAssetName)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET asset status = %d, want 200; body = %s", rec.Code, rec.Body)
	}
	if got := rec.Body.Bytes(); string(got) != string(fixtureAssetBody) {
		t.Errorf("GET asset body = %q, want the fixture bytes %q", got, fixtureAssetBody)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("GET asset Content-Type = %q, want text/javascript", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != assetCacheControl {
		t.Errorf("GET asset Cache-Control = %q, want %q", cc, assetCacheControl)
	}
}

// TestNew_UnknownPath_FallsBackToIndex pins the SPA fallback: an unknown path
// outside the asset directory is answered with the entry document, which is
// what makes a client-side route reachable on a fresh load or a deep link
// (the route itself lives after the "#" or is resolved by the app).
func TestNew_UnknownPath_FallsBackToIndex(t *testing.T) {
	h := New(writeFrontendFixture(t, t.TempDir(), "fallback"), &nextStub{})

	for _, target := range []string{"/some/deep/link", "/callback/social/demo-google"} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200; body = %s", target, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), `id="root"`) {
			t.Errorf("GET %s body is not the built page: %s", target, rec.Body)
		}
	}
}

// TestNew_AssetMiss_IsAReal404 pins the broken-deploy signal: a missing file
// under the asset prefix answers 404, never index.html -- answering an HTML
// document for a script reference would hide the stale index.html behind a
// 200 and surface the breakage as a script error instead.
func TestNew_AssetMiss_IsAReal404(t *testing.T) {
	h := New(writeFrontendFixture(t, t.TempDir(), "miss"), &nextStub{})

	rec := do(t, h, http.MethodGet, "/assets/index-Gone.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing asset status = %d, want 404; body = %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `id="root"`) {
		t.Error("GET missing asset answered index.html, want a real 404")
	}
}

// TestNew_NonGetHead_PassesThrough pins the method rule: only GET and HEAD
// are ever served from the frontend directory; every other method -- which is
// never a page navigation -- reaches the wrapped handler unchanged.
func TestNew_NonGetHead_PassesThrough(t *testing.T) {
	next := &nextStub{}
	h := New(writeFrontendFixture(t, t.TempDir(), "methods"), next)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := do(t, h, method, "/some/deep/link")
		if rec.Code != http.StatusTeapot || rec.Body.String() != "wrapped handler" {
			t.Fatalf("%s /some/deep/link reached the frontend (status %d, body %q), want the wrapped handler", method, rec.Code, rec.Body)
		}
	}
	if got := next.requests(); len(got) != 4 {
		t.Errorf("wrapped handler saw %d requests, want 4: %v", len(got), got)
	}
}

// TestNew_Head_AnswersHeadersOnly pins HEAD support on the static surface:
// the same headers as GET, no body.
func TestNew_Head_AnswersHeadersOnly(t *testing.T) {
	h := New(writeFrontendFixture(t, t.TempDir(), "head"), &nextStub{})

	rec := do(t, h, http.MethodHead, "/")
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

// TestNew_ServerPath_ExactMatchOnly pins WithServerPath's exact-match
// semantics: the declared path passes through to the wrapped handler even
// though a frontend file could answer it, and no other path is covered by
// the declaration.
func TestNew_ServerPath_ExactMatchOnly(t *testing.T) {
	next := &nextStub{}
	h := New(writeFrontendFixture(t, t.TempDir(), "serverpath"), next, WithServerPath("/healthz"))

	rec := do(t, h, http.MethodGet, "/healthz")
	if rec.Code != http.StatusTeapot || rec.Body.String() != "wrapped handler" {
		t.Fatalf("GET /healthz reached the frontend (status %d, body %q), want the wrapped handler", rec.Code, rec.Body)
	}

	// A path merely sharing the leading characters is not covered by an
	// exact declaration (and does not exist in the fixture, so the frontend
	// answers it with the index fallback).
	rec = do(t, h, http.MethodGet, "/healthzz")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("GET /healthzz status = %d, body = %q, want the frontend's index fallback", rec.Code, rec.Body)
	}
}

// TestNew_ServerPath_TrailingSlashSpellingPassesThrough pins the exact path
// declaration's trailing-slash handling: "/healthz/" and "/healthz" name
// the same endpoint -- cleanRel serves both from the same cleaned name --
// so both spellings must reach the wrapped handler, never the frontend's
// index fallback. The declaration's boundary is still exact (a deeper
// segment is not covered), and the slash normalization runs in both
// directions: a declaration written with the trailing slash ("/metrics/")
// covers the slashless request symmetrically.
func TestNew_ServerPath_TrailingSlashSpellingPassesThrough(t *testing.T) {
	next := &nextStub{}
	h := New(writeFrontendFixture(t, t.TempDir(), "serverpath-trailing"), next, WithServerPath("/healthz"))

	rec := do(t, h, http.MethodGet, "/healthz/")
	if rec.Code != http.StatusTeapot || rec.Body.String() != "wrapped handler" {
		t.Fatalf("GET /healthz/ reached the frontend (status %d, body %q), want the wrapped handler", rec.Code, rec.Body)
	}

	// Not a subtree declaration: a deeper segment under the declared path
	// still belongs to the frontend (the fixture holds no such file, so the
	// index fallback answers).
	rec = do(t, h, http.MethodGet, "/healthz/sub")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("GET /healthz/sub status = %d, body = %q, want the frontend's index fallback", rec.Code, rec.Body)
	}

	// The symmetric spelling: a declaration written with the trailing
	// slash covers the slashless request too.
	slashDeclared := New(writeFrontendFixture(t, t.TempDir(), "serverpath-metrics"), &nextStub{}, WithServerPath("/metrics/"))
	rec = do(t, slashDeclared, http.MethodGet, "/metrics")
	if rec.Code != http.StatusTeapot || rec.Body.String() != "wrapped handler" {
		t.Fatalf("GET /metrics under a trailing-slash declaration reached the frontend (status %d, body %q), want the wrapped handler", rec.Code, rec.Body)
	}
}

// TestNew_ServerPrefix_WholeSegmentBoundary pins WithServerPrefix's boundary:
// the prefix itself and everything nested below it pass through, while a path
// that merely shares the leading characters is still the frontend's.
func TestNew_ServerPrefix_WholeSegmentBoundary(t *testing.T) {
	next := &nextStub{}
	h := New(writeFrontendFixture(t, t.TempDir(), "serverprefix"), next, WithServerPrefix("/api"))

	for _, target := range []string{"/api", "/api/v1/orders", "/api/v1/admin/tenants"} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusTeapot || rec.Body.String() != "wrapped handler" {
			t.Fatalf("GET %s reached the frontend (status %d, body %q), want the wrapped handler", target, rec.Code, rec.Body)
		}
	}

	rec := do(t, h, http.MethodGet, "/apifoo")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("GET /apifoo status = %d, body = %q, want the frontend's index fallback (no prefix match across a segment boundary)", rec.Code, rec.Body)
	}
}

// TestNew_AssetPrefix_RenamesTheAssetSurface pins WithAssetPrefix: the named
// directory carries the immutable cache policy and the real-404 miss
// behavior, while a path under the default name the build does not use loses
// that policy entirely (it is an ordinary unknown path, so it falls back).
func TestNew_AssetPrefix_RenamesTheAssetSurface(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bundles"), 0o755); err != nil {
		t.Fatalf("mkdir bundles: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bundles", "app-1234.js"), fixtureAssetBody, 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(fmt.Sprintf(fixtureIndexHTML, "prefix")), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	h := New(dir, &nextStub{}, WithAssetPrefix("bundles"))

	rec := do(t, h, http.MethodGet, "/bundles/app-1234.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET bundle status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != assetCacheControl {
		t.Errorf("GET bundle Cache-Control = %q, want %q", cc, assetCacheControl)
	}

	rec = do(t, h, http.MethodGet, "/bundles/gone-9999.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing bundle status = %d, want the real 404", rec.Code)
	}

	rec = do(t, h, http.MethodGet, "/assets/gone-9999.js")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("GET /assets/... under a renamed asset prefix status = %d, body = %q, want the ordinary index fallback", rec.Code, rec.Body)
	}
}

// TestNew_EmptyAssetPrefix_DisablesTheAssetPolicy pins the documented
// disabled shape: with no asset directory declared, there is no real-404
// miss -- every unknown path, under any name, falls back to the index.
func TestNew_EmptyAssetPrefix_DisablesTheAssetPolicy(t *testing.T) {
	h := New(writeFrontendFixture(t, t.TempDir(), "noassets"), &nextStub{}, WithAssetPrefix(""))

	rec := do(t, h, http.MethodGet, "/assets/gone-9999.js")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("GET /assets/gone-9999.js with the asset policy disabled status = %d, body = %q, want the index fallback", rec.Code, rec.Body)
	}
}

// TestNew_DirectoryRequest_IsA404 pins that a request resolving to a
// directory under the root (the asset directory itself in a real build) is
// answered with a 404, never a listing.
func TestNew_DirectoryRequest_IsA404(t *testing.T) {
	h := New(writeFrontendFixture(t, t.TempDir(), "dir"), &nextStub{})

	for _, target := range []string{"/assets", "/assets/"} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want 404 (no directory listing)", target, rec.Code)
		}
	}
}

// TestNew_NonAssetFile_IsServedNoCache pins the cache policy's other half: a
// real file outside the asset directory (a favicon, say) is served, and with
// the no-cache policy -- its name is not content-addressed, so it must not be
// pinned in a browser cache across deploys.
func TestNew_NonAssetFile_IsServedNoCache(t *testing.T) {
	dir := writeFrontendFixture(t, t.TempDir(), "favicon")
	if err := os.WriteFile(filepath.Join(dir, "favicon.ico"), []byte("icon bytes"), 0o644); err != nil {
		t.Fatalf("write favicon: %v", err)
	}
	h := New(dir, &nextStub{})

	rec := do(t, h, http.MethodGet, "/favicon.ico")
	if rec.Code != http.StatusOK || rec.Body.String() != "icon bytes" {
		t.Fatalf("GET /favicon.ico = %d %q, want 200 %q", rec.Code, rec.Body, "icon bytes")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != defaultCacheControl {
		t.Errorf("GET /favicon.ico Cache-Control = %q, want %q", cc, defaultCacheControl)
	}
}

// TestNew_QueryString_ServedAsPath pins that routing ignores the query
// string: a page request with a query is answered by the same path rules
// (here, the fallback), never 404ed.
func TestNew_QueryString_ServedAsPath(t *testing.T) {
	h := New(writeFrontendFixture(t, t.TempDir(), "query"), &nextStub{})

	rec := do(t, h, http.MethodGet, "/some/deep/link?utm_source=test")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("GET /some/deep/link?utm_source=test status = %d, body = %q, want the index fallback", rec.Code, rec.Body)
	}
}

// TestNew_Traversal_StaysInsideTheRoot is the traversal battery: every escape
// shape -- raw and percent-encoded dot-dot segments, multi-level climbs,
// trailing-dot and dot-only forms, climbs hidden under the asset prefix --
// must be answered from inside the configured root. A canary file sitting one
// directory above the root (where an escaping join would land) must never
// appear in any response, no answer may be a server error, and each shape
// must land on the ordinary answer for its cleaned in-root name: the index
// fallback for a non-asset path, the real 404 for an asset-shaped miss.
func TestNew_Traversal_StaysInsideTheRoot(t *testing.T) {
	dir := writeFrontendFixture(t, t.TempDir(), "traversal")
	const canary = "CANARY-OUTSIDE-THE-ROOT"
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "canary.txt"), []byte(canary), 0o644); err != nil {
		t.Fatalf("write canary: %v", err)
	}
	h := New(dir, &nextStub{})

	cases := []struct {
		name   string
		target string
		want   int
	}{
		{"raw dot-dot from the root", "/../canary.txt", http.StatusOK},
		{"encoded %2e%2e dot-dot", "/%2e%2e/canary.txt", http.StatusOK},
		{"encoded %2f separator after dot-dot", "/..%2fcanary.txt", http.StatusOK},
		{"fully encoded %2e%2e%2f climb", "/%2e%2e%2fcanary.txt", http.StatusOK},
		{"dot-dot through the assets prefix", "/assets/../../canary.txt", http.StatusOK},
		{"encoded climb through the assets prefix", "/assets/..%2f..%2fcanary.txt", http.StatusOK},
		{"encoded segments through the assets prefix", "/assets/%2e%2e/%2e%2e/canary.txt", http.StatusOK},
		{"single-level climb from a subdirectory", "/a/../canary.txt", http.StatusOK},
		{"two-level climb from a nested path", "/a/b/../../canary.txt", http.StatusOK},
		{"staggered climbs from a nested path", "/a/../b/../../canary.txt", http.StatusOK},
		{"climb back through a named file", "/canary.txt/../../canary.txt", http.StatusOK},
		{"climb to the root from one level", "/a/../..", http.StatusOK},
		{"trailing dot on the file name", "/canary.txt.", http.StatusOK},
		{"encoded trailing dot on the file name", "/canary.txt%2e", http.StatusOK},
		{"leading dot segment", "/./canary.txt", http.StatusOK},
		{"dot-only file name", "/...", http.StatusOK},
		{"dot-dot with a trailing space", "/..%20/canary.txt", http.StatusOK},
		{"staggered dot-dot segments", "/.././../canary.txt", http.StatusOK},
		{"dot segment under assets, staying under assets", "/assets/./canary.txt", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, tc.target)
			if strings.Contains(rec.Body.String(), canary) {
				t.Fatalf("GET %s leaked the canary file outside the root: %s", tc.target, rec.Body)
			}
			if rec.Code != tc.want {
				t.Fatalf("GET %s status = %d, want %d (the confined answer); body = %s", tc.target, rec.Code, tc.want, rec.Body)
			}
			if rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), `id="root"`) {
				t.Fatalf("GET %s answered %q, want the index fallback", tc.target, rec.Body)
			}
		})
	}

	// Control: the legitimate hashed-asset request the fixture holds still
	// serves its bytes, so the battery's confinement is not over-broad.
	rec := do(t, h, http.MethodGet, "/assets/"+fixtureAssetName)
	if rec.Code != http.StatusOK || rec.Body.String() != string(fixtureAssetBody) {
		t.Fatalf("GET /assets/%s = %d %q, want the fixture bytes", fixtureAssetName, rec.Code, rec.Body)
	}

	// A traversal disguised at the top of a real, in-root name still only
	// ever opens the root's own file: /assets/../index.html cleans to
	// /index.html, which exists -- the answer is the root's own index.html,
	// never a sibling file.
	for _, target := range []string{"/assets/../index.html", "/../index.html", "/index.html"} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200; body = %s", target, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `id="root"`) || strings.Contains(body, canary) {
			t.Fatalf("GET %s answered %q, want the root's own index.html and never the canary", target, body)
		}
	}
}

// TestNew_SymlinkInsideRoot_FollowsToItsTarget pins the documented symlink
// stance: a symlink inside the root is followed exactly as net/http's own
// file server follows one -- the directory is the operator's own built
// output, and guarding against symlinks in it would be defending the
// deployer against themselves.
func TestNew_SymlinkInsideRoot_FollowsToItsTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	dir := writeFrontendFixture(t, t.TempDir(), "symlink")
	target := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(target, []byte("linked content"), 0o644); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	h := New(dir, &nextStub{})

	rec := do(t, h, http.MethodGet, "/link.txt")
	if rec.Code != http.StatusOK || rec.Body.String() != "linked content" {
		t.Fatalf("GET /link.txt = %d %q, want the followed symlink target's bytes", rec.Code, rec.Body)
	}
}

// TestNew_MissingRoot_Answers404AndPassesServerPaths pins the missing-dist
// behavior: a root that does not exist is not an error at construction --
// the frontend's own surface answers a plain 404 (never a server error or a
// panic), while every declared server path keeps passing through to the
// wrapped handler, so a host can mount the handler before its dist exists.
func TestNew_MissingRoot_Answers404AndPassesServerPaths(t *testing.T) {
	next := &nextStub{}
	missing := filepath.Join(t.TempDir(), "not-built")
	h := New(missing, next, WithServerPrefix("/api"), WithServerPath("/healthz"))

	for _, target := range []string{"/", "/assets/whatever.js", "/some/deep/link"} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s with a missing root status = %d, want 404; body = %s", target, rec.Code, rec.Body)
		}
	}

	for _, target := range []string{"/api/v1/orders", "/healthz"} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusTeapot {
			t.Fatalf("GET %s with a missing root status = %d, want the wrapped handler's answer", target, rec.Code)
		}
	}
}

// TestNew_RelativeDir_PinnedAtConstruction pins the root resolution: a
// relative root is resolved against the working directory once, when the
// handler is built, so a later chdir in the process cannot retarget what the
// handler serves.
func TestNew_RelativeDir_PinnedAtConstruction(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	writeFrontendFixture(t, filepath.Join(first, "dist"), "first-tree")
	writeFrontendFixture(t, filepath.Join(second, "dist"), "second-tree")

	t.Chdir(first)
	h := New("dist", &nextStub{})
	t.Chdir(second)

	rec := do(t, h, http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200; body = %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "first-tree") {
		t.Fatalf("GET / after chdir answered %q, want the tree pinned at construction (first-tree)", body)
	}
	if strings.Contains(body, "second-tree") {
		t.Fatalf("GET / after chdir answered the second tree; the root was not pinned at construction: %s", body)
	}
}

// TestNew_NilNext_Panics pins the one wiring mistake New refuses: a nil
// wrapped handler would panic at the first request that needs to fall
// through, so it panics at construction instead.
func TestNew_NilNext_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New(dir, nil) did not panic, want a wiring-mistake panic")
		}
	}()
	New(t.TempDir(), nil)
}
