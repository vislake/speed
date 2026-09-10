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
	"github.com/vislake/speed/go/pkgcore/apperr"

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
	// person, refreshed on every sign-in through this identity. Both are
	// third-party strings landing in fixed-width columns, so they are
	// bounded to their columns' VARCHAR widths at the repository write
	// boundary (Create and TouchLogin; see model.go's column-width doc
	// comment for the per-column decision), exactly like ExternalID.
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
//
// ExternalID, DisplayName and AvatarURL are provider-reported (see
// ExternalIdentity's doc comment: every field of it is untrusted input from
// a third party), so they are bounded to their columns' VARCHAR widths
// before the insert -- the same repository write boundary that bounds the
// client-supplied session diagnostics (SessionRepository.Create and
// truncateToColumnWidth), where the two dialects are made to agree:
// PostgreSQL enforces a declared width (SQLSTATE 22001) where SQLite would
// silently store the over-width value. The bounds per column, and why each
// of these is cut rather than refusing the sign-in, are model.go's
// column-width doc comment. DisplayName and AvatarURL are display data, so
// cutting them loses only the provider's excess; ExternalID is also a lookup
// key, so it is bounded here, at the write, and FindByExternal applies the
// same bound to its lookup argument -- a stored value and a lookup key can
// only meet when both are in the bounded form. (Two genuinely distinct
// provider accounts whose identifiers share the first identityExternalIDWidth
// runes would collide on the unique index; no real provider issues subjects
// anywhere near the width, and the alternative -- refusing the sign-in --
// breaks a login that PostgreSQL alone would have broken.)
func (r *UserIdentityRepository) Create(ctx context.Context, identity *UserIdentity) error {
	if identity.ID == "" {
		identity.ID = newID()
	}
	identity.ExternalID = truncateToColumnWidth(identity.ExternalID, identityExternalIDWidth)
	identity.DisplayName = truncateToColumnWidth(identity.DisplayName, identityDisplayNameWidth)
	identity.AvatarURL = truncateToColumnWidth(identity.AvatarURL, identityAvatarURLWidth)
	return r.db.WithContext(ctx).Create(identity).Error
}

