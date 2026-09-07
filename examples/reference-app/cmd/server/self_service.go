package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/jobs"
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
// answer leaves: on the path every normal registration takes the clinic
// is already there when the answer arrives, which is what makes
// "register, then sign in" a journey with no gap to race. The path that
// is not normal -- a failed synchronous attempt -- and the honest 201
// semantics that come with it are # Failure semantics' own subject.
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
// there), so the register route answers 201 whether the synchronous
// provisioning attempt succeeded or failed -- authn must not fail a
// registration whose account was created. A failure is therefore logged
// at Error, and it must not stop there: the account is real, the answer
// said "created", the authn.user.created event fires exactly once (the
// in-process bus never redelivers), and the account cannot re-register,
// so a clinic that never appeared would strand the registrant in the dead
// end this file exists to close. Every step of provision is idempotent --
// the tenant is derived from the user id, org's tree and membership
// writes and rbac's role and grant writes reconcile, and the ledger row
// below is a silent no-op on a repeat -- which is what makes a FAILED
// attempt retryable: the retry job scheduled on the app's own job queue
// (scheduleProvisionRetry) re-runs the same provision until it succeeds,
// converging the clinic exactly as a redelivered event would have. The
// job row is the durable record of the unfinished work: it survives a
// restart, so a process that dies between the failed attempt and the
// retry's success loses nothing -- the next boot's queue start
// re-dispatches the due retry (jobs' Start recovers and re-claims it).
// The queue's own retry budget and dead-letter close the loop: past the
// last attempt the job dead-letters with the final cause on its row and
// in the log, the operator signal that this provisioning needs a human
// rather than another attempt.
//
// The 201 semantics that result are the honest ones: register answers 201
// exactly when the account exists. The clinic is already there whenever
// the synchronous attempt succeeded -- the path every normal registration
// takes -- so "register, then sign in" keeps its gap-free shape. After a
// failed synchronous attempt the 201 still answers, and a sign-in in the
// window before the retry converges answers the memberless refusal
// (authn.tenant_membership_required) exactly as it did before
// self-service existed; the retry converges the clinic moments later and
// the same sign-in then lands in it. The browser-shaped e2e gate
// (self-service-signup.spec.ts) never sees that window, because it signs
// in after a registration whose synchronous attempt succeeded.
//
// The self_service_clinics ledger below is written only once provision
// has completed every step (see provision), so boot-time re-discovery
// never names a clinic whose org/rbac half is missing -- a clinic whose
// provisioning never completed has no ledger row, and its retry job row,
// not a ledger row, is the record the next boot re-dispatches
// (sign_in_memberships.go's clinics scan set reads the ledger only).
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
	// queue is the app's own standalone queue a failed synchronous
	// provisioning attempt's recovery is enqueued on
	// (scheduleProvisionRetry), and the queue the retry job's handler is
	// registered on (wireSelfService). Always set by wireSelfService.
	queue *jobs.StandaloneQueue
	// failProvision is the regression suite's failure-injection point
	// (serverConfig.failSelfServiceProvision, consulted at the top of
	// provision): when non-nil it fails every provisioning attempt it is
	// asked about, so a test can place a failure on the synchronous
	// delivery and watch the retry converge the same clinic. Nil in every
	// production wiring.
	failProvision func(userID string) error
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
// comment for why), so every failure below is a logged one -- and a failed
// provisioning attempt is followed by the retry that converges the clinic
// (scheduleProvisionRetry), never left for a redelivery that will not
// come.
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
		// in authn. The Error line is the operator signal and
		// scheduleProvisionRetry is the recovery: the retry job re-runs
		// this same provision until it succeeds, so a failed synchronous
		// attempt never strands the account -- its sign-in refuses only
		// until the retry converges the clinic (the file doc's # Failure
		// semantics).
		log.Error("reference-app: self-service clinic provisioning failed synchronously; scheduling the retry that converges it",
			"user_id", userID, "tenant_id", clinic, "error", err)
		p.scheduleProvisionRetry(ctx, userID, clinic)
		return nil
	}
	return nil
}

// selfServiceProvisionTaskType is the jobs task type of the retry job a
// failed synchronous provisioning attempt enqueues (scheduleProvisionRetry
// below). The dotted spelling follows the task types this app's queue
// already carries (storage.expiry_sweep, notification.deliver,
// pki.expiry_scan); the type is this host's own, so its prefix names the
// feature, not a module.
const selfServiceProvisionTaskType = "self_service.provision_clinic"

