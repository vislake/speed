// Package spa serves a built single-page frontend from a directory on disk in
// front of a host's composed HTTP handler.
//
// The output it serves is the ordinary shape of a frontend build: a root
// directory holding index.html plus a content-addressed asset directory
// (vite's default build emits one named "assets"). Requests are answered from
// that directory; everything the directory does not own passes through to the
// wrapped handler untouched.
//
// # Interception rules
//
//   - Only GET and HEAD requests are ever served from the directory. Every
//     other method falls through to the wrapped handler unchanged.
//   - Paths declared with WithServerPath and WithServerPrefix belong to the
//     wrapped handler and are never intercepted. The caller must declare
//     every path its composed server owns outside those prefixes -- in a
//     typical host, the liveness and metrics endpoints mounted beside the
//     API -- or such a path would be answered from the directory (or fall
//     back to index.html) instead of reaching the handler that owns it.
//   - Everything else is served from the directory: the file when it exists,
//     index.html when an unknown path names no file -- the SPA fallback a
//     deep-linked route depends on, since the route lives client-side -- and
//     a real 404 when a path under the asset prefix names no file. An asset
//     miss deliberately never falls back: a missing hashed asset is the
//     signature of a broken deploy (a stale index.html referencing a bundle
//     this copy does not carry), and answering it with index.html would make
//     the browser execute an HTML document as a script and hide the breakage
//     behind a 200.
//
// # Cache policy
//
// Files under the asset prefix are content-addressed by their names (a
// default vite build appends a content hash to every emitted file), so they
// are served "public, max-age=31536000, immutable": a changed file changes
// its name, so a returning visitor's cached copy of any given name can never
// be stale. Everything else -- index.html above all, the file that references
// the hashed names -- is served "no-cache", so a redeploy reaches returning
// visitors immediately instead of pinning them to previous deploy's assets.
//
// # Confinement and the file system
//
// The root is resolved to an absolute path when the handler is built, once,
// so the tree this handler serves cannot be retargeted later in the process's
// life (a chdir never changes what "/" answers). Request paths are pinned to
// the root before cleaning, so ".." segments collapse at the root instead of
// climbing it, and the cleaned relative path must additionally satisfy
// filepath.IsLocal before it is joined onto the root: a request can name no
// file outside the root through cleaning alone, and the second check keeps
// that confinement visible to static analyzers and in force if the cleaning
// is ever edited away.
//
// A symlink INSIDE the root pointing outside it is followed, exactly as
// net/http's own file server follows one: the directory is the operator's own
// built output, and guarding against symlinks in it would be defending the
// deployer against themselves.
//
// A root that does not exist, or that holds no index.html, is not an error at
// construction: requests that would be served from the directory answer a
// plain 404 and every declared server path still passes through to the
// wrapped handler -- the API-only behavior of a host whose frontend has not
// been built or deployed yet, which is also what keeps the handler mountable
// before the dist exists.
package spa

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// indexFile is the fallback target and the answer for "/" itself: the entry
// document of the built app, which every client-side route is served.
const indexFile = "index.html"

// defaultAssetPrefix is the asset directory name a default vite build emits
// (that app carries no assetsDir override); WithAssetPrefix renames it.
const defaultAssetPrefix = "assets"

// assetCacheControl is the Cache-Control value for files under the asset
// prefix. The immutable directive is safe precisely because the build
// content-addresses every file's name, and the one-year max-age makes a
// redeploy cost a returning visitor nothing beyond a fresh index.html.
const assetCacheControl = "public, max-age=31536000, immutable"

// defaultCacheControl is the Cache-Control value for everything else,
// index.html above all: the file that references the hashed asset names must
// itself never be cached, or a stale copy would pin a returning visitor to
// the previous deploy's assets.
const defaultCacheControl = "no-cache"

// Handler serves a built frontend directory in front of another
// http.Handler. Build one with New; its zero value is not usable.
type Handler struct {
	// root is the served tree, pinned to an absolute path by New.
	root string
	// next receives every request the frontend does not answer.
	next http.Handler
	// serverPaths and serverPrefixes declare the wrapped handler's own
	// surface (see the package doc comment's interception rules).
	serverPaths    []string
	serverPrefixes []string
	// assetPrefix names the content-addressed asset directory; empty
	// disables the asset policy entirely.
	assetPrefix string
}

// Option configures a Handler at construction time.
type Option func(*Handler)

