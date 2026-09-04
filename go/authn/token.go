package authn

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/vislake/speed/go/pkgcore"
)

const (
	// DefaultAccessTokenTTL is how long a new access token stays valid.
	// Fifteen minutes is the deliberate compromise behind the whole
	// session design: a JWT cannot be withdrawn once issued, so the
	// natural-expiry revocation mode's worst case -- how long a signed-out
	// device keeps working -- is exactly this number. Shortening it costs
	// refresh traffic; lengthening it lengthens that window.
	DefaultAccessTokenTTL = 15 * time.Minute

	// DefaultIssuer is the "iss" claim of tokens this module signs
	// and the issuer a verifier requires. It is a constant rather than a
	// URL because it identifies the issuing MODULE inside one deployment,
	// not a public discovery endpoint.
	DefaultIssuer = "speed-authn"

	// tokenSigningAlgorithm pins the JWS algorithm on both sides.
	//
	// EdDSA (Ed25519) rather than an HMAC family is what makes pinning
	// meaningful. With HS256 the verification key IS the signing key, so
	// anyone who can verify can also mint; with Ed25519 a verifier holds
	// only a public key. That in turn is what makes the classic algorithm
	// confusion attack -- take the public key, sign a token with HS256,
	// and hand it to a verifier that trusts the "alg" header -- possible
	// at all, and why it is refused here before any key is even looked up.
	tokenSigningAlgorithm = "EdDSA"
)

// Principal is the result of authentication: who is calling, which tenant
// they are calling inside, which session the call belongs to, and how that
// session was established.
//
// It deliberately carries NO roles and NO permissions. Authorization is
// rbac's job, and rbac must not import this package -- it takes only the
// tenant and the user, assembled by whoever authenticated. Putting a
// permission list here would put the authorization model inside the
// authentication result and, since the Principal travels in a token, freeze
// that list for the token's whole lifetime: a permission revoked at 10:00
// would keep working until the token expired.
type Principal struct {
	// UserID is the authenticated User.ID, the token's "sub" claim.
	UserID string

	// TenantID is the tenant this request acts inside, the token's "tid"
	// claim. It comes from the token and from nowhere else -- never from a
	// header, a query parameter or a body.
	TenantID pkgcore.TenantID

	// SessionID is the session the token belongs to, the token's "sid"
	// claim. It is what makes immediate revocation checkable without a
	// database read.
	SessionID string

	// Email is the account's address. It is populated only where the
	// caller has actually read the user record -- a sign-in result, a /me
	// response -- and is EMPTY on a Principal recovered from a token,
	// because no email claim is minted.
	//
	// Keeping the address out of the token is a deliberate cost: a handler
	// that wants it must read the user row. The alternative writes a piece
	// of personal data into a bearer credential that gets copied into
	// client storage, proxy logs and trace attributes, where nothing this
	// module controls can redact it.
	Email string

	// AMR lists the authentication methods this session was established
	// with ("password", "mfa:totp", "social:google"). Step-up
	// verification and any policy of the form "this operation requires a
	// second factor" decide by inspecting it.
	AMR []string
}

// AccessTokenKeyPurpose is the KeySource purpose access tokens are signed
// and verified under -- the exact string docs/internal/22-pki.md's
// "authn's integration" section names in its own EnsurePurpose example.
const AccessTokenKeyPurpose = "authn.access_token" //nolint:gosec // a KeySource purpose name, not a credential.

// accessTokenKeyAlgorithm is the key algorithm Signer asks KeySource to
// provision for AccessTokenKeyPurpose. Its value ("ed25519") deliberately
// matches go/pki's AlgorithmEd25519 constant's own value -- authn does not
// import go/pki (the whole point of the KeySource seam below), so this is
// authn's own copy of the same string, kept in step by the KeySource
// contract itself rather than by an import edge: whatever a KeySource
// implementation returns from VerificationKeys as a kid's Algorithm is
// compared against this constant by keyFunc's algorithm-consistency check
// (see Verifier.keyFunc), which is what actually keeps the two in sync at
// runtime, not a shared symbol.
const accessTokenKeyAlgorithm = "ed25519"

