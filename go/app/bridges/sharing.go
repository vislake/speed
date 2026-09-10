package bridges

import (
	"context"
	"time"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/sharing"
)

// ShareExpiryReader adapts the config module's lazy read handle onto
// sharing's TenantConfigReader seam:
//
//	sharing.WithTenantConfigReader(app.ShareExpiryReader{Handle: configModule.Handle()})
//
// sharing declares its tenant-overridable default-expiry config item
// (sharing.ConfigDefaultExpiry) but never imports go/config -- the
// identical no-import-edge seam shape the feature-gate bridges use -- so
// the adapter lives here, on the composition layer that may import both.
//
// The handle exists precisely for this ordering: sharing.Module is
// constructed before Kernel.Bootstrap, config's *Service only exists
// after configModule.Attach returns, and the reader must already be
// wired by construction time. Reading through the handle defers the
// resolution to read time (a real request, long after assembly), and a
// read in the pre-Attach window reports the config module's own
// not-attached refusal rather than a nil-pointer panic.
type ShareExpiryReader struct {
	// Handle is the config module's read handle
	// ((*config.Module).Handle()). A nil Handle reads as not-attached.
	Handle *config.Handle
}

// ShareDefaultExpiry implements sharing.TenantConfigReader: the tenant's
// configured value for sharing.ConfigDefaultExpiry with ok=false when the
// tenant configured none (sharing then applies its own built-in default),
// and a genuine read failure reported as err (sharing refuses rather than
// guessing at a default).
func (r ShareExpiryReader) ShareDefaultExpiry(ctx context.Context, tenant pkgcore.TenantID) (time.Duration, bool, error) {
	return r.Handle.TenantDuration(ctx, sharing.ConfigDefaultExpiry, tenant)
}

// compile-time check that ShareExpiryReader satisfies
// sharing.TenantConfigReader.
var _ sharing.TenantConfigReader = ShareExpiryReader{}
