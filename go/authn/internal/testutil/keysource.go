package testutil

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"sync"
	"testing"
	"time"
)

// keySourceAlgorithm is this package's own copy of authn's
// accessTokenKeyAlgorithm / go/pki's AlgorithmEd25519 value ("ed25519").
// Duplicated rather than imported for the no-cycle reason the package
// comment gives -- this package cannot import authn, and importing go/pki
// here would give every authn test a transitive dependency this module's
// own go.mod does not otherwise carry.
const keySourceAlgorithm = "ed25519"

// KeySource is a minimal, in-memory stand-in for authn.KeySource (and,
// transitively, for the shape go/pki's real Service satisfies) shared by
// every test in this module -- both package authn's own tests
// (token_test.go and friends) and the external integration_test package,
// which cannot reach an unexported fake declared inside package authn's own
// test files. It holds one active signing key plus any number of
// additional verification-only keys, matching exactly what authn's
// Signer/Verifier ask a KeySource for. It does NOT reproduce a real
// KeySource's own validation (no duplicate kids, an active key with a
// private half, and so on) -- that behavior belongs to a real
// implementation (go/pki's own repository tests already cover it there),
// not to this test double.
type KeySource struct {
	mu sync.Mutex

	activeKID string
	active    ed25519.PrivateKey
	verify    map[string]keySourceEntry

	// EnsureErr, when non-nil, is what EnsurePurpose returns -- for a test
	// that needs Signer.Issue's first-call bootstrap to fail.
	EnsureErr error
}

type keySourceEntry struct {
	public    ed25519.PublicKey
	algorithm string
}

// NewKeySource returns a KeySource with one freshly generated active key
// under kid.
func NewKeySource(t *testing.T, kid string) *KeySource {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error = %v", err)
	}
	return &KeySource{
		activeKID: kid,
		active:    priv,
		verify:    map[string]keySourceEntry{kid: {public: pub, algorithm: keySourceAlgorithm}},
	}
}

// EnsurePurpose implements authn.KeySource structurally. This fake is
// always already "provisioned" (NewKeySource seeds the active key
// directly), so the only thing worth configuring is a canned failure (see
// EnsureErr).
func (k *KeySource) EnsurePurpose(context.Context, string, string, time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.EnsureErr
}

// ActiveSigner implements authn.KeySource structurally.
func (k *KeySource) ActiveSigner(_ context.Context, _ string) (string, string, func(context.Context, []byte) ([]byte, error), error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.activeKID == "" {
		return "", "", nil, fmt.Errorf("testutil.KeySource: no active key")
	}
	kid := k.activeKID
	priv := k.active
	entry := k.verify[kid]
	sign := func(_ context.Context, input []byte) ([]byte, error) {
		return ed25519.Sign(priv, input), nil
	}
	return kid, entry.algorithm, sign, nil
}

// VerificationKeys implements authn.KeySource structurally.
func (k *KeySource) VerificationKeys(context.Context, string) ([]struct {
	KID       string
	Algorithm string
	Public    crypto.PublicKey
}, error,
) {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]struct {
		KID       string
		Algorithm string
		Public    crypto.PublicKey
	}, 0, len(k.verify))
	for kid, e := range k.verify {
		out = append(out, struct {
			KID       string
			Algorithm string
			Public    crypto.PublicKey
		}{KID: kid, Algorithm: e.algorithm, Public: e.public})
	}
	return out, nil
}

// Rotate promotes a freshly generated key under newKID to active, demoting
// the previous active key to verification-only -- go/pki's own
// PromoteToActive semantics, mirrored here without depending on go/pki.
func (k *KeySource) Rotate(t *testing.T, newKID string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error = %v", err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.activeKID = newKID
	k.active = priv
	k.verify[newKID] = keySourceEntry{public: pub, algorithm: keySourceAlgorithm}
}

// SetAlgorithm overwrites kid's declared Algorithm in the verification set,
// without touching the actual Ed25519 key material -- for a test proving a
// token whose header alg does not match its kid's declared algorithm is
// rejected, even though the underlying signature is genuinely valid.
func (k *KeySource) SetAlgorithm(kid, algorithm string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	entry := k.verify[kid]
	entry.algorithm = algorithm
	k.verify[kid] = entry
}

// SignRaw signs input directly with the active private key, for a test that
// needs to build a jwt.Token by hand rather than through Signer.Issue.
func (k *KeySource) SignRaw(input []byte) []byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	return ed25519.Sign(k.active, input)
}
