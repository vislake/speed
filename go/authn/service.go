package authn

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy"

	obs "github.com/vislake/speed/go/observability"
)

// InstrumentationName identifies this package's own tracer/meter, mirroring
// go/jobs/queue_standalone.go's and go/notification/delivery.go's identical
// use of their own package path for the same purpose.
const InstrumentationName = "github.com/vislake/speed/go/authn"

// Metric instrument names registerAuthMetrics wires under
// InstrumentationName -- the sign-in failure rate, MFA challenge volume,
// and token refresh failure rate the authentication domain must report.
// One counter and one histogram cover all three: each is sliced by its
// "operation" attribute (authOpLogin/authOpSMSCodeLogin/authOpRefresh/
// authOpMFAChallenge) and "outcome" attribute (authOutcomeSucceeded/
// authOutcomeFailed), so "password sign-in failure rate" is
// authCountMetricName{operation=login,outcome=failed} over the same
// operation's succeeded count, the SMS-code channel's the same expression
// under operation=login_sms, and "MFA challenge volume" is the sum of
// authCountMetricName{operation=mfa_challenge,*} -- the identical
// slice-by-status-attribute shape go/jobs' jobAttemptsMetricName and
// go/notification's deliveryCountMetricName both use.
//
// The operation values are per channel and deliberately do not cover every
// sign-in channel: the social and enterprise-SSO channels carry no counts
// at all, because their failure rates mostly track the condition of a
// third-party provider or identity provider rather than an authn-side
// attack. The two channels a caller can brute-force -- password and
// SMS-code -- each have their own operation value, so no label ever
// presents a wider coverage than the one feeding it; an operator who wants
// every brute-forceable channel in one alert unions operation=login and
// operation=login_sms.
const (
	authCountMetricName    = "authn.auth.count"
	authDurationMetricName = "authn.auth.duration"
)

// The "operation" attribute values authCountMetricName/authDurationMetricName
// carry -- a bounded, declared vocabulary (never tenant_id or user_id),
// exactly one per instrumented Service method below.
const (
	// authOpLogin is Service.Login, the email-or-phone plus password
	// channel -- the channel credential stuffing targets.
	authOpLogin = "login"
	// authOpSMSCodeLogin is Service.LoginWithSMSCode, the phone-plus-code
	// channel: a six-digit code behind a per-target wrong-guess budget is
	// brute-forceable, so its sign-in attempts belong on the same
	// failure-rate watch as the password channel's, under their own
	// channel-naming operation value.
	authOpSMSCodeLogin = "login_sms"
	// authOpRefresh is Service.Refresh, a refresh-token rotation.
	authOpRefresh = "refresh"
	// authOpMFAChallenge is Service.VerifyStepUp, a second-factor
	// challenge.
	authOpMFAChallenge = "mfa_challenge"
)

// The "outcome" attribute values authCountMetricName/authDurationMetricName
// carry.
const (
	authOutcomeSucceeded = "succeeded"
	authOutcomeFailed    = "failed"
)

// registerAuthMetrics wires the "authn.auth.count" Counter and
// "authn.auth.duration" Histogram this file's own
// authCountMetricName/authDurationMetricName doc comment names, mirroring
// go/notification/delivery.go's registerDeliveryMetrics: registered once at
// construction (NewService), with the registration error ignored the same
// way go/observability/middleware.go's Middleware ignores it -- the global
// otel API returns a working no-op instrument alongside any error, and
// NewService has no request-scoped ctx/logger available yet to warn with
// the way go/jobs' registerJobMetrics does from its own call site (Start).
func registerAuthMetrics() (metric.Int64Counter, metric.Float64Histogram) {
	meter := otel.Meter(InstrumentationName)
	count, _ := meter.Int64Counter(
		authCountMetricName,
		metric.WithDescription("Number of authentication operations completed, by operation (login, login_sms, refresh, mfa_challenge) and resulting outcome (succeeded or failed). Failure rate for any operation is derivable from this by outcome."),
		metric.WithUnit("{operation}"),
	)
	duration, _ := meter.Float64Histogram(
		authDurationMetricName,
		metric.WithDescription("Duration of one authentication operation, in seconds, by operation and resulting outcome."),
		metric.WithUnit("s"),
	)
	return count, duration
}

