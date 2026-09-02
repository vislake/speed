package dbkit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm/clause"
)

// BlindIndex computes a deterministic HMAC-SHA256 index for normalized under
// key, hex-encoded. Store it in an indexed, non-encrypted column alongside an
// encrypted field to support exact-match lookups — a phone number used as a
// login identifier is the motivating case — without decrypting every row to
// find the one that matches.
//
// It is deterministic on purpose: the same key and the same normalized input
// always produce the same index, which is what lets "WHERE phone_index = ?"
// work at all against encrypted data. That determinism is also its limit —
// BlindIndex supports only exact-match lookups, never partial or fuzzy
// search (e.g. "last four digits of a phone number"); building that would
// mean leaking exactly the structure the encryption exists to hide, and is
// out of scope by design.
//
// normalized must already be in the caller's canonical form — E.164 for
// phone numbers, lowercased for email addresses, and so on. BlindIndex
// performs no normalization of its own, deliberately: guessing at a format
// here would silently compute two different indexes for what the caller
// considers the same value (e.g. "+1 555-0100" vs "+15550100"), which then
// surfaces as a login that mysteriously stops matching rather than as a
// visible error. Normalize identically at write time and at query time.
// Callers that want the normalization bundled with the key, the column, and
// the query condition should construct a BlindIndexer instead of calling
// this function directly.
//
// key must be a secret held completely separately from any key passed to
// NewCipher — see the key-separation warning on NewCipher for why mixing an
// encryption key and a blind-index key is a real cryptographic weakness
// rather than a style preference. Using HMAC rather than a bare hash is what
// keeps the index resistant to offline dictionary/rainbow-table attacks
// against the (typically low-entropy) plaintext space of things like phone
// numbers; a bare SHA-256 of the normalized value would not.
//
// BlindIndex itself performs no key validation: it returns a string, never
// an error, so an empty or short key is silently accepted and produces a
// well-formed-looking index whose key material offers none of the
// dictionary-attack resistance above. Fail-closed key validation lives in
// NewBlindIndexer, which rejects any key other than 32 bytes at
// construction; construct one (and use Index and Equal) rather than calling
// this raw function directly, unless the key has already been validated
// under that same policy.
//
// Rotating key requires recomputing the index for every existing row, run as
// a jobs batch task: unlike Cipher.Decrypt, there is no "retired key"
// fallback here, because a single equality comparison can only ever match
// one index value per row.
func BlindIndex(key []byte, normalized string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(normalized))
	return hex.EncodeToString(mac.Sum(nil))
}

// NormalizeFunc canonicalizes raw, caller-supplied input into the exact
// string that is blind-indexed, the "normalize" in the design contract
// HMAC(secret, normalize(plaintext)): docs/internal/10-compliance-and-audit.md
// requires that an index only ever be computed over a normalized value, or
// one value would produce two different indexes and equality lookups would
// silently stop matching.
//
// A NormalizeFunc must be deterministic and idempotent —
// normalize(normalize(x)) must equal normalize(x) — and must return an error
// rather than a best-effort value when it cannot canonicalize its input:
// silently "fixing up" a value into something the writer did not store is
// exactly how queries turn into mysteriously empty results. The mechanism
// never sees the raw input again after this function: it is normalized
// identically at write time (BlindIndexer.Index) and query time
// (BlindIndexer.Equal), and a value stored under one normalization can never
// be looked up under another.
//
// The built-in NormalizeEmail and NormalizePhoneE164 implement the two
// canonical forms the design docs promise for the motivating cases
// (lowercased email addresses, E.164 phone numbers). A caller with a
// different canonical form supplies its own NormalizeFunc and documents that
// form next to the model's column declaration; see BlindIndexer for how the
// per-column contract stays explicit.
type NormalizeFunc func(raw string) (string, error)

// NormalizeEmail implements the canonical form for email addresses: the
// input, trimmed of surrounding whitespace, lowercased. " User@Example.COM "
// and "user@example.com" both normalize to "user@example.com", so a value
// typed with stray spaces or a different case still indexes to (and queries)
// the same canonical address. It performs no structural validation: an input
// that is not a real email address normalizes consistently on both sides of
// a lookup and is the caller's input-quality problem, not a normalization
// ambiguity.
//
// It returns an error for an input that is empty or all whitespace: an
// absent value has no canonical form and must not be indexed at all — leave
// the index column NULL for it instead (see BlindIndexer).
func NormalizeEmail(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("dbkit: email normalization: input is empty")
	}
	return strings.ToLower(s), nil
}

