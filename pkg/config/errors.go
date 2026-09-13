package config

import "errors"

// The sentinel errors configuration loading returns. Every one of them happens
// during startup and aborts it; callers tell the classes apart with errors.Is,
// and the error text carries what is needed to locate the cause and states the
// fixing action.
var (
	// ErrHelpRequested reports that the command line asked for help, which
	// has been rendered. It is not a failure: the host exits successfully on
	// it.
	ErrHelpRequested = errors.New("config: help requested")
	// ErrMalformedLocator reports a config locator that is not a legal URI,
	// or a shape the selected transport does not serve, such as a remote
	// host handed to the file source. Both leave the host the same thing to
	// do: fix the locator it gave.
	ErrMalformedLocator = errors.New("config: config locator is malformed")
	// ErrUnknownScheme reports a locator scheme no registered Source answers
	// for.
	ErrUnknownScheme = errors.New("config: no source registered for the locator scheme")
	// ErrUndeterminedFormat reports that the format of the data could not be
	// determined: an unrecognised file extension, or a remote locator
	// without a format parameter.
	ErrUndeterminedFormat = errors.New("config: format of the primary source could not be determined")
	// ErrUnknownFormat reports a determined format name no registered Format
	// answers for.
	ErrUnknownFormat = errors.New("config: no format registered for the format name")
	// ErrSourceUnavailable reports a Fetch failure: the file is missing, the
	// remote is unreachable, the permissions are insufficient.
	ErrSourceUnavailable = errors.New("config: primary source could not be read")
	// ErrMalformedConfig reports data the selected format could not parse.
	ErrMalformedConfig = errors.New("config: primary source could not be parsed")
	// ErrConfigConflict reports a conflict between two declarations, found
	// while the manifest was collected.
	ErrConfigConflict = errors.New("config: input item declarations conflict")
	// ErrInvalidSchema reports a single declaration that is defective in
	// itself, with no second party involved.
	ErrInvalidSchema = errors.New("config: input item declaration is invalid")
	// ErrMalformedCommandLine reports a command line this program's syntax
	// does not admit: an argument that ends without the value it takes, a
	// positional argument, or a clustered short name. It is the syntax of
	// the command line that is wrong, which is what separates it from a name
	// the syntax admits and no item declares.
	ErrMalformedCommandLine = errors.New("config: command line is malformed")
	// ErrUnknownKey reports a key the primary source or the command line is
	// not allowed to give: the manifest does not have it, or it has it and
	// the item does not accept that origin. The environment layer does not
	// raise it.
	ErrUnknownKey = errors.New("config: source gave a key it does not accept")
	// ErrTypeMismatch reports a value that cannot be converted to the type
	// of the target field.
	ErrTypeMismatch = errors.New("config: value does not convert to the type of the field")
	// ErrMissingRequired reports a required item none of the primary source,
	// the environment and the command line gave a value for.
	ErrMissingRequired = errors.New("config: required input item has no value")
)