// recordAuthMetric records one completed authentication operation onto
// authCountMetricName/authDurationMetricName, labeled by op and outcome
// only -- deliberately never tenant_id or user_id, for the identical
// cardinality reason go/jobs/queue_standalone.go's registerJobMetrics doc
// comment gives. err's nilness alone decides the outcome: every one of
// Login/LoginWithSMSCode/Refresh/VerifyStepUp's early returns already
// report a non-nil error on any refusal (bad credentials, wrong SMS code,
// rate-limited, revoked session, wrong MFA code), so a plain nil check at
// the record site is both sufficient and immune to a future added return
// path being forgotten -- unlike an explicit outcome flag threaded through
// every branch.
func (s *Service) recordAuthMetric(ctx context.Context, op string, start time.Time, err error) {
	outcome := authOutcomeSucceeded
	if err != nil {
		outcome = authOutcomeFailed
	}
	attrs := metric.WithAttributes(
		attribute.String("operation", op),
		attribute.String("outcome", outcome),
	)
	if s.authCount != nil {
		s.authCount.Add(ctx, 1, attrs)
	}
	if s.authDuration != nil {
		s.authDuration.Record(ctx, time.Since(start).Seconds(), attrs)
	}
}

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
// one refuses rather than defaulting; whether the refusal reaches the
// caller as-is or folds into a uniform failure is the resolveTenant call
// site's decision, taken on the axis of whether a distinguishable answer
// would certify something an attacker could only obtain by guessing --
// resolveTenant's doc comment classifies all four call sites under it.
// The paths whose answer certifies nothing guessable -- authenticated
// callers (tenant switching, refresh) and the social/SSO/SMS-code session
// start, whose certified secret is a one-time code already spent by the
// attempt that reaches this call -- fail closed with
// ErrTenantMembershipUnavailable, because the question it answers is
// precisely "may this person act inside this tenant", and an unanswerable
// authorization question is a refusal. Password sign-in answers an
// anonymous caller who just verified a guessable, reusable credential and
// folds the same refusal into its uniform ErrInvalidCredentials error
// instead: a distinguishable membership error there would certify the
// password (see Login's doc comment).
type MembershipReader interface {
	// ActiveMembership reports whether userID is an active member of
	// tenantID.
	ActiveMembership(ctx context.Context, userID string, tenantID pkgcore.TenantID) (bool, error)

	// TenantsOf lists the tenants userID is an active member of, in the
	// order the caller should prefer them. Sign-in with no explicitly
	// requested tenant uses the first.
	//
	// TenantsOf is called -- and only ever called -- under an elevated
	// context: resolveTenant takes the audited system-context grant
	// (tenancy.WithSystemContext, actor userID, purpose
	// SystemPurposeSignInTenantEnumeration) before this call and passes
	// that context here, because the question spans tenants by definition
	// and a reader backed by real membership rows gates the cross-tenant
	// read on exactly that grant. An implementation therefore does not
	// invent an elevation of its own; it may rely on the passed context
	// already carrying the system reason, and must still fail closed if it
	// does not.
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
	// Locale is the caller-declared language for backend-generated content
	// addressed to this account. It is one TIER of the registration locale
	// chain, not the stored value: an unusable value is skipped (the
	// registration is lenient) and AcceptLanguage stands next in the chain
	// -- see registrationLocale.
	Locale string
	// AcceptLanguage is the request's Accept-Language header, the transport
	// channel the frontend's own language chain sends its resolved
	// language through. It is the registration locale chain's second tier.
	AcceptLanguage string
	// Timezone is the caller-declared IANA timezone name -- the
	// registration form's browser report -- and the registration timezone
	// chain's first tier, lenient like Locale: an unusable name is skipped
	// rather than refused, with the TimeZoneResolver and then "not chosen
	// yet" behind it (see registrationTimeZone).
	Timezone string
	// IP is the requesting client's address, for the registration rate
	// limit and for the timezone chain's IP-resolution tier.
	IP string
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
	// RefreshToken is the opaque long-lived credential a response that
	// mints a NEW token family carries: a sign-in, a refresh, a social or
	// SSO callback. A tenant switch and a step-up verification
	// deliberately issue NO new refresh token -- neither is a new login,
	// and the caller keeps the token it already holds -- so on those
	// responses RefreshToken is empty, and toTokenPairResponse omits the
	// refresh_token field from the wire payload entirely rather than
	// echoing a token the response never minted.
	RefreshToken string
	// RefreshExpiresAt is when RefreshToken stops being accepted. It is
	// zero -- and likewise absent from the wire payload -- exactly when
	// RefreshToken is empty: a tenant switch or a step-up verification,
	// which issue no new refresh token.
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
	features    FeatureGate
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

	// SMS phone-login and MFA state.
	kv                 pkgcore.KVStore
	guard              *rateGuard
	sms                pkgcore.SMSSender
	smsCodeTTL         time.Duration
	smsCodeMaxAttempts int
	verificationCodes  *VerificationCodeRepository
	mfaFactors         *MFAFactorRepository
	recoveryCodes      *RecoveryCodeRepository
	issuer             string

	// secureCookies is WithSecureCookies' value: whether Handler must
	// force Secure on the pre-authentication OAuth cookie regardless of
	// r.TLS. See that option's doc comment.
	secureCookies bool

	// trustedProxies is WithTrustedProxies' compiled value: the IP
	// addresses and CIDR prefixes of the reverse proxies this deployment
	// receives requests through. Handler.clientIP reads a request's
	// forwarding headers only when its direct connection address is within
	// one of them; empty keeps every request recording its direct
	// connection address. See that option's doc comment.
	trustedProxies []netip.Prefix

	// timezoneResolver is WithTimeZoneResolver's value: the registration
	// timezone chain's IP-resolution tier. Nil -- the default, and the
	// reference app's wiring -- skips the tier, and the chain ends at "not
	// chosen yet"; see TimeZoneResolver for why this seam is not
	// fail-closed.
	timezoneResolver TimeZoneResolver

	// vendorClientIPHeaders is WithVendorClientIPHeaders' validated value:
	// the single-hop vendor client-address headers this deployment's proxy
	// genuinely overwrites, which Handler.clientIP reads -- after the
	// X-Forwarded-For chain walk, never before it -- for a request whose
	// peer is within trustedProxies. Empty (the default) reads no vendor
	// header at all. See that option's doc comment.
	vendorClientIPHeaders []VendorClientIPHeader

	// authCount and authDuration back the
	// "authn.auth.count"/"authn.auth.duration" instruments
	// registerAuthMetrics wires from NewService. recordAuthMetric guards
	// against their nil zero value, the same fail-open contract
	// go/jobs.StandaloneQueue's own metric fields document.
	authCount    metric.Int64Counter
	authDuration metric.Float64Histogram
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

	// A freshly built Service must be able to take its one system context
	// (the sign-in tenant enumeration) without its embedder having
	// remembered a separate declaration, so the purpose is declared here as
	// well as on the module's component descriptor (SystemPurposes) --
	// idempotent by contract, and the same purpose twice changes nothing.
	// Without this, a Service built directly through NewService would fail
	// its no-tenant sign-in with a refusal folded into the uniform
	// invalid-credentials answer, which is the hardest possible place to
	// diagnose a missing declaration.
	pkgcore.RegisterSystemPurpose(SystemPurposeSignInTenantEnumeration)

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
	verificationCodes, err := NewVerificationCodeRepository(db)
	if err != nil {
		return nil, err
	}
	mfaFactors, err := NewMFAFactorRepository(db)
	if err != nil {
		return nil, err
	}
	recoveryCodes, err := NewRecoveryCodeRepository(db)
	if err != nil {
		return nil, err
	}

	tokenOpts := []TokenOption{
		WithTokenTTL(cfg.accessTTL),
		WithTokenIssuer(cfg.issuer),
		WithTokenClock(cfg.now),
	}
	signer, err := NewSigner(cfg.keySource, tokenOpts...)
	if err != nil {
		return nil, err
	}
	verifier, err := NewVerifier(cfg.keySource, tokenOpts...)
	if err != nil {
		return nil, err
	}

	manager, err := NewSessionManager(sessionRepo, tokenRepo, kv, bus, cfg.revocationMode,
		cfg.now, cfg.refreshTTL, cfg.sessionTTL, cfg.accessTTL)
	if err != nil {
		return nil, err
	}

	// The revocation source every Middleware built over this service's
	// verifier consults BY DEFAULT: the manager itself. RevocationMode
	// immediate's own doc comment promises that the revocation list is
	// "what Middleware consults on every authenticated request", and this
	// attachment is what makes that promise hold in the default
	// composition -- a host that builds authn.Middleware(service.Verifier())
	// with no options at all gets the check, no WithRevocationChecker
	// required. A natural-mode manager answers false without touching the
	// store, so the attachment costs nothing under the module's own
	// default mode. Middleware's WithRevocationChecker option replaces
	// this source with an explicit one for hosts that want it.
	verifier.revocation = manager

	authCount, authDuration := registerAuthMetrics()

	svc := &Service{
		users:            users,
		sessions:         manager,
		sessionRepo:      sessionRepo,
		attempts:         attempts,
		signer:           signer,
		verifier:         verifier,
		bus:              bus,
		membership:       cfg.membership,
		features:         cfg.featureGate,
		now:              cfg.now,
		params:           cfg.passwordParams,
		policy:           cfg.passwordPolicy,
		identities:       identities,
		providers:        providers,
		states:           states,
		redirects:        cfg.redirects,
		trustedProviders: slices.Clone(cfg.trustedProviders),

		kv:                    kv,
		guard:                 newRateGuard(kv),
		sms:                   cfg.smsSender,
		smsCodeTTL:            cfg.smsCodeTTL,
		smsCodeMaxAttempts:    cfg.smsCodeMaxAttempts,
		verificationCodes:     verificationCodes,
		mfaFactors:            mfaFactors,
		recoveryCodes:         recoveryCodes,
		issuer:                cfg.issuer,
		secureCookies:         cfg.secureCookies,
		trustedProxies:        slices.Clone(cfg.trustedProxyNets),
		vendorClientIPHeaders: slices.Clone(cfg.vendorClientIPHeaders),
		timezoneResolver:      cfg.timezoneResolver,

		authCount:    authCount,
		authDuration: authDuration,
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
// Registration is PRE-TENANT: the account is created in no tenant and the
// event announcing it carries no tenant whatever ctx holds (it is published
// through publishTenantless -- see that method's doc comment for why an
// inherited tenant would seat the account in the caller's tenant). ctx's
// tenant, when a composition left one there, plays no part in anything this
// method does.
//
// Known limitation: a duplicate identifier is reported as a conflict, which
// makes registration an account-enumeration oracle in a way sign-in
// deliberately is not. Removing the oracle means answering every
// registration with "check your inbox" and moving the conflict into an
// email, which requires delivery and verification flows this module does
// not ship; the honest position is that the conflict is visible here.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*User, error) {
	if err := s.guard.CheckRegister(ctx, in.IP); err != nil {
		return nil, err
	}

	email := strings.TrimSpace(in.Email)
	phone := strings.TrimSpace(in.Phone)
	if email == "" && phone == "" {
		return nil, ErrIdentifierRequired
	}
	if err := s.policy.Validate(in.Password); err != nil {
		return nil, err
	}
	// The display name is refused, not truncated, when it exceeds
	// users.display_name's declared column width -- it is the user's own
	// chosen identity text, where a silent shortening would corrupt what
	// they typed (see model.go's column-width constants and
	// ErrDisplayNameTooLong's doc comment).
	if length := utf8.RuneCountInString(in.DisplayName); length > displayNameWidth {
		return nil, ErrDisplayNameTooLong.WithParam("max_length", displayNameWidth)
	}

	// Both preferences are resolved here, at the account's initialization,
	// and written in the same insert that creates it -- never a second
	// update -- so a newly registered account carries its initial locale
	// and timezone from the first read anyone can make. Both chains are
	// lenient (registrationLocale / registrationTimeZone): an unusable
	// declared value is skipped rather than refused, and the chains end at
	// empty, whose effective defaults are the platform's en-US and UTC.
	user := &User{
		DisplayName:   in.DisplayName,
		Locale:        registrationLocale(in.Locale, in.AcceptLanguage),
		Timezone:      s.registrationTimeZone(ctx, in.Timezone, in.IP),
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
		return nil, s.mapRegisterCreateConflict(ctx, user, err)
	}

	// Registration is pre-tenant: the account is created in no tenant, and
	// the event announcing it must carry no tenant regardless of what the
	// caller's context holds -- a caller who merely holds a tenant (their
	// bearer was resolved by a host composition that resolves even
	// allowlisted pre-auth routes) is not attesting one for this account.
	// A tenant on this event would seat the account in the caller's tenant
	// (org's handleUserCreated) and skip the host's tenant-less
	// self-service provisioning of the registrant's own workspace. See
	// publishTenantless's doc comment.
	s.publishTenantless(ctx, pkgcore.Event{
		Type: EventUserCreated,
		Payload: UserCreatedPayload{
			UserID:   user.ID,
			HasEmail: user.Email != "",
			HasPhone: user.Phone != "",
		},
	})
	return user, nil
}

// mapRegisterCreateConflict translates a registration whose insert lost the
// race against a CONCURRENT registration of the same identifier. The
// pre-checks above answer the sequential duplicate; when two registrations
// both pass them, the database's unique index (idx_users_email_index,
// idx_users_phone_index) admits exactly one insert and refuses the other
// with gorm.ErrDuplicatedKey. That refusal must reach the caller as the
// same coded conflict the pre-checks answer -- a bare internal error would
// tell a client the server is broken when the truth is that the identifier
// is taken, and the documented duplicate-registration answer is a conflict
// (see Register's own doc comment).
//
// Which of the two unique indexes refused is answered by probing the
// account's identifiers: the racing writer's commit is necessarily visible
// by the time this insert failed, so the identifier that now resolves is
// the one that was taken. Email is probed first when the account carries
// both, matching the pre-checks' own order.
func (s *Service) mapRegisterCreateConflict(ctx context.Context, user *User, err error) error {
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		return err
	}
	if user.Email != "" {
		if _, findErr := s.users.FindByEmail(ctx, user.Email); findErr == nil {
			return ErrEmailAlreadyRegistered
		}
	}
	if user.Phone != "" {
		if _, findErr := s.users.FindByPhone(ctx, user.Phone); findErr == nil {
			return ErrPhoneAlreadyRegistered
		}
	}
	return err
}

