package demoseed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/vislake/speed/go/authn"
)

// stubHandler records every request it is asked to serve and answers all of
// them with one scripted response, so a test can assert both the answer
// classification and the no-request properties (the demo-domain refusal and
// the lookup-first shape must both send nothing).
type stubHandler struct {
	requests []*http.Request
	status   int
	body     string
}

func (h *stubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.requests = append(h.requests, r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(h.status)
	_, _ = w.Write([]byte(h.body))
}

// lookupScript returns a lookup function answering each call from the next
// entry of results (the last entry repeats), or with err when err is set.
func lookupScript(err error, results ...[]authn.User) func(context.Context, authn.UserSearchQuery) ([]authn.User, error) {
	i := 0
	return func(_ context.Context, _ authn.UserSearchQuery) ([]authn.User, error) {
		if err != nil {
			return nil, err
		}
		if i >= len(results) {
			return nil, nil
		}
		got := results[i]
		i++
		return got, nil
	}
}

// noLookup answers the pre-registration question with "no such account",
// which is the truth for every case the tests below exercise through the
// register POST.
func noLookup() func(context.Context, authn.UserSearchQuery) ([]authn.User, error) {
	return lookupScript(nil, nil)
}

func mustNewSeeder(t *testing.T, handler http.Handler, lookup func(context.Context, authn.UserSearchQuery) ([]authn.User, error), domain string) *Seeder {
	t.Helper()
	seeder, err := NewSeeder(handler, lookup, domain)
	if err != nil {
		t.Fatalf("NewSeeder: %v", err)
	}
	return seeder
}

func TestNewSeeder_RefusesAnEmptyDemoDomain(t *testing.T) {
	for _, domain := range []string{"", "   ", "@"} {
		if _, err := NewSeeder(&stubHandler{}, noLookup(), domain); err == nil {
			t.Errorf("NewSeeder with demo domain %q: want an error, got nil", domain)
		}
	}
}

func TestNewSeeder_RefusesANilLookup(t *testing.T) {
	if _, err := NewSeeder(&stubHandler{}, nil, "example.com"); err == nil {
		t.Fatal("NewSeeder with a nil lookup: want an error, got nil")
	}
}

func TestNewSeeder_TrimsTheDeclaredDomain(t *testing.T) {
	for _, domain := range []string{"example.com", "@example.com", " @example.com "} {
		seeder := mustNewSeeder(t, &stubHandler{status: http.StatusCreated, body: `{"id":"user-1"}`}, noLookup(), domain)
		if seeder.domain != "example.com" {
			t.Errorf("NewSeeder(%q): stored domain = %q, want example.com", domain, seeder.domain)
		}
	}
}

func TestSeeder_Register_PostsThroughTheHandlerAndAnswersTheAssignedID(t *testing.T) {
	handler := &stubHandler{status: http.StatusCreated, body: `{"id":"user-1"}`}
	seeder := mustNewSeeder(t, handler, noLookup(), "example.com")

	userID, alreadyExists, err := seeder.Register(context.Background(), "demo-owner@example.com", "demo password")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if userID != "user-1" || alreadyExists {
		t.Errorf("Register = (%q, %t), want (user-1, false)", userID, alreadyExists)
	}

	if len(handler.requests) != 1 {
		t.Fatalf("the handler served %d requests, want 1", len(handler.requests))
	}
	req := handler.requests[0]
	if req.Method != http.MethodPost || req.URL.Path != registerPath {
		t.Errorf("the request is %s %s, want POST %s", req.Method, req.URL.Path, registerPath)
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Fatalf("decode the register body: %v", err)
	}
	if body["email"] != "demo-owner@example.com" || body["password"] != "demo password" {
		t.Errorf("register body = %v, want the account's email and password", body)
	}
}

// TestSeeder_Register_ExistingAccountIsAnsweredWithoutPosting pins the
// property the register budget rests on: an account a previous boot already
// registered is answered from the lookup alone, and the public register
// route is never touched.
func TestSeeder_Register_ExistingAccountIsAnsweredWithoutPosting(t *testing.T) {
	handler := &stubHandler{status: http.StatusCreated, body: `{"id":"unused"}`}
	lookup := lookupScript(nil, []authn.User{{ID: "user-9"}})
	seeder := mustNewSeeder(t, handler, lookup, "example.com")

	userID, alreadyExists, err := seeder.Register(context.Background(), "demo-owner@example.com", "demo password")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if userID != "user-9" || !alreadyExists {
		t.Errorf("Register = (%q, %t), want (user-9, true)", userID, alreadyExists)
	}
	if len(handler.requests) != 0 {
		t.Errorf("an already-registered account posted the register route %d times, want 0", len(handler.requests))
	}
}

// TestSeeder_Register_ConcurrentRegistrationConflictRecoversTheAssignedID
// pins the first-boot race branch: the lookup saw no account, the POST's
// concurrent twin won, and the answered conflict is recovered into the id
// authn assigned the winner.
func TestSeeder_Register_ConcurrentRegistrationConflictRecoversTheAssignedID(t *testing.T) {
	handler := &stubHandler{
		status: http.StatusConflict,
		body:   `{"code":"` + authn.ErrEmailAlreadyRegistered.Code + `"}`,
	}
	lookup := lookupScript(nil, nil, []authn.User{{ID: "user-9"}})
	seeder := mustNewSeeder(t, handler, lookup, "example.com")

	userID, alreadyExists, err := seeder.Register(context.Background(), "demo-owner@example.com", "demo password")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if userID != "user-9" || !alreadyExists {
		t.Errorf("Register = (%q, %t), want (user-9, true)", userID, alreadyExists)
	}
}

func TestSeeder_Register_RefusesAnEmailOutsideTheDeclaredDomain(t *testing.T) {
	handler := &stubHandler{status: http.StatusCreated, body: `{"id":"user-1"}`}
	lookups := 0
	seeder := mustNewSeeder(t, handler, func(context.Context, authn.UserSearchQuery) ([]authn.User, error) {
		lookups++
		return nil, nil
	}, "example.com")

	for _, email := range []string{"ops@acme.com", "no-at-sign", "trailing@", "ops@acme.example.com"} {
		_, _, err := seeder.Register(context.Background(), email, "demo password")
		if err == nil {
			t.Fatalf("Register(%q): want a refusal, got nil", email)
		}
		for _, want := range []string{email, "example.com", "demo accounts only"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Register(%q) error = %v, want it to name %q", email, err, want)
			}
		}
	}
	if len(handler.requests) != 0 || lookups != 0 {
		t.Errorf("a refused email reached the handler %d times and the lookup %d times, want 0 and 0",
			len(handler.requests), lookups)
	}
}

