package app

// This file serves the reference app's built frontend -- the dist/
// directory examples/reference-app/web's `pnpm build` (tsc plus vite)
// emits -- from the same process that serves the API. An operator
// who points APP_WEB_DIST at a built dist/ gets the deployed product URL
// serving its own frontend, and the Dockerfile builds that dist into
// the image and sets the variable itself.
//
// The serving design is deliberate on one axis the name APP_WEB_DIST
// carries: the dist is served FROM DISK at runtime, never go:embed'd into
// the binary. A go:embed of this directory would have to exist at Go
// build time, which would force the generated, hash-named assets into the
// committed repository (web/dist is gitignored exactly like every other
// build output in this workspace -- see the root .gitignore's "dist/"
// pattern -- because the frontend and the backend move on independent
// cadences, and every Go CI leg of this app -- full-check's reference-app
// suite, api-contract.yml's regeneration build, a consumer's `go build`
// -- would then depend on a committed snapshot of hashed bundle files
// nobody regenerates before building). Reading the directory at runtime
// instead keeps every Go build and test independent of the frontend
// having been built, keeps the vite dev-server workflow of the web host
// working (the dev server serves the page
// and proxies /api to this process; this handler simply never intercepts
// anything unless APP_WEB_DIST names a real directory), and lets tests
// point the same code at a throwaway fixture directory.
//
// The interception rules keep the static surface strictly disjoint from
// the API surface:
//
//   - Only GET and HEAD requests are ever served from the frontend; every
//     other method falls through to the composed API handler unchanged.
//   - The paths this process's server surface owns are never intercepted:
//     everything under /api (every module route -- the admin console
//     included, which mounts at /api/v1/admin -- plus config's two pre-auth
//     endpoints), /healthz and /metrics. Those keep answering through the
//     ordinary tenancy/auth middleware chain, untouched by this wrapper. In
//     this app every mounted route lives under /api and the
//     two probe endpoints are the only non-API paths the server owns, so
//     the check below is the whole ownership list.
//   - Everything else -- "/" itself, a hashed asset under /assets/, a
//     deep link -- is served from the frontend directory: the file when it
//     exists, the SPA fallback (index.html, the app is hash-routed) when
//     an unknown path names no file, and a 404 when a path under /assets/
//     names no file. A missing hashed asset is a broken deploy (a stale
//     index.html referencing a bundle this copy does not carry); answering
//     it with index.html would make the browser execute an HTML document
//     as a script and hide the breakage behind a 200, so asset misses are
//     answered with a real 404 while extension-less unknown paths keep the
//     hash-routed app working.
//   - The static surface never falls into the authn/tenancy middleware
//     chain: withFrontend wraps the OUTSIDE of authn.Middleware's own
//     output in BuildServer, so a request the frontend answers never meets
//     tenant resolution (GET / answers 200 to a caller with no tenant,
//     exactly what a deployed sign-in page needs) and the API requests
//     that do reach the chain pass through unchanged.
//
// Path resolution is deliberately narrow and self-contained rather than a
// generic file server: request paths are pinned to the root before
// cleaning, so ".." segments collapse at the root instead of escaping it,
// and the cleaned relative path is joined onto the configured directory
// before opening -- a request can name no file outside APP_WEB_DIST
// through cleaning alone. The open site itself carries a same-function
// refusal of any ".."-bearing or absolute relative path (see ServeHTTP),
// so the confinement stays visible to taint analyzers that do not track
// the cleaning invariant across cleanWebRel's function boundary and stays
// in force even if that boundary is ever edited away. (A symlink INSIDE
// the dist directory pointing outside it is not followed-guarded, exactly
// as with net/http's own file server: the directory is the operator's own
// built output, and guarding symlinks would be defending the deployer
// against themselves.)

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// indexFile is the SPA fallback target and the answer for "/" itself.
// Hash-routed deep links never reach the server with a distinct path
// (their route lives after the "#"), but the app's social-binding callback
// convention -- <origin>/callback/social/<provider> -- is a real path a
// provider's redirect lands on, and answering it with index.html is what
// makes the app's own handler of that exchange reachable in production,
// exactly as the vite dev server's SPA fallback does in development.
const indexFile = "index.html"

