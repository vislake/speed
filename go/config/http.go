package config

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

const (
	// PathPublic is where this module's unauthenticated public
	// configuration endpoint is mounted. It answers GET and HEAD with the
	// effective values of every Public item plus the enabled feature
	// flags, resolved for the tenant the request's host maps to (see
	// handlePublic). Hosts mount config routes behind their own tenant
	// middleware; the path constant is exported so a host can name the
	// route in an allowlist without stringly duplicating it -- the
	// reference app does exactly that (cmd/server/server.go's middleware
	// allowlist).
	PathPublic = "/api/config/public"

	// PathSystemFeatures is where this module's feature-flag endpoint is
	// mounted: which features are enabled, for the same host-resolved
	// tenant. Both endpoints are GET/HEAD-only and pre-auth in this
	// milestone (see go/config/AGENTS.md's known limitations for the
	// deferred spec-fragment contract).
	PathSystemFeatures = "/api/system/features"
)

// jsonContentType is the Content-Type every response below writes. It is
// the same constant notes' handler uses, kept locally because the module
// cannot import the reference app.
const jsonContentType = "application/json; charset=utf-8"

// errorEnvelope is the JSON body every error response writes: a stable
// code plus structured parameters, never localized text and never a raw
// Go error string (the structured-error envelope of
// docs/internal/11-cross-cutting.md; notes' handler writes the same
// shape). Params is omitted when the error carries none, so the wire shape
// stays {"code": ...} rather than {"code": ..., "params": null}.
type errorEnvelope struct {
	Code   *string        `json:"code,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

// writeError writes err to w as a JSON errorEnvelope. An err that is not
// an *apperr.Error -- something below this handler failed to classify --
// is folded into ErrStorage, the module's internal error, so a caller
// never sees raw Go error text (nor, on a storage failure, the underlying
// cause chain, which can name a driver, a host or a ciphertext detail).
func writeError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = ErrStorage
	}
	envelope := errorEnvelope{Code: &appErr.Code}
	if appErr.Params != nil {
		envelope.Params = appErr.Params
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(envelope)
}

// writeMethodNotAllowed answers the 405 every non-GET/HEAD request to
// either endpoint gets, with the Allow header a well-behaved client
// follows. The code is a plain constant: the endpoints' method contract is
// an HTTP convention, not an apperr-classified operation.
func writeMethodNotAllowed(w http.ResponseWriter) {
	code := "config.method_not_allowed"
	envelope := errorEnvelope{Code: &code}
	w.Header().Set("Allow", "GET, HEAD")
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(http.StatusMethodNotAllowed)
	_ = json.NewEncoder(w).Encode(envelope)
}

// handlePublic serves PathPublic: GET and HEAD return the public
// configuration snapshot -- every Public item's effective value plus every
// enabled feature flag -- for the tenant the request resolves to. The
// response shape is {"config": {key: value, ...}, "features": [...]};
// durations render as their canonical "1m30s" text, booleans and ints as
// their JSON natives, and no Sensitive key can appear at all (pkgcore's
// declaration validation makes Sensitive and Public mutually exclusive).
//
// Tenant resolution follows docs/internal/11-cross-cutting.md's
// unauthenticated rule: custom domain first, platform subdomain second,
// platform defaults last -- and never an error. The module's own resolver
// (WithResolver) is consulted per request; when it reports no tenant, or
// when no resolver is wired, the request reads platform defaults.
//
// The endpoint is deliberately pre-auth in this milestone (its fragment
// contract is deferred to the authn round; see AGENTS.md), and serves only
// what the design marks public -- display decisions, never data.
func (m *Module) handlePublic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeMethodNotAllowed(w)
		return
	}
	if m.service == nil {
		// The window between Register and Attach; a host wiring bug (see
		// (*Module).Attach's doc comment).
		writeError(w, ErrServiceNotAttached)
		return
	}
	values, features, err := m.service.PublicSnapshot(m.requestContext(r))
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"config":   values,
		"features": features,
	})
}

// handleFeatures serves PathSystemFeatures: GET and HEAD return the
// feature-flag half of the snapshot alone, {"features": [...]}, for the
// same host-resolved tenant. The shape is the one the design's
// "/api/system/features query" contract gives the frontend
// (docs/internal/11-cross-cutting.md's feature-flag section): a client
// that only needs to know what is on need not fetch the public
// configuration too. Everything handlePublic says about tenant resolution,
// pre-auth status and method gating applies unchanged.
func (m *Module) handleFeatures(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeMethodNotAllowed(w)
		return
	}
	if m.service == nil {
		writeError(w, ErrServiceNotAttached)
		return
	}
	features, err := m.service.EnabledFlags(m.requestContext(r))
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"features": features})
}

// requestContext returns the request's context carrying the tenant the
// configured resolver maps the request to -- when it maps to one. An
// unmatched host, a resolver error, or no resolver at all leaves the
// context untagged, which is exactly the platform-defaults tier: every
// read below resolves from the system row down, never failing over a host
// the resolver does not know (the unauthenticated display rule of
// docs/internal/04-data-and-tenancy.md).
func (m *Module) requestContext(r *http.Request) context.Context {
	ctx := r.Context()
	if m.resolver == nil {
		return ctx
	}
	tenant, err := m.resolver.Resolve(r)
	if err != nil || tenant == "" {
		return ctx
	}
	return pkgcore.WithTenant(ctx, tenant)
}