// NormalizePhoneE164 implements the canonical form for phone numbers: an
// E.164 number — a leading "+", the country code, and the subscriber
// number — written without the formatting humans put around it. The
// separators space, "-", "(", ")" and "." are dropped, so "+1 555-0100",
// "+1(555)0100", "+1.555.0100" and "+15550100" all normalize to the same
// "+15550100".
//
// Anything that is not an E.164-form number is an error rather than a
// best-effort guess:
//
//   - No leading "+" is an error. A bare national number ("15550100")
//     cannot be normalized to E.164 without knowing its country, and this
//     normalizer deliberately never assumes a default country: silently
//     guessing one would compute a different index than a write that stored
//     the full E.164 form, and lookups would fail. Inputs must carry the
//     country code before they reach this normalizer.
//   - More than 15 digits is an error (the E.164 limit).
//   - Any other character — dialing extensions, letters, a second "+" — is
//     an error, so such input surfaces immediately instead of quietly
//     producing an index that never matches anything.
//
// Normalized input passes through unchanged, making the function
// deterministic and idempotent. It returns an error for an input that is
// empty or all whitespace; an absent value must not be indexed at all (see
// BlindIndexer).
func NormalizePhoneE164(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("dbkit: phone normalization: input is empty")
	}
	if s[0] != '+' {
		return "", errors.New("dbkit: phone normalization: missing leading \"+\": only E.164-form numbers (country code included) can be normalized, and a default country is never assumed")
	}

	var digits strings.Builder
	for _, r := range s[1:] {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
			// Formatting separator, dropped.
		default:
			return "", errors.New("dbkit: phone normalization: unexpected character: only \"+\", ASCII digits, and the separators \" \", \"-\", \"(\", \")\" and \".\" are allowed")
		}
	}

	if digits.Len() == 0 {
		return "", errors.New("dbkit: phone normalization: no digits after the leading \"+\"")
	}
	if digits.Len() > 15 {
		return "", errors.New("dbkit: phone normalization: more than 15 digits: E.164 numbers are at most 15 digits long")
	}
	return "+" + digits.String(), nil
}

// BlindIndexer is the per-column blind-index contract for one queryable
// encrypted field: it binds together the SQL column that stores the index
// values, the HMAC secret key, and the NormalizeFunc that turns raw input
// into that column's canonical form. Both sides of an equality lookup must
// go through the same BlindIndexer — writes store Index(raw) in the column,
// queries filter with Equal(raw) — which is what makes the normalization
// contract impossible to get wrong by accident: the query path never accepts
// a precomputed index value, and a different normalization only enters the
// picture through a second, deliberately constructed BlindIndexer bound to
// the same column.
//
// The blind-index column is declared on the model as an ordinary — never a
// serializer — indexed column, explicitly, next to the encrypted field it
// serves, so the schema stays visible to the gorm structs that versioned
// migrations are generated from (a blind index column holds plain 64-hex
// text, not anything an encryption serializer should touch):
//
//	type Account struct {
//	    Email      string `gorm:"serializer:email_enc"`              // encrypted at rest
//	    EmailIndex string `gorm:"column:email_index;size:64;index"`  // blind index
//	}
//
// and the matching BlindIndexer is constructed at bootstrap, where the
// secrets are injected:
//
//	emailIndex, err := dbkit.NewBlindIndexer("email_index", blindIndexKey, dbkit.NormalizeEmail)
//	if err != nil {
//	    // Key misconfiguration: fail the bootstrap, never start with a
//	    // mechanism that would silently write unusable indexes.
//	}
//
// A write stores the index computed over the same plaintext the serializer
// encrypts — never over ciphertext — into the declared column:
//
//	value, err := emailIndex.Index(form.Email)
//	if err != nil { /* the input has no canonical form; reject it */ }
//	account := Account{Email: form.Email, EmailIndex: value}
//
// and a lookup filters with the raw input:
//
//	cond, err := emailIndex.Equal(form.Email)
//	if err != nil { /* the input has no canonical form; reject it */ }
//	err := db.Where(cond).First(&account).Error
//
// Tenancy is deliberately not this mechanism's concern: the index column
// sits in the same (shared) table as the encrypted field, and tenant
// filtering is applied by the surrounding data-access layer — dbkit's
// tenant-scoping plugin, a dbkit.Repository[T], or the repository a module
// builds on top of them — exactly as it is for every other column.
//
// The mechanism supports equality lookups only, matching what a
// deterministic HMAC index can offer: partial or fuzzy search over the
// column would leak exactly the structure the encryption exists to hide and
// is out of scope by design (see BlindIndex).
//
// key is the column's HMAC secret: 32 bytes at construction (see
// NewBlindIndexer), supplied separately from every key passed to NewCipher
// — see NewCipher's key-separation warning and BlindIndex for why the two
// must never be the same secret — and reachable after construction only
// through Index and Equal: BlindIndexer exposes no accessor that returns
// the key bytes back out, mirroring Cipher.
//
// Rotating key means every existing row's index no longer matches and must
// be recomputed under the new key as a jobs batch task; there is no
// retired-key fallback here, because a column holds one index value per row
// and an equality comparison matches exactly one (see BlindIndex). A
// rotation therefore proceeds by constructing the successor BlindIndexer
// under the new key and recomputing the column before switching lookups
// over to it; the precise ordering, and whether the deployment tolerates a
// transient mismatch window or carries a second column through the
// migration, belongs to that jobs task's design.
//
// The zero BlindIndexer is not valid; construct one with NewBlindIndexer. A
// *BlindIndexer is safe for concurrent use by multiple goroutines: its
// fields are fixed at construction time and only read afterwards.
type BlindIndexer struct {
	column    string
	key       []byte
	normalize NormalizeFunc
}