// selfServiceProvisionMaxRetries is how many retries beyond the first
// attempt the retry job gets (jobs.WithMaxRetries; jobs.DefaultMaxRetries
// is 3). The job is the ONLY automatic recovery a failed synchronous
// provisioning attempt gets -- the authn.user.created event fires once and
// nothing else ever re-runs the chain -- so the budget is deliberately
// generous: at the queue's doubling backoff (1s base, 5m cap, go/jobs'
// DefaultBackoffBase/DefaultBackoffMax) ten retries keep trying for
// minutes, far past the transient failures a synchronous attempt can
// actually hit (database contention with the register's own writes, a
// busy SQLite file), and a provisioning still failing after that is an
// incident the dead-letter row and its log line hand to an operator
// rather than another attempt.
const selfServiceProvisionMaxRetries = 10

// selfServiceProvisionTask is the retry job's payload: the registrant's
// user id, the one fact provision needs (the clinic tenant is derived
// from it, clinicTenantOf). The payload carries no tenant: the job's own
// TenantID is the clinic, so the worker's rebuilt context already names
// it when the handler runs (jobs.Handler's contract).
type selfServiceProvisionTask struct {
	UserID string `json:"user_id"`
}

// selfServiceProvisionJobHandler is the jobs.Handler for
// selfServiceProvisionTaskType: it re-runs the clinic provisioning whose
// synchronous attempt failed. Every step provision takes is idempotent,
// so the handler converges the clinic wherever the earlier attempt died
// -- even one that completed the org and rbac halves and failed only on
// the ledger write is finished by a re-run that lands last on a silent
// no-op (selfServiceClinics.add). A failed attempt returns its error and
// the queue does the rest: retry with backoff while attempts remain, then
// a dead-letter whose row and log carry the last cause.
//
// wireSelfService registers it directly on the app's standalone queue
// rather than declaring it on a module registry: the registry's handler
// drain onto the queue has already run by the time wireSelfService is
// called (server.go). RegisterHandler is safe to call after the queue's
// Start (go/jobs' own doc), and no job of this type can exist before this
// registration completes anyway -- scheduleProvisionRetry, its only
// enqueuer, runs inside a served register request, which cannot arrive
// until buildServer has returned.
type selfServiceProvisionJobHandler struct {
	provisioner *selfServiceProvisioner
}

// compile-time check that the retry handler satisfies jobs.Handler.
var _ jobs.Handler = (*selfServiceProvisionJobHandler)(nil)

// Type implements jobs.Handler.
func (h *selfServiceProvisionJobHandler) Type() string { return selfServiceProvisionTaskType }

// Handle implements jobs.Handler: it re-runs provision for the task's user
// under the clinic tenant context the worker rebuilt from the job's own
// TenantID (jobs.Handler's contract), then returns no result on success
// and the failure on error -- the error becomes the attempt's recorded
// failure text and, past the retry budget, the dead-letter's. The wrap
// carries the registrant's user id so that recorded text names the
// account even read out of context; the clinic tenant stays on the job
// row itself, where the queue already keeps it.
func (h *selfServiceProvisionJobHandler) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	var task selfServiceProvisionTask
	if err := json.Unmarshal(job.Payload, &task); err != nil {
		return jobs.Result{}, fmt.Errorf("reference-app: decode the clinic provisioning retry task: %w", err)
	}
	if task.UserID == "" {
		return jobs.Result{}, fmt.Errorf("reference-app: the clinic provisioning retry task carries no user id")
	}
	if err := h.provisioner.provision(ctx, task.UserID, clinicTenantOf(task.UserID)); err != nil {
		return jobs.Result{}, fmt.Errorf("provisioning the clinic of user %s: %w", task.UserID, err)
	}
	return jobs.Result{}, nil
}