// assetDir is vite's default assets directory name (no assetsDir override
// exists in examples/reference-app/web/vite.config.ts): every hashed
// bundle file the build emits lands under dist/assets/. The prefix is
// what this handler's cache policy and its miss behavior key on -- see
// cachePolicy and the package doc comment above.
const assetDir = "assets"

// cachePolicy returns the Cache-Control header value for a served file.
// The hashed assets under dist/assets/ are content-addressed by their
// names (vite appends a content hash to every emitted file), so a browser
// may hold them forever: the immutable directive is safe precisely
// because a changed file changes its name, and the one-year max-age makes
// a redeploy cost returning visitors nothing beyond a fresh index.html.
// index.html itself must never be cached: it is the file that references
// the hashed names, so a stale copy would pin a returning visitor to the
// previous deploy's assets.
func cachePolicy(rel string) string {
	if rel == assetDir || strings.HasPrefix(rel, assetDir+"/") {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

// assetPath reports whether rel names a file under the asset directory --
// the paths this handler answers with a real 404 when the file is missing
// rather than the SPA fallback (see the package doc comment above).
func assetPath(rel string) bool {
	return rel == assetDir || strings.HasPrefix(rel, assetDir+"/")
}

// frontend serves the built frontend living under dir in front of next,
// the composed API handler. Its zero value is not usable; build it with
// withFrontend.
type frontend struct {
	dir  string
	next http.Handler
}

// withFrontend wraps next -- BuildServer's fully composed API handler,
// authn.Middleware's output included -- so that requests the frontend
// answers never enter the authn/tenancy middleware chain, while every
// request the frontend does not answer reaches next byte-for-byte as it
// would have without the wrapper.
func withFrontend(dir string, next http.Handler) http.Handler {
	return &frontend{dir: dir, next: next}
}

// ServeHTTP implements http.Handler: serve the request from the frontend
// directory when it is one of the frontend's (see the package doc
// comment's interception rules), otherwise pass it to the composed API
// handler untouched.
func (f *frontend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		f.next.ServeHTTP(w, r)
		return
	}
	if serverOwnedPath(r.URL.Path) {
		f.next.ServeHTTP(w, r)
		return
	}
	rel, ok := cleanWebRel(r.URL.Path)
	if !ok {
		f.next.ServeHTTP(w, r)
		return
	}
	if rel == "" {
		// "/" itself: the app's entry page.
		f.serveIndex(w, r)
		return
	}
	// The refusal at the open site itself: rel is cleanWebRel's output, and
	// while cleanWebRel's root pin stands, every "/"-separated request is
	// confined before it reaches this point -- a "/"-pinned path.Clean
	// collapses every ".." segment into the root, and its result never
	// starts with "/" and is never empty here -- so on separator-"/"
	// platforms the refusal cannot fire from this request path. It exists
	// because taint analyzers do not track that cleaning invariant across
	// cleanWebRel's function boundary: CodeQL's go/path-injection fires at
	// the open below on every scan (this file's #nosec G703 comment records
	// gosec's same blindness), so the refusal truncates the tainted flow
	// where the analyzer can see it -- which is also what answers 404, never
	// the SPA fallback, if an edit ever removes the pin. filepath.IsLocal is
	// deliberately the whole clause: it is the one complete confinement
	// check the analyzers recognize for this purpose. CodeQL's
	// path-injection customization admits exactly filepath.IsLocal,
	// strings.Contains(x, ".."), strings.HasPrefix and filepath.Clean("/" +
	// x) results as path sanitizers -- never filepath.IsAbs and never a
	// "/../" literal -- and of those alternatives only IsLocal both refuses
	// a climb and stays visible: HasPrefix also guards the branch a ".."
	// climb never takes, and
	// cleanWebRel's own path.Clean is invisible to a model that knows only
	// the path/filepath package by name. On Windows builds the clause is
	// not belt-and-braces: IsLocal splits on the host separator, so a
	// backslash dot-dot climb ("assets\..\..\x" -- "\" is an ordinary
	// character to path.Clean and every forward-slash clause, but a
	// separator to the filepath.Join below) is refused too.
	if !filepath.IsLocal(rel) {
		http.NotFound(w, r)
		return
	}
	// #nosec G703 -- gosec's taint analysis cannot see the refusal above
	// (it evaluates no guard branches), any more than it can see the
	// root-pinning inside cleanWebRel; the joined name can never leave
	// f.dir either way.
	file, err := os.Open(filepath.Join(f.dir, filepath.FromSlash(rel)))
	switch {
	case err == nil:
		defer func() { _ = file.Close() }()
		f.serveOpened(w, r, rel, file)
	case errors.Is(err, fs.ErrNotExist) && !assetPath(rel):
		// Unknown non-API path: the hash-routed app's SPA fallback.
		f.serveIndex(w, r)
	default:
		http.NotFound(w, r)
	}
}

// serverOwnedPath reports whether p is a path this process's API/server
// surface owns and must keep answering exactly as it does when no
// frontend is configured: everything under /api -- every module route,
// the admin console at /api/v1/admin and config's two pre-auth endpoints
// included -- plus the two non-API probe endpoints, /healthz and
// /metrics. These are all the paths the composed handler registers
// outside the frontend's reach; in this app there is no other mount point
// to enumerate.
func serverOwnedPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/") ||
		p == HealthzPath || p == MetricsPath
}