// NewBlindIndexer builds the BlindIndexer for one blind-index column.
// column must be the column's exact SQL name — the value of the model
// field's `column:` gorm tag — and normalize the canonical form that column
// is indexed under; see BlindIndexer for how the two are used together.
//
// key must be exactly 32 bytes; NewBlindIndexer returns an error wrapping
// ErrInvalidKeySize otherwise — the same policy and sentinel NewCipher
// enforces for encryption keys, so the two secrets share one key shape in
// the secret manager. key must be a secret used for nothing else, and in
// particular must never be one passed to NewCipher (see NewCipher's
// key-separation warning). The bytes are copied: the caller's slice is not
// retained, and no accessor ever returns them back out.
//
// An empty column or a nil normalize is a programming error and returns an
// error rather than producing a BlindIndexer that would silently misbehave
// later — with an empty column it could never write a usable condition, and
// with no normalizer there would be no canonical form to index under at all.
func NewBlindIndexer(column string, key []byte, normalize NormalizeFunc) (*BlindIndexer, error) {
	if column == "" {
		return nil, errors.New("dbkit: blind index: column must not be empty")
	}
	if len(key) != encryptionKeySize {
		return nil, fmt.Errorf("dbkit: blind index column %q: %w: got %d bytes", column, ErrInvalidKeySize, len(key))
	}
	if normalize == nil {
		return nil, fmt.Errorf("dbkit: blind index column %q: normalizer must not be nil", column)
	}

	keyCopy := make([]byte, len(key))
	copy(keyCopy, key)
	return &BlindIndexer{column: column, key: keyCopy, normalize: normalize}, nil
}

// Index normalizes raw with the column's normalizer and returns the
// deterministic HMAC-SHA256 index value to store in the column: 64 hex
// characters. It is the write side of the contract — store its result in
// the column alongside the encrypted field, computed over the same
// plaintext value that gets encrypted.
//
// Index returns an error when raw has no canonical form under this column's
// normalizer, for example an empty input. An absent value must never be
// indexed: leave the column NULL for it instead. Consequently a
// BlindIndexer never yields an empty or best-effort index, and an index
// column ever holding "" is a bug in the write path, never something this
// type produced.
func (b *BlindIndexer) Index(raw string) (string, error) {
	return b.digest(raw)
}

// Equal normalizes raw exactly the way Index did for the row being looked
// up and returns the equality condition for this indexer's column — a gorm
// clause that composes directly into Where:
//
//	cond, err := emailIndex.Equal(" User@Example.COM ")
//	if err != nil { /* the input has no canonical form; reject it */ }
//	err := db.Where(cond).First(&account).Error
//
// Taking the raw input rather than a precomputed index value is deliberate:
// a caller cannot filter on a value computed under different normalization,
// because Equal is the only query path this mechanism offers and it always
// runs the column's own normalizer. The only way to produce a mismatched
// index is to bind a second BlindIndexer with a different normalizer to the
// same column — an arrangement that is visible in the bootstrap code, not
// an accident of a call site.
//
// The returned condition carries no tenant filter: whatever tenant scoping
// the surrounding layer adds (dbkit's tenant-scoping plugin, a
// Repository[T], or the repository a module built on them) applies to this
// query exactly as it would to any other, so for a TenantScoped model the
// plugin's own "WHERE tenant_id = ?" is still appended underneath.
func (b *BlindIndexer) Equal(raw string) (clause.Eq, error) {
	value, err := b.digest(raw)
	if err != nil {
		return clause.Eq{}, err
	}
	return clause.Eq{Column: b.column, Value: value}, nil
}

// digest normalizes raw under b's normalizer and computes the HMAC index
// over the normalized value, wrapping any normalization error with the
// column's identity so a failure names the column it happened on.
func (b *BlindIndexer) digest(raw string) (string, error) {
	normalized, err := b.normalize(raw)
	if err != nil {
		return "", fmt.Errorf("dbkit: blind index column %q: %w", b.column, err)
	}
	return BlindIndex(b.key, normalized), nil
}
