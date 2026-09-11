package i18n

import (
	"slices"
	"strconv"
	"strings"
)

// Negotiate answers an Accept-Language header against the languages a
// renderer supports and returns the supported locale the header prefers, in
// the supported set's own canonical spelling.
//
// The returned locale is the first header part, in the header's own order of
// preference, that names a supported language, where a header tag matches a
// supported locale exactly ("en-US" for supported "en-US") or as its
// language prefix ("en" matching supported "en-US"), following RFC 4647's
// lookup semantics. Matching is case-insensitive, per that RFC's
// "language-range matching is case-insensitive" rule: a client's "EN-US" or
// "zh-cn" answers its language exactly as the canonical spellings do,
// because language tags are case-insensitive identifiers and a caseful
// comparison would refuse a perfectly usable language over letter case.
//
// The order of preference is the header's q weights, descending: a part
// whose q parameter says the language is not acceptable -- a weight of zero
// in any spelling the qvalue grammar allows ("q=0", "q=0.0", "Q=0",
// "q=0.00") -- is dropped, and the parts that remain answer by descending
// weight, so "zh-CN;q=0.8, en-US" answers en-US despite zh-CN coming first.
// Equal weights keep the header's own written order, and a part with no q
// parameter carries the qvalue grammar's default weight of one, as does a
// part whose q value cannot be parsed.
//
// ok reports whether the header matched anything. Negotiate itself never
// falls back to a default language: a header that is empty, absent, accepts
// nothing the supported set offers, or arrives before any supported set
// exists answers ("", false), and the caller owns the next tier of its own
// chain -- the platform default for the content chains, a stored profile
// value for the caller-identity chains. The fallback never crosses
// languages silently: a caller without a further tier answers its platform
// default, never a half-rendered or arbitrary language.
func Negotiate(header string, supported []string) (string, bool) {
	if len(supported) == 0 || strings.TrimSpace(header) == "" {
		return "", false
	}
	var parts []weightedTag
	for _, part := range strings.Split(header, ",") {
		tag := strings.TrimSpace(part)
		if tag == "" {
			continue
		}
		weight := 1.0
		if params := strings.Split(tag, ";"); len(params) > 1 {
			weight = partWeight(params[1:])
			tag = strings.TrimSpace(params[0])
		}
		// A wildcard names no concrete language, and a weight of zero (or a
		// NaN a hostile q spells) says the language is not acceptable; both
		// are dropped rather than ranked.
		if tag == "" || tag == "*" || !(weight > 0) {
			continue
		}
		parts = append(parts, weightedTag{tag: tag, weight: weight})
	}
	// Descending weight; the stable sort keeps equal weights in the header's
	// own written order, which is the tie-break the qvalue grammar leaves to
	// the sender.
	slices.SortStableFunc(parts, func(a, b weightedTag) int {
		switch {
		case a.weight > b.weight:
			return -1
		case a.weight < b.weight:
			return 1
		default:
			return 0
		}
	})
	for _, part := range parts {
		lowerTag := strings.ToLower(part.tag)
		for _, have := range supported {
			if strings.EqualFold(have, part.tag) ||
				strings.HasPrefix(strings.ToLower(have), lowerTag+"-") {
				return have, true
			}
		}
	}
	return "", false
}

// weightedTag is one acceptable Accept-Language part: the language tag it
// names and the q weight that ranks it.
type weightedTag struct {
	tag    string
	weight float64
}

// partWeight reports the q weight one Accept-Language part's parameters give
// it. The q parameter name is case-insensitive (RFC 7230 section 3.2.6); an
// absent q or an unparseable one leaves the part at the qvalue grammar's
// default weight of one -- an unparseable value neither sets a weight nor
// resets one already read -- and where several q parameters appear, the last
// parseable one governs.
func partWeight(params []string) float64 {
	weight := 1.0
	for _, p := range params {
		name, value, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		w, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			continue
		}
		weight = w
	}
	return weight
}