// FindByExternal returns the identity registered for (provider, externalID),
// or ErrNotFound.
//
// The externalID argument is bounded to external_id's column width before
// the comparison: Create stores the bounded form, so a lookup must compare
// in the bounded form or an over-width provider identifier could never find
// the row it created (and a second sign-in would mint a duplicate account
// instead of logging the person in).
func (r *UserIdentityRepository) FindByExternal(ctx context.Context, provider, externalID string) (*UserIdentity, error) {
	var identity UserIdentity
	err := r.db.WithContext(ctx).
		Where("provider = ? AND external_id = ?", provider, truncateToColumnWidth(externalID, identityExternalIDWidth)).
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
// display fields the provider reported. The refreshed values are whatever
// the passed identity carries -- the caller is expected to have merged the
// claims the sign-in just received into it (see signInWithExternalIdentity
// and SSOService.signIn).
//
// The write is a column-level update of exactly that reported set, and the
// column-level form is the point: GORM's plain Updates(struct) silently
// skips every zero-valued field, so an address or display field the
// provider STOPPED reporting (an email withdrawn from the profile, a
// cleared avatar) would stay stale forever. Selecting the reported columns
// explicitly makes their zero values take effect and clears the stored
// value, so the identity converges on what the provider most recently
// reported. The columns are display data only -- this update is never a
// lookup key and never touches the user row.
//
// The two provider-reported strings are bounded to their columns' VARCHAR
// widths before the write, exactly as Create bounds them (model.go's
// column-width doc comment): a provider whose profile LATER grows past a
// column width must not break an existing identity -- the very login that
// refreshed it would fail on PostgreSQL with SQLSTATE 22001 while SQLite
// stored the grown value. The bound is applied to the passed struct itself,
// so the caller's row matches what was stored.
func (r *UserIdentityRepository) TouchLogin(ctx context.Context, identity *UserIdentity, at time.Time) error {
	identity.LastLoginAt = &at
	identity.DisplayName = truncateToColumnWidth(identity.DisplayName, identityDisplayNameWidth)
	identity.AvatarURL = truncateToColumnWidth(identity.AvatarURL, identityAvatarURLWidth)
	// The update stays on the struct path (with the reported columns
	// explicitly Selected, which is what admits their zero values): that
	// is the path that routes the email column through its at-rest
	// serializer, where a map-keyed Updates would write plaintext.
	return r.db.WithContext(ctx).
		Model(&UserIdentity{}).
		Select("display_name", "avatar_url", "email", "last_login_at", "updated_at").
		Where("id = ?", identity.ID).
		Updates(&UserIdentity{
			DisplayName: identity.DisplayName,
			AvatarURL:   identity.AvatarURL,
			Email:       identity.Email,
			LastLoginAt: &at,
		}).Error
}

// DeleteUnlessLastLoginMethod removes an identity that belongs to userID,
// but only when at least one other login method would remain afterwards, and
// reports whether it removed anything. It is the atomic form of the count
// guard Service.UnbindIdentity applies before calling it.
//
// The atomicity is the point of this method's existence, not an incidental:
// the service-level guard reads the user row and the identity list, then
// deletes -- and a caller that lost the race to a CONCURRENT unbind of a
// DIFFERENT identity can pass that stale read and delete the account's last
// remaining method. Two unbinds never delete the same row, so a plain delete
// gives them nothing to race on. This method closes the race with the one
// write target every unbind of the same user shares: after deleting, the
// transaction takes a write lock on the user's own row (an UPDATE to
// updated_at, the dialect-neutral equivalent of SELECT ... FOR UPDATE the
// module does not reach for -- see RefreshTokenRepository.Consume's doc
// comment) and only then re-derives the remaining method count. The first
// unbind to take the user-row lock commits; every later one's re-count runs
// against that commit and refuses when nothing would remain. The scoping of
// the delete to the owner in the WHERE clause, rather than checking
// ownership beforehand, is what makes "remove somebody else's binding"
// impossible even under a concurrent transfer.
//
// Results: (true, nil) when the row was removed and at least one login
// method remains; (false, nil) when no row matched (absent, or not owned);
// (false, ErrLastLoginMethod) when the removal was refused because it would
// leave the account with no way in -- the transaction is rolled back, so
// nothing was removed.
func (r *UserIdentityRepository) DeleteUnlessLastLoginMethod(ctx context.Context, userID, identityID string, at time.Time) (bool, error) {
	removed := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Where("id = ? AND user_id = ?", identityID, userID).
			Delete(&UserIdentity{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			// Nothing to remove: absent, or somebody else's. Commit the
			// no-op transaction (nothing was written yet).
			return nil
		}
		removed = true

		// Serialize concurrent unbinds of this user on the one row all of
		// them touch. See the doc comment for why the re-count below is
		// only sound once this write has been won.
		if err := tx.Model(&User{}).Where("id = ?", userID).
			Update("updated_at", at).Error; err != nil {
			return err
		}

		// The rows that remain ARE the re-derived count: LoginMethodCount
		// credits every bound identity exactly once, so reading the list
		// is the count -- the same derivation Service.UnbindIdentity runs
		// against ListByUser -- in the module's model-argument shape
		// rather than a COUNT(*) that would need a raw table anchor.
		var remaining []UserIdentity
		if err := tx.Where("user_id = ?", userID).Find(&remaining).Error; err != nil {
			return err
		}
		var user User
		if err := tx.Where("id = ?", userID).First(&user).Error; err != nil {
			return err
		}
		if LoginMethodCount(&user, len(remaining)) < 1 {
			// This deletion would leave the account with no way in at all.
			// Rolling the transaction back undoes the delete above.
			return ErrLastLoginMethod
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return removed, nil
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
	// The channel gate runs before anything is resolved or spent: an
	// unknown provider is not a gated channel (and keeps answering
	// ErrProviderUnknown below), but a KNOWN channel the deployment
	// turned off must not even mint a state value or send the browser to
	// the provider -- whether the request is a sign-in or a bind by an
	// already-signed-in user.
	if flag := socialChannelFlag(in.Provider); flag != "" {
		if err := s.channelEnabled(ctx, flag); err != nil {
			return "", err
		}
	}
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
	// The channel gate applies to the callback as well as the authorize
	// step: a flow that started while the channel was on must not complete
	// as a sign-in after an operator turned the channel off mid-flight.
	// An unknown provider names no flag and keeps answering
	// ErrProviderUnknown below.
	if flag := socialChannelFlag(in.Provider); flag != "" {
		if err := s.channelEnabled(ctx, flag); err != nil {
			return nil, err
		}
	}
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
		// Merge what THIS sign-in's provider just reported onto the stored
		// identity before TouchLogin below persists it -- the refresh
		// "refreshed on every sign-in" promises (UserIdentity's doc
		// comment). A field the provider no longer reports arrives as empty
		// and TouchLogin's column-level write clears the stored value
		// rather than leaving it stale.
		identity.Email = strings.TrimSpace(external.Email)
		identity.DisplayName = external.Name
		identity.AvatarURL = external.Avatar
		result.User, result.Identity = user, identity
	case errors.Is(err, ErrNotFound):
		user, created, linkErr := s.resolveSocialAccount(ctx, external, in.IP, in.UserAgent)
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
// identity belongs to. It enforces the module's automatic-link rule for the
// social channels: an existing account may be linked to a new external
// identity automatically ONLY when the provider asserts it verified the
// address AND the platform has put that provider on its trusted list.
// Anything else means the person must sign in the way they already can and
// bind the identity from their settings page.
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
func (s *Service) resolveSocialAccount(
	ctx context.Context,
	external *ExternalIdentity,
	ip, userAgent string,
) (*User, bool, error) {
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
			// The refusal is the account's own security signal -- an
			// external identity claimed this account's address and was
			// refused -- and belongs in its login history like every
			// other failed sign-in, not only in the log line above.
			s.recordRequiresBinding(ctx, MethodSocial, existing.ID, email, ip, userAgent)
			return nil, false, ErrIdentityRequiresBinding
		case !errors.Is(err, ErrNotFound) && !apperr.HasCode(err, ErrInvalidEmail.Code):
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

	// The mint is pre-tenant, exactly like Service.Register's own account
	// creation: a social sign-in resolves no tenant before the account
	// exists, and the event announcing it must carry no tenant whatever
	// the callback's context holds (a host composition may have resolved
	// the caller's own bearer even on an allowlisted pre-auth route -- see
	// publishTenantless's doc comment). A tenant on this event would seat
	// the account in that caller tenant (org's handleUserCreated) instead
	// of leaving the host's tenant-less provisioning to give the account
	// its own workspace.
	s.publishTenantless(ctx, pkgcore.Event{
		Type: EventUserCreated,
		Payload: UserCreatedPayload{
			UserID:   user.ID,
			HasEmail: user.Email != "",
			HasPhone: false,
		},
	})
	return user, true, nil
}

// recordRequiresBinding leaves the login-history row a sign-in refused with
// ErrIdentityRequiresBinding must leave. The refusal is a failed attempt
// like any other, and the account an external identity resolved to by
// address is entitled to see "an identity claimed this address and was
// refused" on its own security page -- the same signal the password
// channel's failure records give their account. userID is that resolved
// account, or empty for a refusal that happened before any account lookup
// (oidc.go's unverified-claim refusal, which must not disclose whether the
// claimed address is registered, and which therefore leaves an anonymous
// row exactly like an attempt against an identifier that matched no
// account). claimedEmail is never stored: only its blind index is, which is
// all login attempts ever carry of an identifier.
func (s *Service) recordRequiresBinding(ctx context.Context, method, userID, claimedEmail, ip, userAgent string) {
	index := ""
	if claimedEmail != "" {
		if value, err := s.users.EmailIndexOf(claimedEmail); err == nil {
			index = value
		}
	}
	s.record(ctx, &LoginAttempt{
		UserID:          userID,
		IdentifierIndex: index,
		Method:          method,
		Result:          LoginResultFailure,
		FailureReason:   FailureReasonRequiresBinding,
		IP:              ip,
		UserAgent:       userAgent,
		CreatedAt:       s.now(),
	})
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

	pair, err := s.mintPair(ctx, user, session, tenantID, issued)
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
// It refuses when the removal would leave the account with no way in at all.
// The refusal matters because there is no self-service recovery from the
// state it prevents: an account with no password, no verified phone number
// and no remaining identity cannot be signed in to, and cannot prove
// ownership to have one restored.
//
// The count guard above is deliberately applied twice. The read-then-delete
// check here answers the ordinary sequential case cheaply, but it is stale
// the moment two unbinds of one account race: each can read the full count
// before either deletes. The authoritative guard is the delete itself --
// DeleteUnlessLastLoginMethod re-derives the remaining method count inside
// its own transaction, so the account can never land at zero login methods
// however the requests interleave.
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

	removed, err := s.identities.DeleteUnlessLastLoginMethod(ctx, userID, identityID, s.now())
	if err != nil {
		return err
	}
	if !removed {
		// The row was gone by the time the guarded delete ran -- an
		// already-completed unbind of the same identity. Same answer as
		// never having existed, so the endpoint never confirms that a
		// binding was (or is) real.
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

// socialChannelFlag maps a wired social channel's name to the feature flag
// that gates it. The five flags this module declares (FeatureFlagSocial*)
// correspond one-to-one with the five providers it ships constructors for;
// a host-registered channel with any other name has no declared flag, and
// the empty string says so -- such a channel is not part of the declared
// flag universe and is not gated (it cannot be: there is no flag for an
// operator to have turned off).
func socialChannelFlag(provider string) string {
	switch provider {
	case ProviderGoogle:
		return FeatureFlagSocialGoogle
	case ProviderGitHub:
		return FeatureFlagSocialGitHub
	case ProviderWeChat:
		return FeatureFlagSocialWeChat
	case ProviderDingTalk:
		return FeatureFlagSocialDingTalk
	case ProviderFeishu:
		return FeatureFlagSocialFeishu
	default:
		return ""
	}
}

// providerIsTrusted reports whether the platform has put name on the list of
// providers whose verified-email assertion may automatically link an existing
// account.
func (s *Service) providerIsTrusted(name string) bool {
	return slices.Contains(s.trustedProviders, name)
}

// publishIdentityBound announces a new binding.
//
// The event is an ACCOUNT-level fact, deliberately tenant-less whatever the
// context holds (see publishTenantless's doc comment): the identity rows it
// announces are platform data, and its three call sites are pre-tenant --
// a bind completing at an unauthenticated social callback (whose audit row
// is recorded tenant-less for the same reason, handler.go's recordAudit
// call site), an auto-link during a social sign-in before any tenant
// resolved, and an enterprise-SSO sign-in's bind, whose tenant-carrying
// twin is the EventUserCreated the SSO mint publishes with the SSO
// tenant's own TenantID declared. A bound identity never provisions
// anything tenant-scoped, so no subscriber should ever read a tenant off
// this event.
func (s *Service) publishIdentityBound(ctx context.Context, identity *UserIdentity, autoLinked bool) {
	s.publishTenantless(ctx, pkgcore.Event{
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
