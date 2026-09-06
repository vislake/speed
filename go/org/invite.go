package org

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/i18n"
	"github.com/vislake/speed/go/ratelimit"
)

// The rate-limit budget InviteService applies to every invitation.
//
// Two independent dimensions, composed by this package the way
// ratelimit.Limiter's contract requires -- one Allow call per dimension, any
// single denial denying the request. The limiter itself has no notion of
// combining dimensions on purpose; see its own doc comment.
//
// The numbers are package constants rather than dynamic configuration items
// because org cannot read a dynamic config value without importing the config
// module, which the dependency graph forbids. Declaring a schema this module
// would then ignore would be worse than a constant: it would be a lying
// schema. See go/org/AGENTS.md.
const (
	// invitesPerTenantRate bounds how many invitations one tenant may send
	// per invitesPerTenantWindow. It is the blast radius of a compromised
	// member account.
	invitesPerTenantRate   = 50
	invitesPerTenantWindow = time.Hour

	// invitesPerEmailRate bounds how often one address may be invited within
	// invitesPerEmailWindow. This is the dimension that matters for the
	// recipient: an unverified address may only be written to as part of the
	// consent-establishing message, and a handful of those a day is the line
	// between an invitation and harassment.
	invitesPerEmailRate   = 3
	invitesPerEmailWindow = 24 * time.Hour
)

// defaultInvitationTTL is how long an invitation stays acceptable unless the
// host overrides it with WithInvitationTTL.
const defaultInvitationTTL = 7 * 24 * time.Hour

// InviteRequest is one invitation to issue.
//
// It carries no tenant: the tenant comes from the context, and there is no
// field here through which a caller could name one.
type InviteRequest struct {
	// Email is the invitee's address. It is normalized by
	// dbkit.NormalizeEmail before anything is stored, sent or counted.
	Email string

	// NodeID is the node the invitee will be bound to on acceptance. It must
	// belong to the caller's tenant.
	NodeID string

	// InviterUserID is the member issuing the invitation, taken from the
	// authenticated caller by the transport layer -- never from a request
	// body.
	InviterUserID string

	// Locale is the language to render the invitation in, typically the
	// request's Accept-Language. A locale the catalog does not serve falls
	// back to the platform default; see negotiateLocale.
	Locale string
}

// InviteResult is what a successful Invite produces.
type InviteResult struct {
	// Invitation is the stored row. Its Email is the plaintext address as
	// given, decrypted in memory; it is safe to inspect and unsafe to log.
	Invitation *Invitation

	// Token is the invitation token, returned EXACTLY ONCE and stored
	// nowhere -- the row keeps only its hash. It exists so a host that
	// delivers the invitation through its own channel can, and so a test can
	// accept an invitation it just issued.
	//
	// It is a bearer credential: whoever holds it joins the tenant. It must
	// never be written into an API response body, a log line, an event
	// payload or a trace attribute. The only place it legitimately goes is
	// the message addressed to the invitee.
	Token string
}