// Login verifies a password and, on success, starts a session and issues the
// first token pair.
//
// Every failure returns ErrInvalidCredentials, with no parameter and no
// timing shortcut: an unknown identifier still costs one argon2id derivation,
// so the response time does not answer the question the error refuses to.
// That "every failure" includes a correct password whose account resolves no
// membership -- no MembershipReader wired, no membership of any tenant, or
// no membership of an explicitly requested one: the membership question is
// asked only after the password verified, so an answer naming it (403
// tenant_membership_required / tenant_membership_unavailable) would certify
// the password, a guessable and reusable secret, to an anonymous caller.
// The distinguishable membership errors belong to the paths where they
// certify nothing an attacker could only obtain by guessing: authenticated
// callers (tenant switching, refresh), and the social/SSO/SMS-code session
// start, whose certified secret is a one-time code the attempt itself has
// already spent -- resolveTenant's doc comment classifies all four call
// sites on that axis. The specific reason is written to the login history
// for the operator and for the account owner's own security page,
// FailureReasonNoMembership among them.
func (s *Service) Login(ctx context.Context, in LoginInput) (*TokenPair, error) {
	start := time.Now()
	pair, err := s.login(ctx, in)
	s.recordAuthMetric(ctx, authOpLogin, start, err)
	return pair, err
}

