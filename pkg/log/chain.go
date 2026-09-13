package log

import (
	"fmt"
	"io"
	"log/slog"
	"math"
)

// allLevels is the level a branch encoder is built with: below every level a
// record can carry, so the branch filters nothing.
//
// Filtering happens once, at the head of the chain. Repeating it on the
// branches would run the same comparison once per output and, worse, would
// give a chain two places where a record can be dropped.
const allLevels = slog.Level(math.MinInt)

// chain is one assembled formal chain: the handler its logger writes through,
// the writers its branches put bytes into, and the release of the references
// this assembly took on those writers.
//
// The writers are kept because the identity of a destination's writer is what
// the deduplication is about: an assembly whose stdout branch writes through a
// second object has two locks on fd 1, and that is not visible in the output
// until two long records interleave.
type chain struct {
	handler slog.Handler
	writers []*destWriter
	release func()
}

// newChain builds the chain from the inside out: a writer and an encoder per
// output, the branches under a fan-out, then redaction, then the level.
//
// It takes a resolved Config rather than a config.Reader. Reading the
// configuration belongs to the descriptor's callbacks, and keeping it out of
// here is also what lets the assembly be exercised at all: the real reader is
// built by config's own module, which reads os.Args unconditionally.
//
// Redaction sits above the fan-out, so a record is judged once and every
// destination gets the same judgement. The level sits above redaction, so a
// record that is filtered out costs neither.
func newChain(cfg resolvedConfig) (*chain, error) {
	// The formats are checked before any writer is opened, so a name outside
	// the closed set fails the assembly without leaving a file open behind it.
	encoders := make([]func(io.Writer) slog.Handler, len(cfg.outputs))
	for i, o := range cfg.outputs {
		encoder, err := encoderFor(o)
		if err != nil {
			return nil, err
		}
		encoders[i] = encoder
	}

	writers, release, err := openWriters(cfg.outputs)
	if err != nil {
		return nil, err
	}

	branches := make([]slog.Handler, len(writers))
	for i, w := range writers {
		branches[i] = encoders[i](w)
	}
	// cfg.level is a plain slog.Level, settled here and never moved again:
	// the bootstrap chain's LevelVar belongs to the bootstrap chain, and a
	// second module's Prepare must not be able to re-filter a chain that is
	// already assembled.
	handler := newLevelHandler(cfg.level, newRedactHandler(newFanoutHandler(branches)))
	return &chain{handler: handler, writers: writers, release: release}, nil
}

// encoderFor picks the encoding of one output. The set is closed, and a name
// outside it fails the assembly naming the legal values.
func encoderFor(o resolvedOutput) (func(io.Writer) slog.Handler, error) {
	opts := &slog.HandlerOptions{Level: allLevels}
	switch o.format {
	case formatText:
		return func(w io.Writer) slog.Handler { return slog.NewTextHandler(w, opts) }, nil
	case formatJSON:
		return func(w io.Writer) slog.Handler { return slog.NewJSONHandler(w, opts) }, nil
	}
	return nil, fmt.Errorf("%w: the output to %s gives %q", ErrUnknownFormat, o.to, o.format)
}
