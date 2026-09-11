package s3

// The typed-config one-step constructor, beside NewObjectStore (a
// hand-assembled typed Config) and the "objectstore.s3" component (the
// assembly's own construction path): FromConfig is for the host code that
// builds a store imperatively and expects a configuration it can already
// pronounce unusable to come back as an error, not a panic.

import (
	"errors"
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
)

// FromConfig returns the "objectstore.s3" store for cfg, together with this
// package's declared Capabilities -- the one-step form of a host-built
// store, and the construction the "objectstore.s3" component itself funnels
// through:
//
//	store, caps, err := objectstores3.FromConfig(cfg)
//
// The returned capability is the exported Capabilities constant the
// component also declares, so a host never spells the bits out and the
// declaration the assembly validates cannot drift from the one the
// constructor reports.
//
// FromConfig reports an unusable configuration as an error where
// NewObjectStore panics: NewObjectStore's panic is its unrecoverable-wiring
// convention for a host that built a typed Config by hand and then wired it
// itself, while this constructor returns the same judgment to a caller
// whose configuration may still be abandoned -- a component New whose block
// decoded a bad value, a host step still assembling its objects. Three
// configurations come back as errors, and never as panics: one missing any
// of the four fields with no safe default (Endpoint, Bucket, AccessKey,
// SecretKey, in one message that names all four), one naming a BucketLookup
// outside the enum, and one minio-go itself rejects at construction, such as
// an endpoint carrying a scheme ("https://...") -- this package's endpoint
// convention is a scheme-less host or host:port, UseSSL deciding the
// transport. Everything else that can fail does so on first use, not here:
// nothing is dialed at construction, matching NewObjectStore's own
// contract.
//
// The returned store owns no closable resource -- the minio-go client
// exposes no close, the same reason "objectstore.s3"'s component declares no
// Close -- so there is nothing for a host to release and the store comes
// back as the seam interface itself.
func FromConfig(cfg Config) (pkgcore.ObjectStore, pkgcore.Capability, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, 0, errors.New("pkgcore/objectstore/s3: FromConfig requires a non-empty Config.Endpoint, Config.Bucket, Config.AccessKey and Config.SecretKey")
	}
	// The store construction below shares newObjectStore with NewObjectStore
	// rather than mirroring its options: the options are the same shape for
	// both, and what distinguishes the two constructors is the judgment they
	// pass down on a configuration that cannot work -- this one's returned
	// error, NewObjectStore's panic.
	store, err := newObjectStore(cfg, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("pkgcore/objectstore/s3: FromConfig: %w", err)
	}
	return store, Capabilities, nil
}