// WithServerPath declares one exact request path the wrapped handler owns:
// requests for it always pass through to the wrapped handler, never served
// from the frontend directory. The declaration covers the path's
// trailing-slash spelling too ("/metrics/" for "/metrics"): the two name the
// same endpoint, so both spellings reach the handler. Declare every exact
// path the composed server mounts outside a declared prefix (a host's
// liveness and metrics endpoints, typically) -- a path the frontend
// intercepts by mistake would answer from disk, or 404, instead of reaching
// its handler.
func WithServerPath(p string) Option {
	return func(h *Handler) { h.serverPaths = append(h.serverPaths, p) }
}

// WithServerPrefix declares a path prefix the wrapped handler owns: the
// prefix itself and every path nested below it pass through, while a path
// that merely shares the leading characters does not. Declaring "/api"
// covers "/api" and "/api/v1/orders", never "/apifoo".
func WithServerPrefix(prefix string) Option {
	return func(h *Handler) { h.serverPrefixes = append(h.serverPrefixes, prefix) }
}

// WithAssetPrefix declares the content-addressed asset directory's name,
// defaulting to "assets" (the directory a default vite build emits): files
// under it are served with the immutable cache policy, and a miss under it
// answers a real 404 instead of the index.html fallback. An empty name
// disables the asset policy entirely -- every file is served no-cache and
// every miss falls back -- for a build whose output carries no
// content-addressed directory at all.
func WithAssetPrefix(name string) Option {
	return func(h *Handler) { h.assetPrefix = name }
}

// New builds a Handler serving the built frontend in dir in front of next.
//
// dir is the frontend build output's root directory (the host's own
// configuration decides which one). It is resolved to an absolute path here,
// once, so a later chdir in the process cannot retarget what the handler
// serves; when it cannot be resolved -- a relative dir in a process with no
// working directory at all -- the cleaned input is kept, since request
// containment never depends on the root being absolute (see the package doc
// comment's confinement section).
//
// Whether to build a Handler at all is the caller's decision: a host with no
// built frontend simply never calls New. A root that does not exist is not an
// error here -- the handler degrades to 404s for its own surface while every
// declared server path keeps passing through.
//
// next must not be nil; a nil handler is a wiring mistake, never a choice,
// and panics here rather than at the first request that needs to fall
// through.
func New(dir string, next http.Handler, opts ...Option) *Handler {
	if next == nil {
		panic("spa: New requires a non-nil next handler")
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		root = filepath.Clean(dir)
	}
	h := &Handler{root: root, next: next, assetPrefix: defaultAssetPrefix}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// compile-time check that Handler implements http.Handler.
var _ http.Handler = (*Handler)(nil)

// ServeHTTP implements http.Handler: serve the request from the frontend
// directory when it is one of the frontend's (see the package doc comment's
// interception rules), otherwise pass it to the wrapped handler untouched.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.next.ServeHTTP(w, r)
		return
	}
	if h.serverOwned(r.URL.Path) {
		h.next.ServeHTTP(w, r)
		return
	}
	rel, ok := cleanRel(r.URL.Path)
	if !ok {
		h.next.ServeHTTP(w, r)
		return
	}
	if rel == "" {
		// "/" itself: the app's entry page.
		h.serveIndex(w, r)
		return
	}
	// The refusal at the open site itself: rel is cleanRel's output, and
	// while cleanRel's root pin stands, every "/"-separated request is
	// confined before it reaches this point -- a "/"-pinned path.Clean
	// collapses every ".." segment into the root, and its result never
	// starts with "/" and is never empty here -- so on separator-"/"
	// platforms the refusal cannot fire from this request path. It exists
	// because taint analyzers do not track that cleaning invariant across
	// cleanRel's function boundary: CodeQL's go/path-injection fires at the
	// open below on every scan (this file's #nosec G703 comment records
	// gosec's same blindness), so the refusal truncates the tainted flow
	// where the analyzer can see it -- which is also what answers 404, never
	// the index fallback, if an edit ever removes the pin. filepath.IsLocal
	// is deliberately the whole clause: it is the one complete confinement
	// check the analyzers recognize for this purpose. CodeQL's
	// path-injection customization admits exactly filepath.IsLocal,
	// strings.Contains(x, ".."), strings.HasPrefix and filepath.Clean("/" +
	// x) results as path sanitizers -- never filepath.IsAbs and never a
	// "/../" literal -- and of those alternatives only IsLocal both refuses
	// a climb and stays visible: HasPrefix also guards the branch a ".."
	// climb never takes, and cleanRel's own path.Clean is invisible to a
	// model that knows only the path/filepath package by name. On Windows
	// builds the clause is not belt-and-braces: IsLocal splits on the host
	// separator, so a backslash dot-dot climb ("assets\..\..\x" -- "\" is an
	// ordinary character to path.Clean and every forward-slash clause, but a
	// separator to the filepath.Join below) is refused too.
	if !filepath.IsLocal(rel) {
		http.NotFound(w, r)
		return
	}
	// #nosec G703 -- gosec's taint analysis cannot see the refusal above
	// (it evaluates no guard branches), any more than it can see the
	// root-pinning inside cleanRel; the joined name can never leave h.root
	// either way.
	file, err := os.Open(filepath.Join(h.root, filepath.FromSlash(rel)))
	switch {
	case err == nil:
		defer func() { _ = file.Close() }()
		h.serveOpened(w, r, rel, file)
	case errors.Is(err, fs.ErrNotExist) && !h.assetPath(rel):
		// Unknown path outside the asset directory: the client-routed app's
		// index fallback.
		h.serveIndex(w, r)
	default:
		http.NotFound(w, r)
	}
}