// InviteService issues, delivers, withdraws and accepts invitations.
//
// # Why this module sends an email at all
//
// Business modules publish events and let notification decide what to send;
// the one exception the security rules carve out is a verification-class
// message, which may be delivered synchronously and directly, and which is
// the only message that may be addressed to an UNVERIFIED contact -- rate
// limited. The invitation is exactly that message. It is the single
// consent-establishing message org ever sends, the invitee is waiting for
// it, and org sends nothing else to somebody who never accepts: no
// reminders, no resends without a fresh explicit Invite call, nothing
// promotional.
//
// When the M2 notification module lands and subscribes to
// org.member.invited, the host turns the org.invitation_email flag off and
// this leg goes quiet without a code change.
type InviteService struct {
	repo    *InvitationRepository
	tree    *TreeService
	members *MemberService

	// indexer computes the blind index of an address. It is required: org
	// refuses to boot without one (ErrEmailIndexerRequired), because an
	// invitation whose address it cannot index is an invitation it can never
	// find again.
	indexer *dbkit.BlindIndexer

	// gate answers feature-flag questions for the tenant in context. Nil
	// means the flags' declared defaults apply.
	gate FeatureGate

	// host is the lazily-read view of the host's Registry -- catalog,
	// mailer, bus and KV store, each read when it is used and never captured
	// during Register.
	host hostSeams

	// limiter counts the two rate-limit dimensions. Nil means "build one
	// from the host's KVStore on first use", which is what production does;
	// tests inject their own.
	limiter ratelimit.Limiter

	// link builds the URL the invitee clicks. Required whenever the
	// invitation email is enabled.
	link InvitationLinkBuilder

	// from is the sender address of the invitation email. pkgcore.Mailer
	// rejects an empty From outright, so it is required on the same terms as
	// link.
	from string

	// emailEnabled is the org.invitation_email flag's declared default,
	// which WithInvitationEmailDisabled flips. A tenant override through
	// gate wins over it.
	emailEnabled bool

	// ttl is how long a new invitation stays acceptable.
	ttl time.Duration

	// now is the clock, a field so expiry is testable without sleeping.
	now func() time.Time

	// newID and newToken are the two entropy sources, fields for the same
	// reason: a test pins them, and production never has to.
	newID    func() string
	newToken func() (token, hash string, err error)
}

// NewInviteService returns an InviteService over db.
//
// indexer may be nil only in a host that never invites anybody;
// Module.Register refuses such a wiring with ErrEmailIndexerRequired, so a
// bootstrapped module always has one.
func NewInviteService(db *gorm.DB, tree *TreeService, members *MemberService, indexer *dbkit.BlindIndexer) *InviteService {
	return &InviteService{
		repo:         NewInvitationRepository(db),
		tree:         tree,
		members:      members,
		indexer:      indexer,
		emailEnabled: true,
		ttl:          defaultInvitationTTL,
		now:          time.Now,
		newID:        uuid.NewString,
		newToken:     newInvitationToken,
	}
}

// Repository returns the service's data-access type, for callers that need
// the promoted dbkit.Repository[Invitation] surface rather than an
// invitation operation.
func (s *InviteService) Repository() *InvitationRepository { return s.repo }

