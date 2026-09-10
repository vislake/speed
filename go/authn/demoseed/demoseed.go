// Package demoseed seeds demo accounts through authn's real register route.
//
// A demo or development deployment wants its demo accounts to be real
// accounts: rows in authn's users table, created through the same register
// endpoint a browser posts to, so the password hashing, the password policy
// and the per-IP register budget a demo exercises are the ones a real
// deployment would enforce. This package is that mechanism -- the register
// call, the answer classification and the lookup-first shape a boot-time
// seed needs -- extracted so a host does not have to re-derive it.
//
// The lookup half is authn's platform-operator search
// (Service.SearchUsers): a boot-time seed is the operator's own
// configuration -- the same trust level as the registration it performs --
// which is why the seed may ask that search directly, while an HTTP caller
// of the same search goes through its admin:search_users gate.
//
// # Demo-only, and made hard to reach by accident
//
// Registering accounts at boot from an operator-set password is a
// demonstration device, never a provisioning path: it creates real accounts
// on a credential every reader of the deployment's demo instructions
// knows, and the lookup that recovers an already-registered account's user
// id is the platform-operator search. Production code must not import this
// package; accounts for real people come from the register surface itself
// or from whatever identity flow the deployment actually runs. The stance
// is carried by the shape rather than only by this comment:
//
//   - The package name and every symbol on it say "demo", so an accidental
//     use reads as what it is at the import site and at the call site.
//   - NewSeeder refuses to construct without a caller-declared demo email
//     domain, and Register refuses -- before it looks anything up and before
//     it sends anything -- an email outside that domain. A production call
//     site therefore has to write the contradiction down: either it declares
//     a real domain as its demo domain, or its first call fails loudly
//     naming the mismatch.
package demoseed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/vislake/speed/go/authn"
)

// registerPath is the register endpoint this package posts to: authn's
// platform mount point (/api/v1/authn, module.go's apiPath) plus the
// fragment's own /register operation. It is restated here rather than
// imported because apiPath is unexported; a rename there stops every host's
// route mounting from matching, which this package's own test pins by
// driving a handler mounted at the same path.
const registerPath = "/api/v1/authn/register"

// Seeder registers demo accounts through a host's composed handler. Build it
// once with NewSeeder, at the wiring site that also declares the demo
// domain, and keep it for the life of the process.
type Seeder struct {
	// handler is the host's composed HTTP handler: the register request is
	// served through it in-process, so it traverses the same middleware,
	// allowlist, mux and handler stack a browser request does.
	handler http.Handler

	// lookup answers the exact-email existence question -- authn.Service's
	// SearchUsers, passed as a method value.
	lookup func(context.Context, authn.UserSearchQuery) ([]authn.User, error)

	// domain is the declared demo domain. Register refuses any email whose
	// domain part does not match it, case-insensitively.
	domain string
}

// NewSeeder returns a Seeder that registers accounts through handler and
// answers the pre-registration existence question through lookup
// (authn.Service's SearchUsers method value: authnService.SearchUsers).
//
// demoDomain is the caller's explicit declaration of where its demo accounts
// live -- "example.com", or "@example.com" (a leading "@" is accepted and
// trimmed). It must not be empty, and it is what makes the demo-only stance
// structural: every account the returned Seeder registers must be on that
// domain, so a production use cannot pass without either declaring a real
// domain as its demo domain or tripping the refusal on its first call.
func NewSeeder(handler http.Handler, lookup func(context.Context, authn.UserSearchQuery) ([]authn.User, error), demoDomain string) (*Seeder, error) {
	domain := strings.TrimPrefix(strings.TrimSpace(demoDomain), "@")
	if domain == "" {
		return nil, errors.New("demoseed: the demo domain must not be empty; declare the email domain the demo accounts live on (for example \"example.com\")")
	}
	if lookup == nil {
		return nil, errors.New("demoseed: the account lookup must not be nil; pass authn.Service's SearchUsers method value")
	}
	return &Seeder{handler: handler, lookup: lookup, domain: domain}, nil
}

