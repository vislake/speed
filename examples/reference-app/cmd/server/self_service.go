package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/i18n"
	"github.com/vislake/speed/go/rbac"
)

// self_service.go closes the reference app's self-service signup chain:
// registration used to end in a dead end -- the account was real, its
// sign-in answered 403 authn.tenant_membership_required ("the account has
// no organization yet"), and nothing the product offered could change
// that state (the acceptance finding this file closes). Under the product
// decision this host now implements, registration CREATES the clinic:
// every self-registered account is provisioned, synchronously with its
// registration, a tenant of its own with an org tree root, its
// membership of that root, and the built-in owner role -- the account a
// registration returns is an account that can sign in and use the
// product, exactly what org's own authn.user.created subscription gives a
// user whose creation event carries a tenant, plus the tenant itself and
// the owner grant the "what may this user do" half of the product needs.
//
// The provisioning follows the same assembly shapes the demo seeding
// already established (demo_users.go's addDemoOrgMembership and
// grantDemoSeedAccount): org's TreeService and MemberService under the
// tenant's own context, rbac's EnsureBuiltinRoles and AssignRole for the
// grant, never a raw write and never a cross-module struct import. The
// tenant id is DERIVED from the registrant's user id rather than minted
// (clinicTenantOf), which is what makes the whole chain safe to repeat:
// an at-least-once bus redelivering the event, or two replicas of a
// distributed composition both consuming it, converge on the SAME tenant
// and the SAME idempotent org/rbac writes (org's own handleUserCreated
// exists for exactly this redelivery reason, go/org/events.go) instead of
// fabricating a second clinic for one account.
//
// # Where the chain hooks: a host subscription, installed after the seeds
//
// The register route is authn's own handler and this host does not
// intercept it. Instead the chain hooks the one fact authn already
// publishes about every registration -- EventUserCreated, whose payload
// carries the new user id and nothing else (authn.UserCreatedPayload
// deliberately carries no personal data). The subscription is installed
// on the app's own bus (reg.Events, the same bus every module subscribed
// to during Bootstrap) AFTER the boot-time demo seeds have run their
// registrations, which is the discriminator that keeps the demo path
// byte-identical: an event published by seedDemoUsers' or
// seedDemoPlatformStaff's own register POSTs (registration happens inside
// buildServer, before the subscription exists) provisions nothing, while
// every registration that arrives once the server is up -- a browser's,
// or a flow test's -- provisions. On the in-process bus the provisioning
// runs synchronously inside the register request itself, before its 201
// answer leaves: an account whose registration answered 201 is an account
// whose clinic is already there, which is what makes "register, then sign
// in" a journey with no gap to race.
//
// # The two-path split with org's own subscriber
//
// org's own handleUserCreated (go/org/events.go) gives a brand-new user a
// workspace when the event carries a tenant, and deliberately skips the
// tenant-less event of a self-registering user (its own doc comment names
// "the tenant-creating path" as an explicit host call). This
// subscription is that call: it provisions only tenant-less events (the
// self-service shape every register route publishes) and leaves
// tenant-bearing events to org's own subscriber, so the two auto paths
// can never both provision one account.
//
// # Failure semantics
//
// The subscriber never returns an error: on the in-memory bus a handler
// error propagates back into authn's own Publish call (go/org/events.go's
// own subscriber documents why returning errors is actively harmful
// there), so a provisioning failure is logged at Error -- an operator
// signal naming the account -- and swallowed, exactly the posture org's
// subscriber takes for its own half. Every step is idempotent, so a
// partially provisioned clinic is completed by any later delivery of the
// same event, and the self_service_clinics ledger below is what lets a
// boot after a restart re-discover every clinic whose provisioning
// completed before the restart (sign_in_memberships.go's clinics scan
// set).
type selfServiceProvisioner struct {
	// orgModule is the module whose TreeService and MemberService the
	// clinic's org rows are created through, under the clinic tenant's own
	// context -- the same services the demo seed and org's own HTTP
	// handler drive, never raw writes.
	orgModule *org.Module
	// rbacService is the service the clinic's built-in roles and the
	// owner grant are ensured through (EnsureBuiltinRoles then
	// AssignRole, the same order seedDemoGrants uses).
	rbacService *rbac.Service
	// clinics is the durable ledger recording every provisioned clinic
	// tenant (below), the boot-time re-discovery source for the
	// membership store's clinics scan set.
	clinics *selfServiceClinics
	// memberships is the sign-in membership store the new clinic tenant
	// is registered into, so authn's "which tenants does this account
	// belong to" question finds the clinic the org rows already answer
	// for.
	memberships *signInMemberships
	// catalog is the merged message catalog the clinic's org root is
	// named from (reg.Locales(), non-nil once Bootstrap has run -- the
	// subscription is installed after Bootstrap, so it is always non-nil
	// here).
	catalog *i18n.Catalog
}

