package pki

import (
	"context"
	"crypto"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// defaultKeyValidity is how long a signing key EnsurePurpose creates stays
// valid before it needs replacing. This round has no rotation job (that is
// round 2's jobs-driven expiry scan), so it is generous by design -- a key
// this module never revisits should not expire out from under a running
// deployment. Round 2 replaces this constant's role entirely once rotation
// has a real policy to consult.
const defaultKeyValidity = 24 * time.Hour * 365

// Service is the key-lifecycle layer's public entry point --
// docs/internal/22-pki.md's "two-layer structure" upper layer, and the type
// authn's round-2 KeySource switch will hold a *Service (or something
// structurally identical to it) behind.
//
// # What round 1 ships vs. what round 2 ships
//
// Every method below matches the shape docs/internal/22-pki.md's "authn's
// integration" section requires of authn's KeySource interface: signatures built
// entirely from standard-library types, because structural interface
// satisfaction across two packages' own named types requires exact,
// literal signature equality (two packages' named structs are never the
// same type -- see keySourceShape below for the compile-time proof this
// package carries of its own conformance). But the IMPLEMENTATION behind
// these signatures is deliberately simplified, per this round's explicit
// scope:
//
//   - EnsurePurpose does not stage a key into SigningKeyStatusPending and
//     wait for a propagation window -- it creates a key and marks it
//     SigningKeyStatusActive in the same call, synchronously. There is no
//     multi-replica cache-propagation race to protect against yet, because
//     nothing in this round reads a cached key set across replicas.
//   - There is no retiring/retired transition, no overlap-period logic
//     (maxCredentialLifetime is accepted and validated but not yet used to
//     size anything), and no revocation transition (SigningKeyStatusRevoked
//     is a real column value no code path in this round ever writes).
//   - There is no jobs-driven expiry scan, and no rotation: a key
//     EnsurePurpose created stays SigningKeyStatusActive until a future
//     round adds the state machine that can move it.
//
// This is a deliberate, documented "keep the column, skip the transition"
// choice -- not a half-built state machine. docs/internal/22-pki.md's own
// instruction for this round is to get the table's value set right, which
// requires the column to already accept all five values, while the
// transition LOGIC for four of those five values is explicitly round 2 and
// round 3 work. See AGENTS.md's Known limitations for the full list.
type Service struct {
	signer      Signer
	signerName  string
	signingKeys *SigningKeyRepository
}

// NewService returns a Service that creates keys through signer (recorded
// on every row under signerName) and persists them through signingKeys.
func NewService(signer Signer, signerName string, signingKeys *SigningKeyRepository) *Service {
	return &Service{signer: signer, signerName: signerName, signingKeys: signingKeys}
}

// EnsurePurpose declares that purpose needs a signing key of algorithm, with
// a retiring overlap period that must eventually cover maxCredentialLifetime
// -- see the type's own doc comment for why that parameter is accepted and
// validated but not yet used to size anything.
//
// Idempotent in the sense a caller needs at bootstrap: if purpose already
// has an active key, EnsurePurpose returns nil without creating a second
// one. It does not currently verify that the existing key's Algorithm
// matches the algorithm argument -- see AGENTS.md's Known limitations.
func (s *Service) EnsurePurpose(ctx context.Context, purpose, algorithm string, maxCredentialLifetime time.Duration) error {
	if purpose == "" {
		return fmt.Errorf("pki: EnsurePurpose requires a non-empty purpose")
	}
	if maxCredentialLifetime <= 0 {
		return fmt.Errorf("pki: EnsurePurpose requires a positive maxCredentialLifetime")
	}

	_, err := s.signingKeys.FindActiveByPurpose(ctx, purpose)
	if err == nil {
		// Already has an active key -- round 1 has no rotation policy to
		// apply, so this is a no-op.
		return nil
	}
	if !isNoActiveKey(err) {
		return err
	}

	keyRef, pub, err := s.signer.GenerateKey(ctx, algorithm)
	if err != nil {
		return err
	}
	pkix, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return fmt.Errorf("pki: marshal public key for purpose %q: %w", purpose, err)
	}

	now := time.Now().UTC()
	key := &SigningKey{
		ID:          uuid.NewString(),
		Purpose:     purpose,
		Algorithm:   algorithm,
		SignerName:  s.signerName,
		KeyRef:      keyRef,
		Status:      SigningKeyStatusActive,
		PublicKey:   pkix,
		NotBefore:   now,
		NotAfter:    now.Add(defaultKeyValidity),
		ActivatedAt: &now,
	}
	if err := s.signingKeys.Create(ctx, key); err != nil {
		return fmt.Errorf("pki: store signing key for purpose %q: %w", purpose, err)
	}

	observability.FromContext(ctx).Info("pki signing key activated",
		"kid", key.ID,
		"purpose", purpose,
		"algorithm", algorithm,
	)
	return nil
}

