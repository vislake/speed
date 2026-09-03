package authn

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	obs "github.com/vislake/speed/go/observability"
)

// UserIdentity is one external account bound to a user: a social channel's
// account, or an enterprise single sign-on subject.
//
// It is IDENTITY-domain data, like User, and carries no tenant_id: the person
// is the same person in every tenant they belong to, and a per-tenant copy of
// their GitHub account would be meaningless. The pair (provider, external_id)
// is globally unique, which is what makes "this external account is already
// bound to somebody" a database constraint rather than a race between two
// concurrent sign-ins.
//
// Email is the address the provider reported. It is stored encrypted, has no
// blind index, and is NEVER used to find an account: lookups go through
// (provider, external_id). It is here for the settings page ("GitHub ·
// you@example.com") and for support, not as an identifier -- treating it as
// one is exactly the account-takeover this module's linking rule refuses.
type UserIdentity struct {
	// ID is an application-generated UUID.
	ID string `gorm:"primaryKey;size:36"`

	// UserID is the owning User.ID. It is not a foreign key: cross-module
	// foreign keys are banned repository-wide and same-module ones make
	// independently released migrations unmanageable.
	UserID string `gorm:"column:user_id;size:36;not null;index:idx_user_identities_user_id"`

	// Provider is one of the Provider* constants, or an
	// "oidc:<tenant>" enterprise channel.
	Provider string `gorm:"column:provider;size:64;not null;uniqueIndex:idx_user_identities_provider_external,priority:1"`

	// ExternalID is the provider's stable identifier for the person.
	ExternalID string `gorm:"column:external_id;size:191;not null;uniqueIndex:idx_user_identities_provider_external,priority:2"`

	// Email is the address the provider reported, encrypted at rest, or
	// empty. It is display data, never a lookup key.
	Email string `gorm:"serializer:authn_pii"`

	// DisplayName and AvatarURL are what the provider reported about the
	// person, refreshed on every sign-in through this identity.
	DisplayName string `gorm:"column:display_name;size:128;not null"`
	AvatarURL   string `gorm:"column:avatar_url;size:512;not null"`

	CreatedAt time.Time `gorm:"autoCreateTime;not null"`
	UpdatedAt time.Time `gorm:"autoUpdateTime;not null"`

	// LastLoginAt is when this identity was last used to sign in, so the
	// settings page can show which binding is actually in use before
	// somebody removes one.
	LastLoginAt *time.Time `gorm:"column:last_login_at"`
}

// TableName pins the table name.
func (UserIdentity) TableName() string { return "user_identities" }

// UserIdentityRepository reads and writes the user_identities table.
//
// Like every other repository in this module it holds a plain *gorm.DB, for
// the reason repository.go's file comment gives: identity data must not
// implement dbkit.TenantScoped, and dbkit.Repository[T] is constrained to
// types that do. The compensating control is the AssertNotTenantScoped suite
// in identity_test.go.
type UserIdentityRepository struct {
	db *gorm.DB
}

// NewUserIdentityRepository binds db.
func NewUserIdentityRepository(db *gorm.DB) (*UserIdentityRepository, error) {
	if db == nil {
		return nil, errors.New("authn: NewUserIdentityRepository requires a database handle")
	}
	return &UserIdentityRepository{db: db}, nil
}

// Create inserts identity, filling in its ID when empty.
func (r *UserIdentityRepository) Create(ctx context.Context, identity *UserIdentity) error {
	if identity.ID == "" {
		identity.ID = newID()
	}
	return r.db.WithContext(ctx).Create(identity).Error
}

// FindByExternal returns the identity registered for (provider, externalID),
// or ErrNotFound.
func (r *UserIdentityRepository) FindByExternal(ctx context.Context, provider, externalID string) (*UserIdentity, error) {
	var identity UserIdentity
	err := r.db.WithContext(ctx).
		Where("provider = ? AND external_id = ?", provider, externalID).
		First(&identity).Error
	if err != nil {
		return nil, translate(err)
	}
	return &identity, nil
}

// FindByID returns the identity with the given id, or ErrNotFound.
func (r *UserIdentityRepository) FindByID(ctx context.Context, id string) (*UserIdentity, error) {
	var identity UserIdentity
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&identity).Error; err != nil {
		return nil, translate(err)
	}
	return &identity, nil
}