// onUserCreated is the subscription installed by wireSelfService for
// authn.EventUserCreated. It implements the "explicit host call when a
// tenant is born" half of org's own handleUserCreated contract: an event
// carrying no tenant (the self-service registration shape) provisions the
// registrant's clinic; an event carrying one is org's own subscriber's
// job and is skipped here, so the two auto paths never both provision
// one account.
//
// The handler always returns nil (see selfServiceProvisioner's own doc
// comment for why), so every failure below is a logged one.
func (p *selfServiceProvisioner) onUserCreated(ctx context.Context, evt pkgcore.Event) error {
	log := obs.FromContext(ctx)
	if evt.TenantID != "" {
		// A user created inside an existing tenant is org's own
		// subscriber's shape (it gives the user the tenant's workspace);
		// this host must not add a second, clinic tenant for such a user.
		return nil
	}
	userID, ok := userIDFromUserCreatedPayload(evt.Payload)
	if !ok {
		// The payload is never logged: it belongs to another module and
		// may carry personal data.
		log.Warn("reference-app ignored a user-created event with an unrecognized payload",
			"event_type", evt.Type)
		return nil
	}

	clinic := clinicTenantOf(userID)
	if err := p.provision(ctx, userID, clinic); err != nil {
		// The register route has already answered (or is about to answer)
		// 201 for this account, so a failure here must not surface as one
		// in authn. The Error line is the operator signal: the account
		// exists and its sign-in will refuse until the clinic's
		// provisioning is completed (a later redelivery of this event, or
		// manual repair) -- the dead end this file exists to close, now
		// with a log line naming its cause.
		log.Error("reference-app: self-service clinic provisioning failed; the account cannot sign in until it is provisioned",
			"user_id", userID, "tenant_id", clinic, "error", err)
		return nil
	}
	return nil
}

// provision creates (or ensures, on a redelivery) the whole clinic shape
// for userID in tenant clinic: the org tree root, the membership, the
// built-in roles, the owner grant, the durable ledger row and the
// membership store's scan-set entry. Every step is idempotent and every
// step runs under the clinic tenant's own context -- org's tree and
// membership rows and rbac's role and binding rows are all tenant data,
// and nothing here reads or writes across a tenant boundary.
func (p *selfServiceProvisioner) provision(ctx context.Context, userID string, clinic pkgcore.TenantID) error {
	tenantCtx := pkgcore.WithTenant(ctx, clinic)

	// The org tree root: mirror org's own ensureRoot (go/org/events.go) --
	// the tenant's root node, created when it has none, re-read when a
	// concurrent delivery (a second replica's identical provisioning) won
	// the creation race. The name and kind are org's own auto-created-root
	// defaults, so the node this host writes for a tenant-less registration
	// is exactly the node org itself would have written had the
	// registration event carried a tenant (see clinicRootName's doc
	// comment).
	root, created, err := ensureClinicRoot(tenantCtx, p.orgModule.Tree(), p.clinicRootName(tenantCtx))
	if err != nil {
		return fmt.Errorf("reference-app: ensure the clinic's org root: %w", err)
	}
	log := obs.FromContext(ctx)
	if created {
		log.Debug("reference-app: created the clinic's org root",
			"tenant_id", clinic, "root_node_id", root.ID)
	}

	// The membership: one seat per person per tenant; an already-present
	// seat (a redelivery) is left exactly where it is.
	if _, err := p.orgModule.Members().Add(tenantCtx, userID, root.ID); err != nil {
		if !orgCodeIs(err, org.ErrMembershipExists.Code) {
			return fmt.Errorf("reference-app: add the registrant to the clinic org: %w", err)
		}
	}

	// The owner grant: roles are tenant rows, so the built-in roles are
	// ensured before the grant names one -- the same order seedDemoGrants
	// uses. EnsureBuiltinRoles reconciles rather than recreates, and
	// AssignRole is a no-op when the binding is already there, so both
	// are safe to repeat on a redelivery.
	if err := p.rbacService.EnsureBuiltinRoles(tenantCtx); err != nil {
		return fmt.Errorf("reference-app: ensure the clinic's built-in roles: %w", err)
	}
	if err := p.rbacService.AssignRole(tenantCtx, rbac.Subject{TenantID: clinic, UserID: userID}, rbac.BuiltinRoleOwner, rbac.Scope{}); err != nil {
		return fmt.Errorf("reference-app: grant the clinic owner role: %w", err)
	}

	// The ledger row and the store's scan-set entry: recorded only once
	// the org and rbac halves have all landed, so a boot-time
	// re-discovery never names a clinic whose rows are half-written (the
	// idempotent steps above would converge such a clinic on the next
	// delivery; the ledger row's late write keeps the window where the
	// store would scan it in a broken state empty).
	if err := p.clinics.add(ctx, clinic, userID); err != nil {
		return fmt.Errorf("reference-app: record the clinic tenant in the self-service ledger: %w", err)
	}
	p.memberships.addClinicTenant(clinic)
	return nil
}

