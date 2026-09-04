package sharing

import (
	"context"
	"io"
)

// This file is the seam Handler's public access route (handler.go) reads a
// share's ResourceRef through to turn it into actual bytes -- the round-2
// answer to AGENTS.md's former "Access never resolves ResourceRef into
// actual bytes" known limitation.
//
// It is a structurally-typed, no-import-edge interface, the same shape
// org.FeatureGate and org.Scope use to reach go/config-shaped or
// go/org-shaped behavior without an import in either direction, and the
// same shape this module's own TenantConfigReader (service.go) already
// uses for its config seam: ResourceRef is documented (model.go's Share
// doc comment) as "typically a reference another module's own key scheme
// produces (e.g. go/storage's object id)" but is never interpreted by this
// module itself, so sharing does not import go/storage (or any other
// resource-owning module) to satisfy this interface -- a host's own
// adapter over *storage.ObjectService (or whatever module actually owns
// the resource) satisfies it structurally instead. This keeps the module
// dependency direction exactly as round 1 left it (go/sharing/AGENTS.md's
// "Cross-module references" reasoning on Share.ResourceRef, restated in
// model.go), even though this round finally does something with the bytes
// behind that reference.

// ResourceContent is what a ResourceResolver reports about the bytes
// behind a Share's ResourceRef -- the minimum an HTTP response needs to
// serve them correctly, deliberately shaped without depending on any
// resource-owning module's own types (go/storage's Object included).
type ResourceContent struct {
	// MIME is the resource's media type, best-effort. The empty string
	// means unknown; Handler falls back to "application/octet-stream"
	// rather than guessing.
	MIME string

	// Size is the resource's byte length, best-effort. A value <= 0 means
	// unknown; Handler omits Content-Length rather than reporting zero.
	Size int64

	// Body is the resource's bytes. The caller (Handler) is responsible
	// for closing it exactly once, after it has been fully written to the
	// response or the request has failed partway through.
	Body io.ReadCloser
}

// ResourceResolver turns a Share's ResourceRef into its actual bytes.
// OpenResource is expected to resolve its own tenant from ctx (the
// tenant Service.AccessPublic already attached before Handler ever calls
// this) exactly as go/storage's ObjectService.OpenContent already does,
// never from ref or any other caller-supplied value -- root CLAUDE.md's
// "the tenant comes from the access token claims, never from request
// parameters" rule applies here too, even though this call sits well
// after the token has already been resolved into a tenant.
//
// A ref this resolver does not recognize, or bytes it cannot open, is
// reported as a plain error -- Handler wraps it in ErrResourceUnavailable
// (a distinct, undisguised failure from the share-access refusal path;
// see that error's own doc comment for why the two must not be collapsed
// together) rather than inspecting it for a more specific classification,
// since this module has no way to know what a given resolver's errors
// mean.
type ResourceResolver interface {
	OpenResource(ctx context.Context, ref string) (ResourceContent, error)
}