// Invite issues an invitation and, unless the invitation email is switched
// off, delivers it.
//
// The order of operations is deliberate:
//
//  1. the feature gate, so a tenant with invitations off cannot even spend
//     rate-limit budget;
//  2. the node, so an invitation can never point outside the caller's tenant;
//  3. the address, normalized and indexed, so an address org could not find
//     again is refused before anything is written;
//  4. both rate-limit dimensions;
//  5. any earlier pending invitation for the same address is revoked and the
//     new row inserted, atomically -- see InvitationRepository.createPending
//     -- so one address has at most one live token at a time and an older
//     link stops working the moment a new one is issued. A concurrent
//     Invite racing for the SAME address loses this step outright
//     (ErrInvitationAlreadyPending) rather than silently creating a second
//     live token: see createPending's own doc comment for the race this
//     closes and why it can only be caught here, at the insert, never by a
//     pre-check.
//  6. the event, then the message.
//
// A delivery failure revokes the invitation it just created. The invitee
// never received the link, so leaving a pending row behind would only mean a
// token nobody holds sitting acceptable for a week; the operator retries and
// gets a fresh one, and the revoked row keeps the attempt visible.
func (s *InviteService) Invite(ctx context.Context, req InviteRequest) (*InviteResult, error) {
	enabled, err := s.featureEnabled(ctx, FeatureInvitations, true)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrInvitationsDisabled
	}
	if s.indexer == nil {
		return nil, ErrEmailIndexerRequired
	}

	node, err := s.tree.Get(ctx, req.NodeID)
	if err != nil {
		return nil, err
	}
	// Structural validation first, then the index. dbkit.NormalizeEmail
	// normalizes without validating -- by design, see its doc comment -- so
	// without this an address that cannot possibly deliver would still index
	// cleanly and spend a rate-limit slot and a mail attempt.
	address, err := validateInviteEmail(req.Email)
	if err != nil {
		return nil, err
	}
	emailIndex, err := s.indexer.Index(address)
	if err != nil {
		// The address itself is never echoed: an error's parameters are
		// rendered, logged and traced, and an address is PII.
		return nil, ErrInvalidEmail.WithCause(err)
	}
	if limitErr := s.checkRateLimits(ctx, emailIndex); limitErr != nil {
		return nil, limitErr
	}

	token, tokenHash, err := s.newToken()
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	id := s.newID()
	if err := validateNodeID(id); err != nil {
		return nil, ErrInternal.WithCause(err)
	}

	invitation := &Invitation{
		ID:            id,
		NodeID:        node.ID,
		Email:         address,
		EmailIndex:    emailIndex,
		InviterUserID: req.InviterUserID,
		Locale:        s.negotiate(req.Locale),
		TokenHash:     tokenHash,
		Status:        InvitationStatusPending,
		ExpiresAt:     s.now().Add(s.ttl),
	}
	if err := s.repo.createPending(ctx, s.indexer, address, invitation); err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil, ErrInvitationAlreadyPending
		}
		return nil, ErrInternal.WithCause(err)
	}

	publishEvent(ctx, s.host, EventMemberInvited, MemberInvited{
		InvitationID:  invitation.ID,
		NodeID:        invitation.NodeID,
		EmailIndex:    invitation.EmailIndex,
		InviterUserID: invitation.InviterUserID,
	})

	if err := s.deliver(ctx, invitation, node.Name, token); err != nil {
		s.revokeAfterFailedDelivery(ctx, invitation)
		return nil, err
	}
	return &InviteResult{Invitation: invitation, Token: token}, nil
}

