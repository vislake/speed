package authn

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
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

// TokenKey is one Ed25519 keypair the token layer signs or verifies with.
type TokenKey struct {
	// ID is the key's "kid", carried in every token's header so a verifier
	// picks the right key instead of trying all of them.
	ID string
	// Private signs. It is nil on a retired, verification-only key.
	Private ed25519.PrivateKey
	// Public verifies. It is required on every key, active or retired.
	Public ed25519.PublicKey
}

// GenerateTokenKey returns a fresh Ed25519 keypair under the given kid. It is
// the convenient way to produce a development or test key; a production
// deployment injects key material from its own secret manager.
func GenerateTokenKey(id string) (TokenKey, error) {
	if id == "" {
		return TokenKey{}, errors.New("authn: token key id must not be empty")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return TokenKey{}, fmt.Errorf("authn: generate token key %q: %w", id, err)
	}
	return TokenKey{ID: id, Private: priv, Public: pub}, nil
}

// KeySet holds the one key new tokens are signed with plus any number of
// retired keys that are still accepted for verification.
//
// The shape mirrors dbkit.NewCipher(activeKey, retiredKeys...) on purpose, so
// key rotation has ONE shape in this codebase: promote the new key to active,
// pass the outgoing key as retired, and tokens signed before the rotation
// keep verifying until they expire on their own. A rotation scheme that
// simply replaced the key would invalidate every outstanding session at the
// moment of the change.
type KeySet struct {
	active TokenKey
	verify map[string]ed25519.PublicKey
}

// NewKeySet builds a KeySet whose active signing key is active and whose
// retired keys verify but never sign.
//
// It rejects an active key without a private half, any key without a public
// half or an id, and duplicate ids -- each of which would otherwise surface
// much later as a token that cannot be verified by the very process that
// signed it.
func NewKeySet(active TokenKey, retired ...TokenKey) (*KeySet, error) {
	if active.ID == "" {
		return nil, errors.New("authn: active token key must have an id")
	}
	if len(active.Private) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("authn: active token key %q must carry an ed25519 private key", active.ID)
	}
	if len(active.Public) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("authn: active token key %q must carry an ed25519 public key", active.ID)
	}

	ks := &KeySet{active: active, verify: map[string]ed25519.PublicKey{active.ID: active.Public}}
	for _, key := range retired {
		if key.ID == "" {
			return nil, errors.New("authn: retired token key must have an id")
		}
		if len(key.Public) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("authn: retired token key %q must carry an ed25519 public key", key.ID)
		}
		if _, exists := ks.verify[key.ID]; exists {
			return nil, fmt.Errorf("authn: duplicate token key id %q", key.ID)
		}
		ks.verify[key.ID] = key.Public
	}
	return ks, nil
}

// publicKey returns the verification key registered under kid.
func (k *KeySet) publicKey(kid string) (ed25519.PublicKey, bool) {
	key, ok := k.verify[kid]
	return key, ok
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
	keys *KeySet
	cfg  tokenConfig
}

// NewSigner returns a Signer that mints tokens with keys' active key.
func NewSigner(keys *KeySet, opts ...TokenOption) (*Signer, error) {
	if keys == nil {
		return nil, errors.New("authn: NewSigner requires a key set")
	}
	return &Signer{keys: keys, cfg: newTokenConfig(opts)}, nil
}

// TTL reports how long tokens this Signer issues stay valid. Callers that
// need to size a revocation-list TTL to the remaining life of outstanding
// access tokens read it here rather than assuming the default.
func (s *Signer) TTL() time.Duration { return s.cfg.ttl }

// Issue mints a signed access token for p and returns it with its expiry.
//
// Every token carries a fresh "jti", so two tokens issued in the same second
// for the same principal are still distinct values -- which matters for
// anything that wants to reference one individually.
func (s *Signer) Issue(p Principal) (string, time.Time, error) {
	if p.UserID == "" {
		return "", time.Time{}, errors.New("authn: cannot issue a token without a user id")
	}
	if p.SessionID == "" {
		return "", time.Time{}, errors.New("authn: cannot issue a token without a session id")
	}
	if p.TenantID == "" {
		return "", time.Time{}, errors.New("authn: cannot issue a token without a tenant id")
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
	token.Header["kid"] = s.keys.active.ID

	signed, err := token.SignedString(s.keys.active.Private)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("authn: sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// Verifier validates access tokens. It is safe for concurrent use.
type Verifier struct {
	keys   *KeySet
	cfg    tokenConfig
	parser *jwt.Parser
}

// NewVerifier returns a Verifier for tokens signed under keys.
func NewVerifier(keys *KeySet, opts ...TokenOption) (*Verifier, error) {
	if keys == nil {
		return nil, errors.New("authn: NewVerifier requires a key set")
	}
	cfg := newTokenConfig(opts)
	return &Verifier{
		keys: keys,
		cfg:  cfg,
		parser: jwt.NewParser(
			// The algorithm allowlist is the single most important
			// line in this file. Without it a verifier honours the
			// "alg" header a token brought with it, and an attacker
			// hands over one saying "none" -- or one HMAC-signed with
			// the public key, which is not secret. With it, both are
			// refused before any key lookup happens.
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
// that is Middleware's job, because it needs a context and an I/O-capable
// store, while a Verifier is a pure function of the token and the keys.
//
// Failures come back as this module's structured errors:
// ErrTokenExpired for a well-formed token past its expiry, ErrTokenInvalid
// for everything else, each wrapping the underlying cause so it is available
// to a log without ever reaching a response body.
func (v *Verifier) Verify(raw string) (Principal, error) {
	claims := &accessClaims{}
	token, err := v.parser.ParseWithClaims(raw, claims, v.keyFunc)
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

// keyFunc resolves a token's "kid" header to the public key that must have
// signed it.
//
// It re-checks the signing method even though the parser's allowlist has
// already run. The redundancy is intentional: this function hands back an
// ed25519.PublicKey, and if it were ever reached with an HMAC method that
// public key would be used as an HMAC secret -- so the check lives next to
// the key that would be misused, not only in the parser configuration
// somebody might later edit.
func (v *Verifier) keyFunc(token *jwt.Token) (any, error) {
	if _, ok := token.Method.(*jwt.SigningMethodEd25519); !ok {
		return nil, fmt.Errorf("authn: unexpected signing method %q", token.Method.Alg())
	}
	kid, ok := token.Header["kid"].(string)
	if !ok || kid == "" {
		return nil, errors.New("authn: token has no kid header")
	}
	key, found := v.keys.publicKey(kid)
	if !found {
		return nil, fmt.Errorf("authn: no verification key registered under kid %q", kid)
	}
	return key, nil
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
