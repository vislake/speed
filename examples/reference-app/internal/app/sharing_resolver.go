// This file is this app's one concrete satisfier of go/sharing's
// structurally-typed ResourceResolver seam (go/sharing/resolver.go) -- the
// no-import-edge shape this codebase uses throughout (OrgFeatureGate over
// *config.Service, DemoOrgSubjectResolverFor over rbac's Subject, ...) so
// sharing itself never imports go/storage (see resolver.go's own doc
// comment for why). Every share this app's tests create points at a
// go/storage object id, so storageSharingResolver is the only resolver
// this app wires.
//
// This resolver is also the app's attestation GATE: simulation outputs
// (AI-generated images over patient
// media) carry an attestation row (internal/attestation), and an attested
// object's public share is served only when the attestation verifies --
// the certificate's chain verifies, the signature over the attested
// message checks out with the leaf's public key, and the digest of the
// bytes about to be served matches the attested digest. The gate runs on
// the full content (the digest comparison needs the exact bytes the
// visitor receives), so the whole body is read into memory -- bounded by
// the same limit internal/attestation attests under -- before anything is
// served. Objects without an attestation row (uploaded patient photos,
// any non-AI object) are served exactly as before; the gate only ever
// narrows.

package app

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/sharing"
	"github.com/vislake/speed/go/storage"

	"github.com/vislake/speed/examples/reference-app/internal/attestation"
)

// storageSharingResolver adapts a *storage.ObjectService to
// sharing.ResourceResolver: OpenResource reads the resource's own resolved
// media type and size off the storage Object (best-effort, exactly as
// storage's own HTTP handler already treats those two optional fields) and
// hands back the open content stream storage.ObjectService.OpenContent
// already resolves the tenant for -- from ctx, the same tenant
// sharing.Service.AccessPublic attached before ever calling this, never
// from ref.
//
// attest, when non-nil, is the attestation gate (internal/attestation):
// every resource whose bytes are about to be served passes through
// attest.CheckContent first, which refuses an attested object whose
// attestation does not verify and passes everything else. It is always
// wired in this app's production composition (server.go constructs the
// resolver with the booted attestation service); nil is the plain
// resolver of the pre-consumer shape, kept so the type stays honest in
// isolation.
type storageSharingResolver struct {
	svc *storage.ObjectService

	// attest is the AI-output attestation gate -- nil skips the gate (see
	// the type's own doc comment).
	attest *attestation.Service
}

// OpenResource implements sharing.ResourceResolver.
func (r *storageSharingResolver) OpenResource(ctx context.Context, ref string) (sharing.ResourceContent, error) {
	obj, rc, err := r.svc.OpenContent(ctx, ref)
	if err != nil {
		return sharing.ResourceContent{}, err
	}
	defer func() { _ = rc.Close() }()

	// The gate compares the live digest against the attested one, so the
	// bytes being served must be in hand before the gate runs -- the whole
	// content, bounded by the attestation package's own serve bound (an
	// object above it can never have been attested, so the bound is a
	// fail-closed cap on what an attestation row can be guarding, never a
	// new limit an honest share could trip). An over-bound read is an
	// internal surprise, answered as the resolver error the sharing module
	// reports as sharing.resource_unavailable -- the same answer the gate's
	// own refusals produce, so an over-bound object never bypasses the
	// digest check by being unreadable.
	raw, err := io.ReadAll(io.LimitReader(rc, attestation.MaxAttestedBytes+1))
	if err != nil {
		return sharing.ResourceContent{}, err
	}
	if int64(len(raw)) > attestation.MaxAttestedBytes {
		return sharing.ResourceContent{}, errors.New("sharing resolver: resource exceeds the attestation-gate read bound")
	}

	if r.attest != nil {
		if err := r.attest.CheckContent(ctx, ref, raw); err != nil {
			// The gate refused: an attested output whose certificate,
			// signature or content no longer verifies. The refusal is
			// logged with its stage (ErrAttestationFailed's wrapped
			// reason) and returned as a plain error, which the sharing
			// module answers with sharing.resource_unavailable -- the one
			// outward shape for "the share surface worked, the resource
			// behind it did not", deliberately undisguised (go/sharing's
			// errors.go ErrResourceUnavailable doc comment).
			observability.FromContext(ctx).Error("sharing resolver: attestation gate refused a shared resource",
				"resource_ref", ref,
				"error", err,
			)
			return sharing.ResourceContent{}, err
		}
	}

	mime := ""
	if obj.MIME != nil {
		mime = *obj.MIME
	}
	size := int64(0)
	if obj.Size != nil {
		size = *obj.Size
	}
	return sharing.ResourceContent{MIME: mime, Size: size, Body: io.NopCloser(bytes.NewReader(raw))}, nil
}

// storageContentOpener adapts *storage.ObjectService to
// attestation.ContentOpener -- the one-method storage read the attestation
// service needs, shaved down to the two-return shape that service's own
// seam declares (internal/attestation/content.go).
type storageContentOpener struct {
	svc *storage.ObjectService
}

// OpenContent implements attestation.ContentOpener.
func (o storageContentOpener) OpenContent(ctx context.Context, objectID string) (io.ReadCloser, error) {
	_, rc, err := o.svc.OpenContent(ctx, objectID)
	return rc, err
}

// compile-time checks pinning this file's two seam satisfiers.
var (
	_ sharing.ResourceResolver  = (*storageSharingResolver)(nil)
	_ attestation.ContentOpener = storageContentOpener{}
)