// Accept turns a token into a membership for userID.
//
// The tenant the invitation lives in comes from the token itself, resolved
// server-side, never from the caller's context, a parameter or a header:
//
//   - A caller whose context carries NO tenant -- a freshly invited person
//     has no membership in the inviting tenant yet and typically no bearer
//     token at all, which is exactly the caller acceptance must serve --
//     resolves the tenant through invitationTokenIndex, the narrow
//     non-tenant-scoped token_hash -> tenant_id table this module writes
//     alongside every invitation (InvitationRepository.createPending), and
//     then re-enters the ordinary tenant-scoped flow below with
//     pkgcore.WithTenant. This is go/sharing's AccessPublic shape: the
//     bearer token the invitee holds IS the credential (it was delivered to
//     the addressed mailbox alone), so no pre-existing tenant claim is
//     needed to use it -- and an invitation link may therefore point at any
//     host that reaches this service, not only the inviting tenant's own.
//   - A caller whose context DOES carry a tenant (a host that still runs
//     accept downstream of tenant-resolving middleware, or an internal
//     caller acting for a member of the inviting tenant) is served
//     byte-for-byte as before: the token is hashed and the row is looked up
//     tenant-scoped, so a token minted for another tenant reports
//     ErrInvitationNotFound and reveals nothing about it.
//
// Either way the invitation row is never read out of the tenant it belongs
// to, and "never accept a caller-supplied tenant id" holds absolutely.
//
// The membership creation is idempotent, so a user who somehow already has a
// seat keeps the one they have rather than gaining a second. Accepting the
// same invitation twice is nonetheless reported
// (ErrInvitationAlreadyAccepted): a replayed link is worth surfacing.
//
// # Single-use under concurrency
//
// Two different callers can present the same token at the same time -- the
// token can be forwarded or shared, or a client can fire the request twice.
// The status checks above this comment only rule out a token that was
// ALREADY terminal at the moment this call started; they do nothing about
// another Accept committing in between. The claim below
// (InvitationRepository.acceptIfPending) is what actually enforces
// single-use: it flips pending -> accepted in the database with a
// "status = 'pending'" compare-and-swap guard, so of any number of
// concurrent callers racing on this token, at most one observes won == true
// and only that one goes on to call members.ensure. A caller that loses the
// race creates no membership at all.
func (s *InviteService) Accept(ctx context.Context, token, userID string) (*Membership, error) {
	if userID == "" {
		return nil, ErrMembershipNotFound.WithParam("user_id", userID)
	}
	hash := hashInvitationToken(token)
	if _, ok := pkgcore.TenantFromContext(ctx); !ok {
		// A tenantless acceptor: resolve the invitation's own tenant from
		// the token and enter it. The resolved tenant replaces whatever the
		// context may otherwise carry -- pkgcore.WithTenant overwrites --
		// so the tenant-scoped flow below runs strictly inside the
		// invitation's tenant either way. See the method doc comment for
		// why this resolution is the one deliberately narrow platform-data
		// read org performs.
		tenant, err := s.repo.tenantForTokenHash(ctx, hash)
		if err != nil {
			return nil, err
		}
		ctx = pkgcore.WithTenant(ctx, tenant)
	}
	invitation, err := s.repo.byTokenHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	switch invitation.Status {
	case InvitationStatusAccepted:
		return nil, ErrInvitationAlreadyAccepted.WithParam("invitation_id", invitation.ID)
	case InvitationStatusRevoked:
		return nil, ErrInvitationRevoked.WithParam("invitation_id", invitation.ID)
	}
	if !s.now().Before(invitation.ExpiresAt) {
		return nil, ErrInvitationExpired.WithParam("invitation_id", invitation.ID)
	}

	// Claim the invitation atomically, in the database, BEFORE creating any
	// membership -- see the doc comment above for why the ordering here is
	// not negotiable.
	acceptedAt := s.now()
	won, err := s.repo.acceptIfPending(ctx, invitation.ID, acceptedAt)
	if err != nil {
		return nil, err
	}
	if !won {
		return nil, s.reportLostAcceptRace(ctx, token)
	}
	invitation.Status = InvitationStatusAccepted
	invitation.AcceptedAt = &acceptedAt

	membership, created, err := s.members.ensure(ctx, userID, invitation.NodeID)
	if err != nil {
		// This call won the claim above but failed to turn it into a
		// membership. The failure's own nature decides how the claim is
		// settled (settleFailedAccept's doc comment): when the very node
		// this invitation points into no longer exists the invitation can
		// never be fulfilled and ends REVOKED -- never back to pending,
		// which would leave a bearer token acceptable for the rest of its
		// TTL while every accept it admits fails identically. Only a
		// transient failure gives the claim back to pending, so a retry is
		// not permanently locked out. Either way the settle is a guarded
		// write over the accepted state this call won, and a settle failure
		// is logged, not returned: the caller is already receiving ensure's
		// own error, and reporting a second one would hide the first.
		s.settleFailedAccept(ctx, invitation, err)
		return nil, err
	}

	if created {
		publishEvent(ctx, s.host, EventMemberJoined, MemberJoined{
			MembershipID: membership.ID,
			UserID:       membership.UserID,
			NodeID:       membership.NodeID,
			InvitationID: invitation.ID,
		})
	}
	return membership, nil
}

// reportLostAcceptRace re-reads the invitation after this call's
// acceptIfPending lost the compare-and-swap race, and reports the terminal
// state a concurrent Accept (or Revoke) left it in -- the same reporting a
// caller who observed that terminal state directly, at the top of Accept,
// would have gotten.
func (s *InviteService) reportLostAcceptRace(ctx context.Context, token string) error {
	current, err := s.repo.byTokenHash(ctx, hashInvitationToken(token))
	if err != nil {
		return err
	}
	if current.Status == InvitationStatusRevoked {
		return ErrInvitationRevoked.WithParam("invitation_id", current.ID)
	}
	return ErrInvitationAlreadyAccepted.WithParam("invitation_id", current.ID)
}