// ActiveSigner returns the kid, algorithm and a context-aware signing
// function for purpose's currently active key. ErrNoActiveKey if
// EnsurePurpose was never called for purpose (or, in a later round, if
// every key for it has since been revoked).
//
// It returns a signing function rather than a crypto.Signer for the exact
// reason Signer.Sign itself takes a context.Context -- see that interface's
// doc comment.
func (s *Service) ActiveSigner(ctx context.Context, purpose string) (string, string, func(context.Context, []byte) ([]byte, error), error) {
	key, err := s.signingKeys.FindActiveByPurpose(ctx, purpose)
	if err != nil {
		return "", "", nil, err
	}
	keyRef := key.KeyRef
	sign := func(ctx context.Context, input []byte) ([]byte, error) {
		return s.signer.Sign(ctx, keyRef, input)
	}
	return key.ID, key.Algorithm, sign, nil
}

// VerificationKeys returns every key for purpose that is still safe to
// verify against -- every row whose Status is not SigningKeyStatusRevoked.
// The anonymous return-slice element type is not a stylistic choice: a
// named type here would break structural satisfaction of authn's KeySource
// (docs/internal/22-pki.md's "authn's integration" section explains why two packages' named
// types can never satisfy one another structurally), so it is written out
// in full, matching KeySource's own declaration exactly.
func (s *Service) VerificationKeys(ctx context.Context, purpose string) ([]struct {
	KID       string
	Algorithm string
	Public    crypto.PublicKey
}, error,
) {
	rows, err := s.signingKeys.ListVerifiableByPurpose(ctx, purpose)
	if err != nil {
		return nil, err
	}
	out := make([]struct {
		KID       string
		Algorithm string
		Public    crypto.PublicKey
	}, 0, len(rows))
	for _, row := range rows {
		pub, err := x509.ParsePKIXPublicKey(row.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("pki: parse public key for kid %q: %w", row.ID, err)
		}
		out = append(out, struct {
			KID       string
			Algorithm string
			Public    crypto.PublicKey
		}{KID: row.ID, Algorithm: row.Algorithm, Public: pub})
	}
	return out, nil
}

// isNoActiveKey reports whether err is (a decorated) ErrNoActiveKey.
func isNoActiveKey(err error) bool {
	found, ok := apperr.As(err)
	return ok && found.Code == ErrNoActiveKey.Code
}

// keySourceShape mirrors, field for field and in the same order,
// go/authn's future KeySource interface as docs/internal/22-pki.md's
// "authn's integration" section specifies it. It exists purely as a compile-time
// proof that *Service already satisfies that shape TODAY, without this
// module importing authn (which would invert the module dependency
// direction docs/internal/01-architecture.md fixes: authn depends on pki,
// never the reverse). When authn's round-2 KeySource is declared for real,
// this is the interface it must be declared identically to; a mismatch
// here is this module's bug to fix, not authn's.
type keySourceShape interface {
	EnsurePurpose(ctx context.Context, purpose, algorithm string, maxCredentialLifetime time.Duration) error
	ActiveSigner(ctx context.Context, purpose string) (kid string, algorithm string, sign func(context.Context, []byte) ([]byte, error), err error)
	VerificationKeys(ctx context.Context, purpose string) ([]struct {
		KID       string
		Algorithm string
		Public    crypto.PublicKey
	}, error)
}

// compile-time check that *Service satisfies keySourceShape.
var _ keySourceShape = (*Service)(nil)
