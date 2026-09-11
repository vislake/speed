package storage

// component.go registers the "storage" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as the "storage" module's single implementation. The component builds the
// same *Module every other caller builds through NewModule, so the module's
// services, HTTP surface and registration behavior are one implementation
// reachable two ways.

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/storage/locales"
	"github.com/vislake/speed/go/storage/migrations"
)

// storageComponentConfig is the "storage" component's configuration schema:
// one field per key a composition block may carry, each overriding the
// matching construction default. Decoding is strict, so a block naming any
// other key fails before anything is constructed.
//
//	max_upload_bytes      int64       single-upload size ceiling
//	max_image_pixels      int64       image pixel-count ceiling
//	derivative_max_edge   int         generated derivatives' longer-edge cap
//	upload_ttl            duration    how long an unfinished upload may live
//	max_object_lifetime   duration    default retention and its ceiling
//	no_expiry_allowed     bool        permits never-expiring objects
//	allowed_types         []string    admitted media types (replaces the default)
type storageComponentConfig struct {
	MaxUploadBytes    int64         `json:"max_upload_bytes"`
	MaxImagePixels    int64         `json:"max_image_pixels"`
	DerivativeMaxEdge int           `json:"derivative_max_edge"`
	UploadTTL         time.Duration `json:"upload_ttl"`
	MaxObjectLifetime time.Duration `json:"max_object_lifetime"`
	NoExpiryAllowed   bool          `json:"no_expiry_allowed"`
	AllowedTypes      []string      `json:"allowed_types"`
}

// storageComponent is the component descriptor for "storage". It declares
// MultiReplicaSafe: the module's state is its rows in the shared database
// and its bytes in the selected object store, and the object lifecycle is
// designed for several replicas serving it at once.
//
// Requires the database and the jobs queue as mandatory products -- a module
// that accepts uploads it can never finish processing is worse than a boot
// failure (Register's own ErrQueueRequired rule) -- plus the object store
// every byte moves through and the event bus its completion and deletion
// events publish on. Provides (*Module)(nil) so a consumer's token resolves
// against this component's product.
//
// Init is deliberately not declared: declaration (the module's Register
// call) is made today by the host's bootstrap path, not by this descriptor.
var storageComponent = pkgcore.Component{
	Name:         "storage",
	Module:       "storage",
	Provides:     []any{(*Module)(nil)},
	Capabilities: pkgcore.MultiReplicaSafe,
	ConfigSchema: (*storageComponentConfig)(nil),
	Requires: []pkgcore.Requirement{
		{Token: (*gorm.DB)(nil)},
		{Token: (*jobs.Queue)(nil)},
		{Token: (*pkgcore.ObjectStore)(nil)},
		{Token: (*pkgcore.EventBus)(nil)},
	},
	Migrations:  migrations.FS,
	Locales:     locales.FS,
	OpenAPISpec: openAPISpecYAML,
	New: func(_ context.Context, reg *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c storageComponentConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		queue, err := pkgcore.Get[jobs.Queue](reg)
		if err != nil {
			return nil, err
		}
		opts := []Option{
			WithQueue(queue),
			WithMaxUploadBytes(c.MaxUploadBytes),
			WithMaxImagePixels(c.MaxImagePixels),
			WithDerivativeMaxEdge(c.DerivativeMaxEdge),
			WithUploadTTL(c.UploadTTL),
			WithMaxObjectLifetime(c.MaxObjectLifetime),
			WithAllowedTypes(c.AllowedTypes...),
		}
		if c.NoExpiryAllowed {
			opts = append(opts, WithNoExpiryAllowed())
		}
		return NewModule(db, opts...), nil
	},
}

func init() { pkgcore.MustRegister(storageComponent) }
