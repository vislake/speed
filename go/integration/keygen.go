package integration

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// apiKeyLiteralPrefix is the fixed literal every raw key starts with, so a
// value found in a log line, a config file or a git diff is recognizable as
// a speed API key on sight -- the same reasoning stripe and github use for
// their own "sk_"/"ghp_" prefixes. Round 1 issues one kind of key and draws
// no live/test distinction (the design doc's own "sk_live_a1b2" is an
// illustration of the shape, not a requirement to model two environments
// this round does not have); a later round that adds one can grow this
// into a parameter without changing the storage shape, since Prefix always
// stores whatever newAPIKeyToken actually produced.
const apiKeyLiteralPrefix = "sk_"

// apiKeyTokenBytes is the entropy of a raw key: 32 bytes from crypto/rand,
// the same strength go/org's invitation token and go/config's cipher key
// use. The key is a bearer credential exactly like an invitation token, so
// it gets the identical entropy budget.
const apiKeyTokenBytes = 32

// apiKeyDisplayPrefixRunes is how many characters of the encoded random
// portion (beyond the literal prefix) are kept in Prefix for display -- the
// design doc's own example, "sk_live_a1b2", shows four; this round keeps
// eight for a lower collision rate between two keys' display prefixes in a
// tenant with many keys, while still leaving the rest of the value entirely
// unguessable (32 bytes of entropy minus eight base64url characters is
// still far beyond brute-force range).
const apiKeyDisplayPrefixRunes = 8

// newAPIKeyToken returns a fresh raw API key and its stored hash. The raw
// value is base64url-encoded (RawURLEncoding, matching go/org's invitation
// token) so it is safe to put in a header value or a shell variable with no
// escaping, and prefixed with apiKeyLiteralPrefix.
//
// prefix is the plaintext portion Service.Create stores in APIKey.Prefix
// and returns as part of CreatedAPIKey -- apiKeyLiteralPrefix plus the
// first apiKeyDisplayPrefixRunes characters of the encoded random value.
// Storing it separately from computing it on read means a display prefix
// survives unchanged even if apiKeyDisplayPrefixRunes is ever tuned in a
// later release: an already-issued row keeps exactly the prefix it showed
// its caller once, not a value recomputed under new constants.
//
// A crypto/rand failure is fatal to the operation and is reported, never
// worked around with a weaker source -- the same posture org.
// newInvitationToken takes.
func newAPIKeyToken() (raw, prefix, hash string, err error) {
	buf := make([]byte, apiKeyTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", "", fmt.Errorf("integration: generating an API key: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(buf)
	raw = apiKeyLiteralPrefix + encoded

	displayLen := apiKeyDisplayPrefixRunes
	if displayLen > len(encoded) {
		displayLen = len(encoded)
	}
	prefix = apiKeyLiteralPrefix + encoded[:displayLen]

	return raw, prefix, hashAPIKeyToken(raw), nil
}

// hashAPIKeyToken returns the hex-encoded SHA-256 of a raw API key: what
// APIKey.Hash stores, and what a lookup would key on. Plain SHA-256, not a
// deliberately-slow password hash, is correct here for the same reason
// org.hashInvitationToken gives: the input is 32 bytes of full-entropy
// randomness, not a human-chosen secret, so there is no dictionary an
// attacker could use to make a slow hash worth paying for.
func hashAPIKeyToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// webhookSecretLiteralPrefix is the fixed literal every raw webhook secret
// starts with, so a value found in a log line or a receiver's config file
// is recognizable on sight -- the identical reasoning apiKeyLiteralPrefix's
// own doc comment gives, following Stripe's "whsec_" convention for the
// same kind of secret.
const webhookSecretLiteralPrefix = "whsec_"

// webhookSecretBytes is the entropy of a raw webhook secret: 32 bytes from
// crypto/rand, the same strength apiKeyTokenBytes and go/org's invitation
// token use -- this secret is an HMAC key, not a bearer credential
// presented on the wire the way an API key is, but it is still the entire
// basis of the authenticity guarantee webhook_signature.go's signature
// gives a receiver, so it gets the identical entropy budget.
const webhookSecretBytes = 32

// newWebhookSecret returns a fresh raw webhook signing secret,
// base64url-encoded (RawURLEncoding, matching newAPIKeyToken) and prefixed
// with webhookSecretLiteralPrefix.
//
// Unlike an API key, a webhook secret is never hashed for storage: it is
// stored reversibly encrypted (WebhookSecretSerializerName, webhook_model.go)
// because every delivery attempt must read it back in plaintext to compute
// that attempt's HMAC signature. newWebhookSecret itself has no opinion on
// that -- it only generates the value -- but its doc comment records the
// contrast because a reviewer used to round 1's "never store the raw value"
// API key rule should not "fix" webhook_model.go's Secret column into a
// hash by analogy.
//
// A crypto/rand failure is fatal to the operation and is reported, never
// worked around with a weaker source, matching newAPIKeyToken's identical
// posture.
func newWebhookSecret() (string, error) {
	buf := make([]byte, webhookSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("integration: generating a webhook secret: %w", err)
	}
	return webhookSecretLiteralPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}
