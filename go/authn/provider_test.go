package authn

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// assertErrorCode fails unless err is an *apperr.Error with the given code.
//
// Matching on the code rather than with errors.Is against a sentinel is
// required here for the reason service.go's hasCode documents: apperr's
// builders derive a new value instead of mutating the receiver, so a
// decorated error is never identical to the sentinel it came from.
func assertErrorCode(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %s", wantCode)
	}
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error = %v (%T), want an *apperr.Error with code %s", err, err, wantCode)
	}
	if appErr.Code != wantCode {
		t.Fatalf("error code = %s, want %s", appErr.Code, wantCode)
	}
}

// parseQuery returns the query parameters of an authorization URL.
func parseQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed.Query()
}

// stubProvider is a SocialProvider that returns whatever a test tells it to.
type stubProvider struct {
	name     string
	identity *ExternalIdentity
	err      error

	mu           sync.Mutex
	exchanges    int
	lastCode     string
	lastRedirect string
}

func (p *stubProvider) Name() string { return p.name }

func (p *stubProvider) AuthorizeURL(state, redirectURI string) string {
	return "https://provider.example.test/authorize?state=" + url.QueryEscape(state) +
		"&redirect_uri=" + url.QueryEscape(redirectURI)
}

func (p *stubProvider) Exchange(_ context.Context, code, redirectURI string) (*ExternalIdentity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exchanges++
	p.lastCode, p.lastRedirect = code, redirectURI
	if p.err != nil {
		return nil, p.err
	}
	clone := *p.identity
	return &clone, nil
}

func TestNewProviderRegistry_RejectsAWiringMistake(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		providers []SocialProvider
	}{
		{name: "a nil entry", providers: []SocialProvider{nil}},
		{name: "an entry with no name", providers: []SocialProvider{&stubProvider{name: ""}}},
		{
			name: "a duplicate",
			providers: []SocialProvider{
				&stubProvider{name: ProviderGoogle},
				&stubProvider{name: ProviderGoogle},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewProviderRegistry(tc.providers...); err == nil {
				t.Error("NewProviderRegistry() error = nil, want a wiring error")
			}
		})
	}
}

func TestProviderRegistry_GetAndNames(t *testing.T) {
	t.Parallel()

	registry, err := NewProviderRegistry(&stubProvider{name: ProviderGitHub}, &stubProvider{name: ProviderGoogle})
	if err != nil {
		t.Fatalf("NewProviderRegistry() error = %v", err)
	}

	if _, ok := registry.Get(ProviderGoogle); !ok {
		t.Error("Get(google) reported the channel as missing")
	}
	if _, ok := registry.Get(ProviderWeChat); ok {
		t.Error("Get(wechat) found a channel that was never registered")
	}

	names := registry.Names()
	if len(names) != 2 || names[0] != ProviderGitHub || names[1] != ProviderGoogle {
		t.Errorf("Names() = %v, want them sorted", names)
	}

	var empty *ProviderRegistry
	if _, ok := empty.Get(ProviderGoogle); ok {
		t.Error("a nil registry answered Get affirmatively")
	}
	if names := empty.Names(); names != nil {
		t.Errorf("a nil registry returned Names() = %v", names)
	}
}

// TestNewRedirectAllowlist_RefusesAnythingItCannotMatchExactly pins the
// entries a deployment must not be able to register. A relative URI or one
// carrying a fragment cannot be compared meaningfully against what a provider
// will echo back.
func TestNewRedirectAllowlist_RefusesAnythingItCannotMatchExactly(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"/callback",
		"app.example.com/callback",
		"https://app.example.com/callback#token",
	} {
		if _, err := NewRedirectAllowlist(raw); err == nil {
			t.Errorf("NewRedirectAllowlist(%q) error = nil, want a rejection", raw)
		}
	}
}

// TestRedirectAllowlist_MatchesExactly is the security property. Prefix
// matching on a redirect URI is how OAuth deployments get taken over:
// "https://app.example.com" as a prefix also allows
// "https://app.example.com.attacker.test".
func TestRedirectAllowlist_MatchesExactly(t *testing.T) {
	t.Parallel()

	allowlist, err := NewRedirectAllowlist(
		"https://app.example.com/callback",
		"https://app.example.com/callback", // a duplicate is collapsed
		" ",
		"speedapp://auth/callback",
	)
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	if got := len(allowlist.URIs()); got != 2 {
		t.Errorf("URIs() has %d entries, want 2 after the duplicate and the blank are dropped", got)
	}

	allowed := []string{"https://app.example.com/callback", "speedapp://auth/callback"}
	for _, uri := range allowed {
		if !allowlist.Allows(uri) {
			t.Errorf("Allows(%q) = false, want true", uri)
		}
	}

	refused := []string{
		"https://app.example.com/callback/",
		"https://app.example.com/callback?next=/",
		"https://app.example.com.attacker.test/callback",
		"https://app.example.com/callback/../evil",
		"http://app.example.com/callback",
		"",
	}
	for _, uri := range refused {
		if allowlist.Allows(uri) {
			t.Errorf("Allows(%q) = true; matching must be exact", uri)
		}
	}
}