func TestSeeder_Register_MatchesTheDomainCaseInsensitively(t *testing.T) {
	handler := &stubHandler{status: http.StatusCreated, body: `{"id":"user-1"}`}
	seeder := mustNewSeeder(t, handler, noLookup(), "example.com")

	if _, _, err := seeder.Register(context.Background(), "Demo-Owner@EXAMPLE.com", "demo password"); err != nil {
		t.Fatalf("Register with a differently-cased domain: %v", err)
	}
}

func TestSeeder_Register_NamesTheRateLimitDistinctly(t *testing.T) {
	seeder := mustNewSeeder(t,
		&stubHandler{status: http.StatusTooManyRequests, body: `{"code":"` + authn.ErrRateLimited.Code + `"}`},
		noLookup(), "example.com")

	_, _, err := seeder.Register(context.Background(), "demo-owner@example.com", "demo password")
	if err == nil {
		t.Fatal("Register against an exhausted register budget: want an error, got nil")
	}
	for _, want := range []string{"public register rate limit", authn.ErrRateLimited.Code, "retry after the sliding window"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rate-limit error = %v, want it to name %q", err, want)
		}
	}
}

// TestSeeder_Register_OtherRefusalsKeepTheGenericClassification is the
// control for the rate-limit test above: an ordinary refusal (a password the
// policy rejects) must never read as the rate limit.
func TestSeeder_Register_OtherRefusalsKeepTheGenericClassification(t *testing.T) {
	seeder := mustNewSeeder(t,
		&stubHandler{status: http.StatusBadRequest, body: `{"code":"authn.password_too_short"}`},
		noLookup(), "example.com")

	_, _, err := seeder.Register(context.Background(), "demo-owner@example.com", "x")
	if err == nil {
		t.Fatal("Register with a policy-refused password: want an error, got nil")
	}
	if strings.Contains(err.Error(), "rate limit") {
		t.Errorf("policy-refusal error names the rate limit: %v", err)
	}
	for _, want := range []string{"HTTP 400", "authn.password_too_short"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("policy-refusal error = %v, want it to name %q", err, want)
		}
	}
}

