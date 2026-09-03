package main

// authn_e2e_test.go is this round's M1 exit condition: it drives the
// reference app's real, composed HTTP server (buildServer's actual output,
// exactly like server_test.go and public_config_test.go do) through all
// three sign-in entry points authn ships -- password, social (against a
// local GitHub-shaped test server, never a live provider), and phone plus
// an SMS code (the standalone deployment mode's console sender, captured
// through serverConfig.SMSOutput) -- each yielding a working access token
// that then successfully calls the notes API, plus the self-service
// session-management surface: list devices, view login history, revoke
// one device, and prove that device's refresh token now fails while
// another device's still works.
//
// Every one of the three channels authenticates the SAME demo account
// (registered once, with both an email and a phone number), which is what
// lets the social step exercise the auto-link rule
// (go/authn/identity.go's resolveSocialAccount) rather than the still-
// deferred brand-new-JIT-account path (go/authn/AGENTS.md's Known
// limitations: a brand-new account from an unmatched external identity
// cannot start a session until something grants it tenant membership,
// which this app's demoMemberships never does automatically -- see its own
// doc comment in server.go). Registering first and granting membership by
// hand sidesteps exactly that limitation, honestly, rather than working
// around it.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sync"
	"testing"

	"github.com/vislake/speed/go/authn"
)

// e2eEmail, e2ePhone and e2eRedirectURI are the one demo account and the one
// redirect URI this whole test authenticates against.
const (
	e2eEmail       = "e2e-demo@example.com"
	e2ePhone       = "+8613800000001"
	e2eRedirectURI = "https://e2e-test.example/callback"
)

// githubStub is a small, independent copy of
// go/authn/provider_github_test.go's own githubStub -- three endpoints
// standing in for GitHub's OAuth token exchange and profile/email lookups.
// It cannot import that file's unexported type: it belongs to go/authn's
// own internal test sources, a different module from this reference app
// entirely. This copy carries only what THIS test needs to drive the
// social channel through the real, composed HTTP server rather than
// go/authn's Service directly.
type githubStub struct {
	Server *httptest.Server

	mu     sync.Mutex
	userID int64
	email  string
}

func newGitHubStub(t *testing.T) *githubStub {
	t.Helper()

	stub := &githubStub{userID: 990001}
	mux := http.NewServeMux()

	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gh-e2e-access-token","token_type":"bearer","scope":"read:user,user:email"}`))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		stub.mu.Lock()
		id := stub.userID
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         id,
			"login":      "e2e-github-login",
			"name":       "E2E Demo",
			"email":      nil,
			"avatar_url": "",
		})
	})
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, _ *http.Request) {
		stub.mu.Lock()
		email := stub.email
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"email": email, "primary": true, "verified": true},
		})
	})

	stub.Server = httptest.NewServer(mux)
	t.Cleanup(stub.Server.Close)
	return stub
}

// provider wires a GitHubProvider at the stub, with the stub's own HTTP
// client because the stub listens on loopback -- which authn's default
// SSRF-guarded client correctly refuses (go/authn's own provider tests
// establish the same pattern).
func (s *githubStub) provider() authn.SocialProvider {
	return authn.NewGitHubProvider("e2e-github-client", "e2e-github-secret",
		authn.WithProviderAuthBaseURL(s.Server.URL),
		authn.WithProviderAPIBaseURL(s.Server.URL),
		authn.WithProviderHTTPClient(s.Server.Client()),
	)
}

// buildAuthnE2EServer wires buildServer with a real GitHub-shaped test
// provider, a redirect URI allowlisted for it, github's channel on the
// trusted list (so a verified email auto-links rather than refusing --
// go/authn/identity.go's resolveSocialAccount), and a captured SMS output
// buffer -- everything server.go's default production wiring leaves empty
// (serverConfig.SocialProviders/RedirectAllowlist/TrustedProviders' own doc
// comments explain why).
func buildAuthnE2EServer(t *testing.T) (*httptest.Server, serverConfig, *bytes.Buffer, *githubStub) {
	t.Helper()

	cfg := testConfig(t)
	var smsOut bytes.Buffer
	cfg.SMSOutput = &smsOut

	github := newGitHubStub(t)
	cfg.SocialProviders = []authn.SocialProvider{github.provider()}
	cfg.TrustedProviders = []string{authn.ProviderGitHub}

	allowlist, err := authn.NewRedirectAllowlist(e2eRedirectURI)
	if err != nil {
		t.Fatalf("build redirect allowlist: %v", err)
	}
	cfg.RedirectAllowlist = allowlist

	handler, cleanup, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, cfg, &smsOut, github
}