// KeySource is what Signer and Verifier need from a signing-key lifecycle
// provider. It is declared here, in go/authn, rather than imported from
// go/pki: docs/internal/22-pki.md's "authn's integration" section is
// explicit that authn must not import pki -- key-material lifecycle and "who is calling"
// are unrelated concerns, and pki's X.509 layer in particular has no
// business anywhere near JWT verification. go/pki's *Service satisfies this
// interface STRUCTURALLY, with zero import edge in either direction: every
// method below is declared using ONLY standard-library types, because two
// packages' own named types are never the same type, so structural
// satisfaction across a package boundary requires literal signature
// equality (this is also why VerificationKeys' return element is an
// anonymous struct rather than a named one -- a named type here would
// silently break the match). See go/pki/service.go's keySourceShape for the
// identical compile-time proof kept on that side of the boundary.
//
// A production deployment supplies a *pki.Service (WithKeySource,
// module.go); a unit test supplies a minimal fake -- neither needs the
// other package's types.
type KeySource interface {
	// EnsurePurpose declares that purpose needs a signing key of algorithm,
	// with a retiring overlap period that must eventually cover
	// maxCredentialLifetime. Signer calls it lazily, once, on its first
	// Issue (see Signer.ensureOnce) -- not from Module.Register, which per
	// pkgcore.Module's own contract may perform no I/O, and this call
	// necessarily does (it may create a signing key on first boot).
	EnsurePurpose(ctx context.Context, purpose, algorithm string, maxCredentialLifetime time.Duration) error

	// ActiveSigner returns the kid, algorithm and a context-aware signing
	// function for purpose's currently active key. Signer.Issue calls it on
	// every token issuance.
	ActiveSigner(ctx context.Context, purpose string) (kid string, algorithm string, sign func(context.Context, []byte) ([]byte, error), err error)

	// VerificationKeys returns every key for purpose that is still safe to
	// verify against. Verifier.keyFunc calls it on every token
	// verification.
	VerificationKeys(ctx context.Context, purpose string) ([]struct {
		KID       string
		Algorithm string
		Public    crypto.PublicKey
	}, error)
}

// tokenConfig accumulates the options shared by Signer and Verifier.
type tokenConfig struct {
	ttl    time.Duration
	issuer string
	now    func() time.Time
}

// TokenOption configures a Signer or a Verifier.
type TokenOption func(*tokenConfig)

// WithTokenTTL sets how long issued access tokens stay valid. It has no
// effect on a Verifier, which reads each token's own expiry. A non-positive
// duration is ignored.
func WithTokenTTL(d time.Duration) TokenOption {
	return func(c *tokenConfig) {
		if d > 0 {
			c.ttl = d
		}
	}
}

// WithTokenIssuer sets the "iss" claim a Signer writes and a Verifier
// requires. An empty issuer is ignored.
func WithTokenIssuer(issuer string) TokenOption {
	return func(c *tokenConfig) {
		if issuer != "" {
			c.issuer = issuer
		}
	}
}

// WithTokenClock replaces the source of the current time, so a test can issue
// a token that is already expired without sleeping. A nil function is
// ignored.
func WithTokenClock(now func() time.Time) TokenOption {
	return func(c *tokenConfig) {
		if now != nil {
			c.now = now
		}
	}
}

