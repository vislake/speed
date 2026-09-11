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
// The serving mechanism is pkgcore/spa's, and that package's doc comment
// carries the full contract: the GET/HEAD-only static surface, the
// index.html fallback for unknown client routes, the hashed-asset miss
// 404, the cache policy and the path confinement. What is only this app's
// is here: the APP_WEB_DIST opt-in, and the allowlist below declaring the
// paths this process's composed server owns -- it must stay the complete
// ownership list, or a server path would be answered by the frontend
// instead of its handler. In this app every mounted route lives under
// /api (the admin console included, at /api/v1/admin, plus config's two
// pre-auth endpoints), and the two probe endpoints are the only non-API
// paths the server owns.
//
// webSPASpec declares the frontend directory as the engine's SPA spec: the
// engine wraps the whole composed handler (authn.Middleware's output
// included) in the shared SPA file server, so the requests the frontend
// answers never meet tenant resolution -- GET / answers 200 to a caller
// with no tenant, exactly what a deployed sign-in page needs -- while the
// API requests that do reach the chain pass through unchanged.

import (
	speedapp "github.com/vislake/speed/go/app"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore/spa"
)

// webSPASpec returns the engine's SPA declaration for dir, configured for
// this app's surface: /api and everything nested below it, plus the two
// probe endpoints, always reach the composed handler, and every other
// GET/HEAD request is served from dir (see the package doc comment above).
func webSPASpec(dir string) *speedapp.SPASpec {
	return &speedapp.SPASpec{
		Dir: dir,
		Options: []spa.Option{
			spa.WithServerPrefix("/api"),
			spa.WithServerPath(obs.HealthzPath),
			spa.WithServerPath(obs.MetricsPath),
		},
	}
}