// ListByUser returns a user's bound identities, oldest first, which is the
// order they were added in and the order the settings page shows.
func (r *UserIdentityRepository) ListByUser(ctx context.Context, userID string) ([]UserIdentity, error) {
	var identities []UserIdentity
	err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at ASC, id ASC").
		Find(&identities).Error
	return identities, err
}

// TouchLogin records that identity was just used to sign in, refreshing the
// display fields the provider reported.
func (r *UserIdentityRepository) TouchLogin(ctx context.Context, identity *UserIdentity, at time.Time) error {
	identity.LastLoginAt = &at
	return r.db.WithContext(ctx).
		Where("id = ?", identity.ID).
		Updates(&UserIdentity{
			DisplayName: identity.DisplayName,
			AvatarURL:   identity.AvatarURL,
			Email:       identity.Email,
			LastLoginAt: &at,
		}).Error
}

// Delete removes an identity that belongs to userID and reports whether it
// did. Scoping the delete to the owner in the WHERE clause, rather than
// checking ownership beforehand, is what makes "remove somebody else's
// binding" impossible even under a concurrent transfer.
func (r *UserIdentityRepository) Delete(ctx context.Context, userID, id string) (bool, error) {
	res := r.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", id, userID).
		Delete(&UserIdentity{})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// SocialAuthorizeInput describes the authorization request to build.
type SocialAuthorizeInput struct {
	// Provider is the channel name.
	Provider string

	// RedirectURI is where the provider should send the browser back to.
	// It must be on the deployment's allowlist.
	RedirectURI string

	// SessionBinding ties the flow to the browser that started it. Derive
	// it from a pre-authentication cookie with BindingFromCookie.
	SessionBinding string

	// LinkUserID is set when an ALREADY SIGNED-IN user is binding a new
	// identity rather than signing in. It comes from the caller's verified
	// Principal, never from a request parameter.
	LinkUserID string
}

// SocialCallbackInput describes an authorization callback.
type SocialCallbackInput struct {
	// Provider is the channel whose callback endpoint was hit.
	Provider string
	// Code is the authorization code the provider returned.
	Code string
	// State is the value the provider echoed back.
	State string
	// SessionBinding must equal what the authorization request supplied.
	SessionBinding string
	// TenantID requests a tenant for the new session's first access token.
	// Empty means "the user's first tenant". It is a request, never a
	// grant: membership is verified either way.
	TenantID pkgcore.TenantID
	// Device, UserAgent and IP describe the client.
	Device    string
	UserAgent string
	IP        string
}

// SocialLoginResult is what a completed callback produced.
type SocialLoginResult struct {
	// User is the account the flow resolved to.
	User *User
	// Identity is the external identity row, created or reused.
	Identity *UserIdentity
	// Tokens is the new session's token pair. It is nil when the flow was
	// a BINDING by an already-signed-in user, which starts no session.
	Tokens *TokenPair
	// Created reports whether a new account was provisioned.
	Created bool
	// Bound reports whether the flow attached an identity to an
	// already-signed-in user rather than signing anyone in.
	Bound bool
	// AutoLinked reports whether an existing account was linked to a new
	// external identity automatically, under the verified-and-trusted
	// rule. It is the flag a security notice keys on.
	AutoLinked bool
}

// SocialAuthorizeURL validates the request, issues a single-use state value
// and returns the URL to send the browser to.
func (s *Service) SocialAuthorizeURL(ctx context.Context, in SocialAuthorizeInput) (string, error) {
	provider, err := s.socialProvider(in.Provider)
	if err != nil {
		return "", err
	}
	if !s.redirects.Allows(in.RedirectURI) {
		return "", ErrRedirectURINotAllowed
	}

	state, err := s.states.Issue(ctx, StateBinding{
		Provider:       provider.Name(),
		RedirectURI:    in.RedirectURI,
		SessionBinding: in.SessionBinding,
		LinkUserID:     in.LinkUserID,
	})
	if err != nil {
		return "", ErrInternal.WithCause(err)
	}
	return provider.AuthorizeURL(state, in.RedirectURI), nil
}

// SocialCallback completes an authorization flow.
//
// The order of the steps is the security design, not an implementation
// detail: the state is consumed BEFORE the code is exchanged, so a forged or
// replayed callback never reaches the provider at all, and the redirect URI
// used at the token endpoint comes from the server-side state record rather
// than from the request, so a caller cannot substitute one.
func (s *Service) SocialCallback(ctx context.Context, in SocialCallbackInput) (*SocialLoginResult, error) {
	provider, err := s.socialProvider(in.Provider)
	if err != nil {
		return nil, err
	}
	if in.Code == "" {
		return nil, ErrOAuthStateInvalid
	}

	binding, err := s.states.Consume(ctx, in.State, provider.Name(), in.SessionBinding)
	if err != nil {
		return nil, err
	}

	external, err := provider.Exchange(ctx, in.Code, binding.RedirectURI)
	if err != nil {
		return nil, err
	}
	if external == nil || external.ExternalID == "" {
		return nil, ErrSocialIdentityIncomplete.WithParam("provider", provider.Name())
	}
	// A provider implementation that reported the wrong channel would bind
	// an identity under a name nothing looks it up by.
	external.Provider = provider.Name()

	if binding.LinkUserID != "" {
		return s.bindExternalIdentity(ctx, binding.LinkUserID, external)
	}
	return s.signInWithExternalIdentity(ctx, external, in)
}

// bindExternalIdentity attaches external to an already-signed-in user.
func (s *Service) bindExternalIdentity(ctx context.Context, userID string, external *ExternalIdentity) (*SocialLoginResult, error) {
	user, err := s.users.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrAuthenticationRequired
		}
		return nil, err
	}

	existing, err := s.identities.FindByExternal(ctx, external.Provider, external.ExternalID)
	switch {
	case err == nil && existing.UserID == userID:
		// Binding something already bound to the same account is a
		// no-op rather than an error: a double-submitted callback must
		// not look like a failure to the person who clicked once.
		return &SocialLoginResult{User: user, Identity: existing, Bound: true}, nil
	case err == nil:
		return nil, ErrIdentityAlreadyBound
	case !errors.Is(err, ErrNotFound):
		return nil, err
	}

	identity, err := s.createIdentity(ctx, userID, external)
	if err != nil {
		return nil, err
	}
	s.publishIdentityBound(ctx, identity, false)
	return &SocialLoginResult{User: user, Identity: identity, Bound: true}, nil
}