// authnJSON issues method against srv.URL+path, optionally bearer-authenticated,
// optionally carrying a JSON body, and decodes a JSON response into out
// (skipped when out is nil, e.g. a 204/202 with no body). It fails the test
// outright on a transport error or a JSON-decode error, but leaves status-
// code assertions to the caller, exactly like this package's other request
// helpers (server_test.go's notesRequest, public_config_test.go's doAs).
func authnJSON(t *testing.T, client *http.Client, method, urlStr, token string, body, out any) *http.Response {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body for %s %s: %v", method, urlStr, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, urlStr, reader)
	if err != nil {
		t.Fatalf("build %s %s request: %v", method, urlStr, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, urlStr, err)
	}
	if out != nil {
		defer resp.Body.Close()
		if decodeErr := json.NewDecoder(resp.Body).Decode(out); decodeErr != nil {
			t.Fatalf("decode %s %s response: %v", method, urlStr, decodeErr)
		}
	}
	return resp
}

// tokenPairResponse is the wire shape of AuthnTokenPair
// (go/authn/api/openapi.yaml).
type tokenPairResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Principal    struct {
		UserID    string `json:"user_id"`
		TenantID  string `json:"tenant_id"`
		SessionID string `json:"session_id"`
	} `json:"principal"`
}

// smsCodePattern extracts the six-digit code from the console SMS sender's
// captured output -- go/authn/verification.go's renderSMSCode formats it as
// "Your verification code is 123456...", but the exact surrounding text is
// locale-dependent (DefaultLocale is zh-CN -- go/authn/verification.go), so
// this matches the digits alone rather than the sentence around them.
var smsCodePattern = regexp.MustCompile(`\b(\d{6})\b`)

