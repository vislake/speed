// This file is this app's one concrete satisfier of go/sharing's
// structurally-typed ResourceResolver seam (go/sharing/resolver.go) -- the
// no-import-edge shape this codebase uses throughout (orgFeatureGate over
// *config.Service, demoOrgSubjectResolver over rbac's Subject, ...) so
// sharing itself never imports go/storage (see resolver.go's own doc
// comment for why). Every share this app's tests create points at a
// go/storage object id, so storageSharingResolver is the only resolver
// this app wires.
package main

import (
	"context"

	"github.com/vislake/speed/go/sharing"
	"github.com/vislake/speed/go/storage"
)

// storageSharingResolver adapts a *storage.ObjectService to
// sharing.ResourceResolver: OpenResource reads the resource's own resolved
// media type and size off the storage Object (best-effort, exactly as
// storage's own HTTP handler already treats those two optional fields) and
// hands back the open content stream storage.ObjectService.OpenContent
// already resolves the tenant for -- from ctx, the same tenant
// sharing.Service.AccessPublic attached before ever calling this, never
// from ref.
type storageSharingResolver struct {
	svc *storage.ObjectService
}

// OpenResource implements sharing.ResourceResolver.
func (r *storageSharingResolver) OpenResource(ctx context.Context, ref string) (sharing.ResourceContent, error) {
	obj, rc, err := r.svc.OpenContent(ctx, ref)
	if err != nil {
		return sharing.ResourceContent{}, err
	}
	mime := ""
	if obj.MIME != nil {
		mime = *obj.MIME
	}
	size := int64(0)
	if obj.Size != nil {
		size = *obj.Size
	}
	return sharing.ResourceContent{MIME: mime, Size: size, Body: rc}, nil
}

// compile-time check that *storageSharingResolver satisfies
// sharing.ResourceResolver.
var _ sharing.ResourceResolver = (*storageSharingResolver)(nil)
