package log

import (
	"context"
	"log/slog"
)

// redactHandler is the fixed layer of the chain that judges every record
// against the registered rules. It sits ahead of the fan-out, so a record is
// judged once however many outputs are configured, and every destination and
// format is covered by the same judgement.
//
// What it guarantees is that a record passes through it. What gets masked
// depends on what was registered: this package holds no rules of its own.
type redactHandler struct {
	next slog.Handler
	// groups is the key path prefix this handler was derived under. The group
	// names WithGroup opened live here and nowhere else — a record carries the
	// leaf key alone — so a rule registered against a group name only matches
	// because this layer holds the prefix and judges it.
	groups []string
}

// newRedactHandler puts the redaction layer in front of a downstream handler.
func newRedactHandler(next slog.Handler) *redactHandler {
	return &redactHandler{next: next}
}

// Enabled defers to the downstream handler. The level is judged by the level
// layer at the head of the chain, not here.
func (h *redactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle judges the record's own attributes and hands a rebuilt record
// downstream.
//
// The record is rebuilt rather than rewritten: the copies of a slog.Record
// share their underlying attribute array, so writing into the one this layer
// was handed would reach every other holder of that record. The rebuilt record
// carries the original call site, which is what the source location downstream
// prints is derived from.
func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	rules := currentRules()
	inherited := rules.matchesAny(h.groups)

	attrs := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, redactAttr(rules, inherited, a))
		return true
	})

	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	out.AddAttrs(attrs...)
	return h.next.Handle(ctx, out)
}

// WithAttrs judges the attributes as they are bound and passes the judged ones
// downstream.
//
// The judgement happens here, once, because the standard library resolves and
// preformats bound attributes at binding time: a layer that only judged in
// Handle would never see them again, and a credential bound through With would
// reach the destination in the clear. The result is reused by every record the
// derived logger writes, so a rule registered after the binding does not reach
// it.
func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	rules := currentRules()
	inherited := rules.matchesAny(h.groups)

	judged := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		judged[i] = redactAttr(rules, inherited, a)
	}
	return &redactHandler{next: h.next.WithAttrs(judged), groups: h.groups}
}

// WithGroup pushes the group name onto this layer's key path prefix and opens
// the group downstream as well.
//
// Both halves are needed: without the prefix a rule registered against the
// group name matches nothing, because the records arriving here carry the leaf
// key alone; without passing it down the grouping disappears from the output.
// An empty name returns the receiver, as the Handler contract requires.
func (h *redactHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	groups := make([]string, len(h.groups)+1)
	copy(groups, h.groups)
	groups[len(h.groups)] = name
	return &redactHandler{next: h.next.WithGroup(name), groups: groups}
}

// redactAttr judges one attribute and returns the attribute to pass on.
//
// inherited says whether a segment of the key path above this attribute already
// carries a registered name, which is how a hit on a group name reaches the
// members of that group.
//
// Resolution and recursion alternate: a value is resolved, and only if the
// result is a group are its members visited — each of which is resolved in
// turn. Resolving once at the top and then walking the groups would leave a
// deferred value nested inside a resolved group unresolved, and a credential
// behind it unjudged.
func redactAttr(rules *ruleSet, inherited bool, a slog.Attr) slog.Attr {
	value := a.Value.Resolve()
	hit := inherited || rules.matchesKey(a.Key)

	if value.Kind() == slog.KindGroup {
		members := value.Group()
		judged := make([]slog.Attr, len(members))
		for i, member := range members {
			judged[i] = redactAttr(rules, hit, member)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(judged...)}
	}
	if hit {
		return slog.Attr{Key: a.Key, Value: slog.StringValue(maskText)}
	}
	return slog.Attr{Key: a.Key, Value: maskValue(rules, value)}
}

// maskValue applies the value-shape rules to a resolved value. They reach a
// string value and the text of an error; a value of any other shape is judged
// by its key path alone.
//
// A masked error is passed on as a string: the masked text is what has to reach
// the destination, and the original error cannot carry it.
func maskValue(rules *ruleSet, value slog.Value) slog.Value {
	switch value.Kind() {
	case slog.KindString:
		if masked, ok := rules.maskString(value.String()); ok {
			return slog.StringValue(masked)
		}
	case slog.KindAny:
		if err, ok := value.Any().(error); ok {
			if masked, ok := rules.maskString(err.Error()); ok {
				return slog.StringValue(masked)
			}
		}
	}
	return value
}