// signInWithExternalIdentity resolves external to an account and starts a
// session for it.
func (s *Service) signInWithExternalIdentity(ctx context.Context, external *ExternalIdentity, in SocialCallbackInput) (*SocialLoginResult, error) {
	result := &SocialLoginResult{}

	identity, err := s.identities.FindByExternal(ctx, external.Provider, external.ExternalID)
	switch {
	case err == nil:
		user, findErr := s.users.FindByID(ctx, identity.UserID)
		if findErr != nil {
			return nil, findErr
		}
		result.User, result.Identity = user, identity
	case errors.Is(err, ErrNotFound):
		user, created, linkErr := s.resolveSocialAccount(ctx, external)
		if linkErr != nil {
			return nil, linkErr
		}
		newIdentity, createErr := s.createIdentity(ctx, user.ID, external)
		if createErr != nil {
			return nil, createErr
		}
		result.User, result.Identity = user, newIdentity
		result.Created = created
		result.AutoLinked = !created
		s.publishIdentityBound(ctx, newIdentity, result.AutoLinked)
	default:
		return nil, err
	}

	if result.User.Status != UserStatusActive {
		return nil, ErrInvalidCredentials
	}

	tokens, err := s.startExternalSession(ctx, result.User, socialAMR(external.Provider), MethodSocial, in.TenantID, in.Device, in.UserAgent, in.IP)
	if err != nil {
		return nil, err
	}
	result.Tokens = tokens

	if touchErr := s.identities.TouchLogin(ctx, result.Identity, s.now()); touchErr != nil {
		obs.FromContext(ctx).Warn("social identity last-login could not be recorded",
			"provider", result.Identity.Provider, "user_id", result.User.ID, "error", touchErr)
	}
	return result, nil
}