func TestSeeder_Register_ToleranceOfAnUnparseableAnswerBody(t *testing.T) {
	seeder := mustNewSeeder(t,
		&stubHandler{status: http.StatusServiceUnavailable, body: "not json"},
		noLookup(), "example.com")

	_, _, err := seeder.Register(context.Background(), "demo-owner@example.com", "demo password")
	if err == nil {
		t.Fatal("Register against a non-JSON failure answer: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("error = %v, want it to name HTTP 503", err)
	}
}

func TestSeeder_Register_201WithoutAnIDIsAnError(t *testing.T) {
	seeder := mustNewSeeder(t,
		&stubHandler{status: http.StatusCreated, body: `{}`},
		noLookup(), "example.com")

	if _, _, err := seeder.Register(context.Background(), "demo-owner@example.com", "demo password"); err == nil {
		t.Fatal("Register against a 201 with no id: want an error, got nil")
	}
}

func TestSeeder_Register_ManyAccountsForOneEmailIsAnError(t *testing.T) {
	lookup := lookupScript(nil, []authn.User{{ID: "user-1"}, {ID: "user-2"}})
	handler := &stubHandler{status: http.StatusCreated, body: `{"id":"unused"}`}
	seeder := mustNewSeeder(t, handler, lookup, "example.com")

	_, _, err := seeder.Register(context.Background(), "demo-owner@example.com", "demo password")
	if err == nil {
		t.Fatal("Register when the lookup answers two accounts: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "want 0 or exactly 1") {
		t.Errorf("error = %v, want it to name the impossible lookup answer", err)
	}
	if len(handler.requests) != 0 {
		t.Errorf("an impossible lookup answer posted the register route %d times, want 0", len(handler.requests))
	}
}

func TestSeeder_Register_WrapsALookupFailure(t *testing.T) {
	lookupErr := errors.New("kv store unavailable")
	seeder := mustNewSeeder(t, &stubHandler{}, lookupScript(lookupErr), "example.com")

	_, _, err := seeder.Register(context.Background(), "demo-owner@example.com", "demo password")
	if err == nil {
		t.Fatal("Register when the lookup fails: want an error, got nil")
	}
	if !errors.Is(err, lookupErr) {
		t.Errorf("error = %v, want it to wrap %v", err, lookupErr)
	}
}

func TestSeeder_Register_RecoveryLookupFailureIsAnError(t *testing.T) {
	// The first lookup finds nothing, the POST answers the concurrent-boot
	// conflict, and the recovery lookup fails: the call must report that,
	// not a half-recovered account.
	conflict := &stubHandler{
		status: http.StatusConflict,
		body:   `{"code":"` + authn.ErrEmailAlreadyRegistered.Code + `"}`,
	}
	calls := 0
	lookup := func(context.Context, authn.UserSearchQuery) ([]authn.User, error) {
		calls++
		if calls == 1 {
			return nil, nil
		}
		return nil, errors.New("kv store unavailable")
	}
	seeder := mustNewSeeder(t, conflict, lookup, "example.com")

	if _, _, err := seeder.Register(context.Background(), "demo-owner@example.com", "demo password"); err == nil {
		t.Fatal("Register whose recovery lookup fails: want an error, got nil")
	}
}

// TestSeeder_Register_UsesTheRecordedPathOfTheRealRoute guards the restated
// registerPath constant against a silent drift from the route hosts mount:
// the request the handler sees must land on the string the constant declares,
// and the handler here is a real mux mounted the way a host mounts it.
func TestSeeder_Register_UsesTheRecordedPathOfTheRealRoute(t *testing.T) {
	mux := http.NewServeMux()
	var seen string
	mux.HandleFunc(http.MethodPost+" "+registerPath, func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"user-1"}`))
	})

	seeder := mustNewSeeder(t, mux, noLookup(), "example.com")
	if _, _, err := seeder.Register(context.Background(), "demo-owner@example.com", "demo password"); err != nil {
		t.Fatalf("Register through a mux mounted at registerPath: %v", err)
	}
	if seen != registerPath {
		t.Errorf("the handler saw path %q, want %q", seen, registerPath)
	}
}