// serverOwned reports whether p is a path the wrapped handler owns and the
// frontend must never intercept: an exact match on a declared server path
// ignoring trailing slashes -- "/metrics/" and "/metrics" name the same
// endpoint, exactly as cleanRel serves both from the same cleaned name, so
// answering only the slashless spelling would hand the other to the
// frontend -- or a prefix match at a "/" boundary on a declared server
// prefix.
func (h *Handler) serverOwned(p string) bool {
	for _, exact := range h.serverPaths {
		if strings.TrimRight(p, "/") == strings.TrimRight(exact, "/") {
			return true
		}
	}
	for _, prefix := range h.serverPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// assetPath reports whether rel names a file under the asset directory -- the
// paths answered with a real 404 when the file is missing rather than the
// index fallback (see the package doc comment above). An unset asset prefix
// (WithAssetPrefix("")) matches nothing, which is the documented meaning of
// disabling the asset policy.
func (h *Handler) assetPath(rel string) bool {
	return h.assetPrefix != "" &&
		(rel == h.assetPrefix || strings.HasPrefix(rel, h.assetPrefix+"/"))
}

// cacheControl returns the Cache-Control header value for a served file (see
// the package doc comment's cache policy).
func (h *Handler) cacheControl(rel string) string {
	if h.assetPath(rel) {
		return assetCacheControl
	}
	return defaultCacheControl
}

// cleanRel returns p's cleaned form relative to the frontend root ("" for the
// root itself), or ok == false when p cannot name a file inside it. The
// cleaning pins p to the root with a leading "/" before path.Clean collapses
// every empty and ".." segment, so a ".." can never climb above the root --
// "../../../etc/passwd" collapses to "/etc/passwd", a name inside the root
// that simply does not exist there, never a path outside it. The trailing
// ".."-segment check is defensive: path.Clean of a root-pinned path cannot
// produce it, but the pin is exactly the property that keeps the check above
// safe, so the explicit refusal documents what happens if an edit removes it.
func cleanRel(p string) (string, bool) {
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

// serveIndex answers with the frontend's index.html -- the "/" answer and the
// fallback for unknown paths outside the asset directory. indexFile is a
// package constant, never request-derived, so no traversal check is needed
// here.
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	// #nosec G703 -- the path joined here is the constant indexFile under
	// h.root, the operator-configured directory itself; no request data
	// reaches this open, so there is no traversal surface.
	file, err := os.Open(filepath.Join(h.root, indexFile))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = file.Close() }()
	h.serveOpened(w, r, indexFile, file)
}

// serveOpened serves the already-opened file under rel -- content type by
// extension and last-modified conditional handling both come from
// http.ServeContent, which also answers a HEAD request with headers only.
func (h *Handler) serveOpened(w http.ResponseWriter, r *http.Request, rel string, file *os.File) {
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		// A directory under the frontend root (in a default build only the
		// asset directory itself exists) is not a file to serve; a request
		// resolving to one gets the same 404 an asset miss gets, never a
		// listing.
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", h.cacheControl(rel))
	// ServeContent needs an io.ReadSeeker for its conditional/range
	// handling; *os.File is one, so file is passed through directly.
	http.ServeContent(w, r, path.Base(rel), info.ModTime(), file)
}