// clinicRootName renders the name of a clinic's org root from the merged
// catalog, mirroring org's own defaultWorkspaceName (go/org/events.go):
// the message id is org's own auto-created-root name (its zh-CN value is
// the Chinese word for "workspace", go/org/locales/{zh-CN,en-US}.toml)
// and is referenced by its literal string the same way this app's role
// seeding references rbac's "rbac.role.member" description id
// (demo_subject.go's seedDemoReaderRole) -- a coordination point between
// this host's glue and the module that owns the message, never a new
// piece of copy living in Go. The locale is the platform default
// (zh-CN), exactly org's own choice for its auto-created roots: this
// node is created before anybody has expressed a preference, and the
// tenant renames it the moment it means something else to them. A
// missing catalog or message falls back to the tenant id -- an
// identifier is not user-facing text, the same fallback org's own
// function documents.
func (p *selfServiceProvisioner) clinicRootName(ctx context.Context) string {
	fallback := func() string {
		tenant, err := pkgcore.MustTenantFromContext(ctx)
		if err != nil {
			return "workspace"
		}
		return string(tenant)
	}
	if p.catalog == nil {
		return fallback()
	}
	name, err := p.catalog.Lookup(i18n.LocaleZHCN, "org.default_workspace_name", nil)
	if err != nil {
		obs.FromContext(ctx).Warn("reference-app could not render the clinic root name",
			"message_id", "org.default_workspace_name", "error", err)
		return fallback()
	}
	return name
}

// ensureClinicRoot returns the clinic tenant's root node, creating it
// when the tenant has none yet -- the idempotent shape org's own
// ensureRoot (go/org/events.go) uses, including the race handling: two
// concurrent deliveries can both find no root, and the loser of that race
// sees ErrRootAlreadyExists (from the pre-check or from the single-root
// unique index) and re-reads instead of failing. created reports whether
// this call created the root.
func ensureClinicRoot(ctx context.Context, tree *org.TreeService, name string) (*org.OrgNode, bool, error) {
	existing, err := tree.Root(ctx)
	switch {
	case err == nil:
		return existing, false, nil
	case !orgCodeIs(err, org.ErrNodeNotFound.Code):
		return nil, false, err
	}

	// "workspace" is org's own defaultRootKind literal (go/org/events.go):
	// the same neutral kind org's auto path gives a brand-new user's root.
	// Kind is the tenant's own business vocabulary and org enumerates none
	// of it, so the automatic path's kind is deliberately the most neutral
	// word available -- this host mirrors it, and a tenant renames the node
	// (and, with it, whatever kind its own vocabulary needs) the moment the
	// root means something specific to them.
	created, err := tree.CreateRoot(ctx, name, "workspace")
	if err == nil {
		return created, true, nil
	}
	if orgCodeIs(err, org.ErrRootAlreadyExists.Code) {
		reRead, reReadErr := tree.Root(ctx)
		return reRead, false, reReadErr
	}
	return nil, false, err
}

