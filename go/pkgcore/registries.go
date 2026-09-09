package pkgcore

import (
	"errors"
	"fmt"
)

// ErrMissingSeamConfig is returned by a built-in Registration's constructor
// when cfg lacks a key the implementation cannot run without and has no safe
// default for -- an SMTP relay host (smtpMailerFromConfig, mailer_builtins.go),
// an S3 bucket and its credentials (the S3 constructor in the objectstore/s3
// subpackage). It never fires for the in-process seams, which need no
// configuration at all.
var ErrMissingSeamConfig = errors.New("pkgcore: seam implementation is missing required configuration")

// mustRegister adds r to registry and panics if that fails. It is only ever
// called by the four seam registry builders this package's built-in
// registration files define (newBuiltinEventBusRegistry in
// eventbus_builtins.go, newBuiltinKVStoreRegistry in kv_builtins.go,
// newBuiltinMailerRegistry in mailer_builtins.go and
// newBuiltinObjectStoreRegistry in objectstore_builtins.go), against names
// those files control, so a failure -- a duplicate name -- is a programming
// error in this package, not a condition a caller could hit or would want to
// recover from: the same unrecoverable startup-error convention
// NewLocalObjectStore and NewSMTPMailer already use for a wiring mistake that
// cannot be corrected at runtime. Every distributed subpackage
// (eventbus/redis, eventbus/nats, eventbus/postgres, kv/redis, kv/memcached,
// kv/nats, kv/postgres, objectstore/s3) carries an unexported copy of this
// same helper, since it cannot be exported from here without exposing an
// implementation detail no other caller needs.
func mustRegister[T any](registry *SeamRegistry[T], r Registration[T]) {
	if err := registry.Register(r); err != nil {
		panic(fmt.Sprintf("pkgcore: builtin implementation registration failed: %v", err))
	}
}