// cleanWebRel returns p's cleaned form relative to the frontend directory
// ("" for the root itself), or ok == false when p cannot name a file
// inside that directory. The cleaning pins p to the root with a leading
// "/" before path.Clean collapses every empty and ".." segment, so a ".."
// can never climb above the root -- "../../../etc/passwd" collapses to
// "/etc/passwd", a name inside the frontend directory that simply does
// not exist there, never a path outside it. The trailing ".."-segment
// check is defensive: path.Clean of a root-pinned path cannot produce it,
// but the pin is exactly the property that keeps the check above safe, so
// the explicit refusal documents what happens if an edit removes it.
func cleanWebRel(p string) (string, bool) {
	cleaned := path.Clean("/" + p)
	if cleaned == "/" {
		return "", true
	}
	rel := strings.TrimPrefix(cleaned, "/")
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// serveIndex answers with the frontend's index.html -- the "/" answer and
// the SPA fallback for unknown non-API paths. indexFile is a package
// constant, never request-derived, so no traversal check is needed here.
func (f *frontend) serveIndex(w http.ResponseWriter, r *http.Request) {
	// #nosec G703 -- the path joined here is the constant indexFile under
	// f.dir, the operator-configured directory itself (APP_WEB_DIST); no
	// request data reaches this open, so there is no traversal surface.
	file, err := os.Open(filepath.Join(f.dir, indexFile))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = file.Close() }()
	f.serveOpened(w, r, indexFile, file)
}

// serveOpened serves the already-opened file under rel -- content type by
// extension and last-modified conditional handling both come from
// http.ServeContent, which also answers a HEAD request with headers only.
func (f *frontend) serveOpened(w http.ResponseWriter, r *http.Request, rel string, file *os.File) {
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		// A directory under the frontend root (only dist/assets/ exists in
		// a real build) is not a file to serve; ".." requests that resolve
		// to one get the same 404 an asset miss gets, never a listing.
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", cachePolicy(rel))
	// ServeContent needs an io.ReadSeeker for its conditional/range
	// handling; *os.File is one, so file is passed through directly.
	http.ServeContent(w, r, path.Base(rel), info.ModTime(), file)
}

// compile-time check that frontend implements http.Handler.
var _ http.Handler = (*frontend)(nil)