// TestAuthnE2E_ThreeLoginEntryPoints_AndSessionManagement is this round's
// M1 exit condition in full: password, social and phone+SMS sign-in each
// produce a working session against the real composed server, and the
// self-service session surface (list, history, revoke) behaves correctly
// across them.
func TestAuthnE2E_ThreeLoginEntryPoints_AndSessionManagement(t *testing.T) {
	srv, cfg, smsOut, github := buildAuthnE2EServer(t)
	client := srv.Client()

	// ---- Registration: one account, both an email and a phone number ----
	var registered struct {
		ID string `json:"id"`
	}
	registerResp := authnJSON(t, client, http.MethodPost, srv.URL+"/api/v1/authn/register", "",
		map[string]string{"email": e2eEmail, "phone": e2ePhone, "password": testPassword},
		&registered)
	if registerResp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want %d", registerResp.StatusCode, http.StatusCreated)
	}
	if registered.ID == "" {
		t.Fatal("register: response carried no id")
	}
	cfg.Memberships.Grant(registered.ID, "tenant-e2e")
	github.mu.Lock()
	github.email = e2eEmail
	github.mu.Unlock()

	// ---- (1) Password sign-in ----
	var passwordPair tokenPairResponse
	passwordResp := authnJSON(t, client, http.MethodPost, srv.URL+"/api/v1/authn/login/password", "",
		map[string]string{"identifier": e2eEmail, "password": testPassword, "tenant_id": "tenant-e2e", "device": "e2e-password"},
		&passwordPair)
	if passwordResp.StatusCode != http.StatusOK {
		t.Fatalf("password login status = %d, want %d", passwordResp.StatusCode, http.StatusOK)
	}
	if passwordPair.AccessToken == "" || passwordPair.RefreshToken == "" || passwordPair.Principal.SessionID == "" {
		t.Fatalf("password login response incomplete: %+v", passwordPair)
	}
	createNoteAs(t, srv, passwordPair.AccessToken, "created via password sign-in")

	// ---- (3) Phone + SMS-code sign-in (done before social, so its code is
	// the only one in the captured SMS output when we read it) ----
	smsRequestResp := authnJSON(t, client, http.MethodPost, srv.URL+"/api/v1/authn/login/sms/request", "",
		map[string]string{"phone": e2ePhone}, nil)
	if smsRequestResp.StatusCode != http.StatusAccepted {
		t.Fatalf("request SMS code status = %d, want %d", smsRequestResp.StatusCode, http.StatusAccepted)
	}
	match := smsCodePattern.FindStringSubmatch(smsOut.String())
	if match == nil {
		t.Fatalf("no 6-digit code found in captured SMS output: %q", smsOut.String())
	}
	code := match[1]

	var smsPair tokenPairResponse
	smsLoginResp := authnJSON(t, client, http.MethodPost, srv.URL+"/api/v1/authn/login/sms", "",
		map[string]string{"phone": e2ePhone, "code": code, "tenant_id": "tenant-e2e", "device": "e2e-sms"},
		&smsPair)
	if smsLoginResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(smsLoginResp.Body)
		t.Fatalf("sms login status = %d, want %d; body = %s", smsLoginResp.StatusCode, http.StatusOK, body)
	}
	if smsPair.AccessToken == "" || smsPair.RefreshToken == "" || smsPair.Principal.SessionID == "" {
		t.Fatalf("sms login response incomplete: %+v", smsPair)
	}
	createNoteAs(t, srv, smsPair.AccessToken, "created via sms sign-in")

	// ---- (2) Social sign-in (GitHub), auto-linked onto the same account
	// by its verified email -- github is on cfg.TrustedProviders ----
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("build cookie jar: %v", err)
	}
	socialClient := &http.Client{Jar: jar}

	authorizeURL := srv.URL + "/api/v1/authn/social/github/authorize?redirect_uri=" + url.QueryEscape(e2eRedirectURI)
	var authorize struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	authorizeResp := authnJSON(t, socialClient, http.MethodGet, authorizeURL, "", nil, &authorize)
	if authorizeResp.StatusCode != http.StatusOK {
		t.Fatalf("social authorize status = %d, want %d", authorizeResp.StatusCode, http.StatusOK)
	}
	authorizeParsed, err := url.Parse(authorize.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse authorize_url %q: %v", authorize.AuthorizeURL, err)
	}
	state := authorizeParsed.Query().Get("state")
	if state == "" {
		t.Fatalf("authorize_url %q carried no state", authorize.AuthorizeURL)
	}

	var social struct {
		Tokens     tokenPairResponse `json:"tokens"`
		Created    bool              `json:"created"`
		Bound      bool              `json:"bound"`
		AutoLinked bool              `json:"auto_linked"`
	}
	callbackResp := authnJSON(t, socialClient, http.MethodPost, srv.URL+"/api/v1/authn/social/github/callback", "",
		map[string]string{"code": "e2e-github-code", "state": state, "tenant_id": "tenant-e2e"}, &social)
	if callbackResp.StatusCode != http.StatusOK {
		t.Fatalf("social callback status = %d, want %d", callbackResp.StatusCode, http.StatusOK)
	}
	if !social.AutoLinked {
		t.Errorf("social callback auto_linked = false, want true (github is on the trusted list and the email is verified)")
	}
	if social.Created {
		t.Error("social callback created = true, want false -- it should have auto-linked the account registered above, not made a new one")
	}
	if social.Tokens.AccessToken == "" || social.Tokens.RefreshToken == "" || social.Tokens.Principal.SessionID == "" {
		t.Fatalf("social callback response carried no tokens: %+v", social)
	}
	createNoteAs(t, srv, social.Tokens.AccessToken, "created via social sign-in")

	// All three sessions authenticated the SAME account and tenant, so all
	// three notes must be visible together, and every session id must be
	// distinct -- three real sessions, not one reused across channels.
	allNotes := listNotesAs(t, srv, passwordPair.AccessToken)
	if len(allNotes) != 3 {
		t.Fatalf("notes visible after all three sign-ins = %d, want 3 (%+v)", len(allNotes), allNotes)
	}
	sessionIDs := map[string]bool{
		passwordPair.Principal.SessionID:  true,
		smsPair.Principal.SessionID:       true,
		social.Tokens.Principal.SessionID: true,
	}
	if len(sessionIDs) != 3 {
		t.Fatalf("session ids were not all distinct: password=%q sms=%q social=%q",
			passwordPair.Principal.SessionID, smsPair.Principal.SessionID, social.Tokens.Principal.SessionID)
	}

	// ---- Session management: list devices, view login history ----
	var sessionsList struct {
		Sessions []struct {
			ID        string `json:"id"`
			Device    string `json:"device"`
			IsCurrent bool   `json:"is_current"`
		} `json:"sessions"`
	}
	sessionsResp := authnJSON(t, client, http.MethodGet, srv.URL+"/api/v1/authn/sessions", passwordPair.AccessToken, nil, &sessionsList)
	if sessionsResp.StatusCode != http.StatusOK {
		t.Fatalf("list sessions status = %d, want %d", sessionsResp.StatusCode, http.StatusOK)
	}
	seen := map[string]bool{}
	for _, s := range sessionsList.Sessions {
		seen[s.ID] = true
	}
	for name, id := range map[string]string{"password": passwordPair.Principal.SessionID, "sms": smsPair.Principal.SessionID, "social": social.Tokens.Principal.SessionID} {
		if !seen[id] {
			t.Errorf("session list is missing the %s session %q; got %+v", name, id, sessionsList.Sessions)
		}
	}

	var history struct {
		Attempts []struct {
			Method string `json:"method"`
			Result string `json:"result"`
		} `json:"attempts"`
	}
	historyResp := authnJSON(t, client, http.MethodGet, srv.URL+"/api/v1/authn/login-history", passwordPair.AccessToken, nil, &history)
	if historyResp.StatusCode != http.StatusOK {
		t.Fatalf("list login history status = %d, want %d", historyResp.StatusCode, http.StatusOK)
	}
	methodsSeen := map[string]bool{}
	for _, a := range history.Attempts {
		if a.Result == "success" {
			methodsSeen[a.Method] = true
		}
	}
	for _, method := range []string{"password", "sms", "social"} {
		if !methodsSeen[method] {
			t.Errorf("login history has no successful %q attempt; got %+v", method, history.Attempts)
		}
	}

	// ---- Revoke the sms device, then prove its refresh fails while the
	// password session's refresh still works ----
	revokeResp := authnJSON(t, client, http.MethodDelete, srv.URL+"/api/v1/authn/sessions/"+smsPair.Principal.SessionID, passwordPair.AccessToken, nil, nil)
	if revokeResp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(revokeResp.Body)
		t.Fatalf("revoke sms session status = %d, want %d; body = %s", revokeResp.StatusCode, http.StatusNoContent, body)
	}

	revokedRefreshResp := authnJSON(t, client, http.MethodPost, srv.URL+"/api/v1/authn/token/refresh", "",
		map[string]string{"refresh_token": smsPair.RefreshToken}, nil)
	if revokedRefreshResp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(revokedRefreshResp.Body)
		t.Fatalf("refresh the revoked sms session status = %d, want %d; body = %s",
			revokedRefreshResp.StatusCode, http.StatusUnauthorized, body)
	}

	var stillWorks tokenPairResponse
	stillWorksResp := authnJSON(t, client, http.MethodPost, srv.URL+"/api/v1/authn/token/refresh", "",
		map[string]string{"refresh_token": passwordPair.RefreshToken}, &stillWorks)
	if stillWorksResp.StatusCode != http.StatusOK {
		t.Fatalf("refresh the still-active password session status = %d, want %d", stillWorksResp.StatusCode, http.StatusOK)
	}
	if stillWorks.AccessToken == "" {
		t.Fatal("refreshing the still-active password session carried no access_token")
	}
	// The fresh access token still works against the notes API too, not
	// just against authn's own endpoints.
	createNoteAs(t, srv, stillWorks.AccessToken, "created after refreshing the surviving session")
	finalNotes := listNotesAs(t, srv, stillWorks.AccessToken)
	if len(finalNotes) != 4 {
		t.Fatalf("notes visible after the surviving session's refresh = %d, want 4 (%+v)", len(finalNotes), finalNotes)
	}
}
