package authn

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
)

// newTestKeySet returns a key set with one active key, for the common case.
func newTestKeySet(t *testing.T) (*KeySet, TokenKey) {
	t.Helper()
	key, err := GenerateTokenKey("kid-active")
	if err != nil {
		t.Fatalf("GenerateTokenKey() error = %v", err)
	}
	keys, err := NewKeySet(key)
	if err != nil {
		t.Fatalf("NewKeySet() error = %v", err)
	}
	return keys, key
}

// testPrincipal is the principal the token tests round-trip.
func testPrincipal() Principal {
	return Principal{
		UserID:    "user-1",
		TenantID:  pkgcore.TenantID("tenant-a"),
		SessionID: "session-1",
		AMR:       []string{MethodPassword, "mfa:totp"},
	}
}

func TestSignerVerifier_RoundTrip(t *testing.T) {
	t.Parallel()

	keys, _ := newTestKeySet(t)
	clock := testutil.NewClock(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))

	signer, err := NewSigner(keys, WithTokenClock(clock.Now), WithTokenTTL(15*time.Minute))
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}
	verifier, err := NewVerifier(keys, WithTokenClock(clock.Now))
	if err != nil {
		t.Fatalf("NewVerifier() error = %v", err)
	}

	want := testPrincipal()
	token, expiresAt, err := signer.Issue(want)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if got, exp := expiresAt, clock.Now().Add(15*time.Minute); !got.Equal(exp) {
		t.Errorf("expiry = %v, want %v", got, exp)
	}

	got, err := verifier.Verify(token)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got.UserID != want.UserID || got.TenantID != want.TenantID || got.SessionID != want.SessionID {
		t.Errorf("Verify() = %+v, want sub/tid/sid from %+v", got, want)
	}
	if strings.Join(got.AMR, " ") != strings.Join(want.AMR, " ") {
		t.Errorf("AMR = %v, want %v", got.AMR, want.AMR)
	}
}

// TestVerify_NoEmailClaimIsMinted pins the deliberate decision not to put
// personal data into a bearer credential. A Principal recovered from a token
// has no Email, and a change that starts minting one has to change this test
// first.
func TestVerify_NoEmailClaimIsMinted(t *testing.T) {
	t.Parallel()

	keys, _ := newTestKeySet(t)
	signer, _ := NewSigner(keys)
	verifier, _ := NewVerifier(keys)

	principal := testPrincipal()
	principal.Email = "someone@example.com"

	token, _, err := signer.Issue(principal)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if strings.Contains(token, "example.com") {
		t.Fatal("the signed token contains the email address")
	}

	got, err := verifier.Verify(token)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got.Email != "" {
		t.Errorf("Verify().Email = %q, want empty: no email claim is minted", got.Email)
	}
}

func TestVerify_ExpiredTokenIsRejected(t *testing.T) {
	t.Parallel()

	keys, _ := newTestKeySet(t)
	clock := testutil.NewClock(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	signer, _ := NewSigner(keys, WithTokenClock(clock.Now), WithTokenTTL(time.Minute))
	verifier, _ := NewVerifier(keys, WithTokenClock(clock.Now))

	token, _, err := signer.Issue(testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	if _, freshErr := verifier.Verify(token); freshErr != nil {
		t.Fatalf("Verify() error = %v while the token is still fresh", freshErr)
	}

	clock.Advance(2 * time.Minute)
	_, err = verifier.Verify(token)
	if !hasCode(err, ErrTokenExpired.Code) {
		t.Fatalf("Verify() error = %v, want code %q", err, ErrTokenExpired.Code)
	}
}

// TestVerify_RejectsAlgNone is one half of the algorithm-confusion defence: a
// token that declares no signature at all must never be accepted, however
// well-formed its claims are.
func TestVerify_RejectsAlgNone(t *testing.T) {
	t.Parallel()

	keys, key := newTestKeySet(t)
	verifier, _ := NewVerifier(keys)

	claims := &accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    DefaultIssuer,
			Subject:   "user-1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		TenantID:  "tenant-a",
		SessionID: "session-1",
	}
	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	token.Header["kid"] = key.ID
	unsigned, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("build the alg=none token: %v", err)
	}

	if _, err := verifier.Verify(unsigned); !hasCode(err, ErrTokenInvalid.Code) {
		t.Fatalf("Verify() error = %v, want code %q: an unsigned token must be refused", err, ErrTokenInvalid.Code)
	}
}

