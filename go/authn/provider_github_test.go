package authn

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// githubStub is a local stand-in for GitHub's three endpoints. Every GitHub
// test drives this rather than the network, so the suite is deterministic and
// runs offline.
type githubStub struct {
	Server *httptest.Server

	mu         sync.Mutex
	user       any
	emails     any
	userStatus int
	tokenForm  url.Values
	authHeader string
	apiVersion string
	failToken  bool
}

func newGitHubStub(t *testing.T) *githubStub {
	t.Helper()

	stub := &githubStub{userStatus: http.StatusOK}
	mux := http.NewServeMux()

	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		stub.mu.Lock()
		stub.tokenForm = r.PostForm
		fail := stub.failToken
		stub.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad_verification_code"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"gh-access-token","token_type":"bearer","scope":"read:user,user:email"}`))
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.authHeader = r.Header.Get("Authorization")
		stub.apiVersion = r.Header.Get("X-GitHub-Api-Version")
		user, status := stub.user, stub.userStatus
		stub.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(user)
	})

	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, _ *http.Request) {
		stub.mu.Lock()
		emails := stub.emails
		stub.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(emails)
	})

	stub.Server = httptest.NewServer(mux)
	t.Cleanup(stub.Server.Close)
	return stub
}

// provider wires a GitHubProvider at the stub, with a plain HTTP client
// because the stub listens on loopback -- which the guarded default client
// correctly refuses.
func (s *githubStub) provider() *GitHubProvider {
	return NewGitHubProvider("gh-client-id", "gh-client-secret",
		WithProviderAuthBaseURL(s.Server.URL),
		WithProviderAPIBaseURL(s.Server.URL),
		WithProviderHTTPClient(s.Server.Client()),
	)
}

func (s *githubStub) setUser(user any)     { s.mu.Lock(); defer s.mu.Unlock(); s.user = user }
func (s *githubStub) setEmails(emails any) { s.mu.Lock(); defer s.mu.Unlock(); s.emails = emails }

func TestGitHubProvider_NameAndAuthorizeURL(t *testing.T) {
	t.Parallel()

	stub := newGitHubStub(t)
	provider := stub.provider()

	if got := provider.Name(); got != ProviderGitHub {
		t.Errorf("Name() = %q, want %q", got, ProviderGitHub)
	}

	query := parseQuery(t, provider.AuthorizeURL("state-value", "https://app.example.com/callback"))
	if got := query.Get("client_id"); got != "gh-client-id" {
		t.Errorf("client_id = %q", got)
	}
	if got := query.Get("state"); got != "state-value" {
		t.Errorf("state = %q", got)
	}
	// user:email must be requested: GitHub omits a private primary
	// address from /user entirely, and without the scope such an account
	// looks like one with no email at all.
	if got := query.Get("scope"); got != "read:user user:email" {
		t.Errorf("scope = %q, want read:user and user:email", got)
	}
	if got := query.Get("client_secret"); got != "" {
		t.Error("the authorization URL carries the client secret")
	}
}

func TestGitHubProvider_Exchange_UsesTheNumericIDAndThePrimaryEmail(t *testing.T) {
	t.Parallel()

	stub := newGitHubStub(t)
	stub.setUser(map[string]any{
		"id": 4711, "login": "octocat", "name": "The Octocat",
		"email": "public@example.com", "avatar_url": "https://example.com/octocat.png",
	})
	stub.setEmails([]map[string]any{
		{"email": "secondary@example.com", "primary": false, "verified": true},
		{"email": "primary@example.com", "primary": true, "verified": true},
	})

	identity, err := stub.provider().Exchange(t.Context(), "the-code", "https://app.example.com/callback")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}

	// The numeric id is the only identifier that survives a rename;
	// keying on the login would hand a renamed account's identity to
	// whoever claims the freed name.
	if identity.ExternalID != "4711" {
		t.Errorf("ExternalID = %q, want the numeric account id", identity.ExternalID)
	}
	if identity.Email != "primary@example.com" {
		t.Errorf("Email = %q, want the primary address rather than the one on /user", identity.Email)
	}
	if !identity.EmailVerified {
		t.Error("EmailVerified = false for a verified primary address")
	}
	if identity.Name != "The Octocat" {
		t.Errorf("Name = %q", identity.Name)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.authHeader != "Bearer gh-access-token" {
		t.Errorf("Authorization header = %q", stub.authHeader)
	}
	if stub.apiVersion != githubAPIVersionHeader {
		t.Errorf("X-GitHub-Api-Version = %q, want %q", stub.apiVersion, githubAPIVersionHeader)
	}
	if got := stub.tokenForm.Get("redirect_uri"); got != "https://app.example.com/callback" {
		t.Errorf("token exchange redirect_uri = %q", got)
	}
}