// scheduleProvisionRetry enqueues the retry job that re-runs a failed
// synchronous provisioning attempt until it succeeds. It is called
// exactly on the synchronous failure path -- a successful provisioning
// never enqueues -- and its own failures are logged, never propagated:
// the register route must not fail for want of a retry. The job is not
// idempotency-keyed, deliberately: at most one synchronous attempt exists
// per registration (the event fires once on the in-process bus), so at
// most one retry job is enqueued per failed registration, and a duplicate
// would be harmless anyway -- the handler converges idempotently whatever
// it finds. A key would be worse than useless: jobs' idempotency-key
// semantics pin a Job forever regardless of its outcome, so a keyed
// retry that dead-lettered could never be re-enqueued by any later
// mechanism.
func (p *selfServiceProvisioner) scheduleProvisionRetry(ctx context.Context, userID string, clinic pkgcore.TenantID) {
	log := obs.FromContext(ctx)
	if p.queue == nil {
		log.Error("reference-app: no job queue to retry the clinic provisioning on; the account cannot sign in until it is provisioned by hand",
			"user_id", userID, "tenant_id", clinic)
		return
	}
	payload, err := json.Marshal(selfServiceProvisionTask{UserID: userID})
	if err != nil {
		// One string field cannot fail to marshal; the guard exists so a
		// future payload change fails loudly here rather than silently
		// stranding the account.
		log.Error("reference-app: encoding the clinic provisioning retry failed",
			"user_id", userID, "tenant_id", clinic, "error", err)
		return
	}
	if _, err := p.queue.Enqueue(ctx, jobs.Task{
		Type:     selfServiceProvisionTaskType,
		TenantID: clinic,
		Payload:  payload,
	}, jobs.WithMaxRetries(selfServiceProvisionMaxRetries)); err != nil {
		log.Error("reference-app: enqueueing the clinic provisioning retry failed",
			"user_id", userID, "tenant_id", clinic, "error", err)
	}
}

// provision creates (or ensures, on a redelivery or a retry) the whole
// clinic shape for userID in tenant clinic: the org tree root, the
// membership, the
// built-in roles, the owner grant, the durable ledger row and the
// membership store's scan-set entry. Every step is idempotent and every
// step runs under the clinic tenant's own context -- org's tree and
// membership rows and rbac's role and binding rows are all tenant data,
// and nothing here reads or writes across a tenant boundary.
func (p *selfServiceProvisioner) provision(ctx context.Context, userID string, clinic pkgcore.TenantID) error {
	// failProvision is the regression suite's injection point
	// (selfServiceProvisioner's own doc comment): armed, it fails this
	// attempt before any step runs, so a test can place the failure on
	// the synchronous delivery and watch the retry converge the same
	// clinic. Never set in production wiring.
	if p.failProvision != nil {
		if err := p.failProvision(userID); err != nil {
			return fmt.Errorf("reference-app: injected provisioning failure: %w", err)
		}
	}
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

// add records one provisioned clinic tenant. The insert carries
// clause.OnConflict{DoNothing: true}, so a repeated add of the same
// clinic tenant is a silent no-op rather than a duplicate-key error
// (measured: a plain Create of an existing tenant_id answers the driver's
// duplicate-key error on this stack, whatever the old doc comment's
// "no-op" claim said), which is what lets the provisioning path call this
// on every run -- a retry job converging a clinic whose earlier steps
// another run already completed lands here last and must not fail the
// whole provision -- without special-casing repeats. OnConflict DoNothing
// is portable across both dbkit dialects (the same clause go/config's and
// go/ai-gateway's own idempotent inserts use).
func (c *selfServiceClinics) add(ctx context.Context, tenant pkgcore.TenantID, userID string) error {
	return c.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&selfServiceClinic{
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
// clinics into must keep those clinics' owners sign-in-able), subscribes
// the provisioner to authn's user-created event, and registers the retry
// job's handler on the standalone queue the caller hands in -- the queue
// a failed synchronous provisioning attempt enqueues its recovery onto
// (scheduleProvisionRetry), whose job rows survive a restart and are
// re-dispatched by the next boot's queue start. failProvision is the
// regression suite's failure-injection hook (nil in every production
// wiring; a test arms it through serverConfig.failSelfServiceProvision
// before calling buildServer), handed to the provisioner it builds.
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
func wireSelfService(ctx context.Context, reg *pkgcore.Registry, db *gorm.DB, orgModule *org.Module, rbacService *rbac.Service, memberships *signInMemberships, queue *jobs.StandaloneQueue, failProvision func(userID string) error) error {
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
		orgModule:     orgModule,
		rbacService:   rbacService,
		clinics:       clinics,
		memberships:   memberships,
		catalog:       reg.Locales(),
		queue:         queue,
		failProvision: failProvision,
	}
	reg.Events.Subscribe(authn.EventUserCreated, provisioner.onUserCreated)
	if err := queue.RegisterHandler(&selfServiceProvisionJobHandler{provisioner: provisioner}); err != nil {
		return fmt.Errorf("reference-app: register the clinic provisioning retry handler: %w", err)
	}
	return nil
}
