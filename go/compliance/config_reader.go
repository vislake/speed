package compliance

import (
	"context"
	"time"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/pkgcore"
)

// ConfigReader adapts config's lazy Handle (config.Handle) to this module's
// ExportDeliveryExpiryReader: the sanctioned adapter for a host's
// WithExportConfigReader wiring. The reader can be constructed while a host
// assembles, before Attach has produced the *config.Service -- the handle
// exists from config.NewModule on -- and resolves
// ConfigExportDeliveryExpiry through it per call, so Export reads the
// tenant-resolved value live.
//
// It is the replacement for a hand-written adapter over a later-filled
// **config.Service: the handle carries the same not-attached refusal
// (config.ErrServiceNotAttached) those adapters hand-rolled, so a reader
// consulted in the pre-Attach window fails closed instead of answering a
// fabricated value.
type ConfigReader struct {
	handle *config.Handle
}

// NewConfigReader returns the ExportDeliveryExpiryReader over handle --
// typically configModule.Handle(), captured where the config module was
// constructed. A nil handle reads as not attached, so a host that leaves it
// unset fails closed rather than panicking.
func NewConfigReader(handle *config.Handle) ConfigReader {
	return ConfigReader{handle: handle}
}

// ExportDeliveryExpiry implements ExportDeliveryExpiryReader: it resolves
// the tenant's configured compliance.export_delivery_expiry through the
// handle, with ok false when no explicit tenant or system row produced the
// value (see config.Service.TenantDuration's contract), so Export applies
// its own defaultExportDeliveryExpiry fallback. A read failure -- including
// the config.ErrServiceNotAttached refusal before the config module is
// attached -- is returned as-is, and Export reports it wrapped in
// ErrExportDeliveryFailed rather than guessing.
func (r ConfigReader) ExportDeliveryExpiry(ctx context.Context, tenant pkgcore.TenantID) (time.Duration, bool, error) {
	return r.handle.TenantDuration(ctx, ConfigExportDeliveryExpiry, tenant)
}

// compile-time check that ConfigReader satisfies ExportDeliveryExpiryReader.
var _ ExportDeliveryExpiryReader = ConfigReader{}