// settleFailedAccept ends the claim Accept won on an invitation whose
// membership creation just failed. See Accept's own comment for why this is
// best-effort -- the caller is already receiving ensure's own error, so the
// settle's own outcome is logged, never returned.
//
// # The failure's nature decides the end state
//
//   - ensure answered the coded ErrNodeNotFound: the very node this
//     invitation would bind its invitee to no longer exists, so the
//     invitation can never be fulfilled. It ends REVOKED -- never back to
//     pending, which is the state the pre-fix code left it in: a bearer
//     token stayed acceptable for the rest of its TTL while every accept it
//     admitted failed identically, List kept showing an invitation that
//     could only fail, and a caller who observed the brief accepted
//     interlude was answered org.invitation_already_accepted though no
//     membership ever came of it. This is the P2-org-12 state change.
//   - any other failure is treated as the transient it is (a busy SQLite, a
//     lost insert race) and the claim is given back to pending, exactly as
//     the pre-fix code did, so a retry after the transient is not locked
//     out -- the revert Revoke's own lost-race handling presupposes.
//
// # The write is a guarded transition, never an unconditional overwrite
//
// Whichever end state the failure earns, the write itself is
// InvitationRepository.settleClaim's guarded transition: it executes only
// against a row still in the accepted state THIS call won, and touches only
// the Status and AcceptedAt columns. The pre-fix failure path (then named
// revertAcceptClaim) issued an unconditional, full-row Repository.Update of
// the caller's in-memory snapshot instead, which would stamp its stale view
// of every column over whatever state the row had moved to. A settle that
// errors, or that matches nothing (the accepted state this call won is
// already gone -- a concurrent writer moved the row first, and that
// writer's outcome must not be overwritten), is logged and left alone.
func (s *InviteService) settleFailedAccept(ctx context.Context, invitation *Invitation, cause error) {
	status := InvitationStatusPending
	if hasCode(cause, ErrNodeNotFound.Code) {
		status = InvitationStatusRevoked
	}
	won, err := s.repo.settleClaim(ctx, invitation.ID, status)
	switch {
	case err != nil:
		obs.FromContext(ctx).Warn("org could not settle an invitation claim after membership creation failed",
			"invitation_id", invitation.ID, "error", err)
	case !won:
		obs.FromContext(ctx).Warn("org could not settle an invitation claim: the accepted state the accept won was already gone",
			"invitation_id", invitation.ID)
	}
}

