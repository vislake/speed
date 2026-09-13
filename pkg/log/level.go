package log

import (
	"context"
	"log/slog"
)

// levelHandler is the head of a chain: it answers Enabled from the level it
// holds and passes everything that gets through straight down.
//
// The judgement belongs at the head because slog asks Enabled before it builds
// the record: a record that is filtered out here costs neither the record, nor
// the call site lookup, nor the redaction pass. What it does not save is the
// evaluation of the arguments — Go has evaluated those before the logging
// method is entered, so a call site that wants to avoid an expensive argument
// has to ask the level itself.
//
// Each chain carries its own layer. The downstream is fixed when the layer is
// built — the bootstrap chain's is a handler writing to standard output, the
// formal chain's is the fan-out — so the two are never the same instance.
type levelHandler struct {
	// level is where the threshold is read from. The bootstrap chain passes
	// the root package's LevelVar, which Prepare sets from configuration;
	// a formal chain passes a plain slog.Level settled in New, which is why
	// a later Prepare cannot move the level of a chain already built.
	level slog.Leveler
	next  slog.Handler
	// nowhereToWrite says this chain has no branches at all, which an
	// explicitly empty output list settles at assembly time. It is a fact
	// about the assembly, not a state, so it travels unchanged into every
	// derived layer.
	nowhereToWrite bool
}

// newLevelHandler puts a level layer in front of a downstream handler.
func newLevelHandler(level slog.Leveler, next slog.Handler) *levelHandler {
	return &levelHandler{level: level, next: next}
}

// newSilentLevelHandler puts a level layer that reports nothing enabled in
// front of a downstream handler. It is what a chain over an empty output list
// gets: no record it would build has anywhere to go.
func newSilentLevelHandler(level slog.Leveler, next slog.Handler) *levelHandler {
	return &levelHandler{level: level, next: next, nowhereToWrite: true}
}

// Enabled judges the record against this chain's own level. The downstream is
// not consulted: the branch handlers deliberately filter nothing, so asking
// them would only repeat the same comparison once per output.
//
// A chain with no branches answers false without reading the level, so an
// empty output list costs neither the record nor the redaction pass. Whether
// there are branches was settled when the chain was assembled, which is why
// this is a field and not a question put to the fan-out: an ordinary
// configuration pays one comparison, the same as before.
func (h *levelHandler) Enabled(_ context.Context, level slog.Level) bool {
	if h.nowhereToWrite {
		return false
	}
	return level >= h.level.Level()
}

// Handle passes the record on. Everything that reaches here is above the
// threshold.
func (h *levelHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.next.Handle(ctx, r)
}

// WithAttrs derives a layer over a downstream that carries the bound
// attributes. The level travels with it, so a derived logger filters the same
// way the one it came from does.
func (h *levelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &levelHandler{level: h.level, next: h.next.WithAttrs(attrs), nowhereToWrite: h.nowhereToWrite}
}

// WithGroup opens the group downstream, where the redaction layer and the
// encoders need it. An empty name returns the receiver, as the Handler
// contract requires.
func (h *levelHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &levelHandler{level: h.level, next: h.next.WithGroup(name), nowhereToWrite: h.nowhereToWrite}
}
