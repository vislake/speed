package authn

import (
	"errors"
	"testing"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// newGoogleProvider wires a Google channel at a local identity provider.
//
// Both base URLs are overridden and a plain HTTP client is injected: the test
// server listens on loopback, which the module's SSRF-guarded default client
// correctly refuses. That refusal is proven separately in
// internal/safehttp/safehttp_test.go.
func newGoogleProvider(t *testing.T, server *testutil.OIDCServer) *GoogleProvider {
	t.Helper()
	return NewGoogleProvider("google-client-id", "google-client-secret",
		WithProviderAuthBaseURL(server.URL()),
		WithProviderAPIBaseURL(server.URL()),
		WithProviderHTTPClient(server.Client()),
	)
}

func TestGoogleProvider_Name(t *testing.T) {
	t.Parallel()

	if got := NewGoogleProvider("id", "secret").Name(); got != ProviderGoogle {
		t.Errorf("Name() = %q, want %q", got, ProviderGoogle)
	}
}

func TestGoogleProvider_AuthorizeURL_CarriesTheStateAndRedirect(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "google-client-id")
	provider := newGoogleProvider(t, server)

	raw := provider.AuthorizeURL("state-value", "https://app.example.com/callback")
	query := parseQuery(t, raw)

	for key, want := range map[string]string{
		"client_id":     "google-client-id",
		"redirect_uri":  "https://app.example.com/callback",
		"response_type": "code",
		"state":         "state-value",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("authorize %s = %q, want %q", key, got, want)
		}
	}
	// The client SECRET must never appear in a URL the browser is sent to.
	if got := query.Get("client_secret"); got != "" {
		t.Error("the authorization URL carries the client secret")
	}
}

// TestGoogleProvider_Exchange_VerifiesASignedIDToken is the happy path, and it
// is a real verification: the ID token is signed by a key the test generated,
// served through the local JWKS, and checked by go-oidc against the discovery
// document. Nothing here is stubbed.
func TestGoogleProvider_Exchange_VerifiesASignedIDToken(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "google-client-id")
	server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
		Subject:       "google-subject-1",
		Email:         "person@example.com",
		EmailVerified: true,
		Name:          "A Person",
		Picture:       "https://example.com/avatar.png",
	}))

	identity, err := newGoogleProvider(t, server).Exchange(t.Context(), "the-code", "https://app.example.com/callback")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}

	if identity.Provider != ProviderGoogle {
		t.Errorf("Provider = %q, want %q", identity.Provider, ProviderGoogle)
	}
	if identity.ExternalID != "google-subject-1" {
		t.Errorf("ExternalID = %q, want the subject claim", identity.ExternalID)
	}
	if identity.Email != "person@example.com" {
		t.Errorf("Email = %q", identity.Email)
	}
	if !identity.EmailVerified {
		t.Error("EmailVerified = false for a token whose email_verified claim is true")
	}
	if identity.Name != "A Person" || identity.Avatar == "" {
		t.Errorf("display fields = %q / %q", identity.Name, identity.Avatar)
	}
	if len(identity.Raw) == 0 {
		t.Error("Raw is empty; the provider's own document is kept for support")
	}

	// The token exchange must present the same redirect URI the
	// authorization request used, or a compliant provider refuses it.
	forms := server.TokenForms()
	if len(forms) != 1 {
		t.Fatalf("token endpoint was called %d times, want once", len(forms))
	}
	if got := forms[0].Get("redirect_uri"); got != "https://app.example.com/callback" {
		t.Errorf("token exchange redirect_uri = %q", got)
	}
	if got := forms[0].Get("code"); got != "the-code" {
		t.Errorf("token exchange code = %q", got)
	}
}