// Revoke withdraws a pending invitation, making its token useless. Revoking
// an already-accepted invitation reports ErrInvitationAlreadyAccepted rather
// than silently un-joining somebody: removing a member is
// MemberService.Remove's job, and it publishes a different event. Revoking
// an already-revoked invitation is a no-op success, so a redelivered revoke
// is safe.
//
// # The write is a compare-and-swap, never an unconditional overwrite
//
// The read above classifies the invitation's state at one instant; the
// write below must not trust that instant to still hold. Accept owns the
// identical transition in the other direction and gates it on the row still
// being pending (acceptIfPending), and Revoke gates its own the same way
// (revokeIfPending): between this method's read and its write, a concurrent
// Accept may have won the pending -> accepted claim and created the
// membership -- an unconditional Status = Revoked write landing after that
// would leave the invitation terminal-revoked while the membership stays
// live, the invitation row and the roster disagreeing about whether the
// invitee joined. If the compare-and-swap loses (won == false), the state
// was changed by a concurrent caller in between, so this method re-reads
// and classifies like Accept's own lost-race reporter does: an accepted
// invitation answers ErrInvitationAlreadyAccepted, a revoked one (another
// revoke won) is the idempotent success, and a row still pending means the
// winner was an Accept whose membership creation failed on a TRANSIENT
// error and settled its claim back to pending -- a permanent failure (the
// invitation's node is gone) settles the claim to revoked instead, which
// this re-read sees as the revoked case above. The pending-after-loss
// transient is what lets the revoke simply try its own claim again, for a
// bounded number of attempts, after which the coded ErrConcurrentUpdate
// reports that the invitation's status stayed in flux longer than this
// call's budget covers.
func (s *InviteService) Revoke(ctx context.Context, invitationID string) error {
	invitation, err := s.repo.FindByID(ctx, invitationID)
	if err != nil {
		if hasCode(err, dbkit.ErrRecordNotFound.Code) {
			return ErrInvitationNotFound.WithParam("invitation_id", invitationID)
		}
		return err
	}
	switch invitation.Status {
	case InvitationStatusAccepted:
		return ErrInvitationAlreadyAccepted.WithParam("invitation_id", invitationID)
	case InvitationStatusRevoked:
		return nil
	}

	const revokeAttemptBudget = 3
	for attempt := 0; ; attempt++ {
		won, casErr := s.repo.revokeIfPending(ctx, invitationID)
		if casErr != nil {
			return casErr
		}
		if won {
			return nil
		}
		// Lost the race: re-read to learn the state a concurrent caller
		// committed. Accept's lost-race reporter re-reads once and classifies;
		// this loop exists only for the single narrow case where the row is
		// STILL pending after the loss (an accept claimed it, failed to
		// create the membership on a transient error, and settled its claim
		// back to pending -- a permanent failure settles the claim to
		// revoked instead, which the re-read's switch above already reports
		// as the idempotent success) -- then the revoke's own claim is worth
		// trying again, briefly.
		current, findErr := s.repo.FindByID(ctx, invitationID)
		if findErr != nil {
			return findErr
		}
		switch current.Status {
		case InvitationStatusAccepted:
			return ErrInvitationAlreadyAccepted.WithParam("invitation_id", invitationID)
		case InvitationStatusRevoked:
			return nil
		}
		if attempt+1 >= revokeAttemptBudget {
			return ErrConcurrentUpdate.WithParam("invitation_id", invitationID)
		}
	}
}

// List returns the caller tenant's pending invitations, newest first.
//
// Expired-but-unaccepted rows are still Pending in the database -- expiry is
// evaluated at acceptance time, not by a sweeper -- so the caller sees them
// and can tell from ExpiresAt that they are stale. Hiding them would make an
// invitation that was sent look like one that never was.
func (s *InviteService) List(ctx context.Context) ([]Invitation, error) {
	return s.repo.byStatus(ctx, InvitationStatusPending)
}

// featureEnabled asks the host's feature gate about key, falling back to the
// flag's declared default when no gate is wired.
//
// A gate error is propagated rather than swallowed: an unavailable gate must
// not silently become "enabled" for a flag that guards outbound messages.
func (s *InviteService) featureEnabled(ctx context.Context, key string, fallback bool) (bool, error) {
	if s.gate == nil {
		return fallback, nil
	}
	enabled, err := s.gate.IsEnabled(ctx, key)
	if err != nil {
		return false, ErrInternal.WithCause(err)
	}
	return enabled, nil
}

// checkRateLimits applies both dimensions, tenant first. Either denial
// reports ErrInvitationRateLimited with the dimension that denied it, so an
// operator can tell "this tenant is sending too much" from "this person is
// being invited too often" without the error carrying the address.
//
// The per-address key is built from the BLIND INDEX, never the address
// itself: a rate-limit key lives in the KV store, is visible in Redis and
// tends to appear in diagnostics, and an email address in one of those is a
// PII leak.
func (s *InviteService) checkRateLimits(ctx context.Context, emailIndex string) error {
	tenant, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	limiter, err := s.rateLimiter()
	if err != nil {
		return err
	}

	dimensions := []struct {
		name  string
		key   string
		limit ratelimit.Limit
	}{
		{"tenant", "org:invite:tenant:" + string(tenant), ratelimit.Limit{Rate: invitesPerTenantRate, Per: invitesPerTenantWindow}},
		{"email", "org:invite:email:" + emailIndex, ratelimit.Limit{Rate: invitesPerEmailRate, Per: invitesPerEmailWindow}},
	}
	for _, d := range dimensions {
		decision, err := limiter.Allow(ctx, d.key, d.limit)
		if err != nil {
			return ErrInternal.WithCause(err)
		}
		if !decision.Allowed {
			return ErrInvitationRateLimited.
				WithParam("dimension", d.name).
				WithParam("retry_after_seconds", int(decision.ResetAfter.Seconds()))
		}
	}
	return nil
}

