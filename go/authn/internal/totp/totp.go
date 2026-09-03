// Package totp implements time-based one-time passwords, RFC 6238 built
// directly on RFC 4226's HOTP over the standard library's own crypto/hmac
// and crypto/sha1 -- no third-party dependency.
//
// The root CLAUDE.md is explicit that every dependency added to a speed
// module lands in every consuming project's go.sum, and RFC 6238 is roughly
// a hundred lines on top of an HMAC that is already in the standard
// library. Pulling in a whole TOTP library for that would cost every
// consumer of authn a dependency to save this package a page of code, so it
// is not pulled in; see this module's AGENTS.md for the fuller reasoning
// and the specific library that was weighed and rejected.
//
// This package deliberately implements ONE convention -- SHA-1, 6 digits,
// a 30-second step -- because that is what every mainstream authenticator
// app (Google Authenticator, Authy, 1Password, Microsoft Authenticator, the
// built-in ones in iOS and Android) hard-codes on the client side. A
// generator that accepted a different hash, digit count or period would
// produce a secret and a QR code that most of those apps silently get
// wrong, which is a worse failure mode than not offering the choice at all.
// The generic HOTP core underneath (unexported) is still algorithm- and
// digit-count-agnostic, which is what lets this package's own tests run the
// official RFC 4226 Appendix D and RFC 6238 Appendix B test vectors
// directly against it, including their SHA-256 and SHA-512 variants that
// the public API never exposes.
//
// No QR image is rendered here. ProvisioningURI builds the otpauth:// URI
// an authenticator app scans; turning that URI into a QR code image is
// display logic that belongs on the frontend, which already owns every
// other rendering decision in this codebase (see this module's AGENTS.md
// Known limitations for the dependency that was weighed and rejected for
// server-side QR generation).
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // SHA-1 is TOTP's own wire format (RFC 6238), used here only as an HMAC building block, not for anything requiring collision resistance.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// Digits is the length of the code this package generates and
	// validates. It matches every mainstream authenticator app's fixed
	// assumption -- see the package doc comment.
	Digits = 6

	// Period is the time step a code is valid for.
	Period = 30 * time.Second

	// SecretBytes is how much entropy GenerateSecret draws: 160 bits,
	// RFC 4226 Appendix B's recommended HOTP secret length and the value
	// every mainstream authenticator app expects.
	SecretBytes = 20
)

// ErrInvalidSecret is returned when a secret is not valid base32, so it
// cannot be the shared key this package's functions operate on.
var ErrInvalidSecret = errors.New("totp: secret is not valid base32")

// secretEncoding is RFC 4648 base32 without padding, upper case: the
// conventional otpauth:// secret alphabet, and what GenerateSecret produces.
var secretEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret draws SecretBytes of randomness and returns it base32
// encoded, ready to store (encrypted, at the caller's discretion) and to
// hand to ProvisioningURI.
func GenerateSecret() (string, error) {
	raw := make([]byte, SecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("totp: draw secret: %w", err)
	}
	return secretEncoding.EncodeToString(raw), nil
}

// Code computes the Digits-long code for secret at instant t, using the
// fixed SHA-1/Period convention this package implements.
func Code(secret string, t time.Time) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	return hotp(key, counterAt(t, Period), Digits, sha1.New), nil
}

// Validate reports whether code is a valid TOTP code for secret at the
// current moment, tolerating up to skew adjacent time steps of clock drift
// in either direction (skew is clamped to zero or above).
//
// It returns the matched step counter alongside the boolean, which is one
// value more than the RFC 6238 convention a caller might expect from a bare
// "is this code valid" check. That extra value is deliberate and is what
// makes replay prevention correct rather than approximate: with skew
// tolerance in effect the step that actually matched may be the previous or
// the next one, not necessarily "now", and a caller recording "the last
// accepted step" (to refuse the identical code being replayed inside its
// own validity window -- see this module's mfa.go) needs to record exactly
// which step matched, not merely the instant it happened to check at.
func Validate(secret, code string, skew int) (bool, int64) {
	key, err := decodeSecret(secret)
	if err != nil {
		return false, 0
	}
	if skew < 0 {
		skew = 0
	}
	now := int64(counterAt(time.Now(), Period)) //nolint:gosec // deliberately time.Now(): see the doc comment on Validate.
	for delta := -skew; delta <= skew; delta++ {
		counter := now + int64(delta)
		if counter < 0 {
			continue
		}
		want := hotp(key, uint64(counter), Digits, sha1.New)
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true, counter
		}
	}
	return false, 0
}

// ProvisioningURI builds the otpauth://totp/... URI an authenticator app
// scans (as a QR code the FRONTEND renders -- see the package doc comment).
// issuer is the deployment's display name; accountName is what
// distinguishes this account inside that issuer, conventionally an email
// address or a user id.
func ProvisioningURI(issuer, accountName, secret string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(accountName)
	values := url.Values{}
	values.Set("secret", secret)
	values.Set("issuer", issuer)
	values.Set("algorithm", "SHA1")
	values.Set("digits", strconv.Itoa(Digits))
	values.Set("period", strconv.Itoa(int(Period.Seconds())))
	return "otpauth://totp/" + label + "?" + values.Encode()
}

// decodeSecret parses a base32 secret, tolerant of the lower case, stray
// whitespace and padding that a person retyping one by hand tends to
// introduce, and of the padding external tools sometimes emit even though
// GenerateSecret never produces it.
func decodeSecret(secret string) ([]byte, error) {
	cleaned := strings.ToUpper(strings.Join(strings.Fields(secret), ""))
	cleaned = strings.TrimRight(cleaned, "=")
	if cleaned == "" {
		return nil, ErrInvalidSecret
	}
	key, err := secretEncoding.DecodeString(cleaned)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidSecret, err)
	}
	return key, nil
}

// counterAt is RFC 6238's time-step counter: the number of whole Periods
// that have elapsed since the Unix epoch at t. Per is a package constant
// here, always strictly positive, so the division below is always safe.
func counterAt(t time.Time, per time.Duration) uint64 {
	return uint64(t.Unix() / int64(per.Seconds())) //nolint:gosec // t.Unix() is non-negative for every real wall-clock time this package is ever evaluated at.
}

// hotp is RFC 4226's HOTP algorithm, generic over the digest and the digit
// count so this package's own tests can run it directly against the
// official RFC 4226 Appendix D (SHA-1, 6 digits) and RFC 6238 Appendix B
// (SHA-1/SHA-256/SHA-512, 8 digits) test vectors. Code and Validate are the
// only callers in non-test code, and they always pass sha1.New and Digits.
func hotp(key []byte, counter uint64, digits int, newHash func() hash.Hash) string {
	var counterBytes [8]byte
	binary.BigEndian.PutUint64(counterBytes[:], counter)

	mac := hmac.New(newHash, key)
	mac.Write(counterBytes[:])
	sum := mac.Sum(nil)

	// Dynamic truncation, RFC 4226 section 5.3: the low nibble of the
	// last byte selects a 4-byte window, whose top bit is then masked off
	// to keep the result a positive 31-bit integer.
	offset := sum[len(sum)-1] & 0x0f
	truncated := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])

	modulus := uint32(1)
	for range digits {
		modulus *= 10
	}
	return fmt.Sprintf("%0*d", digits, truncated%modulus)
}
