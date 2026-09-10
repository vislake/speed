package s3

// Self-registration for the built-in "objectstore.s3" implementation,
// mirroring the database/sql driver-registration pattern: importing this
// package -- for side effect alone, if the host calls nothing else in it --
// registers "objectstore.s3" on pkgcore's shared ObjectStoreRegistry, the
// name pkgcore.PresetDistributed already names for the "objectstore" seam.
// The registration lives here, beside the implementation it adapts, rather
// than in pkgcore's own objectstore_builtins.go: if the implementation
// sat in its own package while the registration stayed behind,
// PresetDistributed would point at a name nothing could resolve.
//
// The trade this package accepts, same as any database/sql driver: a
// distributed-mode host that forgets to import it turns "missing
// objectstore.s3" from a compile-time failure into a Bootstrap-time
// pkgcore.ErrUnknownImplementation -- the accepted database/sql trade, an
// error that names the import which fixes it.
//
// # Declared capabilities
//
// The registration below declares MultiReplicaSafe | SurvivesRestart. The
// MultiReplicaSafe half is the ordinary claim of a shared backend: any
// number of replicas may address the same bucket, and object stores hold no
// per-replica state to split. The SurvivesRestart half promises that the
// state this store reads and writes — the objects PutObject stores, the
// keys GetObject and DeleteObject address — outlives a restart of the
// service that holds them, per pkgcore.Capability's own definition: the
// bytes live inside the S3-compatible service, whose durable storage is
// exactly what an object service exists to provide, never in this process
// (the way a throwaway local store's bytes live in its temp directory).
// The service-side premise is the operator's to provide, as it is for every
// S3-backed consumer: a self-hosted object service must keep its storage
// on durable media, and a managed one (AWS S3, Aliyun OSS) is durable by
// its own service contract. The integration tier verifies the claim against
// a genuine restart of this package's real S3-compatible container
// (integration_test/declared_survives_restart_test.go, running
// objectstoretest.AssertSurvivesRestart), so the declaration is a promise
// a protocol checks rather than an unexamined label.

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
)

// Capabilities is what "objectstore.s3" declares about itself: any number of
// replicas may address the same bucket, and the objects live inside the
// S3-compatible service, whose durable storage outlives any one process --
// the two claims the doc comment above spells out. The built-in registration
// below and the Registration factory a host wraps a self-built Config in
// both declare this one exported value, so the declaration a host reads off
// this package and the one assembly validates cannot drift apart. A host
// injecting a hand-built store with pkgcore.WithObjectStore passes it as the
// injection's capability argument.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

func init() {
	mustRegister(pkgcore.ObjectStoreRegistry, pkgcore.Registration[pkgcore.ObjectStore]{
		Name:         "objectstore.s3",
		Capabilities: Capabilities,
		New:          objectStoreFromConfig,
	})
}

// mustRegister adds r to registry and panics if that fails. It is only ever
// called here, against the one name this file controls, so a failure -- a
// duplicate name -- is a programming error in this file, not a condition a
// caller could hit or would want to recover from. pkgcore's own
// registries.go has an unexported helper of the same name and shape for its
// root-package built-ins; this package cannot call that one (it is
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