// resolveSocialAccount decides which account an unrecognised external
// identity belongs to, and is where docs/internal/05's first social-login
// security rule is enforced.
//
// The rule: an existing account may be linked to a new external identity
// automatically ONLY when the provider asserts it verified the address AND
// the platform has put that provider on its trusted list. Anything else means
// the person must sign in the way they already can and bind the identity from
// their settings page.
//
// Why both conditions and not just the first: "verified" is the provider's
// word, and the provider is a third party a deployment chose to accept logins
// from -- not one it audited. A provider that will hand out an account with
// somebody else's address, verified or not, hands out that person's account
// here too. The trusted list is where a deployment records which providers it
// is actually willing to stake its accounts on.
//
// The third case is subtler and is the reason this function creates an
// account with NO email rather than with an unverified one: storing an
// unverified third-party address in the unique email index would let anybody
// squat the address of a user who has not registered yet, and would then let a
// later, genuinely verified login link straight into that squatted account.
// The address is kept on the identity row, where it is display data.
func (s *Service) resolveSocialAccount(ctx context.Context, external *ExternalIdentity) (*User, bool, error) {
	email := strings.TrimSpace(external.Email)
	linkable := email != "" && external.EmailVerified && s.providerIsTrusted(external.Provider)

	if email != "" {
		existing, err := s.users.FindByEmail(ctx, email)
		switch {
		case err == nil && linkable:
			return existing, false, nil
		case err == nil:
			obs.FromContext(ctx).Info("social sign-in refused an automatic account link",
				"provider", external.Provider,
				"user_id", existing.ID,
				"email_verified", external.EmailVerified,
				"provider_trusted", s.providerIsTrusted(external.Provider),
			)
			return nil, false, ErrIdentityRequiresBinding
		case !errors.Is(err, ErrNotFound) && !hasCode(err, ErrInvalidEmail.Code):
			return nil, false, err
		}
	}

	user := &User{
		DisplayName: external.Name,
		Status:      UserStatusActive,
	}
	if linkable {
		user.Email = email
		user.EmailVerified = true
	}
	if err := s.users.Create(ctx, user); err != nil {
		return nil, false, err
	}

	s.publish(ctx, pkgcore.Event{
		Type: EventUserCreated,
		Payload: UserCreatedPayload{
			UserID:   user.ID,
			HasEmail: user.Email != "",
			HasPhone: false,
		},
	})
	return user, true, nil
}

// createIdentity inserts the external identity row for userID.
func (s *Service) createIdentity(ctx context.Context, userID string, external *ExternalIdentity) (*UserIdentity, error) {
	identity := &UserIdentity{
		UserID:      userID,
		Provider:    external.Provider,
		ExternalID:  external.ExternalID,
		Email:       strings.TrimSpace(external.Email),
		DisplayName: external.Name,
		AvatarURL:   external.Avatar,
	}
	if err := s.identities.Create(ctx, identity); err != nil {
		return nil, err
	}
	return identity, nil
}

// startExternalSession starts a session for a user authenticated by something
// other than a password, and records the attempt in the login history.
func (s *Service) startExternalSession(
	ctx context.Context,
	user *User,
	amr []string,
	method string,
	requested pkgcore.TenantID,
	device, userAgent, ip string,
) (*TokenPair, error) {
	tenantID, err := s.resolveTenant(ctx, user.ID, requested)
	if err != nil {
		s.record(ctx, &LoginAttempt{
			UserID:        user.ID,
			Method:        method,
			Result:        LoginResultFailure,
			FailureReason: FailureReasonNoMembership,
			IP:            ip,
			UserAgent:     userAgent,
			CreatedAt:     s.now(),
		})
		return nil, err
	}

	session, issued, err := s.sessions.Start(ctx, StartSessionInput{
		UserID:    user.ID,
		TenantID:  tenantID,
		AMR:       amr,
		Device:    device,
		UserAgent: userAgent,
		IP:        ip,
	})
	if err != nil {
		return nil, err
	}

	pair, err := s.mintPair(user, session, tenantID, issued)
	if err != nil {
		return nil, err
	}

	s.record(ctx, &LoginAttempt{
		UserID:    user.ID,
		Method:    method,
		Result:    LoginResultSuccess,
		SessionID: session.ID,
		IP:        ip,
		UserAgent: userAgent,
		CreatedAt: s.now(),
	})
	s.publish(ctx, pkgcore.Event{
		Type:     EventUserLoggedIn,
		TenantID: tenantID,
		Payload: UserLoggedInPayload{
			UserID:    user.ID,
			SessionID: session.ID,
			TenantID:  string(tenantID),
			Method:    method,
			AMR:       amr,
			IP:        ip,
		},
	})
	return pair, nil
}

