package i18n

import (
	"strconv"
	"strings"
)

// Negotiate answers an Accept-Language header against the languages a
// renderer supports and returns the supported locale the header asks for
// first, in the supported set's own canonical spelling.
//
// The returned locale is the first header part that names a supported
// language, where a header tag matches a supported locale exactly ("en-US"
// for supported "en-US") or as its language prefix ("en" matching supported
// "en-US"), following RFC 4647's lookup semantics. Matching is
// case-insensitive, per that RFC's "language-range matching is
// case-insensitive" rule: a client's "EN-US" or "zh-cn" answers its
// language exactly as the canonical spellings do, because language tags are
// case-insensitive identifiers and a caseful comparison would refuse a
// perfectly usable language over letter case.
//
// The header's relative q-values are deliberately ignored beyond the zero
// weight: a part whose q parameter says the language is not acceptable -- a
// weight of zero in any spelling the qvalue grammar allows ("q=0", "q=0.0",
// "Q=0", "q=0.00") -- is skipped, and the remaining parts answer in header
// order, which is exactly the latitude RFC 7231's section 5.3.1 gives a
// server ("the most specific reference has precedence"; order of preference
// is expressed by the header's own ordering).
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
	for _, part := range strings.Split(header, ",") {
		tag := strings.TrimSpace(part)
		if tag == "" || tag == "*" {
			continue
		}
		if params := strings.Split(tag, ";"); len(params) > 1 {
			if !partAccepted(params[1:]) {
				// The part's q parameter weights it 0: not acceptable.
				continue
			}
			tag = strings.TrimSpace(params[0])
		}
		if tag == "" {
			continue
		}
		lowerTag := strings.ToLower(tag)
		for _, have := range supported {
			if strings.EqualFold(have, tag) ||
				strings.HasPrefix(strings.ToLower(have), lowerTag+"-") {
				return have, true
			}
		}
	}
	return "", false
}

// partAccepted reports whether one Accept-Language part's parameters still
// leave the part acceptable. The only parameter this negotiation reads is
// the q weighting, whose parameter name is case-insensitive (RFC 7230
// section 3.2.6): a weight of zero -- in any spelling the qvalue grammar
// produces, so "q=0", "q=0.0", "Q=0" and their padded variants -- marks the
// part not acceptable, and any parseable non-zero weight leaves it
// acceptable. An unparseable q value is not a zero weight and leaves the
// part standing at its default weight; where several q parameters appear,
// the last one governs. Relative ordering between non-zero weights is the
// caller's (header-order) business.
func partAccepted(params []string) bool {
	weight := 1.0
	weighted := false
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
		weighted = true
	}
	return !weighted || weight > 0
}