func TestRedirectAllowlist_EmptyRefusesEverything(t *testing.T) {
	t.Parallel()

	var allowlist RedirectAllowlist
	if !allowlist.Empty() {
		t.Error("the zero allowlist does not report itself empty")
	}
	if allowlist.Allows("https://app.example.com/callback") {
		t.Error("the zero allowlist allowed a redirect; a deployment that has not enabled social login must refuse every flow")
	}
}

// newStateStore returns a StateStore over the standalone deployment mode's own
// key-value implementation, which doubles as the test double.
func newStateStore(t *testing.T, ttl time.Duration) (*StateStore, pkgcore.KVStore) {
	t.Helper()
	kv := pkgcore.NewMemoryKVStore()
	store, err := NewStateStore(kv, ttl)
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}
	return store, kv
}

func TestNewStateStore_RequiresAKeyValueStore(t *testing.T) {
	t.Parallel()

	if _, err := NewStateStore(nil, time.Minute); err == nil {
		t.Error("NewStateStore(nil) error = nil, want a wiring error")
	}
	store, _ := newStateStore(t, 0)
	if store == nil {
		t.Fatal("NewStateStore() returned nil for a zero ttl, which should fall back to the default")
	}
}

func TestStateStore_IssueThenConsume(t *testing.T) {
	t.Parallel()

	store, _ := newStateStore(t, time.Minute)
	binding := StateBinding{
		Provider:       ProviderGoogle,
		RedirectURI:    "https://app.example.com/callback",
		SessionBinding: BindingFromCookie("pre-auth-cookie"),
		LinkUserID:     "user-1",
		Nonce:          "nonce-1",
	}

	state, err := store.Issue(t.Context(), binding)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if state == "" {
		t.Fatal("Issue() returned an empty state")
	}

	got, err := store.Consume(t.Context(), state, ProviderGoogle, binding.SessionBinding)
	if err != nil {
		t.Fatalf("Consume() error = %v", err)
	}
	if got != binding {
		t.Errorf("Consume() = %+v, want %+v", got, binding)
	}
}

// TestStateStore_ConsumeIsSingleUse is the replay defence. A state value that
// worked once must never work again, because an authorization callback URL
// ends up in browser history, in proxy logs and in referrer headers.
func TestStateStore_ConsumeIsSingleUse(t *testing.T) {
	t.Parallel()

	store, _ := newStateStore(t, time.Minute)
	state, err := store.Issue(t.Context(), StateBinding{Provider: ProviderGoogle})
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	if _, firstErr := store.Consume(t.Context(), state, ProviderGoogle, ""); firstErr != nil {
		t.Fatalf("the first Consume() error = %v", firstErr)
	}
	_, err = store.Consume(t.Context(), state, ProviderGoogle, "")
	assertErrorCode(t, err, ErrOAuthStateInvalid.Code)
}

// TestStateStore_ConsumeHasExactlyOneWinnerUnderConcurrency proves the
// single-use guarantee is arbitrated by the store rather than by a read the
// caller performed a moment earlier. It matters under -race, which is where
// the CI matrix runs this.
func TestStateStore_ConsumeHasExactlyOneWinnerUnderConcurrency(t *testing.T) {
	t.Parallel()

	store, _ := newStateStore(t, time.Minute)
	state, err := store.Issue(t.Context(), StateBinding{Provider: ProviderGoogle})
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()
			if _, consumeErr := store.Consume(context.Background(), state, ProviderGoogle, ""); consumeErr == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if winners != 1 {
		t.Errorf("%d concurrent Consume() calls succeeded, want exactly 1", winners)
	}
}

