package notification

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"time"
)

// This file owns the double opt-in verification code: its shape, its
// lifetime, and the two one-way functions that let the module store a code
// it can later recognise without ever holding the plaintext.
//
// The design keeps exactly one pending code per contact, riding on the
// verified_contacts row itself (R2 adjudication: verification_codes is
// deliberately not a table). A row in status pending carries the code's
// SHA-256 hash and its expiry in its own columns; every other status leaves
// those columns as inert dead data -- the status gate, never the columns,
// is what makes a consumed code unusable.

// contactCodeDigits is how many decimal digits a verification code has. The
// code space is 10^contactCodeDigits; at 6 digits that is one million
// attempts to brute force, which is why verifying a code is rate limited
// per address and per tenant (see contact.go's rate-limit table).
const contactCodeDigits = 6

// contactCodeTTL is how long a pending code stays usable. A code a patient
// never typed in expires after this, and the pending contact then needs a
// resend -- the resend path stamps a fresh code rather than ever reusing
// the expired one.
const contactCodeTTL = 5 * time.Minute

// contactCodeSpace is 10^contactCodeDigits, the uniform upper bound
// generateContactCode draws from. crypto/rand needs an explicit bound; the
// constant is spelled out next to contactCodeDigits so the two cannot
// drift.
var contactCodeSpace = new(big.Int).Exp(big.NewInt(10), big.NewInt(contactCodeDigits), nil)

// generateContactCode returns a fresh code: contactCodeDigits decimal
// digits, drawn uniformly from the whole space through crypto/rand -- never
// math/rand, whose seedable stream must not feed a security control. The
// leading-zero form ("000123") is produced with %0*d so every code has
// exactly contactCodeDigits digits; a patient typing a shorter code must
// not succeed because the stored hash was of a differently padded value.
func generateContactCode() (string, error) {
	n, err := rand.Int(rand.Reader, contactCodeSpace)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", contactCodeDigits, n), nil
}

// hashContactCode returns the SHA-256 of code, hex encoded. This is the
// only form of a verification code that ever touches the database or the
// logs: the plaintext lives in exactly one place -- the message the
// transport delivered -- and a database read, a backup or a log line that
// leaks the hash leaks nothing the attacker can replay.
func hashContactCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// contactCodeHashesEqual compares two stored hashes in constant time. Both
// sides are always 64 hex characters here (every code is hashed before
// comparison), so the comparison length carries no information; the
// constant-time compare exists to make sure an attacker measuring response
// timing can never learn how many prefix characters of their guess matched
// the stored hash.
func contactCodeHashesEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// contactCodeMinutes is the {{.minutes}} parameter every verification-code
// message template interpolates: the code's lifetime in whole minutes, so
// the message can tell the patient how long the code stays valid without
// the template hardcoding a number that drifts from contactCodeTTL.
const contactCodeMinutes = int(contactCodeTTL / time.Minute)
