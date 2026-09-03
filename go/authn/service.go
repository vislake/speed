package authn

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	obs "github.com/vislake/speed/go/observability"
)

// MembershipReader answers the one question authn must ask about
// organizations without importing the module that owns them.
//
// The link between a user and a tenant is a membership, and memberships
// belong to org: they carry roles, an organization-tree node and an
// invitation lifecycle, none of which authn has any business knowing.
// Declaring the narrow interface here and letting the host inject an
// implementation is how the dependency stays pointed the right way -- org may
// depend on authn, never the reverse -- and it is the same shape a business
// module uses for every cross-module fact it needs but does not own.
//
// A nil MembershipReader is not a permissive default. Every path that needs
// one fails closed with ErrTenantMembershipUnavailable, because the question
// it answers is precisely "may this person act inside this tenant", and an
// unanswerable authorization question is a refusal.
type MembershipReader interface {
	// ActiveMembership reports whether userID is an active member of
	// tenantID.
	ActiveMembership(ctx context.Context, userID string, tenantID pkgcore.TenantID) (bool, error)

	// TenantsOf lists the tenants userID is an active member of, in the
	// order the caller should prefer them. Sign-in with no explicitly
	// requested tenant uses the first.
	TenantsOf(ctx context.Context, userID string) ([]pkgcore.TenantID, error)
}

// RegisterInput describes a new account. At least one of Email and Phone must
// be present -- an account with no identifier could never be signed in to.
type RegisterInput struct {
	// Email is the address to register, in any spelling; it is stored
	// encrypted and indexed under its canonical form.
	Email string
	// Phone is the number to register, in any formatting; it is stored
	// encrypted and indexed under its E.164 form.
	Phone string
	// Password is the plaintext password. It is validated against the
	// policy, hashed with argon2id, and never retained.
	Password string
	// DisplayName is the name shown in the product.
	DisplayName string
	// Locale is the language backend-generated content for this user is
	// rendered in.
	Locale string
}

// LoginInput describes a password sign-in attempt.
type LoginInput struct {
	// Identifier is the email address or phone number typed at the form.
	Identifier string
	// Password is the plaintext password.
	Password string
	// TenantID is the tenant to issue the first access token for. Empty
	// means "the first tenant this user is a member of". A value here is a
	// REQUEST, never a grant: membership is verified either way.
	TenantID pkgcore.TenantID
	// Device, UserAgent and IP describe the client, recorded on the
	// session and the login attempt.
	Device    string
	UserAgent string
	IP        string
}

// TokenPair is what a successful sign-in, refresh or tenant switch returns.
type TokenPair struct {
	// AccessToken is the short-lived signed credential for API calls.
	AccessToken string
	// AccessExpiresAt is when AccessToken stops verifying.
	AccessExpiresAt time.Time
	// RefreshToken is the opaque long-lived credential. A tenant switch
	// deliberately returns the SAME refresh token it was given: switching
	// tenants is not a new login and must not start a new token family.
	RefreshToken string
	// RefreshExpiresAt is when RefreshToken stops being accepted. It is
	// zero on a tenant switch, which issues no new refresh token.
	RefreshExpiresAt time.Time
	// Principal is the identity the access token asserts, with Email
	// filled in from the user record.
	Principal Principal
}

// Service is authn's business logic: registration, password sign-in, token
// refresh, sign-out and tenant switching.
type Service struct {
	users       *UserRepository
	sessions    *SessionManager
	sessionRepo *SessionRepository
	attempts    *LoginAttemptRepository
	signer      *Signer
	verifier    *Verifier
	bus         pkgcore.EventBus
	membership  MembershipReader
	now         func() time.Time
	params      PasswordParams
	policy      PasswordPolicy

	// Federation state: the social channels a deployment wired, the
	// single-use state store their callbacks are validated against, the
	// redirect URIs they may return to, and the providers whose
	// verified-email assertion is allowed to link an existing account.
	identities       *UserIdentityRepository
	providers        *ProviderRegistry
	states           *StateStore
	redirects        RedirectAllowlist
	trustedProviders []string
	sso              *SSOService
}

