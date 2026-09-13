// Package log carries structured logging: the capability modules take up from
// each other, the handler chain the configuration assembles, the built-in
// formats and destinations, the redaction layer every record passes through,
// and the storage of a logger in a context.
//
// The calling surface is the standard library's *slog.Logger. This package
// defines no logging API of its own, so a call site depends on log/slog rather
// than on this project.
//
// Everything lives in the root package: the formats and the destinations use
// the standard library alone, so there is no third-party dependency to isolate
// in a subpackage.
package log

import "log/slog"

// Logger is the capability this module delivers. It is taken up through the
// registry, and it is not the logger object: a *slog.Logger is a concrete type
// and cannot designate a capability.
//
// Declaring a dependency on it orders this module ahead of the dependant, so
// the dependant constructs with logging already assembled from configuration.
// Day-to-day logging does not go through it: a call site with a context uses
// FromContext, and one without uses Default.
type Logger interface {
	// Named returns a logger carrying an attribute that identifies the
	// calling module. The handler chain behind it is shared; a module takes
	// one in New and holds it for the logs it writes without a request
	// context.
	Named(name string) *slog.Logger
	// Redaction returns the registration interface for redaction rules.
	Redaction() Redaction
}

// Redaction registers redaction rules. Rules are append-only: there is no way
// to remove one or to switch the layer off, so a registration that is too wide
// is corrected by changing the registering code, not at run time.
//
// A registration that carries no usable rule panics. It is a programming error
// at the call site, and a registration returns nothing, so dropping it would
// leave the registrant believing it holds a protection it does not have.
//
// A rule only governs judgements made after it is registered, and judgements
// happen at two moments: a record's own attributes are judged as the record is
// written, while an attribute bound through With is judged once at binding time
// and the result reused. A module should therefore register in its own New
// before it takes a logger, because Named already binds an attribute.
type Redaction interface {
	// AddKeys registers sensitive key names. A key path is compared segment
	// by segment, so a registered name matches a group opened by WithGroup
	// as well as a leaf attribute. An empty name panics, and the whole call
	// is checked before anything is registered.
	AddKeys(keys ...string)
	// AddPattern registers a value-shape rule under a name. The matcher runs
	// on every resolved string value and error text of every record, so its
	// cost lands on the whole process's logging path. A nil matcher panics.
	AddPattern(name string, match Matcher)
}

// Matcher reports the spans of a string value that need masking, and returns
// nothing when the value carries none.
type Matcher func(value string) []Span

// Span is a half-open range of a value, masked from Start up to but not
// including End.
type Span struct {
	Start, End int
}
