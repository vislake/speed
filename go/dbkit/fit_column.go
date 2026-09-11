package dbkit

import (
	"strings"
	"unicode/utf8"
)

// FitColumnValue renders v storable in a column of at most maxRunes
// characters: the result is always valid UTF-8 and never longer than
// maxRunes runes, whatever bytes v arrived with -- the two properties a
// VARCHAR(n) column requires on PostgreSQL, which counts characters and
// refuses invalid byte sequences with SQLSTATE 22021 (SQLite stores either
// silently, so the database cannot be the guard). It is the write-boundary
// fit for caller- or peer-supplied text on its way into a declaratively
// bounded column, so a value that cannot round-trip through the database
// never reaches it; a value that is already valid UTF-8 and within the
// bound is returned unchanged.
//
// Invalid UTF-8 runs are sanitized to the Unicode replacement character,
// one per consecutive run -- never dropped, since dropping them could
// concatenate two arbitrary byte runs into a different valid value -- and
// the cut, when one is needed, happens after that sanitization, at
// maxRunes runes rather than maxRunes bytes: a byte cut could split a
// multi-byte character and leave invalid UTF-8 in the value the column was
// about to refuse.
//
// cut reports whether the value had to be shortened. A value that only
// needed invalid-UTF-8 sanitization reports cut=false, so a caller that
// records the change can tell the two apart.
func FitColumnValue(v string, maxRunes int) (fitted string, cut bool) {
	if len(v) <= maxRunes && utf8.ValidString(v) {
		return v, false
	}
	runes := []rune(strings.ToValidUTF8(v, "\uFFFD"))
	cut = len(runes) > maxRunes
	if cut {
		runes = runes[:maxRunes]
	}
	return string(runes), cut
}