// NewService assembles a Service over db, using bus and kv -- the pkgcore
// seams -- for events and for the revocation list, so the same code runs
// under both deployment modes without ever asking which one it is in.
//
// The signing keys and the blind-index key are mandatory options: there is no
// safe default for either, and a generated-at-startup fallback would mean
// every restart invalidated every session and every stored index.
func NewService(db *gorm.DB, bus pkgcore.EventBus, kv pkgcore.KVStore, opts ...Option) (*Service, error) {
	if db == nil {
		return nil, errors.New("authn: NewService requires a database handle")
	}
	if bus == nil {
		return nil, errors.New("authn: NewService requires an event bus")
	}
	if kv == nil {
		return nil, errors.New("authn: NewService requires a key-value store")
	}

	cfg, err := newOptions(opts)
	if err != nil {
		return nil, err
	}

	emailIndexer, err := dbkit.NewBlindIndexer("email_index", cfg.blindIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		return nil, err
	}
	phoneIndexer, err := dbkit.NewBlindIndexer("phone_index", cfg.blindIndexKey, dbkit.NormalizePhoneE164)
	if err != nil {
		return nil, err
	}

	users, err := NewUserRepository(db, emailIndexer, phoneIndexer)
	if err != nil {
		return nil, err
	}
	sessionRepo, err := NewSessionRepository(db)
	if err != nil {
		return nil, err
	}
	tokenRepo, err := NewRefreshTokenRepository(db)
	if err != nil {
		return nil, err
	}
	attempts, err := NewLoginAttemptRepository(db)
	if err != nil {
		return nil, err
	}
	identities, err := NewUserIdentityRepository(db)
	if err != nil {
		return nil, err
	}
	states, err := NewStateStore(kv, cfg.oauthStateTTL)
	if err != nil {
		return nil, err
	}
	providers, err := NewProviderRegistry(cfg.providers...)
	if err != nil {
		return nil, err
	}

	tokenOpts := []TokenOption{
		WithTokenTTL(cfg.accessTTL),
		WithTokenIssuer(cfg.issuer),
		WithTokenClock(cfg.now),
	}
	signer, err := NewSigner(cfg.keys, tokenOpts...)
	if err != nil {
		return nil, err
	}
	verifier, err := NewVerifier(cfg.keys, tokenOpts...)
	if err != nil {
		return nil, err
	}

	manager, err := NewSessionManager(sessionRepo, tokenRepo, kv, bus, cfg.revocationMode,
		cfg.now, cfg.refreshTTL, cfg.sessionTTL, cfg.accessTTL)
	if err != nil {
		return nil, err
	}

	svc := &Service{
		users:            users,
		sessions:         manager,
		sessionRepo:      sessionRepo,
		attempts:         attempts,
		signer:           signer,
		verifier:         verifier,
		bus:              bus,
		membership:       cfg.membership,
		now:              cfg.now,
		params:           cfg.passwordParams,
		policy:           cfg.passwordPolicy,
		identities:       identities,
		providers:        providers,
		states:           states,
		redirects:        cfg.redirects,
		trustedProviders: slices.Clone(cfg.trustedProviders),
	}

	sso, err := newSSOService(svc, db, cfg)
	if err != nil {
		return nil, err
	}
	svc.sso = sso
	return svc, nil
}

// Identities returns the external-identity repository, for callers that need
// to read a binding directly.
func (s *Service) Identities() *UserIdentityRepository { return s.identities }

// Providers returns the registry of wired social channels, whose Names() is
// what the login page's enabled-channel list is built from.
func (s *Service) Providers() *ProviderRegistry { return s.providers }

// SSO returns the enterprise single sign-on relying party.
func (s *Service) SSO() *SSOService { return s.sso }

// Users returns the user repository, for callers that need to read a user
// record directly -- the HTTP layer's /me handler, for instance.
func (s *Service) Users() *UserRepository { return s.users }

// Sessions returns the session manager, which owns revocation and is what
// Middleware's WithRevocationChecker option is given.
func (s *Service) Sessions() *SessionManager { return s.sessions }

