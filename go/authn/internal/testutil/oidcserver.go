package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// OIDCServer is a complete, local OpenID Connect identity provider: discovery
// document, JWKS, authorization endpoint and token endpoint, backed by a
// freshly generated RSA key.
//
// It exists so that every test of this module's OpenID Connect code -- the
// Google channel and the per-tenant enterprise relying party -- runs against a
// real signed ID token that a real JWKS verifies, with NO network call. A test
// that stubbed the verification instead would prove the plumbing and skip the
// part that actually matters.
//
// The server listens on loopback, which the module's own SSRF guard correctly
// refuses to connect to. Tests therefore inject a plain HTTP client; that is
// the guard working, not a hole in it, and there is a separate test proving
// the guarded path refuses a private address.
type OIDCServer struct {
	// Server is the running test server. Its URL is the issuer.
	Server *httptest.Server

	// ClientID is the audience every issued ID token is minted for.
	ClientID string

	key *rsa.PrivateKey
	kid string

	mu sync.Mutex
	// nextIDToken is handed out by the token endpoint on the next
	// exchange. Tests set it with QueueIDToken.
	nextIDToken string
	// tokenForms records what each token-endpoint call was sent, so a test
	// can assert that the client sent the right redirect_uri and code.
	tokenForms []url.Values
	// failTokenExchange makes the token endpoint refuse, so a test can
	// prove the failure path.
	failTokenExchange bool
	// omitIDToken makes the token endpoint answer without one.
	omitIDToken bool
}

// NewOIDCServer starts an identity provider whose ID tokens are minted for
// clientID.
func NewOIDCServer(t *testing.T, clientID string) *OIDCServer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the identity provider's signing key: %v", err)
	}

	server := &OIDCServer{ClientID: clientID, key: key, kid: "test-key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", server.handleDiscovery)
	mux.HandleFunc("/jwks", server.handleJWKS)
	mux.HandleFunc("/token", server.handleToken)
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	server.Server = httptest.NewServer(mux)
	t.Cleanup(server.Server.Close)
	return server
}

// URL returns the issuer URL.
func (s *OIDCServer) URL() string { return s.Server.URL }

// Client returns an HTTP client that can reach the server. It is a plain
// client with no SSRF guard, for the reason in the type's doc comment.
func (s *OIDCServer) Client() *http.Client { return s.Server.Client() }

// QueueIDToken sets the ID token the next token exchange returns.
func (s *OIDCServer) QueueIDToken(idToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextIDToken = idToken
}

// FailTokenExchange makes the token endpoint refuse every exchange.
func (s *OIDCServer) FailTokenExchange() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failTokenExchange = true
}

// OmitIDToken makes the token endpoint answer with an access token and no ID
// token, which is what a misconfigured provider does and what the code under
// test must refuse rather than shrug at.
func (s *OIDCServer) OmitIDToken() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.omitIDToken = true
}

// TokenForms returns what each token-endpoint call carried.
func (s *OIDCServer) TokenForms() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]url.Values, len(s.tokenForms))
	copy(out, s.tokenForms)
	return out
}

// IDTokenClaims is the claim set SignIDToken mints.
type IDTokenClaims struct {
	// Subject is the "sub" claim, the provider's stable identifier.
	Subject string
	// Email and EmailVerified are the address claims the account-linking
	// rule turns on.
	Email         string
	EmailVerified any
	// Name and Picture are display claims.
	Name    string
	Picture string
	// Nonce is echoed into the token, binding it to one flow.
	Nonce string
	// Audience overrides the audience, so a test can mint a token for
	// somebody else's client and prove it is refused.
	Audience string
	// Issuer overrides the issuer for the same reason.
	Issuer string
	// ExpiresAt overrides expiry; the zero value means one hour ahead.
	ExpiresAt time.Time
}

// SignIDToken mints and signs an ID token with RS256.
func (s *OIDCServer) SignIDToken(t *testing.T, in IDTokenClaims) string {
	t.Helper()

	issuer := in.Issuer
	if issuer == "" {
		issuer = s.URL()
	}
	audience := in.Audience
	if audience == "" {
		audience = s.ClientID
	}
	expiry := in.ExpiresAt
	if expiry.IsZero() {
		expiry = time.Now().Add(time.Hour)
	}

	claims := jwt.MapClaims{
		"iss": issuer,
		"aud": audience,
		"sub": in.Subject,
		"iat": time.Now().Add(-time.Minute).Unix(),
		"exp": expiry.Unix(),
	}
	if in.Email != "" {
		claims["email"] = in.Email
	}
	if in.EmailVerified != nil {
		claims["email_verified"] = in.EmailVerified
	}
	if in.Name != "" {
		claims["name"] = in.Name
	}
	if in.Picture != "" {
		claims["picture"] = in.Picture
	}
	if in.Nonce != "" {
		claims["nonce"] = in.Nonce
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = s.kid
	signed, err := token.SignedString(s.key)
	if err != nil {
		t.Fatalf("sign the id token: %v", err)
	}
	return signed
}

// handleDiscovery serves the OpenID Connect discovery document.
func (s *OIDCServer) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                s.URL(),
		"authorization_endpoint":                s.URL() + "/authorize",
		"token_endpoint":                        s.URL() + "/token",
		"jwks_uri":                              s.URL() + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

// handleJWKS serves the public half of the signing key.
func (s *OIDCServer) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"kid": s.kid,
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(s.key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(s.key.E)).Bytes()),
		}},
	})
}

// maxTokenFormBytes bounds the token endpoint's request body so a test
// double never accepts an unbounded form post.
const maxTokenFormBytes = 1 << 16

// handleToken serves the token endpoint.
func (s *OIDCServer) handleToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenFormBytes)
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.tokenForms = append(s.tokenForms, r.PostForm)
	fail, omit, idToken := s.failTokenExchange, s.omitIDToken, s.nextIDToken
	s.mu.Unlock()

	if fail {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "invalid_grant"})
		return
	}

	body := map[string]any{
		"access_token": "test-access-token",
		"token_type":   "bearer",
		"expires_in":   3600,
	}
	if !omit {
		body["id_token"] = idToken
	}
	writeJSON(w, body)
}

// writeJSON writes v as a JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