// ListIdentities returns a user's bound external identities.
func (s *Service) ListIdentities(ctx context.Context, userID string) ([]UserIdentity, error) {
	if userID == "" {
		return nil, ErrAuthenticationRequired
	}
	return s.identities.ListByUser(ctx, userID)
}

// UnbindIdentity detaches an external identity from its owner.
//
// It refuses when the removal would leave the account with no way in at all
// (docs/internal/05's fourth social-login rule). The refusal matters because
// there is no self-service recovery from the state it prevents: an account
// with no password, no verified phone number and no remaining identity cannot
// be signed in to, and cannot prove ownership to have one restored.
func (s *Service) UnbindIdentity(ctx context.Context, userID, identityID string) error {
	if userID == "" {
		return ErrAuthenticationRequired
	}
	identity, err := s.identities.FindByID(ctx, identityID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrIdentityNotFound
		}
		return err
	}
	// Somebody else's identity gets the same answer as one that does not
	// exist, so the endpoint never confirms that a binding is real.
	if identity.UserID != userID {
		return ErrIdentityNotFound
	}

	user, err := s.users.FindByID(ctx, userID)
	if err != nil {
		return err
	}
	identities, err := s.identities.ListByUser(ctx, userID)
	if err != nil {
		return err
	}
	if LoginMethodCount(user, len(identities)) <= 1 {
		return ErrLastLoginMethod
	}

	removed, err := s.identities.Delete(ctx, userID, identityID)
	if err != nil {
		return err
	}
	if !removed {
		return ErrIdentityNotFound
	}

	s.publish(ctx, pkgcore.Event{
		Type: EventIdentityUnbound,
		Payload: IdentityUnboundPayload{
			UserID:     userID,
			IdentityID: identity.ID,
			Provider:   identity.Provider,
		},
	})
	return nil
}

// LoginMethodCount reports how many distinct ways user can currently sign in.
//
// A password counts once. A VERIFIED phone number counts once -- an
// unverified one does not, because the code-based sign-in it would enable
// refuses to send to an unverified number in the first place. Each bound
// external identity counts once. The email address deliberately does not
// count on its own: it is an identifier for the password method, not a method.
func LoginMethodCount(user *User, identityCount int) int {
	if user == nil {
		return 0
	}
	count := identityCount
	if user.PasswordHash != "" {
		count++
	}
	if user.Phone != "" && user.PhoneVerified {
		count++
	}
	return count
}

// socialProvider looks a channel up, returning ErrProviderUnknown with the
// requested name as a parameter.
func (s *Service) socialProvider(name string) (SocialProvider, error) {
	provider, ok := s.providers.Get(name)
	if !ok {
		return nil, ErrProviderUnknown.WithParam("provider", name)
	}
	return provider, nil
}

// providerIsTrusted reports whether the platform has put name on the list of
// providers whose verified-email assertion may automatically link an existing
// account.
func (s *Service) providerIsTrusted(name string) bool {
	return slices.Contains(s.trustedProviders, name)
}

// publishIdentityBound announces a new binding.
func (s *Service) publishIdentityBound(ctx context.Context, identity *UserIdentity, autoLinked bool) {
	s.publish(ctx, pkgcore.Event{
		Type: EventIdentityBound,
		Payload: IdentityBoundPayload{
			UserID:     identity.UserID,
			IdentityID: identity.ID,
			Provider:   identity.Provider,
			AutoLinked: autoLinked,
		},
	})
}

// socialAMR is the authentication-method list a social sign-in establishes,
// following the OpenID Connect "amr" convention of a namespaced token.
func socialAMR(provider string) []string {
	return []string{fmt.Sprintf("%s:%s", MethodSocial, provider)}
}