// TestVerify_RejectsHMACSignedWithThePublicKey is the other half, and the
// attack this whole design is shaped around: an Ed25519 verification key is
// PUBLIC, so if the verifier honoured the token's own "alg" header an
// attacker could take that public key, use it as an HMAC secret, and mint
// tokens the server would accept as its own.
func TestVerify_RejectsHMACSignedWithThePublicKey(t *testing.T) {
	t.Parallel()

	keys, key := newTestKeySet(t)
	verifier, _ := NewVerifier(keys)

	claims := &accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    DefaultIssuer,
			Subject:   "attacker",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		TenantID:  "tenant-a",
		SessionID: "session-1",
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = key.ID
	forged, err := token.SignedString([]byte(key.Public))
	if err != nil {
		t.Fatalf("build the HMAC-forged token: %v", err)
	}

	if _, err := verifier.Verify(forged); !hasCode(err, ErrTokenInvalid.Code) {
		t.Fatalf("Verify() error = %v, want code %q: an HMAC token signed with the public verification key must be refused", err, ErrTokenInvalid.Code)
	}
}

func TestVerify_RejectsUnknownAndMissingKid(t *testing.T) {
	t.Parallel()

	keys, _ := newTestKeySet(t)
	verifier, _ := NewVerifier(keys)

	// A token signed by a completely different, unregistered key.
	other, err := GenerateTokenKey("kid-unregistered")
	if err != nil {
		t.Fatalf("GenerateTokenKey() error = %v", err)
	}
	otherSet, err := NewKeySet(other)
	if err != nil {
		t.Fatalf("NewKeySet() error = %v", err)
	}
	otherSigner, _ := NewSigner(otherSet)
	foreign, _, err := otherSigner.Issue(testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if _, foreignErr := verifier.Verify(foreign); !hasCode(foreignErr, ErrTokenInvalid.Code) {
		t.Errorf("Verify(unknown kid) error = %v, want code %q", foreignErr, ErrTokenInvalid.Code)
	}

	// A correctly signed token whose kid header was removed.
	claims := &accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: DefaultIssuer, Subject: "user-1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		TenantID: "tenant-a", SessionID: "session-1",
	}
	unkeyed := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	delete(unkeyed.Header, "kid")
	signed, err := unkeyed.SignedString(other.Private)
	if err != nil {
		t.Fatalf("build the kid-less token: %v", err)
	}
	if _, err := verifier.Verify(signed); !hasCode(err, ErrTokenInvalid.Code) {
		t.Errorf("Verify(no kid) error = %v, want code %q", err, ErrTokenInvalid.Code)
	}
}

