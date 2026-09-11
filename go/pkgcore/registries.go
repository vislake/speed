package pkgcore

import "errors"

// ErrMissingSeamConfig is returned by an implementation's constructor when
// the configuration it received lacks a key it cannot run without and has no
// safe default for -- an SMTP relay host, an S3 bucket and its credentials.
// It never fires for the in-process implementations, which need no
// configuration at all.
var ErrMissingSeamConfig = errors.New("pkgcore: implementation is missing required configuration")
