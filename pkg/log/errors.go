package log

import "errors"

// The sentinel errors the assembly of the handler chain returns. Every one of
// them aborts the startup: a logging configuration that cannot be honoured is
// not something to run on quietly, because the missing records only show up
// once somebody needs them. Callers tell the classes apart with errors.Is, and
// the error text names the offending output and states the fixing action.
var (
	// ErrInvalidLevel reports a level name outside the built-in set. The set
	// is in the message, because listing it is the fixing action.
	ErrInvalidLevel = errors.New("log: level is not one of debug, info, warn, error")
	// ErrUnknownDestination reports a destination name outside the built-in
	// set.
	ErrUnknownDestination = errors.New("log: destination is not one of stdout, stderr, file")
	// ErrUnknownFormat reports a format name outside the built-in set.
	ErrUnknownFormat = errors.New("log: format is not one of text, json")
	// ErrMissingPath reports a file output that gives no path.
	ErrMissingPath = errors.New("log: file destination has no path")
	// ErrInvalidFileParam reports a negative rotation or pruning parameter.
	// It is a sentinel of its own rather than part of another: the host has
	// to change a number here, while the neighbouring sentinels have it
	// change a name or a path.
	ErrInvalidFileParam = errors.New("log: file parameter is negative")
	// ErrConflictingOutput reports two outputs that point at one destination
	// and give it inconsistent rotation or pruning parameters. One
	// destination has one writer, and one writer has one set of parameters.
	ErrConflictingOutput = errors.New("log: two outputs give one destination inconsistent parameters")
	// ErrOutputUnavailable reports a destination that could not be opened: a
	// missing directory, insufficient permissions, an occupied path.
	ErrOutputUnavailable = errors.New("log: destination could not be opened")
)
