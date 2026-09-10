package config

import (
	"context"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// Handle is the Module's lazy read handle: the value a host can obtain (and
// capture in a wiring closure) long before the module's Service exists, and
// read through once Attach has produced it. NewModule creates it, so
// (*Module).Handle returns the same handle for the module's whole life --
// before Bootstrap, between Register and Attach, and after Attach alike.
//
// It exists because config's Service is only produced by Attach, which per
// that method's own contract runs strictly after Kernel.Bootstrap returns,
// while the modules that read config values through a seam -- org's and
// authn's feature gates, compliance's export-delivery-expiry reader, and a
// host's own adapters -- must already be constructed by then: a
// construction-time Module Option cannot capture a *config.Service that does
// not exist yet. Capturing this handle instead moves the resolution to read
// time, when the Service is live.
//
// Its read methods carry the same signatures as the *Service methods they
// resolve to, so a wiring closure over a handle reads exactly like a call on
// the attached Service. They deliberately expose only those reads a seam
// consumes (IsEnabled, TenantDuration), never the whole Service: the handle
// is a resolution device for the pre-Attach wiring window, not a second
// Service surface.
//
// A read through the handle while the module has not been Attached (or
// whose Attach failed) reports ErrServiceNotAttached -- the same coded
// refusal the module's own routes answer with in that window -- never a
// nil-pointer panic and never a fabricated default. A nil *Handle reads the
// same way, so a host that leaves the field unset fails closed.
type Handle struct {
	m *Module
}

// Handle returns m's read handle. It is safe to call at any point in the
// module's life and always returns the same non-nil handle; whether reads
// through it succeed is decided separately, per call, by whether Attach has
// run (see Handle's own doc comment for the pre-Attach refusal). A host
// captures it while assembling -- typically immediately after NewModule --
// and hands it to the seams that read configuration later.
func (m *Module) Handle() *Handle { return m.handle }

// service resolves the Service the Module Attach produced. The read is
// taken under the module's attach lock, so a read racing the one Attach
// call (or performed through a nil handle) can never observe a
// half-published Service.
func (h *Handle) service() (*Service, error) {
	if h == nil || h.m == nil {
		return nil, ErrServiceNotAttached
	}
	h.m.attachMu.Lock()
	defer h.m.attachMu.Unlock()
	if h.m.service == nil {
		return nil, ErrServiceNotAttached
	}
	return h.m.service, nil
}

// IsEnabled reports whether the feature flag key is enabled for the tenant
// the context carries, exactly as (*Service).IsEnabled does. See that
// method's doc comment for the flag-and-dependency semantics; a read before
// Attach reports ErrServiceNotAttached (see Handle's own doc comment).
func (h *Handle) IsEnabled(ctx context.Context, key string) (bool, error) {
	svc, err := h.service()
	if err != nil {
		return false, err
	}
	return svc.IsEnabled(ctx, key)
}

// TenantDuration resolves key to its effective duration value for tenant,
// exactly as (*Service).TenantDuration does -- the three-tier resolution
// (tenant, then system, then schema default) whose ok result tells a
// tenant-configurable duration seam whether an explicit row produced the
// value. See that method's doc comment for the full contract; a read before
// Attach reports ErrServiceNotAttached (see Handle's own doc comment).
func (h *Handle) TenantDuration(ctx context.Context, key string, tenant pkgcore.TenantID) (time.Duration, bool, error) {
	svc, err := h.service()
	if err != nil {
		return 0, false, err
	}
	return svc.TenantDuration(ctx, key, tenant)
}