// channelEnabled is the enforcement point this module's declared feature
// flags run through (see module.go's FeatureGate and the FeatureFlag*
// constants for the contract). It refuses a request that tries to use a
// sign-in channel the deployment turned off.
//
// A nil gate allows every channel: this deployment has no feature-flag
// module, so there is nowhere an operator could have disabled anything, and
// the channels the host wired are the channels that exist. With a gate
// wired, a channel whose flag is off refuses with ErrChannelDisabled, and a
// gate that cannot be read refuses too: failing CLOSED on "cannot answer
// whether the channel is on" is the same policy CheckLogin and the
// revocation check apply to their own unanswerable questions, and the
// alternative -- letting a config outage quietly re-enable every channel an
// operator disabled -- is exactly the bypass this seam exists to close.
func (s *Service) channelEnabled(ctx context.Context, key string) error {
	if s.features == nil {
		return nil
	}
	enabled, err := s.features.IsEnabled(ctx, key)
	if err != nil {
		obs.FromContext(ctx).Error("sign-in channel flag could not be read", "channel", key, "error", err)
		return ErrInternal.WithCause(err)
	}
	if !enabled {
		return ErrChannelDisabled.WithParam("channel", key)
	}
	return nil
}

// login implements Login. It is its own unexported method so Login's body
// can wrap it in the two lines that record authOpLogin's count/duration
// without introducing a NAMED return value into a function this long --
// every one of its existing "if err := ...; err != nil" blocks would
// otherwise shadow that named return and trip govet's shadow analyzer
// (enabled via this repo's .golangci.yml govet enable-all setting), the
// same constraint that gives Refresh and VerifyStepUp this shape.
func (s *Service) login(ctx context.Context, in LoginInput) (*TokenPair, error) {
	// The channel gate runs before anything else -- before the identifier
	// is even normalized -- so a disabled channel costs an attacker
	// nothing to probe and buys the account nothing in lockout state: the
	// flag's value is public (the login page reads it pre-auth), so there
	// is no information to protect and no reason to burn rate-limit or
	// argon2id budget on a channel the deployment turned off.
	if err := s.channelEnabled(ctx, FeatureFlagPasswordLogin); err != nil {
		return nil, err
	}

	identifier := strings.TrimSpace(in.Identifier)
	// account is the blind index the progressive lockout and the
	// per-account sliding window key on. It is computed even for an
	// identifier this module cannot recognize (identifierIndex returns ""
	// rather than an error for that case), so CheckLogin always sees a
	// best-effort account dimension -- checked BEFORE any password work,
	// so a locked-out or rate-limited caller cannot force an argon2id
	// derivation merely by retrying.
	account := s.identifierIndex(identifier)
	if err := s.guard.CheckLogin(ctx, account, in.IP); err != nil {
		return nil, err
	}

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
		if apperr.HasCode(err, ErrInvalidEmail.Code) || apperr.HasCode(err, ErrInvalidPhone.Code) {
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
		s.recordFailure(ctx, in, "", account, FailureReasonUnknownUser)
		s.guard.RecordLoginFailure(ctx, account)
		return nil, ErrInvalidCredentials
	}

	index := account
	if user.PasswordHash == "" {
		s.burnPasswordWork(in.Password)
		s.recordFailure(ctx, in, user.ID, index, FailureReasonNoPassword)
		s.guard.RecordLoginFailure(ctx, index)
		return nil, ErrInvalidCredentials
	}

	ok, err := VerifyPassword(user.PasswordHash, in.Password)
	if err != nil {
		obs.FromContext(ctx).Error("stored password hash is unreadable", "user_id", user.ID, "error", err)
		s.recordFailure(ctx, in, user.ID, index, FailureReasonBadPassword)
		s.guard.RecordLoginFailure(ctx, index)
		return nil, ErrInvalidCredentials
	}
	if !ok {
		s.recordFailure(ctx, in, user.ID, index, FailureReasonBadPassword)
		s.guard.RecordLoginFailure(ctx, index)
		return nil, ErrInvalidCredentials
	}
	if user.Status != UserStatusActive {
		s.recordFailure(ctx, in, user.ID, index, FailureReasonSuspended)
		s.guard.RecordLoginFailure(ctx, index)
		return nil, ErrInvalidCredentials
	}

	tenantID, err := s.resolveTenant(ctx, user.ID, in.TenantID)
	if err != nil {
		// The password verified and the account is active, so this is the
		// last point at which the caller's credential was right -- and the
		// one at which an answer naming the real cause (403
		// tenant_membership_required / tenant_membership_unavailable) would
		// certify the password to an anonymous caller, the single fact every
		// failure above this line refuses to state. The reachable surface is
		// not an edge case: a generated project whose membership seam is not
		// wired (nil reader, resolveTenant's first branch) answers this way
		// for every correct password, every attempt. Nothing is lost by the
		// uniform answer: recordFailure below writes the real cause into the
		// login history, which is where the reason belongs (see Login's doc
		// comment).
		//
		// The uniform answer is also the whole of this path's effect on
		// the login guard, deliberately: recordFailure below is called,
		// but guard.RecordLoginFailure is not -- the one failure in login
		// with a real, known account that never feeds the progressive
		// lockout (the empty-identifier and no-canonical-form failures
		// above skip it only because their account is the empty string,
		// which makes the call a no-op). The lockout's escalating delay
		// is the guard's response to a run of wrong guesses (see
		// ratelimit.go's RecordLoginFailure), and this path is not a
		// guess: the password just verified, and the membership answer --
		// from the host's MembershipReader, or its absence -- is not
		// something repeated attempts extract differently. Feeding the
		// lockout here would escalate on non-guesses: in an unwired
		// generated project, where every correct-password login of every
		// account is this path, a few correct-password attempts would
		// refuse the account's own legitimate sign-in for a growing
		// window -- 30s after the first failure, loginLockoutMax after
		// roughly five (ratelimit.go's own constants). The sliding
		// windows CheckLogin applies still count each attempt; what is
		// skipped is only the failure-accumulating lockout. The failure
		// itself stays on the record -- the history row and the
		// EventLoginFailed event below are written exactly as every
		// failure above is.
		s.recordFailure(ctx, in, user.ID, index, FailureReasonNoMembership)
		return nil, ErrInvalidCredentials
	}

	s.guard.RecordLoginSuccess(ctx, index)
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

	pair, err := s.mintPair(ctx, user, session, tenantID, issued)
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
//
// That re-verification deliberately runs BEFORE the presented token is
// rotated (consumed), not after. Rotating first would leave a
// re-verification failure with the presented token permanently spent and
// the caller never having received its replacement; the client's own,
// entirely legitimate retry with that same token would then hit the replay
// detector, which cannot tell that retry apart from an actually stolen
// token, and pay the actual-theft price for it: the whole refresh-token
// family and the session revoked, a "suspected theft" event fired -- over
// what is really a transient MembershipReader outage or a passing
// user-status flap. Resolving the token and its session first, running
// every re-verification a caller needs, and only then committing the
// rotation keeps a re-verification failure from ever touching the token at
// all: the client's retry, once whatever failed clears, presents the exact
// same still-active token and succeeds normally. An ACTUALLY replayed token
// -- one really already rotated by a prior, successful call -- is untouched
// by this ordering: resolveRotation catches it at the same first step,
// before any re-verification runs.
func (s *Service) Refresh(ctx context.Context, presented string) (*TokenPair, error) {
	start := time.Now()
	pair, err := s.refresh(ctx, presented)
	s.recordAuthMetric(ctx, authOpRefresh, start, err)
	return pair, err
}

// refresh implements Refresh as its own unexported method for the
// shadow-avoidance reason documented on Login's doc comment.
func (s *Service) refresh(ctx context.Context, presented string) (*TokenPair, error) {
	record, session, err := s.sessions.resolveRotation(ctx, presented)
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

	session, issued, err := s.sessions.commitRotation(ctx, record, session)
	if err != nil {
		return nil, err
	}

	return s.mintPair(ctx, user, session, tenantID, issued)
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
	// The same idiom Rotate uses: a session that is Active in name but past
	// its own ExpiresAt is not usable either. Nothing here ever flips
	// Status away from active when a session merely times out, so this
	// check -- not the status one above -- is what actually catches it;
	// without it a session's practical lifetime stretched past its
	// configured TTL by however long the caller's already-issued access
	// token still had left to run.
	if session.Status != SessionStatusActive || !s.now().Before(session.ExpiresAt) {
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
	switched, setErr := s.sessionRepo.SetCurrentTenant(ctx, session.ID, target)
	if setErr != nil {
		return nil, setErr
	}
	if !switched {
		// The update's WHERE status = active clause matched no row, which
		// means the session was revoked between the read above and this
		// write. Refusing to mint is what closes the race: without this
		// check the caller would receive a full-lifetime token pair for a
		// session a concurrent sign-out just killed -- and under the
		// natural revocation mode nothing downstream consults the
		// revocation list, so that token would stay valid for its whole
		// TTL. The database has already decided, and the answer is the
		// same one a read that noticed the revoked status would give.
		return nil, ErrSessionRevoked
	}
	session.CurrentTenantID = string(target)

	pair, err := s.mintPair(ctx, user, session, target, IssuedRefreshToken{})
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

// mintPair signs an access token for the session and assembles the
// response, using the session's OWN authentication methods. issued.Secret is
// empty for a tenant switch, which reuses the caller's existing refresh
// token rather than minting one.
func (s *Service) mintPair(ctx context.Context, user *User, session *Session, tenantID pkgcore.TenantID, issued IssuedRefreshToken) (*TokenPair, error) {
	return s.mintPairWithAMR(ctx, user, session, tenantID, issued, session.AMRList())
}

// mintPairWithAMR is mintPair generalized to an explicit amr, which
// VerifyStepUp (mfa.go) uses to mint a token carrying an ENRICHED AMR --
// the session's own methods plus the second factor just verified -- without
// persisting that enrichment back to the session row. See VerifyStepUp's
// own doc comment for why not persisting it is what bounds the elevation to
// one access-token lifetime.
func (s *Service) mintPairWithAMR(ctx context.Context, user *User, session *Session, tenantID pkgcore.TenantID, issued IssuedRefreshToken, amr []string) (*TokenPair, error) {
	principal := Principal{
		UserID:    user.ID,
		TenantID:  tenantID,
		SessionID: session.ID,
		AMR:       amr,
	}
	principal.Email = user.Email

	access, expiresAt, err := s.signer.Issue(ctx, principal)
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
//
// The no-tenant branch takes the audited system-context grant this module
// declares for its own tenant enumeration (SystemPurposeSignInTenantEnumeration,
// actor = the user asking, via tenancy.WithSystemContext) and calls
// MembershipReader.TenantsOf under that elevated context -- the question
// spans tenants by definition, so the authn side of the house takes the
// grant itself and the reader never has to invent one. A grant that cannot
// be taken fails closed through the same ErrTenantMembershipUnavailable
// path as a failing reader, so the classification below is unaffected.
//
// It always answers with the same distinguishable errors
// (ErrTenantMembershipRequired, ErrTenantMembershipUnavailable); whether a
// call site lets that answer reach its caller or folds it into a uniform
// failure is that site's decision, taken on one axis: would a
// distinguishable answer certify something an attacker could only obtain by
// guessing? A site folds when it would; otherwise it lets the error
// through, because the question the error answers is precisely "may this
// person act inside this tenant" and an unanswerable authorization question
// is a refusal. The four call sites sort themselves under that axis:
//
//   - login (password sign-in): the caller is anonymous and the credential
//     this call sits just past is a password -- the one secret in this
//     module an attacker can keep guessing at, and one whose validity
//     survives the attempt, so an answer naming the membership cause would
//     certify it. login folds: recordFailure writes the specific reason
//     (FailureReasonNoMembership) to the login history and the caller
//     answers the uniform ErrInvalidCredentials every other sign-in
//     failure answers (see Login's doc comment).
//
//   - startExternalSession (social sign-in, enterprise SSO and SMS-code
//     session start): the caller is anonymous, but the secret the exchange
//     just certified is not something an attacker could only obtain by
//     guessing -- for social and SSO it is an IdP authorization code, minted
//     per session by the identity provider and bound to this client and
//     redirect URI, and for SMS-code login a one-time code delivered to the
//     number the caller must own to receive it (successful verification IS
//     the ownership proof, per LoginWithSMSCode). All of them are
//     single-use, spent by this very attempt, so a distinguishable answer
//     certifies nothing that survives to be exploited. It answers as-is: a
//     no-membership user of a working Google login is told membership is
//     the problem, the actionable answer, not that their login was
//     invalid.
//
//   - refresh: the caller is authenticated -- resolveRotation above
//     accepted a refresh token only the session's user could present. A
//     distinguishable answer certifies no guessable secret, only the
//     caller's own membership facts. It answers as-is.
//
//   - SwitchTenant: the caller is authenticated -- the principal was
//     verified against the live session above this call. Same reasoning as
//     refresh: the answer certifies the caller's own membership in the
//     target tenant, the one fact this method exists to establish. It
//     answers as-is.
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

	// The enumeration spans tenants by definition, so the reader is handed
	// an elevated context: the audited system-context grant, attributed to
	// the account asking about its own memberships, is taken here and the
	// elevated context -- never the caller's original one -- goes to
	// TenantsOf (MembershipReader's own doc comment states the
	// precondition). A grant that cannot be taken (no bus, a failing audit
	// publish) folds into the same fail-closed refusal as any other
	// membership failure at this call site, so the classification contract
	// above is unchanged: the refusal is never "no membership".
	sysCtx, err := tenancy.WithSystemContext(ctx, s.bus, pkgcore.SystemReason{
		Actor:   userID,
		Purpose: SystemPurposeSignInTenantEnumeration,
	})
	if err != nil {
		obs.FromContext(ctx).Error("membership lookup failed", "user_id", userID, "error", err)
		return "", ErrTenantMembershipUnavailable.WithCause(err)
	}
	tenants, err := s.membership.TenantsOf(sysCtx, userID)
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
//
// The write is one guarded single-column UPDATE
// (UserRepository.ReplacePasswordHashIfStillCurrent), never a whole-row
// save of this sign-in's pre-verification snapshot: user was read before
// the argon2 derivation ran, and saving that snapshot back in full would
// silently undo whatever another caller committed on the row in between --
// a phone-verified flag from a concurrent SMS sign-in, or a password hash
// a concurrent writer already replaced (which the password_hash guard
// refuses outright).
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
	updated, err := s.users.ReplacePasswordHashIfStillCurrent(ctx, user.ID, user.PasswordHash, hash)
	if err != nil {
		obs.FromContext(ctx).Warn("password rehash could not be stored", "user_id", user.ID, "error", err)
		return
	}
	if !updated {
		obs.FromContext(ctx).Warn("password rehash skipped: the stored hash changed since this sign-in read it",
			"user_id", user.ID)
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
	// A failed sign-in is a pre-tenant fact -- no tenant is attested by a
	// sign-in that resolved none, whatever the caller's context holds (see
	// publishTenantless's doc comment) -- so it never inherits one.
	s.publishTenantless(ctx, pkgcore.Event{
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
//
// An event whose TenantID the emitting site did not set picks up the tenant
// of the context it is published in, when one is present. The handlers of
// protected operations layer the acting principal's own TenantID onto the
// ctx they hand the service (pkgcore.WithTenant, principalCtx -- the same
// layering recordAudit uses for audit rows), so the security-relevant
// facts those operations announce -- identity unbound, MFA enrolled,
// recovery codes regenerated -- carry the same tenant their audit rows do.
// A site that knows its event's tenant sets TenantID explicitly (the
// session and SSO events do); it is never overwritten here.
//
// The context-tenant inheritance is ONLY safe for those protected-operation
// events, whose ctx the module's own handler layered: whether a pre-tenant
// site's event stays tenant-less must never depend on what the HOST's
// composition put in the request context -- authn's pre-auth routes can sit
// behind a tenancy middleware whose allowlist exempts a route from the 403
// on a resolution FAILURE without skipping resolution, so a valid bearer on
// an allowlisted route still gets its tenant injected. Every pre-tenant
// site therefore publishes through publishTenantless, never through this
// method (see publishTenantless's doc comment for what that guarantees).
func (s *Service) publish(ctx context.Context, evt pkgcore.Event) {
	s.publishOn(ctx, evt, true)
}

// publishTenantless emits a PRE-TENANT fact -- an event that must carry no
// tenant, whatever the context it is published in holds. Registration and
// the social sign-in mint, a failed sign-in and an identity bound at an
// unauthenticated callback announce facts that happened in no tenant: the
// tenant_id a request merely holds (its caller's bearer, a body field) is
// not an attestation, and no amount of host composition may make one.
//
// The "no tenant" property is load-bearing for the subscribers, which is
// what makes an accidental leak a real defect rather than a cosmetic one:
// org's handleUserCreated (go/org/events.go) seats a user whose
// authn.user.created event carries a tenant in that tenant, while a host's
// tenant-less self-service provisioning (the reference app's
// self_service.go) creates the registrant's own workspace exactly when the
// event carries none -- so a registration event that picked up the
// caller's context tenant would hand the caller's tenant a membership it
// was never granted AND deny the new account the workspace its
// registration creates.
//
// A site that needs its event to carry a tenant declares it explicitly
// through publish with evt.TenantID set (the enterprise-SSO mint does, from
// the SSO configuration); a site whose event must never carry one calls
// this method. Either way the declaration is the site's own, never an
// accident of the context it runs in.
func (s *Service) publishTenantless(ctx context.Context, evt pkgcore.Event) {
	if evt.TenantID != "" {
		// A pre-tenant site declaring a tenant is a programming error: the
		// declaration would silently be dropped here. Logged at Error --
		// the event has already been committed, so this must not turn the
		// caller's success into a failure, but an operator must hear about
		// the site that is lying about what it publishes.
		obs.FromContext(ctx).Error("authn pre-tenant event published with a tenant; the tenant is dropped",
			"event_type", evt.Type)
		evt.TenantID = ""
	}
	s.publishOn(ctx, evt, false)
}

// publishOn is the delivery half shared by publish and publishTenantless;
// inheritTenant decides whether an event without an explicit TenantID may
// pick one up from ctx.
func (s *Service) publishOn(ctx context.Context, evt pkgcore.Event, inheritTenant bool) {
	if inheritTenant && evt.TenantID == "" {
		if tenantID, ok := pkgcore.TenantFromContext(ctx); ok {
			evt.TenantID = tenantID
		}
	}
	if err := s.bus.Publish(ctx, evt); err != nil {
		obs.FromContext(ctx).Warn("domain event publish failed", "event_type", evt.Type, "error", err)
	}
}
