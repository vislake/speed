package observability

import (
	"io"
	"net/http"
)

const (
	// HealthzPath is the liveness endpoint every host mounts for its
	// orchestrator's probe. HealthzHandler always answers 200 and no tenant
	// is ever required, which is why a host's tenancy middleware must
	// allowlist it: a probe must not depend on tenant resolution.
	HealthzPath = "/healthz"

	// MetricsPath is the Prometheus scrape endpoint; MetricsHandler serves
	// it, and a host's tenancy middleware must allowlist it for the same
	// reason it allowlists HealthzPath.
	MetricsPath = "/metrics"
)

// HealthzHandler returns the liveness handler: 200 with body "ok",
// regardless of request method, headers or body. It reads no state and
// consults no dependency deliberately -- a probe that failed because, say,
// the database was unreachable would take the replica out of the pool
// exactly when an operator needs it up to diagnose the outage. Readiness
// is a different question with a different answer (a future endpoint), and
// this one stays a pure process-liveness signal.
func HealthzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
}

// MountLiveness registers the liveness route set -- HealthzPath and
// MetricsPath -- on mux as GET patterns, which is what orchestrator probes
// and Prometheus scrapers send: net/http's ServeMux serves HEAD from a
// registered "GET "+path pattern automatically, and any other method
// answers 405 Method Not Allowed rather than reaching a handler that has
// nothing to say to it.
//
// The metrics side re-reads MetricsHandler() on every request rather than
// capturing it at mount time, so this route's behavior does not depend on
// Init having already run by mount time: the ordinary boot calls Init
// before serving, but the indirection keeps that an implementation detail
// of the boot sequence rather than a hidden ordering requirement on the
// assembly -- a test (or a host) that mounts this handler can call it
// without caring whether Init has run yet in this process, or ever will.
func MountLiveness(mux *http.ServeMux) {
	mux.Handle(http.MethodGet+" "+HealthzPath, HealthzHandler())
	mux.Handle(http.MethodGet+" "+MetricsPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		MetricsHandler().ServeHTTP(w, r)
	}))
}