// TestStateStore_ConsumeRefusesAMismatch walks every binding a state commits
// the callback to. Each one closes a different attack: a channel mismatch
// stops a code issued by one provider being presented to another, and a
// session mismatch stops an attacker starting a flow of their own and tricking
// a victim's browser into finishing it -- which would bind the ATTACKER's
// social account to the victim's session.
func TestStateStore_ConsumeRefusesAMismatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		provider       string
		sessionBinding string
		state          func(issued string) string
	}{
		{
			name:           "a different channel",
			provider:       ProviderGitHub,
			sessionBinding: "cookie-hash",
			state:          func(issued string) string { return issued },
		},
		{
			name:           "a different browser",
			provider:       ProviderGoogle,
			sessionBinding: "somebody-elses-cookie-hash",
			state:          func(issued string) string { return issued },
		},
		{
			name:           "no browser binding at all",
			provider:       ProviderGoogle,
			sessionBinding: "",
			state:          func(issued string) string { return issued },
		},
		{
			name:           "a state that was never issued",
			provider:       ProviderGoogle,
			sessionBinding: "cookie-hash",
			state:          func(string) string { return "a-forged-state-value" },
		},
		{
			name:           "an empty state",
			provider:       ProviderGoogle,
			sessionBinding: "cookie-hash",
			state:          func(string) string { return "" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, _ := newStateStore(t, time.Minute)
			issued, err := store.Issue(t.Context(), StateBinding{
				Provider:       ProviderGoogle,
				SessionBinding: "cookie-hash",
			})
			if err != nil {
				t.Fatalf("Issue() error = %v", err)
			}

			_, err = store.Consume(t.Context(), tc.state(issued), tc.provider, tc.sessionBinding)
			assertErrorCode(t, err, ErrOAuthStateInvalid.Code)
		})
	}
}

// TestStateStore_StoresNoUsableStateValue proves the stored key is derived
// from the state rather than being the state, so a dump of the key-value
// store does not hand somebody a usable value.
func TestStateStore_StoresNoUsableStateValue(t *testing.T) {
	t.Parallel()

	store, kv := newStateStore(t, time.Minute)
	state, err := store.Issue(t.Context(), StateBinding{Provider: ProviderGoogle})
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	if _, found, getErr := kv.Get(t.Context(), oauthStateKeyPrefix+state); getErr != nil || found {
		t.Error("the state value itself is a key in the store")
	}
	if _, found, getErr := kv.Get(t.Context(), stateKey(state)); getErr != nil || !found {
		t.Error("the derived key does not hold the record")
	}
}

func TestBindingFromCookie_HashesAndHandlesTheEmptyCase(t *testing.T) {
	t.Parallel()

	if got := BindingFromCookie(""); got != "" {
		t.Errorf("BindingFromCookie(\"\") = %q, want the empty string", got)
	}
	binding := BindingFromCookie("a-pre-auth-cookie-value")
	if strings.Contains(binding, "a-pre-auth-cookie-value") {
		t.Error("BindingFromCookie returned something containing the raw cookie")
	}
	if binding != BindingFromCookie("a-pre-auth-cookie-value") {
		t.Error("BindingFromCookie is not deterministic")
	}
}

func TestNewOAuthNonce_IsRandom(t *testing.T) {
	t.Parallel()

	first, err := NewOAuthNonce()
	if err != nil {
		t.Fatalf("NewOAuthNonce() error = %v", err)
	}
	second, err := NewOAuthNonce()
	if err != nil {
		t.Fatalf("NewOAuthNonce() error = %v", err)
	}
	if first == "" || first == second {
		t.Errorf("nonces %q and %q are not distinct random values", first, second)
	}
}

// TestFlexBool_AcceptsOnlyWhatItShould pins the decoder that decides whether
// an email address counts as verified. Widening the accepted set widens what
// the auto-link rule will act on.
func TestFlexBool_AcceptsOnlyWhatItShould(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw     string
		want    bool
		wantErr bool
	}{
		{raw: `true`, want: true},
		{raw: `false`, want: false},
		{raw: `"true"`, want: true},
		{raw: `"false"`, want: false},
		{raw: `"1"`, wantErr: true},
		{raw: `"yes"`, wantErr: true},
		{raw: `"TRUE"`, wantErr: true},
		{raw: `1`, wantErr: true},
		// A JSON null reaches UnmarshalJSON and decodes to false, which
		// is the safe direction: an absent or null claim must never be
		// read as "the provider verified this address".
		{raw: `null`, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			var got flexBool
			err := json.Unmarshal([]byte(tc.raw), &got)
			if tc.wantErr {
				if err == nil {
					t.Errorf("Unmarshal(%s) error = nil, want a rejection", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s) error = %v", tc.raw, err)
			}
			if bool(got) != tc.want {
				t.Errorf("Unmarshal(%s) = %v, want %v", tc.raw, bool(got), tc.want)
			}
		})
	}

	encoded, err := json.Marshal(flexBool(true))
	if err != nil || string(encoded) != "true" {
		t.Errorf("Marshal(flexBool(true)) = %s, %v; want true", encoded, err)
	}
}