// Register ensures email is registered as a real authn account and answers
// the user id authn assigned, plus whether the account was already there.
//
// The existence question is asked of the lookup FIRST, and only a genuinely
// absent account is registered by POSTing the register payload to the
// host's composed handler. That ordering is why a restart against a database
// the account already lives in never touches the public register route, and
// therefore never debits its per-IP budget (limitRegisterByIP,
// go/authn/ratelimit.go): under a distributed deployment the budget lives
// in a shared store and accumulates across restarts, so a seed that posted
// on every boot could exhaust it within the quota window and fail the boot.
//
// Answers are classified by code, exactly as the register API reports them.
// A 201 yields the new user id. The already-registered conflict yields
// alreadyExists=true and the id recovered from the lookup -- a concurrent
// first boot that registered the account between this call's lookup and its
// POST is the race that answer exists for, and it must not strand the
// account. The rate-limit refusal is reported with the limit and the remedy
// rather than as a generic failure, because with this call posting only
// absent accounts a 429 means the public budget is genuinely exhausted --
// by other register traffic, or by several first-boots of fresh databases
// within the hour -- a transient, operator-actionable state. Every other
// non-201 answer is an error naming the status and the answer code, so a
// policy refusal or anything else fails the call rather than silently
// producing a half-seeded demo.
func (s *Seeder) Register(ctx context.Context, email, password string) (userID string, alreadyExists bool, err error) {
	if domain, ok := emailDomain(email); !ok || !strings.EqualFold(domain, s.domain) {
		return "", false, fmt.Errorf(
			"demoseed: %q is outside the declared demo domain %q; this package seeds demo accounts only",
			email, s.domain)
	}

	existingID, exists, err := s.lookupRegisteredUserID(ctx, email)
	if err != nil {
		return "", false, fmt.Errorf("demoseed: check whether account %q is already registered: %w", email, err)
	}
	if exists {
		return existingID, true, nil
	}

	userID, alreadyExists, err = s.postRegister(ctx, email, password)
	if err != nil {
		return "", false, err
	}
	if alreadyExists {
		// A concurrent first boot registered the account between the lookup
		// above and this POST; recover its id.
		userID, err = s.registeredUserID(ctx, email)
		if err != nil {
			return "", false, err
		}
	}
	return userID, alreadyExists, nil
}

// emailDomain returns the domain part of email -- everything after its last
// "@" -- reporting false for a string with no "@" or with nothing after it,
// which no declared domain can then match.
func emailDomain(email string) (string, bool) {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return "", false
	}
	return email[at+1:], true
}

// lookupRegisteredUserID asks the lookup whether the exact-email account
// exists, answering (id, true) for the exactly-one-account case, (_, false)
// for none, and an error for a lookup failure or an impossible many-account
// collision.
func (s *Seeder) lookupRegisteredUserID(ctx context.Context, email string) (string, bool, error) {
	users, err := s.lookup(ctx, authn.UserSearchQuery{Email: email})
	if err != nil {
		return "", false, err
	}
	switch len(users) {
	case 0:
		return "", false, nil
	case 1:
		return users[0].ID, true, nil
	default:
		return "", false, fmt.Errorf("SearchUsers answered %d accounts for %q, want 0 or exactly 1", len(users), email)
	}
}

// registeredUserID resolves the user id authn assigned to an email that is
// already registered -- the one thing the register conflict answer never
// discloses.
func (s *Seeder) registeredUserID(ctx context.Context, email string) (string, error) {
	users, err := s.lookup(ctx, authn.UserSearchQuery{Email: email})
	if err != nil {
		return "", fmt.Errorf("look up pre-existing registration for %q: %w", email, err)
	}
	if len(users) != 1 {
		return "", fmt.Errorf("look up pre-existing registration for %q: SearchUsers answered %d accounts, want exactly 1",
			email, len(users))
	}
	return users[0].ID, nil
}

// postRegister POSTs one register payload to the composed handler --
// in-process, through the same middleware, allowlist, mux and handler stack
// a browser request traverses -- and returns the user id authn assigned, or
// alreadyExists=true when the account is already in the users table.
//
// The callers (Register and the recovery above) already asked the lookup
// whether the account exists, so reaching this POST at all means the account
// was genuinely absent a moment ago; the already-registered answer here is
// the concurrent-first-boot race.
func (s *Seeder) postRegister(ctx context.Context, email, password string) (userID string, alreadyExists bool, err error) {
	payload, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return "", false, fmt.Errorf("demoseed: marshal register body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registerPath, bytes.NewReader(payload))
	if err != nil {
		return "", false, fmt.Errorf("demoseed: build register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		var envelope struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&envelope)
		if resp.StatusCode == http.StatusConflict && envelope.Code == authn.ErrEmailAlreadyRegistered.Code {
			return "", true, nil
		}
		if resp.StatusCode == http.StatusTooManyRequests && envelope.Code == authn.ErrRateLimited.Code {
			return "", false, fmt.Errorf(
				"demoseed: registering %q was refused by authn's public register rate limit (HTTP 429 %s, 10 registrations per hour per client IP): this call only registers genuinely new accounts, so the limit is exhausted by other register traffic -- retry after the sliding window closes, or investigate the register traffic",
				email, authn.ErrRateLimited.Code)
		}
		return "", false, fmt.Errorf(
			"demoseed: registering %q answered HTTP %d with code %q, want 201 or the already-registered conflict",
			email, resp.StatusCode, envelope.Code)
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", false, fmt.Errorf("demoseed: decode register response: %w", err)
	}
	if created.ID == "" {
		return "", false, fmt.Errorf("demoseed: register response carried no id")
	}
	return created.ID, false, nil
}
