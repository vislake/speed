// flat_config.go carries the flat scalar configuration map -- the form a
// built-in implementation package's shared constructor takes, bridged from
// the structured ComponentConfig its component descriptor's New callback
// receives (config.go carries that structured value) -- plus the sentinel
// such a constructor returns when the map lacks a required key.

package pkgcore

import "errors"

// Config is the flat scalar form a built-in implementation package's shared
// constructor takes: what a configuration file or environment can naturally
// provide, keyed by whatever name the implementation documents. A component
// descriptor's New callback receives the structured ComponentConfig and
// bridges its fields onto this flat map when it funnels through the
// package's one construction path, so the keys a composition block spells
// and the keys the constructor reads stay the same. The type is deliberately
// shallow -- a constructor that wants a typed configuration builds it from
// these strings -- because it is the boundary between two things that must
// stay separate: composing which implementation runs (the composition
// configuration's business, decided by selecting a component descriptor) and
// sourcing that implementation's settings (the block's own values; pkgcore
// itself never reads environment or files).
type Config map[string]string

// ErrMissingSeamConfig is returned by an implementation's constructor when
// the configuration it received lacks a key it cannot run without and has no
// safe default for -- an SMTP relay host, an S3 bucket and its credentials.
// It never fires for the in-process implementations, which need no
// configuration at all.
var ErrMissingSeamConfig = errors.New("pkgcore: implementation is missing required configuration")
