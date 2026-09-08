package attestation

import (
	"context"
	"io"
)

// ContentOpener opens an output object's stored bytes for the tenant in
// ctx -- the storage read surface this package needs, declared as its own
// two-return seam because go/storage's ObjectService.OpenContent also
// returns the object row (three returns), and the attestation paths have
// no use for the row's metadata. The host satisfies it with a tiny
// adapter over *storage.ObjectService (cmd/server/sharing_resolver.go's
// storageContentOpener); tests implement it directly, which is the whole
// point of the seam.
type ContentOpener interface {
	// OpenContent opens the stored bytes of the object named by objectID,
	// resolving the tenant from ctx itself exactly as the storage module's
	// own read paths do -- an object of another tenant (or no tenant in
	// ctx) is refused before any byte is returned, so a caller cannot use
	// this seam to reach an object it does not own.
	OpenContent(ctx context.Context, objectID string) (io.ReadCloser, error)
}