// newTokenConfig applies opts over the defaults.
func newTokenConfig(opts []TokenOption) tokenConfig {
	cfg := tokenConfig{ttl: DefaultAccessTokenTTL, issuer: DefaultIssuer, now: time.Now}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// Signer issues access tokens. It is safe for concurrent use.
type Signer struct {
	keySource KeySource
	purpose   string
	cfg       tokenConfig

	// ensureOnce/ensureErr make the first Issue call responsible for
	// EnsurePurpose (KeySource's own doc comment explains why it cannot
	// happen in Module.Register). Every later Issue call skips straight to
	// ActiveSigner: EnsurePurpose only ever does real work on a purpose's
	// very first bootstrap (KeySource.EnsurePurpose's own contract is
	// idempotent no-op once an active key exists), so paying its cost on
	// every token issuance would be pure waste. A failure is cached and
	// returned on every subsequent call too, rather than retried silently --
	// a signing key that failed to provision is a startup-shaped problem an
	// operator needs to see, not one that should keep failing quietly
	// forever behind a swallowed error.
	ensureOnce sync.Once
	ensureErr  error
}

// NewSigner returns a Signer that mints tokens for AccessTokenKeyPurpose
// through keySource.
func NewSigner(keySource KeySource, opts ...TokenOption) (*Signer, error) {
	if keySource == nil {
		return nil, errors.New("authn: NewSigner requires a KeySource")
	}
	return &Signer{keySource: keySource, purpose: AccessTokenKeyPurpose, cfg: newTokenConfig(opts)}, nil
}

// TTL reports how long tokens this Signer issues stay valid. Callers that
// need to size a revocation-list TTL to the remaining life of outstanding
// access tokens read it here rather than assuming the default.
func (s *Signer) TTL() time.Duration { return s.cfg.ttl }

// ensure runs EnsurePurpose exactly once for this Signer's lifetime, using
// ctx's cancellation/deadline/trace for that one call -- see the ensureOnce
// field's own doc comment for why this happens here rather than at
// construction. maxCredentialLifetime is this Signer's own configured TTL:
// docs/internal/22-pki.md's section on why the retiring overlap period's
// length is declared by the consumer is explicit that the retiring overlap
// period must cover an access token's full lifetime, and TTL is exactly
// that number.
func (s *Signer) ensure(ctx context.Context) error {
	s.ensureOnce.Do(func() {
		s.ensureErr = s.keySource.EnsurePurpose(ctx, s.purpose, accessTokenKeyAlgorithm, s.cfg.ttl)
	})
	return s.ensureErr
}

// Issue mints a signed access token for p and returns it with its expiry.
//
// Every token carries a fresh "jti", so two tokens issued in the same second
// for the same principal are still distinct values -- which matters for
// anything that wants to reference one individually.
func (s *Signer) Issue(ctx context.Context, p Principal) (string, time.Time, error) {
	if p.UserID == "" {
		return "", time.Time{}, errors.New("authn: cannot issue a token without a user id")
	}
	if p.SessionID == "" {
		return "", time.Time{}, errors.New("authn: cannot issue a token without a session id")
	}
	if p.TenantID == "" {
		return "", time.Time{}, errors.New("authn: cannot issue a token without a tenant id")
	}
	if err := s.ensure(ctx); err != nil {
		return "", time.Time{}, fmt.Errorf("authn: ensure signing key purpose %q: %w", s.purpose, err)
	}

	kid, algorithm, sign, err := s.keySource.ActiveSigner(ctx, s.purpose)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("authn: no active signing key for purpose %q: %w", s.purpose, err)
	}
	if algorithm != accessTokenKeyAlgorithm {
		return "", time.Time{}, fmt.Errorf("authn: active signing key %q for purpose %q declares algorithm %q, want %q", kid, s.purpose, algorithm, accessTokenKeyAlgorithm)
	}

	issuedAt := s.cfg.now()
	expiresAt := issuedAt.Add(s.cfg.ttl)

	claims := &accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.cfg.issuer,
			Subject:   p.UserID,
			ID:        newID(),
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			NotBefore: jwt.NewNumericDate(issuedAt),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
		TenantID:  string(p.TenantID),
		SessionID: p.SessionID,
		AMR:       p.AMR,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = kid

	// SigningString, not SignedString: sign is KeySource's context-aware
	// signing function, not a raw ed25519.PrivateKey jwt.Token.SignedString
	// could take directly (a KMS-backed KeySource's Sign is a network call
	// with no room for a context in that method's own signature -- see
	// KeySource's own doc comment). jwt's own SigningMethodEd25519.Sign
	// signs the signing string's raw bytes with crypto.Hash(0) (PureEdDSA,
	// no pre-hash) -- exactly the "input is the complete message" contract
	// go/pki's Signer.Sign documents for AlgorithmEd25519, so handing sign
	// the signing string's bytes directly, unmodified, is correct.
	signingString, err := token.SigningString()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("authn: build access token signing string: %w", err)
	}
	sig, err := sign(ctx, []byte(signingString))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("authn: sign access token: %w", err)
	}
	signed := signingString + "." + token.EncodeSegment(sig)
	return signed, expiresAt, nil
}

// Verifier validates access tokens. It is safe for concurrent use.
type Verifier struct {
	keySource KeySource
	purpose   string
	cfg       tokenConfig
	parser    *jwt.Parser
}