// rateLimiter returns the injected limiter, or builds one over the host's
// KVStore. Building it here rather than at construction is what keeps the
// rule that host seams are read at call time.
func (s *InviteService) rateLimiter() (ratelimit.Limiter, error) {
	if s.limiter != nil {
		return s.limiter, nil
	}
	if s.host == nil {
		return nil, ErrInternal.WithCause(errNoKVStore)
	}
	kv := s.host.KVStore()
	if kv == nil {
		return nil, ErrInternal.WithCause(errNoKVStore)
	}
	return ratelimit.New(kv), nil
}

// deliver renders and sends the invitation, unless the invitation email is
// switched off for this tenant -- in which case the invitation still exists
// and org.member.invited has already been published, which is exactly the
// arrangement a notification module subscribes into.
func (s *InviteService) deliver(ctx context.Context, invitation *Invitation, nodeName, token string) error {
	enabled, err := s.featureEnabled(ctx, FeatureInvitationEmail, s.emailEnabled)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	if s.link == nil || s.from == "" {
		return ErrInvitationMailRequired
	}

	acceptURL, err := s.link(ctx, token)
	if err != nil {
		return ErrInternal.WithCause(err)
	}
	mail, err := renderInvitationMail(s.catalog(), s.from, invitationMailData{
		to:        invitation.Email,
		nodeName:  nodeName,
		acceptURL: acceptURL,
		locale:    invitation.Locale,
	})
	if err != nil {
		return err
	}
	if err := sendMail(ctx, s.host, mail); err != nil {
		return err
	}

	// Neither the address nor the token is logged: the blind index
	// identifies the recipient for support purposes without being one.
	obs.FromContext(ctx).Info("org invitation sent",
		"invitation_id", invitation.ID,
		"node_id", invitation.NodeID,
		"email_index", invitation.EmailIndex,
		"locale", invitation.Locale,
	)
	return nil
}

// revokeAfterFailedDelivery withdraws an invitation whose message could not
// be delivered. The write is revokeIfPending's compare-and-swap, never an
// unconditional overwrite, for the same reason Revoke's own write is (see
// Revoke's doc comment): this call's own token was handed to the caller of
// Invite, so a host that passes it on can have an Accept in flight while the
// delivery-failure revocation runs -- and a revoke stamping over a claim
// that already produced a live membership would leave the invitation row and
// the roster disagreeing. A lost CAS means the accept won and is left alone.
// A failure to revoke is logged, not returned: the caller is already
// receiving the delivery error, and reporting the second one would hide the
// first.
func (s *InviteService) revokeAfterFailedDelivery(ctx context.Context, invitation *Invitation) {
	if _, err := s.repo.revokeIfPending(ctx, invitation.ID); err != nil {
		obs.FromContext(ctx).Warn("org could not revoke an undelivered invitation",
			"invitation_id", invitation.ID, "error", err)
	}
}

// negotiate resolves the requested locale against the catalog the host
// carries, read at call time.
func (s *InviteService) negotiate(requested string) string {
	return negotiateLocale(s.catalog(), requested)
}

// catalog reads the merged message catalog from the host seam. It is a
// method, not a field, for the reason hostSeams documents at length:
// Registry.Locales() is nil during Register, so a captured catalog is a nil
// catalog.
func (s *InviteService) catalog() *i18n.Catalog {
	if s.host == nil {
		return nil
	}
	return s.host.Locales()
}
