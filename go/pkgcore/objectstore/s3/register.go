package s3

// Self-registration for the built-in "objectstore.s3" implementation,
// mirroring the database/sql driver-registration pattern: importing this
// package -- for side effect alone, if the host calls nothing else in it --
// registers "objectstore.s3" on pkgcore's shared ObjectStoreRegistry, the
// name pkgcore.PresetDistributed already names for the "objectstore" seam.
// Before the split this registration lived in pkgcore's own
// builtin_implementations.go, alongside the implementation it adapts; moving
// the implementation out without moving the registration would have left
// PresetDistributed pointing at a name nothing could ever resolve.
//
// The trade this package accepts, same as any database/sql driver: a
// distributed-mode host that forgets to import it turns "missing
// objectstore.s3" from a compile-time failure into a Bootstrap-time
// pkgcore.ErrUnknownImplementation (docs/internal/03-deployment-modes.md's
// implementation-registry section names this cost and accepts it explicitly).

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
)

func init() {
	mustRegister(pkgcore.ObjectStoreRegistry, pkgcore.Registration[pkgcore.ObjectStore]{
		Name:         "objectstore.s3",
		Capabilities: pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart,
		New:          objectStoreFromConfig,
	})
}

// mustRegister adds r to registry and panics if that fails. It is only ever
// called here, against the one name this file controls, so a failure -- a
// duplicate name -- is a programming error in this file, not a condition a
// caller could hit or would want to recover from. pkgcore's own
// builtin_implementations.go has an unexported helper of the same name and
// shape for its own five built-ins; this package cannot call that one (it is
// unexported to the root package), so it carries its own copy rather than
// inventing a different convention.
func mustRegister[T any](registry *pkgcore.SeamRegistry[T], r pkgcore.Registration[T]) {
	if err := registry.Register(r); err != nil {
		panic(fmt.Sprintf("pkgcore/objectstore/s3: builtin implementation registration failed: %v", err))
	}
}

// objectStoreFromConfig adapts pkgcore.Config onto NewObjectStore. Like
// pkgcore's own smtpMailerFromConfig, the fields NewObjectStore itself
// panics on missing are checked first and reported as
// pkgcore.ErrMissingSeamConfig instead, because none of endpoint, bucket or
// the credential pair has a safe default.
func objectStoreFromConfig(cfg pkgcore.Config) (pkgcore.ObjectStore, error) {
	endpoint := cfg["endpoint"]
	bucket := cfg["bucket"]
	accessKey := cfg["access_key"]
	secretKey := cfg["secret_key"]
	if endpoint == "" || bucket == "" || accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf(
			"pkgcore/objectstore/s3: builtin objectstore.s3 seam: %w: requires \"endpoint\", \"bucket\", \"access_key\" and \"secret_key\"",
			pkgcore.ErrMissingSeamConfig,
		)
	}

	return NewObjectStore(Config{
		Endpoint:  endpoint,
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
		Region:    cfg["region"],
		UseSSL:    cfg["use_ssl"] == "true",
	}), nil
}