// LoginHistory returns the login-attempt repository.
func (s *Service) LoginHistory() *LoginAttemptRepository { return s.attempts }

// Verifier returns the access-token verifier, for wiring Middleware.
func (s *Service) Verifier() *Verifier { return s.verifier }

// Register creates an account and publishes EventUserCreated.
//
// Known limitation, stated rather than hidden: a duplicate identifier is
// reported as a conflict, which makes registration an account-enumeration
// oracle in a way sign-in deliberately is not. Closing it means answering
// every registration with "check your inbox" and moving the conflict into an
// email, which needs the delivery and verification flows; until those land,
// the honest position is that the conflict is visible here.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*User, error) {
	email := strings.TrimSpace(in.Email)
	phone := strings.TrimSpace(in.Phone)
	if email == "" && phone == "" {
		return nil, ErrIdentifierRequired
	}
	if err := s.policy.Validate(in.Password); err != nil {
		return nil, err
	}

	user := &User{
		DisplayName:   in.DisplayName,
		Locale:        in.Locale,
		Status:        UserStatusActive,
		EmailVerified: false,
		PhoneVerified: false,
	}
	if email != "" {
		if _, err := s.users.EmailIndexOf(email); err != nil {
			return nil, err
		}
		if _, err := s.users.FindByEmail(ctx, email); err == nil {
			return nil, ErrEmailAlreadyRegistered
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		user.Email = email
	}
	if phone != "" {
		if _, err := s.users.PhoneIndexOf(phone); err != nil {
			return nil, err
		}
		if _, err := s.users.FindByPhone(ctx, phone); err == nil {
			return nil, ErrPhoneAlreadyRegistered
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		user.Phone = phone
	}

	hash, err := HashPassword(in.Password, s.params)
	if err != nil {
		return nil, err
	}
	user.PasswordHash = hash

	if err := s.users.Create(ctx, user); err != nil {
		return nil, err
	}

	s.publish(ctx, pkgcore.Event{
		Type: EventUserCreated,
		Payload: UserCreatedPayload{
			UserID:   user.ID,
			HasEmail: user.Email != "",
			HasPhone: user.Phone != "",
		},
	})
	return user, nil
}

// Login verifies a password and, on success, starts a session and issues the
// first token pair.
//
// Every failure returns ErrInvalidCredentials, with no parameter and no
// timing shortcut: an unknown identifier still costs one argon2id derivation,
// so the response time does not answer the question the error refuses to. The
// specific reason is written to the login history for the operator and for
// the account owner's own security page.
func (s *Service) Login(ctx context.Context, in LoginInput) (*TokenPair, error) {
	identifier := strings.TrimSpace(in.Identifier)
	if identifier == "" {
		// Recorded and announced like any other failure rather than
		// returned early. An empty identifier is still an attempt from
		// an IP address, and the per-IP dimension of the lockout logic
		// has to be able to count it; skipping it here would leave a
		// free, uncounted probe.
		s.recordFailure(ctx, in, "", "", FailureReasonUnknownUser)
		return nil, ErrInvalidCredentials
	}

	user, err := s.findByIdentifier(ctx, identifier)
	if err != nil {
		if hasCode(err, ErrInvalidEmail.Code, ErrInvalidPhone.Code) {
			// An identifier with no canonical form cannot belong to
			// any account, and saying so would answer the same
			// question the generic error refuses to answer. It is
			// reported exactly like an unregistered address.
			s.burnPasswordWork(in.Password)
			s.recordFailure(ctx, in, "", "", FailureReasonUnknownUser)
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	if user == nil {
		// Burn the same work a real verification would, so "no such
		// account" and "wrong password" are not distinguishable by a
		// stopwatch. Without this the enumeration oracle the error
		// message closes is simply reopened as a timing side channel.
		s.burnPasswordWork(in.Password)
		s.recordFailure(ctx, in, "", s.identifierIndex(identifier), FailureReasonUnknownUser)
		return nil, ErrInvalidCredentials
	}

	index := s.identifierIndex(identifier)
	if user.PasswordHash == "" {
		s.burnPasswordWork(in.Password)
		s.recordFailure(ctx, in, user.ID, index, FailureReasonNoPassword)
		return nil, ErrInvalidCredentials
	}

	ok, err := VerifyPassword(user.PasswordHash, in.Password)
	if err != nil {
		obs.FromContext(ctx).Error("stored password hash is unreadable", "user_id", user.ID, "error", err)
		s.recordFailure(ctx, in, user.ID, index, FailureReasonBadPassword)
		return nil, ErrInvalidCredentials
	}
	if !ok {
		s.recordFailure(ctx, in, user.ID, index, FailureReasonBadPassword)
		return nil, ErrInvalidCredentials
	}
	if user.Status != UserStatusActive {
		s.recordFailure(ctx, in, user.ID, index, FailureReasonSuspended)
		return nil, ErrInvalidCredentials
	}

	tenantID, err := s.resolveTenant(ctx, user.ID, in.TenantID)
	if err != nil {
		s.recordFailure(ctx, in, user.ID, index, FailureReasonNoMembership)
		return nil, err
	}

	s.upgradePasswordHash(ctx, user, in.Password)

	amr := []string{MethodPassword}
	session, issued, err := s.sessions.Start(ctx, StartSessionInput{
		UserID:    user.ID,
		TenantID:  tenantID,
		AMR:       amr,
		Device:    in.Device,
		UserAgent: in.UserAgent,
		IP:        in.IP,
	})
	if err != nil {
		return nil, err
	}

	pair, err := s.mintPair(user, session, tenantID, issued)
	if err != nil {
		return nil, err
	}

	s.record(ctx, &LoginAttempt{
		UserID:          user.ID,
		IdentifierIndex: index,
		Method:          MethodPassword,
		Result:          LoginResultSuccess,
		SessionID:       session.ID,
		IP:              in.IP,
		UserAgent:       in.UserAgent,
		CreatedAt:       s.now(),
	})
	s.publish(ctx, pkgcore.Event{
		Type:     EventUserLoggedIn,
		TenantID: tenantID,
		Payload: UserLoggedInPayload{
			UserID:    user.ID,
			SessionID: session.ID,
			TenantID:  string(tenantID),
			Method:    MethodPassword,
			AMR:       amr,
			IP:        in.IP,
		},
	})
	return pair, nil
}

// Refresh rotates a refresh token and issues a new access token for the
// session's current tenant.
//
// Membership is re-verified here rather than trusted from the session row.
// That is what makes removing someone from a tenant actually end their access
// to it: without the re-check they would keep refreshing into a tenant they
// no longer belong to until the session itself expired, which is weeks.
func (s *Service) Refresh(ctx context.Context, presented string) (*TokenPair, error) {
	session, issued, err := s.sessions.Rotate(ctx, presented)
	if err != nil {
		return nil, err
	}

	tenantID, err := s.resolveTenant(ctx, session.UserID, pkgcore.TenantID(session.CurrentTenantID))
	if err != nil {
		return nil, err
	}

	user, err := s.users.FindByID(ctx, session.UserID)
	if err != nil {
		return nil, err
	}
	if user.Status != UserStatusActive {
		return nil, ErrInvalidCredentials
	}

	return s.mintPair(user, session, tenantID, issued)
}

// SwitchTenant issues a new access token for a different tenant, REUSING the
// session and its refresh token: switching tenants is not a new sign-in.
//
// The membership check here is the single most important line of the method.
// The target tenant arrives from the client, and trusting it is the textbook
// horizontal-privilege-escalation entry point in a multi-tenant product --
// which is why a missing MembershipReader refuses rather than defaults.
func (s *Service) SwitchTenant(ctx context.Context, principal Principal, target pkgcore.TenantID) (*TokenPair, error) {
	if principal.UserID == "" || principal.SessionID == "" {
		return nil, ErrAuthenticationRequired
	}
	if target == "" {
		return nil, ErrTenantMembershipRequired
	}

	session, err := s.sessionRepo.FindByID(ctx, principal.SessionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrSessionRevoked
		}
		return nil, err
	}
	if session.UserID != principal.UserID {
		// The token said one user and the session belongs to another.
		// Nothing legitimate produces this.
		return nil, ErrTokenInvalid
	}
	if session.Status != SessionStatusActive {
		return nil, ErrSessionRevoked
	}

	if _, tenantErr := s.resolveTenant(ctx, principal.UserID, target); tenantErr != nil {
		return nil, tenantErr
	}

	user, err := s.users.FindByID(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	if user.Status != UserStatusActive {
		return nil, ErrInvalidCredentials
	}

	previous := session.CurrentTenantID
	if setErr := s.sessionRepo.SetCurrentTenant(ctx, session.ID, target); setErr != nil {
		return nil, setErr
	}
	session.CurrentTenantID = string(target)

	pair, err := s.mintPair(user, session, target, IssuedRefreshToken{})
	if err != nil {
		return nil, err
	}

	s.publish(ctx, pkgcore.Event{
		Type:     EventTenantSwitched,
		TenantID: target,
		Payload: TenantSwitchedPayload{
			UserID:       user.ID,
			SessionID:    session.ID,
			FromTenantID: previous,
			ToTenantID:   string(target),
		},
	})
	return pair, nil
}

// Logout revokes a session, which invalidates its refresh tokens at once and
// -- in immediate revocation mode -- its outstanding access tokens too.
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return ErrAuthenticationRequired
	}
	return s.sessions.Revoke(ctx, sessionID, RevokeReasonLogout)
}

// mintPair signs an access token for the session and assembles the response.
// issued.Secret is empty for a tenant switch, which reuses the caller's
// existing refresh token rather than minting one.
func (s *Service) mintPair(user *User, session *Session, tenantID pkgcore.TenantID, issued IssuedRefreshToken) (*TokenPair, error) {
	principal := Principal{
		UserID:    user.ID,
		TenantID:  tenantID,
		SessionID: session.ID,
		AMR:       session.AMRList(),
	}
	principal.Email = user.Email

	access, expiresAt, err := s.signer.Issue(principal)
	if err != nil {
		return nil, err
	}

	pair := &TokenPair{
		AccessToken:     access,
		AccessExpiresAt: expiresAt,
		Principal:       principal,
	}
	if issued.Record != nil {
		pair.RefreshToken = issued.Secret
		pair.RefreshExpiresAt = issued.Record.ExpiresAt
	}
	return pair, nil
}

// resolveTenant answers "may this user act inside this tenant", picking the
// user's first tenant when none was requested. It never falls back to a
// permissive answer.
func (s *Service) resolveTenant(ctx context.Context, userID string, requested pkgcore.TenantID) (pkgcore.TenantID, error) {
	if s.membership == nil {
		return "", ErrTenantMembershipUnavailable
	}

	if requested != "" {
		member, err := s.membership.ActiveMembership(ctx, userID, requested)
		if err != nil {
			obs.FromContext(ctx).Error("membership lookup failed", "user_id", userID, "error", err)
			return "", ErrTenantMembershipUnavailable.WithCause(err)
		}
		if !member {
			return "", ErrTenantMembershipRequired
		}
		return requested, nil
	}

	tenants, err := s.membership.TenantsOf(ctx, userID)
	if err != nil {
		obs.FromContext(ctx).Error("membership lookup failed", "user_id", userID, "error", err)
		return "", ErrTenantMembershipUnavailable.WithCause(err)
	}
	if len(tenants) == 0 {
		return "", ErrTenantMembershipRequired
	}
	return tenants[0], nil
}

// findByIdentifier looks the account up by email or by phone.
//
// The two are told apart by the presence of an "@", which is the one
// character an email address always has and an E.164 phone number never does.
// A miss returns (nil, nil) rather than an error, because "no such account"
// is an expected sign-in outcome rather than a failure.
func (s *Service) findByIdentifier(ctx context.Context, identifier string) (*User, error) {
	find := s.users.FindByPhone
	if strings.Contains(identifier, "@") {
		find = s.users.FindByEmail
	}

	user, err := find(ctx, identifier)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return user, nil
}

// identifierIndex returns the blind index to record on a login attempt, or
// the empty string when the identifier has no canonical form. It never
// returns an error: failing to write a history row must not fail a sign-in.
func (s *Service) identifierIndex(identifier string) string {
	indexOf := s.users.PhoneIndexOf
	if strings.Contains(identifier, "@") {
		indexOf = s.users.EmailIndexOf
	}
	index, err := indexOf(identifier)
	if err != nil {
		return ""
	}
	return index
}

// burnPasswordWork performs one argon2id derivation and discards it, so that
// a sign-in against an unknown account costs the same as one against a known
// account with the wrong password.
func (s *Service) burnPasswordWork(password string) {
	_, _ = HashPassword(password, s.params)
}

// upgradePasswordHash re-hashes a verified password under the current
// parameters when the stored hash was produced under weaker ones. It is the
// one moment the plaintext is available and the hash is known to be correct,
// so it is the only place the corpus can migrate without asking users to
// change anything.
//
// A failure here is logged and swallowed: the sign-in already succeeded, and
// refusing it because an optimisation did not apply would be a worse outcome
// than an un-upgraded hash.
func (s *Service) upgradePasswordHash(ctx context.Context, user *User, password string) {
	stale, err := NeedsRehash(user.PasswordHash, s.params)
	if err != nil || !stale {
		return
	}
	hash, err := HashPassword(password, s.params)
	if err != nil {
		obs.FromContext(ctx).Warn("password rehash failed", "user_id", user.ID, "error", err)
		return
	}
	user.PasswordHash = hash
	if err := s.users.Save(ctx, user); err != nil {
		obs.FromContext(ctx).Warn("password rehash could not be stored", "user_id", user.ID, "error", err)
	}
}

// recordFailure writes a failed attempt to the login history and publishes
// EventLoginFailed.
func (s *Service) recordFailure(ctx context.Context, in LoginInput, userID, index, reason string) {
	s.record(ctx, &LoginAttempt{
		UserID:          userID,
		IdentifierIndex: index,
		Method:          MethodPassword,
		Result:          LoginResultFailure,
		FailureReason:   reason,
		IP:              in.IP,
		UserAgent:       in.UserAgent,
		CreatedAt:       s.now(),
	})
	s.publish(ctx, pkgcore.Event{
		Type: EventLoginFailed,
		Payload: LoginFailedPayload{
			UserID: userID,
			Method: MethodPassword,
			Reason: reason,
			IP:     in.IP,
		},
	})
}

// record writes one login-history row, logging rather than returning a
// failure: the sign-in outcome must not depend on whether its audit row
// persisted, and the caller has already decided that outcome.
func (s *Service) record(ctx context.Context, attempt *LoginAttempt) {
	if err := s.attempts.Create(ctx, attempt); err != nil {
		obs.FromContext(ctx).Error("login attempt could not be recorded", "result", attempt.Result, "error", err)
	}
}

// publish emits evt, logging a delivery failure instead of returning it. The
// fact the event describes has already been committed.
func (s *Service) publish(ctx context.Context, evt pkgcore.Event) {
	if err := s.bus.Publish(ctx, evt); err != nil {
		obs.FromContext(ctx).Warn("domain event publish failed", "event_type", evt.Type, "error", err)
	}
}

// hasCode reports whether err is, or wraps, an *apperr.Error whose Code is
// one of codes.
//
// Matching on the code rather than with errors.Is against a sentinel is
// required, not stylistic: apperr's builders (WithParam, WithCause) DERIVE a
// new value instead of mutating the receiver, precisely so a shared sentinel
// is safe to decorate per request -- which means a decorated error is never
// identical to the sentinel it came from, and errors.Is against that sentinel
// is always false.
func hasCode(err error, codes ...string) bool {
	appErr, ok := apperr.As(err)
	if !ok {
		return false
	}
	return slices.Contains(codes, appErr.Code)
}
