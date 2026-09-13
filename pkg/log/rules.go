package log

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// maskText is what a rule leaves behind: it replaces the value of an attribute
// whose key path carries a registered name, and the spans a value-shape rule
// reports inside a string.
const maskText = "[REDACTED]"

// rootMu is the root package's mutex over the process-level state this package
// holds. It covers the construction of a new redaction snapshot and the pointer
// swap that publishes it; a judgement does not take it, because a judgement
// only reads the snapshot pointer.
var rootMu sync.Mutex

// pattern is one registered value-shape rule. The name has no bearing on the
// judgement: registrations only accumulate, so two rules sharing a name simply
// both run.
type pattern struct {
	name  string
	match Matcher
}

// ruleSet is an immutable snapshot of the registered rules. Nothing reachable
// from a published snapshot is ever written again, so a judgement that has read
// the pointer can go on reading the snapshot without synchronisation.
type ruleSet struct {
	keys     map[string]struct{}
	patterns []pattern
}

// rules points at the current snapshot. It is read once per judgement — once
// per record, and once per attribute binding — so a record is judged against
// one snapshot throughout, never against a half-updated rule set.
var rules atomic.Pointer[ruleSet]

// emptyRuleSet is what a judgement reads before the first registration. The
// rule set starts empty: this package holds the mechanism and none of the
// rules, so a process where nobody registers masks nothing.
var emptyRuleSet = &ruleSet{}

// currentRules reads the snapshot a single judgement works against.
func currentRules() *ruleSet {
	if rs := rules.Load(); rs != nil {
		return rs
	}
	return emptyRuleSet
}

// matchesKey reports whether one segment of a key path carries a registered
// name. An empty segment never matches: the empty string is not registrable, so
// an anonymous inline group does not turn every attribute under it into a hit.
func (rs *ruleSet) matchesKey(key string) bool {
	if key == "" || len(rs.keys) == 0 {
		return false
	}
	_, ok := rs.keys[key]
	return ok
}

// matchesAny reports whether any of the segments carries a registered name. It
// is how the group names a handler opened with WithGroup enter the judgement:
// those names live in the handler's state, not in the record.
func (rs *ruleSet) matchesAny(segments []string) bool {
	for _, s := range segments {
		if rs.matchesKey(s) {
			return true
		}
	}
	return false
}

// maskString applies the value-shape rules to a string and reports whether
// anything was masked. Every matcher judges the original string, so the spans
// one matcher reports are not shifted by the masking another one asked for.
//
// A span outside the string, or one that ends where it starts, is dropped: the
// matchers come from other modules, and a judgement runs on the logging path of
// the whole process, which is no place to panic over a bad offset.
func (rs *ruleSet) maskString(s string) (string, bool) {
	if len(rs.patterns) == 0 || s == "" {
		return s, false
	}
	var spans []Span
	for _, p := range rs.patterns {
		for _, span := range p.match(s) {
			if span.Start < 0 || span.End > len(s) || span.End <= span.Start {
				continue
			}
			spans = append(spans, span)
		}
	}
	if len(spans) == 0 {
		return s, false
	}
	slices.SortFunc(spans, func(a, b Span) int { return a.Start - b.Start })
	merged := spans[:1]
	for _, span := range spans[1:] {
		last := &merged[len(merged)-1]
		if span.Start <= last.End {
			if span.End > last.End {
				last.End = span.End
			}
			continue
		}
		merged = append(merged, span)
	}
	var b strings.Builder
	written := 0
	for _, span := range merged {
		b.WriteString(s[written:span.Start])
		b.WriteString(maskText)
		written = span.End
	}
	b.WriteString(s[written:])
	return b.String(), true
}

// redactionRegistry is the registration interface this module's product hands
// out. It carries no state of its own: the rules live in the process-level
// snapshot, which the bootstrap chain and every formal chain read, because a
// sensitive key name is a fact about the process rather than about one chain.
type redactionRegistry struct{}

// processRedaction is the single registration interface. Registrations are
// append-only, so it needs no lifecycle of its own.
var processRedaction Redaction = redactionRegistry{}

// AddKeys registers sensitive key names. Empty names are ignored: they would
// match nothing, since a key path segment is only compared when it is non-empty.
func (redactionRegistry) AddKeys(keys ...string) {
	rootMu.Lock()
	defer rootMu.Unlock()

	current := currentRules()
	next := &ruleSet{
		keys:     make(map[string]struct{}, len(current.keys)+len(keys)),
		patterns: current.patterns,
	}
	maps.Copy(next.keys, current.keys)
	added := false
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, ok := next.keys[key]; ok {
			continue
		}
		next.keys[key] = struct{}{}
		added = true
	}
	if !added {
		return
	}
	rules.Store(next)
}

// AddPattern registers a value-shape rule. A nil matcher is ignored rather than
// stored, because storing it would panic on the first record that reaches it.
func (redactionRegistry) AddPattern(name string, match Matcher) {
	if match == nil {
		return
	}
	rootMu.Lock()
	defer rootMu.Unlock()

	current := currentRules()
	next := &ruleSet{
		keys:     current.keys,
		patterns: make([]pattern, len(current.patterns), len(current.patterns)+1),
	}
	copy(next.patterns, current.patterns)
	next.patterns = append(next.patterns, pattern{name: name, match: match})
	rules.Store(next)
}