// clinicTenantOf derives the clinic tenant id for userID. The derivation
// is what makes provisioning safe across replicas and redeliveries (see
// selfServiceProvisioner's own doc comment): the same account always
// resolves to the same tenant, so two consumers of one registration event
// converge on one clinic instead of minting two. The id is prefixed so it
// can never collide with a configured host tenant's id (cfg.HostTenants
// values are this host's own names) and stays within the VARCHAR(64)
// tenant_id columns every module's tables carry.
func clinicTenantOf(userID string) pkgcore.TenantID {
	return pkgcore.TenantID("tenant-" + userID)
}

// selfServiceUserIDKeys are the field spellings this host accepts for the
// user id inside an authn.user.created payload, probed in order -- the
// identical probe org's own subscriber runs (go/org/events.go's
// userCreatedUserIDKeys), for the identical reason: the payload reaches
// the subscriber as data, not as a type. A same-process publish delivers
// authn's struct (whose fields carry no JSON tags), while the distributed
// mode's Redis bus delivers a map built from that struct's JSON form, so
// both spellings must be accepted.
var selfServiceUserIDKeys = []string{"user_id", "userId", "UserID", "userID"}

// userIDFromUserCreatedPayload extracts the user id from an
// authn.user.created payload of any shape, by round-tripping it through
// JSON into a map and probing the accepted key spellings -- the same
// normalization org's own userIDFromPayload (go/org/events.go) documents,
// and never a type assertion against authn's concrete payload type.
func userIDFromUserCreatedPayload(payload any) (string, bool) {
	if payload == nil {
		return "", false
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", false
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return "", false
	}
	for _, key := range selfServiceUserIDKeys {
		value, ok := fields[key]
		if !ok {
			continue
		}
		id, ok := value.(string)
		if ok && id != "" {
			return id, true
		}
	}
	return "", false
}

// selfServiceClinicsTable is the durable ledger of every self-service
// clinic tenant this host has provisioned. Its rows are the boot-time
// re-discovery source for the membership store's clinics scan set
// (sign_in_memberships.go's addClinicTenant): org's memberships table
// already carries the real membership facts and survives restarts, but
// the "which tenants does this account belong to" question has to know
// WHERE to look for them, and the configured-host-tenant universe alone
// cannot name a tenant that was born at runtime -- so the ledger exists to
// answer "which self-service tenants are there", nothing more. The
// org/rbac rows remain the authority on membership and grants.
const selfServiceClinicsTable = "self_service_clinics"

// createSelfServiceClinicsTableSQL is executed imperatively, with a plain
// CREATE TABLE IF NOT EXISTS -- the same bootstrapping pattern this app's
// own bookkeeping tables already use (internal/smilesim's
// creditReservationsTable, internal/cases's tables; see
// internal/smilesim/reservation_store.go's own doc comment for why such
// app-level tables do not join dbkit.MigrationRegistry). The statement is
// portable across both dbkit dialects anyway, matching the backend coding
// standard's dual-dialect rule, even though only SQLite is exercised by
// this app today.
//
// Rows are platform data, like go/jobs' own jobRecord and go/config's row
// (root CLAUDE.md's data-domain census entries for both): the ledger must
// be listed wholesale at boot (one query over every row), an access
// pattern dbkit.Repository[T]'s tenant-injecting plugin cannot serve, so
// it carries no tenant scoping at all -- a clinic tenant is a value in the
// tenant_id column, never a context this table is read under.
const createSelfServiceClinicsTableSQL = `CREATE TABLE IF NOT EXISTS ` + selfServiceClinicsTable + ` (
	tenant_id  VARCHAR(64) NOT NULL PRIMARY KEY,
	user_id    VARCHAR(64) NOT NULL,
	created_at TIMESTAMP NOT NULL
)`