// NewVerifier returns a Verifier for tokens signed under
// AccessTokenKeyPurpose, resolving verification keys through keySource on
// every call.
func NewVerifier(keySource KeySource, opts ...TokenOption) (*Verifier, error) {
	if keySource == nil {
		return nil, errors.New("authn: NewVerifier requires a KeySource")
	}
	cfg := newTokenConfig(opts)
	return &Verifier{
		keySource: keySource,
		purpose:   AccessTokenKeyPurpose,
		cfg:       cfg,
		parser: jwt.NewParser(
			// The algorithm allowlist is the single most important
			// line in this file. Without it a verifier honours the
			// "alg" header a token brought with it, and an attacker
			// hands over one saying "none" -- or one HMAC-signed with
			// the public key, which is not secret. With it, both are
			// refused before any key lookup happens.
			//
			// docs/internal/22-pki.md's "authn's signing algorithm" section
			// keeps this allowlist single-EdDSA deliberately: every
			// Signer implementation (local today; vault/kmsaws in
			// round 4) can direct-sign Ed25519, so nothing forces a
			// second algorithm into the allowlist, and a second
			// algorithm here is a second attack surface for no
			// deployment that needs it.
			jwt.WithValidMethods([]string{tokenSigningAlgorithm}),
			jwt.WithIssuer(cfg.issuer),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
			jwt.WithTimeFunc(cfg.now),
		),
	}, nil
}

// Verify parses and validates raw and returns the Principal it asserts.
//
// The returned Principal's Email is always empty: no email claim is minted
// (see Principal.Email). Verify also does NOT consult the revocation list --
// that is Middleware's job, because it needs an I/O-capable store beyond
// what a Verifier holds.
//
// Failures come back as this module's structured errors:
// ErrTokenExpired for a well-formed token past its expiry, ErrTokenInvalid
// for everything else, each wrapping the underlying cause so it is available
// to a log without ever reaching a response body.
func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	claims := &accessClaims{}
	token, err := v.parser.ParseWithClaims(raw, claims, v.keyFunc(ctx))
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return Principal{}, ErrTokenExpired.WithCause(err)
		}
		return Principal{}, ErrTokenInvalid.WithCause(err)
	}
	if !token.Valid {
		return Principal{}, ErrTokenInvalid
	}
	if claims.Subject == "" || claims.SessionID == "" || claims.TenantID == "" {
		return Principal{}, ErrTokenInvalid.WithCause(errors.New("authn: token is missing sub, sid or tid"))
	}

	return Principal{
		UserID:    claims.Subject,
		TenantID:  pkgcore.TenantID(claims.TenantID),
		SessionID: claims.SessionID,
		AMR:       claims.AMR,
	}, nil
}

// keyFunc returns a jwt.Keyfunc closed over ctx -- jwt.Keyfunc's own
// signature carries no context.Context, and KeySource.VerificationKeys
// needs one, so Verify builds a fresh closure per call rather than Verifier
// holding a jwt.Keyfunc field.
//
// The returned function re-checks the signing method even though the
// parser's allowlist has already run: it hands back a public key that, if
// this were ever reached with an HMAC method, would be used as an HMAC
// secret -- so the check lives next to the key that would be misused, not
// only in the parser configuration somebody might later edit. It also
// enforces docs/internal/22-pki.md's "authn's signing algorithm" defense-in-depth
// rule: the kid's own declared Algorithm (from VerificationKeys) must equal
// accessTokenKeyAlgorithm, the algorithm this Verifier was built to trust --
// redundant while the parser allowlist admits only EdDSA, but a real second
// gate rather than documentation of one, so a future algorithm addition
// that forgets to update this check fails closed instead of silently
// trusting a key that never should have been offered for this purpose.
func (v *Verifier) keyFunc(ctx context.Context) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, fmt.Errorf("authn: unexpected signing method %q", token.Method.Alg())
		}
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errors.New("authn: token has no kid header")
		}

		keys, err := v.keySource.VerificationKeys(ctx, v.purpose)
		if err != nil {
			return nil, fmt.Errorf("authn: load verification keys for purpose %q: %w", v.purpose, err)
		}
		for _, key := range keys {
			if key.KID != kid {
				continue
			}
			if key.Algorithm != accessTokenKeyAlgorithm {
				return nil, fmt.Errorf("authn: kid %q declares algorithm %q, want %q", kid, key.Algorithm, accessTokenKeyAlgorithm)
			}
			return key.Public, nil
		}
		return nil, fmt.Errorf("authn: no verification key registered under kid %q", kid)
	}
}

// accessClaims is the claim set of an access token: the registered claims
// plus the three speed-specific ones.
//
// There is no permission claim and no email claim, for the reasons given on
// Principal.
type accessClaims struct {
	jwt.RegisteredClaims

	// TenantID is the tenant the bearer is currently acting inside.
	TenantID string `json:"tid"`
	// SessionID is the session the token belongs to.
	SessionID string `json:"sid"`
	// AMR lists how the session was authenticated.
	AMR []string `json:"amr,omitempty"`
}