// TestGitHubProvider_Exchange_OnlyThePrimaryEmailCounts is the account-linking
// input that is easiest to get wrong. A GitHub account may carry several
// verified addresses; accepting any of them would let somebody who added and
// verified a second address at GitHub link into whichever account here happens
// to use it.
func TestGitHubProvider_Exchange_OnlyThePrimaryEmailCounts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		emails        []map[string]any
		userEmail     string
		wantEmail     string
		wantVerified  bool
		wantSameAsAny bool
	}{
		{
			name: "an unverified primary is not verified",
			emails: []map[string]any{
				{"email": "other@example.com", "primary": false, "verified": true},
				{"email": "primary@example.com", "primary": true, "verified": false},
			},
			wantEmail:    "primary@example.com",
			wantVerified: false,
		},
		{
			name: "a verified secondary is never chosen",
			emails: []map[string]any{
				{"email": "victim@example.com", "primary": false, "verified": true},
				{"email": "attacker@example.com", "primary": true, "verified": true},
			},
			wantEmail:    "attacker@example.com",
			wantVerified: true,
		},
		{
			name:         "no email list at all falls back to /user, never as verified",
			emails:       nil,
			userEmail:    "public@example.com",
			wantEmail:    "public@example.com",
			wantVerified: false,
		},
		{
			name:         "no address anywhere",
			emails:       nil,
			userEmail:    "",
			wantEmail:    "",
			wantVerified: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stub := newGitHubStub(t)
			stub.setUser(map[string]any{"id": 99, "login": "somebody", "email": tc.userEmail})
			stub.setEmails(tc.emails)

			identity, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
			if err != nil {
				t.Fatalf("Exchange() error = %v", err)
			}
			if identity.Email != tc.wantEmail {
				t.Errorf("Email = %q, want %q", identity.Email, tc.wantEmail)
			}
			if identity.EmailVerified != tc.wantVerified {
				t.Errorf("EmailVerified = %v, want %v", identity.EmailVerified, tc.wantVerified)
			}
		})
	}
}

func TestGitHubProvider_Exchange_RefusesAnUnusableResponse(t *testing.T) {
	t.Parallel()

	t.Run("the token endpoint refused the code", func(t *testing.T) {
		t.Parallel()
		stub := newGitHubStub(t)
		stub.mu.Lock()
		stub.failToken = true
		stub.mu.Unlock()

		_, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
		assertErrorCode(t, err, ErrSocialExchangeFailed.Code)
	})

	t.Run("the user document carries no id", func(t *testing.T) {
		t.Parallel()
		stub := newGitHubStub(t)
		stub.setUser(map[string]any{"login": "somebody"})
		stub.setEmails(nil)

		_, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
		assertErrorCode(t, err, ErrSocialIdentityIncomplete.Code)
	})

	t.Run("the user endpoint failed", func(t *testing.T) {
		t.Parallel()
		stub := newGitHubStub(t)
		stub.mu.Lock()
		stub.userStatus = http.StatusForbidden
		stub.mu.Unlock()

		_, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
		assertErrorCode(t, err, ErrSocialExchangeFailed.Code)
	})
}