// selfServiceClinic is one ledger row: a provisioned clinic tenant and
// the registrant whose self-service registration created it. user_id is
// recorded for operational attribution (which account owns which clinic);
// nothing in the sign-in path reads it.
type selfServiceClinic struct {
	TenantID  string    `gorm:"column:tenant_id;primaryKey;size:64"`
	UserID    string    `gorm:"column:user_id;size:64"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

// selfServiceClinics is the ledger store over this app's own database
// connection -- the same connection every module shares, never a second
// pool.
type selfServiceClinics struct {
	db *gorm.DB
}

// EnsureSchema creates the ledger table when it does not exist yet.
func (c *selfServiceClinics) EnsureSchema(ctx context.Context) error {
	return c.db.WithContext(ctx).Exec(createSelfServiceClinicsTableSQL).Error
}

// add records one provisioned clinic tenant. The tenant_id primary key
// makes a repeated add of the same clinic a no-op, which is what lets the
// provisioning path call this on every delivery of a registration event
// without special-casing redeliveries.
func (c *selfServiceClinics) add(ctx context.Context, tenant pkgcore.TenantID, userID string) error {
	return c.db.WithContext(ctx).Create(&selfServiceClinic{
		TenantID: string(tenant),
		UserID:   userID,
	}).Error
}

// list returns every provisioned clinic tenant, in ledger order.
func (c *selfServiceClinics) list(ctx context.Context) ([]selfServiceClinic, error) {
	var rows []selfServiceClinic
	if err := c.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// wireSelfService installs the self-service signup chain into a composed
// server: it re-discovers every already-provisioned clinic from the
// durable ledger (a boot against a database a previous boot provisioned
// clinics into must keep those clinics' owners sign-in-able), then
// subscribes the provisioner to authn's user-created event.
//
// buildServer calls it AFTER the demo seeds have run, which is the
// discriminator that keeps the demo path intact (selfServiceProvisioner's
// own doc comment): the demo accounts' registrations happen inside
// buildServer, before this subscription exists, so they provision nothing
// -- a demo account keeps exactly the memberships and grants the seed
// gives it, and its no-tenant sign-in keeps landing in tenant-acme, the
// first configured tenant. Every registration that reaches the server
// afterwards -- a browser's, or a flow test's -- is a self-service
// registration and provisions its clinic.
//
// The guarantee is per replica: on the in-process bus the seeds' own
// events precede the subscription entirely, and on the Redis bus a
// replica's reader group starts at the live end of the stream
// (go/pkgcore/eventbus/redis/eventbus.go's delivery note), so this
// replica's own seed events are never delivered to its own subscription
// either. A seed event from ANOTHER replica whose boot windows overlap
// this one's could reach this replica's subscription in that overlap --
// the provisioning converges idempotently on a clinic that changes
// nothing the demo account's configured-tenant answers depend on (its
// memberships, grants and first-tenant resolution all stay as the seed
// made them), which is the residual cost of the ordering discriminator
// under a genuinely concurrent multi-replica boot.
func wireSelfService(ctx context.Context, reg *pkgcore.Registry, db *gorm.DB, orgModule *org.Module, rbacService *rbac.Service, memberships *signInMemberships) error {
	clinics := &selfServiceClinics{db: db}
	if err := clinics.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("reference-app: ensure the self-service clinic ledger schema: %w", err)
	}

	rows, err := clinics.list(ctx)
	if err != nil {
		return fmt.Errorf("reference-app: read the self-service clinic ledger: %w", err)
	}
	for _, row := range rows {
		// A clinic a previous boot provisioned must be visible to the
		// sign-in store in THIS boot too: the org membership rows are
		// already in the database, and the clinics scan set is what tells
		// authn's "which tenants" question where to look for them.
		memberships.addClinicTenant(pkgcore.TenantID(row.TenantID))
	}

	provisioner := &selfServiceProvisioner{
		orgModule:   orgModule,
		rbacService: rbacService,
		clinics:     clinics,
		memberships: memberships,
		catalog:     reg.Locales(),
	}
	reg.Events.Subscribe(authn.EventUserCreated, provisioner.onUserCreated)
	return nil
}
