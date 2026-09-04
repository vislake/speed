package authn

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
)

// compile-time check that testutil.KeySource satisfies this package's own
// KeySource interface -- it is declared structurally in testutil (which
// cannot import authn, see that package's own doc comment), so this
// assignment is the actual proof the two agree.
var _ KeySource = (*testutil.KeySource)(nil)

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

	keys := testutil.NewKeySource(t, "kid-active")
	clock := testutil.NewClock(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()

	signer, err := NewSigner(keys, WithTokenClock(clock.Now), WithTokenTTL(15*time.Minute))
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}
	verifier, err := NewVerifier(keys, WithTokenClock(clock.Now))
	if err != nil {
		t.Fatalf("NewVerifier() error = %v", err)
	}

	want := testPrincipal()
	token, expiresAt, err := signer.Issue(ctx, want)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if got, exp := expiresAt, clock.Now().Add(15*time.Minute); !got.Equal(exp) {
		t.Errorf("expiry = %v, want %v", got, exp)
	}

	got, err := verifier.Verify(ctx, token)
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

	keys := testutil.NewKeySource(t, "kid-active")
	ctx := context.Background()
	signer, _ := NewSigner(keys)
	verifier, _ := NewVerifier(keys)

	principal := testPrincipal()
	principal.Email = "someone@example.com"

	token, _, err := signer.Issue(ctx, principal)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if strings.Contains(token, "example.com") {
		t.Fatal("the signed token contains the email address")
	}

	got, err := verifier.Verify(ctx, token)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got.Email != "" {
		t.Errorf("Verify().Email = %q, want empty: no email claim is minted", got.Email)
	}
}