// TestGoogleProvider_Exchange_EmailVerifiedIsNeverInvented walks the claim
// shapes that decide whether an account may be auto-linked. Getting any of
// these wrong turns the linking rule into a rubber stamp.
func TestGoogleProvider_Exchange_EmailVerifiedIsNeverInvented(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		email         string
		emailVerified any
		want          bool
	}{
		{name: "boolean true", email: "a@example.com", emailVerified: true, want: true},
		{name: "boolean false", email: "a@example.com", emailVerified: false, want: false},
		{name: "string true", email: "a@example.com", emailVerified: "true", want: true},
		{name: "string false", email: "a@example.com", emailVerified: "false", want: false},
		{name: "claim absent", email: "a@example.com", emailVerified: nil, want: false},
		{name: "verified but no address at all", email: "", emailVerified: true, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := testutil.NewOIDCServer(t, "google-client-id")
			server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
				Subject:       "google-subject-1",
				Email:         tc.email,
				EmailVerified: tc.emailVerified,
			}))

			identity, err := newGoogleProvider(t, server).Exchange(t.Context(), "code", "https://app.example.com/callback")
			if err != nil {
				t.Fatalf("Exchange() error = %v", err)
			}
			if identity.EmailVerified != tc.want {
				t.Errorf("EmailVerified = %v, want %v", identity.EmailVerified, tc.want)
			}
		})
	}
}

// TestGoogleProvider_Exchange_RefusesATokenItCannotTrust covers every way the
// verification can fail. Each of these is a token an attacker could plausibly
// present, and each must be refused with a generic error rather than with the
// provider's own message.
func TestGoogleProvider_Exchange_RefusesATokenItCannotTrust(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		arrange func(t *testing.T, server *testutil.OIDCServer)
		wantErr string
	}{
		{
			name: "the token endpoint refused the code",
			arrange: func(_ *testing.T, server *testutil.OIDCServer) {
				server.FailTokenExchange()
			},
			wantErr: ErrSocialExchangeFailed.Code,
		},
		{
			name: "no id token came back",
			arrange: func(_ *testing.T, server *testutil.OIDCServer) {
				server.OmitIDToken()
			},
			wantErr: ErrSocialExchangeFailed.Code,
		},
		{
			name: "the id token was minted for a different client",
			arrange: func(t *testing.T, server *testutil.OIDCServer) {
				server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
					Subject:  "google-subject-1",
					Audience: "somebody-elses-client-id",
				}))
			},
			wantErr: ErrSocialExchangeFailed.Code,
		},
		{
			name: "the id token claims a different issuer",
			arrange: func(t *testing.T, server *testutil.OIDCServer) {
				server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
					Subject: "google-subject-1",
					Issuer:  "https://accounts.attacker.test",
				}))
			},
			wantErr: ErrSocialExchangeFailed.Code,
		},
		{
			name: "the id token is not a token at all",
			arrange: func(_ *testing.T, server *testutil.OIDCServer) {
				server.QueueIDToken("not.a.jwt")
			},
			wantErr: ErrSocialExchangeFailed.Code,
		},
		{
			name: "the id token carries no subject",
			arrange: func(t *testing.T, server *testutil.OIDCServer) {
				server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
					Email: "person@example.com",
				}))
			},
			wantErr: ErrSocialIdentityIncomplete.Code,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := testutil.NewOIDCServer(t, "google-client-id")
			tc.arrange(t, server)

			_, err := newGoogleProvider(t, server).Exchange(t.Context(), "code", "https://app.example.com/callback")
			assertErrorCode(t, err, tc.wantErr)

			// Whatever went wrong at the provider, its own words must
			// not reach a client: those bodies routinely echo the
			// client secret they were sent.
			appErr, ok := apperr.As(err)
			if !ok {
				t.Fatalf("error %v is not an *apperr.Error", err)
			}
			if len(appErr.Params) > 0 {
				for key, value := range appErr.Params {
					if text, isText := value.(string); isText && key != "provider" && text != "" {
						t.Errorf("error carries free text in parameter %q: %q", key, text)
					}
				}
			}
		})
	}
}

// TestGoogleProvider_Exchange_ReportsAnUnreachableIssuer proves discovery
// failure is a refusal rather than a panic or a nil identity.
func TestGoogleProvider_Exchange_ReportsAnUnreachableIssuer(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "google-client-id")
	provider := NewGoogleProvider("google-client-id", "secret",
		WithProviderAPIBaseURL(server.URL()+"/nowhere"),
		WithProviderHTTPClient(server.Client()),
	)

	_, err := provider.Exchange(t.Context(), "code", "https://app.example.com/callback")
	assertErrorCode(t, err, ErrSocialExchangeFailed.Code)
	if errors.Is(err, nil) {
		t.Fatal("Exchange() returned no error for an unreachable issuer")
	}
}
