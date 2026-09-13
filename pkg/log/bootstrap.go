package log

import "log/slog"

// bootstrapChain is the handler chain the process default logger writes
// through: the level layer, the redaction layer, and a text handler writing to
// standard output. It is built while the package initialises, because records
// are written before any module is constructed.
//
// The redaction layer is on it for the same reason it is on a formal chain:
// Default() is used for the whole life of the process, so records written
// through it after a module has registered its keys are covered too. What this
// layer cannot cover is the first assembly's Prepare — registering goes
// through this module's product, and that does not exist until New.
var bootstrapChain = newBootstrapChain()

// defaultLogger is the process default logger. It is a singleton: callers
// compare what FromContext hands back against it, and there is one chain
// behind it for the life of the process.
var defaultLogger = slog.New(bootstrapChain)

// newBootstrapChain assembles the chain. The encoder writes through the
// standard output writer the root package registered, not through os.Stdout:
// fd 1 carries one writer and one lock, and a configured stdout output joins
// this very writer instead of opening a second one.
//
// The encoder filters nothing. Filtering is the level layer's, and this chain
// has one.
func newBootstrapChain() slog.Handler {
	encoder := slog.NewTextHandler(bootstrapStdoutWriter, &slog.HandlerOptions{Level: allLevels})
	return newLevelHandler(bootstrapLevel, newRedactHandler(encoder))
}

// Default returns the process default logger, for the call sites that have no
// context to take one from.
//
// It writes to standard output and filters by the bootstrap level, whatever a
// host configured. The configured destinations are reachable through the
// Logger capability alone, which is taken up through the registry: the default
// logger has to work before any module is constructed, while the configured
// destinations do not exist until New. A record that turns up on standard
// output instead of the configured file therefore says the path it came from
// never had a logger injected — noticeable, which is the point of the split.
//
// The returned logger is safe for concurrent use: the chain behind it does not
// change after package initialisation, and the only mutable part is the level
// the head reads.
func Default() *slog.Logger { return defaultLogger }