func TestVerify_ExpiredTokenIsRejected(t *testing.T) {
	t.Parallel()

	keys := testutil.NewKeySource(t, "kid-active")
	clock := testutil.NewClock(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	signer, _ := NewSigner(keys, WithTokenClock(clock.Now), WithTokenTTL(time.Minute))
	verifier, _ := NewVerifier(keys, WithTokenClock(clock.Now))

	token, _, err := signer.Issue(ctx, testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	if _, freshErr := verifier.Verify(ctx, token); freshErr != nil {
		t.Fatalf("Verify() error = %v while the token is still fresh", freshErr)
	}

	clock.Advance(2 * time.Minute)
	_, err = verifier.Verify(ctx, token)
	if !hasCode(err, ErrTokenExpired.Code) {
		t.Fatalf("Verify() error = %v, want code %q", err, ErrTokenExpired.Code)
	}
}

// TestVerify_RejectsAlgNone is one half of the algorithm-confusion defence: a
// token that declares no signature at all must never be accepted, however
// well-formed its claims are.
func TestVerify_RejectsAlgNone(t *testing.T) {
	t.Parallel()

	keys := testutil.NewKeySource(t, "kid-active")
	ctx := context.Background()
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
	token.Header["kid"] = "kid-active"
	unsigned, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("build the alg=none token: %v", err)
	}

	if _, err := verifier.Verify(ctx, unsigned); !hasCode(err, ErrTokenInvalid.Code) {
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

	keys := testutil.NewKeySource(t, "kid-active")
	ctx := context.Background()
	verifier, _ := NewVerifier(keys)

	verificationKeys, err := keys.VerificationKeys(ctx, AccessTokenKeyPurpose)
	if err != nil || len(verificationKeys) != 1 {
		t.Fatalf("VerificationKeys() = %v, %v, want exactly one entry", verificationKeys, err)
	}
	pub, ok := verificationKeys[0].Public.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("VerificationKeys()[0].Public is %T, want ed25519.PublicKey", verificationKeys[0].Public)
	}

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
	token.Header["kid"] = "kid-active"
	forged, err := token.SignedString([]byte(pub))
	if err != nil {
		t.Fatalf("build the HMAC-forged token: %v", err)
	}

	if _, err := verifier.Verify(ctx, forged); !hasCode(err, ErrTokenInvalid.Code) {
		t.Fatalf("Verify() error = %v, want code %q: an HMAC token signed with the public verification key must be refused", err, ErrTokenInvalid.Code)
	}
}

func TestVerify_RejectsUnknownAndMissingKid(t *testing.T) {
	t.Parallel()

	keys := testutil.NewKeySource(t, "kid-active")
	ctx := context.Background()
	verifier, _ := NewVerifier(keys)

	// A token signed by a completely different, unregistered key.
	other := testutil.NewKeySource(t, "kid-unregistered")
	otherSigner, _ := NewSigner(other)
	foreign, _, err := otherSigner.Issue(ctx, testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if _, foreignErr := verifier.Verify(ctx, foreign); !hasCode(foreignErr, ErrTokenInvalid.Code) {
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
	sstr, err := unkeyed.SigningString()
	if err != nil {
		t.Fatalf("build the kid-less token's signing string: %v", err)
	}
	signed := sstr + "." + unkeyed.EncodeSegment(other.SignRaw([]byte(sstr)))
	if _, err := verifier.Verify(ctx, signed); !hasCode(err, ErrTokenInvalid.Code) {
		t.Errorf("Verify(no kid) error = %v, want code %q", err, ErrTokenInvalid.Code)
	}
}

// TestSignerVerifier_TokenSignedBeforeRotationStillVerifies is the rotation
// property: a token minted before the rotation keeps working, and every NEW
// token is signed with the new key. Without it, rotating a key would sign
// every outstanding session out at once. The rotation itself happens inside
// the shared KeySource -- exactly go/pki's own PromoteToActive shape,
// mirrored by testutil.KeySource.Rotate without depending on go/pki.
func TestSignerVerifier_TokenSignedBeforeRotationStillVerifies(t *testing.T) {
	t.Parallel()

	keys := testutil.NewKeySource(t, "kid-old")
	ctx := context.Background()
	signer, _ := NewSigner(keys)
	verifier, _ := NewVerifier(keys)

	oldToken, _, err := signer.Issue(ctx, testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	keys.Rotate(t, "kid-new")

	if _, oldErr := verifier.Verify(ctx, oldToken); oldErr != nil {
		t.Errorf("a token signed before the rotation no longer verifies: %v", oldErr)
	}

	freshToken, _, err := signer.Issue(ctx, testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	parsed, _, err := jwt.NewParser().ParseUnverified(freshToken, &accessClaims{})
	if err != nil {
		t.Fatalf("parse the fresh token's header: %v", err)
	}
	if kid := parsed.Header["kid"]; kid != "kid-new" {
		t.Errorf("new tokens are signed under kid %v, want the active key %q", kid, "kid-new")
	}
}

func TestIssue_RefusesAnIncompletePrincipal(t *testing.T) {
	t.Parallel()

	keys := testutil.NewKeySource(t, "kid-active")
	ctx := context.Background()
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
			if _, _, err := signer.Issue(ctx, tc.principal); err == nil {
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

	keys := testutil.NewKeySource(t, "kid-active")
	ctx := context.Background()
	signer, _ := NewSigner(keys, WithTokenIssuer("some-other-service"))
	verifier, _ := NewVerifier(keys)

	token, _, err := signer.Issue(ctx, testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if _, err := verifier.Verify(ctx, token); !hasCode(err, ErrTokenInvalid.Code) {
		t.Fatalf("Verify() error = %v, want code %q", err, ErrTokenInvalid.Code)
	}
}

// TestIssue_EveryTokenCarriesADistinctJTI proves two tokens issued at the same
// instant for the same principal are still distinguishable.
func TestIssue_EveryTokenCarriesADistinctJTI(t *testing.T) {
	t.Parallel()

	keys := testutil.NewKeySource(t, "kid-active")
	clock := testutil.NewClock(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	signer, _ := NewSigner(keys, WithTokenClock(clock.Now))

	first, _, err := signer.Issue(ctx, testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	second, _, err := signer.Issue(ctx, testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if first == second {
		t.Fatal("two tokens issued at the same instant are byte-identical; the jti is not per-token")
	}
}

// TestVerify_RejectsAlgorithmMismatch is the new gate
// docs/internal/22-pki.md's "authn's signing algorithm" section requires: a token
// header's alg must match the algorithm the kid's KeySource entry itself
// declares, even though the token is a completely genuine, correctly
// verifying Ed25519/EdDSA signature and the parser's own allowlist (single
// EdDSA) has already passed it. This is deliberately redundant with that
// allowlist today -- it becomes load-bearing the day a second algorithm is
// ever added, and the whole point is that the gate is already in place
// before that day arrives.
func TestVerify_RejectsAlgorithmMismatch(t *testing.T) {
	t.Parallel()

	keys := testutil.NewKeySource(t, "kid-active")
	ctx := context.Background()
	signer, _ := NewSigner(keys)
	verifier, _ := NewVerifier(keys)

	token, _, err := signer.Issue(ctx, testPrincipal())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	// The token above is a real, validly signed EdDSA token. Only the
	// KeySource's own record of what algorithm "kid-active" is changes.
	keys.SetAlgorithm("kid-active", "some-other-algorithm")

	if _, err := verifier.Verify(ctx, token); !hasCode(err, ErrTokenInvalid.Code) {
		t.Fatalf("Verify() with a mismatched declared algorithm error = %v, want code %q", err, ErrTokenInvalid.Code)
	}
}

// TestSigner_EnsurePurposeFailureIsSurfaced proves a KeySource that cannot
// provision the access-token purpose fails Issue loudly rather than minting
// a token some other way.
func TestSigner_EnsurePurposeFailureIsSurfaced(t *testing.T) {
	t.Parallel()

	keys := testutil.NewKeySource(t, "kid-active")
	keys.EnsureErr = fmt.Errorf("boom")
	ctx := context.Background()
	signer, _ := NewSigner(keys)

	if _, _, err := signer.Issue(ctx, testPrincipal()); err == nil {
		t.Fatal("Issue() with a failing EnsurePurpose succeeded, want an error")
	}
}

// countingKeySource wraps a *testutil.KeySource purely to count
// EnsurePurpose calls, since testutil.KeySource's own EnsurePurpose is a
// plain field read with nothing to instrument from outside the package.
type countingKeySource struct {
	*testutil.KeySource
	calls *int
}

func (c *countingKeySource) EnsurePurpose(ctx context.Context, purpose, algorithm string, maxCredentialLifetime time.Duration) error {
	*c.calls++
	return c.KeySource.EnsurePurpose(ctx, purpose, algorithm, maxCredentialLifetime)
}

var _ KeySource = (*countingKeySource)(nil)

// TestSigner_EnsurePurposeRunsExactlyOnce proves EnsurePurpose is not paid
// on every Issue call -- Signer.ensureOnce's own doc comment explains why.
func TestSigner_EnsurePurposeRunsExactlyOnce(t *testing.T) {
	t.Parallel()

	keys := testutil.NewKeySource(t, "kid-active")
	ctx := context.Background()

	var calls int
	counting := &countingKeySource{KeySource: keys, calls: &calls}
	signer, _ := NewSigner(counting)

	if _, _, err := signer.Issue(ctx, testPrincipal()); err != nil {
		t.Fatalf("Issue() (1st): %v", err)
	}
	if _, _, err := signer.Issue(ctx, testPrincipal()); err != nil {
		t.Fatalf("Issue() (2nd): %v", err)
	}
	if calls != 1 {
		t.Errorf("EnsurePurpose was called %d times across two Issue calls, want exactly 1", calls)
	}
}