// TestKeySet_RetiredKeyVerifiesButDoesNotSign is the rotation property: a
// token minted before the rotation keeps working, and every NEW token is
// signed with the new key. Without it, rotating a key would sign every
// outstanding session out at once.
func TestKeySet_RetiredKeyVerifiesButDoesNotSign(t *testing.T) {
	t.Parallel()

	oldKey, err := GenerateTokenKey("kid-old")
	if err != nil {
		t.Fatalf("GenerateTokenKey() error = %v", err)
	}
	newKey, err := GenerateTokenKey("kid-new")
	if err != nil {
		t.Fatalf("GenerateTokenKey() error = %v", err)
	}

	before, err := NewKeySet(oldKey)
	if err != nil {
		t.Fatalf("NewKeySet() error = %v", err)
	}
	oldSigner, _ := NewSigner(before)
	oldToken, _, err := oldSigner.Issue(testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	// Rotate: the new key becomes active, the old one is kept for
	// verification only.
	after, err := NewKeySet(newKey, TokenKey{ID: oldKey.ID, Public: oldKey.Public})
	if err != nil {
		t.Fatalf("NewKeySet() error = %v", err)
	}
	rotatedSigner, _ := NewSigner(after)
	rotatedVerifier, _ := NewVerifier(after)

	if _, oldErr := rotatedVerifier.Verify(oldToken); oldErr != nil {
		t.Errorf("a token signed before the rotation no longer verifies: %v", oldErr)
	}

	freshToken, _, err := rotatedSigner.Issue(testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	parsed, _, err := jwt.NewParser().ParseUnverified(freshToken, &accessClaims{})
	if err != nil {
		t.Fatalf("parse the fresh token's header: %v", err)
	}
	if kid := parsed.Header["kid"]; kid != newKey.ID {
		t.Errorf("new tokens are signed under kid %v, want the active key %q", kid, newKey.ID)
	}

	// The retired key must not be usable for signing: a set whose ACTIVE
	// key has no private half is rejected outright.
	if _, err := NewKeySet(TokenKey{ID: oldKey.ID, Public: oldKey.Public}); err == nil {
		t.Error("NewKeySet() accepted a verification-only key as the active signing key")
	}
}

func TestNewKeySet_RejectsMalformedKeys(t *testing.T) {
	t.Parallel()

	good, err := GenerateTokenKey("kid-good")
	if err != nil {
		t.Fatalf("GenerateTokenKey() error = %v", err)
	}

	cases := []struct {
		name    string
		active  TokenKey
		retired []TokenKey
	}{
		{name: "active key with no id", active: TokenKey{Private: good.Private, Public: good.Public}},
		{name: "active key with no private half", active: TokenKey{ID: "x", Public: good.Public}},
		{name: "active key with no public half", active: TokenKey{ID: "x", Private: good.Private}},
		{name: "active key with a truncated private half", active: TokenKey{ID: "x", Private: ed25519.PrivateKey("short"), Public: good.Public}},
		{name: "retired key with no id", active: good, retired: []TokenKey{{Public: good.Public}}},
		{name: "retired key with no public half", active: good, retired: []TokenKey{{ID: "r"}}},
		{name: "duplicate kid", active: good, retired: []TokenKey{{ID: good.ID, Public: good.Public}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewKeySet(tc.active, tc.retired...); err == nil {
				t.Error("NewKeySet() error = nil, want a rejection")
			}
		})
	}
}

func TestIssue_RefusesAnIncompletePrincipal(t *testing.T) {
	t.Parallel()

	keys, _ := newTestKeySet(t)
	signer, _ := NewSigner(keys)

	cases := []struct {
		name      string
		principal Principal
	}{
		{name: "no user", principal: Principal{TenantID: "t", SessionID: "s"}},
		{name: "no session", principal: Principal{UserID: "u", TenantID: "t"}},
		{name: "no tenant", principal: Principal{UserID: "u", SessionID: "s"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := signer.Issue(tc.principal); err == nil {
				t.Error("Issue() error = nil, want a refusal")
			}
		})
	}
}

// TestVerify_RejectsAForeignIssuer proves the "iss" claim is enforced, so a
// token minted by another service that happens to share a key cannot be
// replayed here.
func TestVerify_RejectsAForeignIssuer(t *testing.T) {
	t.Parallel()

	keys, _ := newTestKeySet(t)
	signer, _ := NewSigner(keys, WithTokenIssuer("some-other-service"))
	verifier, _ := NewVerifier(keys)

	token, _, err := signer.Issue(testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if _, err := verifier.Verify(token); !hasCode(err, ErrTokenInvalid.Code) {
		t.Fatalf("Verify() error = %v, want code %q", err, ErrTokenInvalid.Code)
	}
}

// TestIssue_EveryTokenCarriesADistinctJTI proves two tokens issued at the same
// instant for the same principal are still distinguishable.
func TestIssue_EveryTokenCarriesADistinctJTI(t *testing.T) {
	t.Parallel()

	keys, _ := newTestKeySet(t)
	clock := testutil.NewClock(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	signer, _ := NewSigner(keys, WithTokenClock(clock.Now))

	first, _, err := signer.Issue(testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	second, _, err := signer.Issue(testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if first == second {
		t.Fatal("two tokens issued at the same instant are byte-identical; the jti is not per-token")
	}
}
