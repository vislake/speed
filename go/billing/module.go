package billing

import (
	"embed"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/billing/locales"
	"github.com/vislake/speed/go/billing/migrations"
)

// moduleName is billing's pkgcore.Module.Name(), and the key
// dbkit.MigrationRegistry.Register builds its dependency graph on.
const moduleName = "billing"

// The permissions this module contributes, following the resource:action
// convention every other module's Register uses.
const (
	PermissionPlanManage         = "billing:plan:manage"
	PermissionSubscriptionRead   = "billing:subscription:read"
	PermissionSubscriptionManage = "billing:subscription:manage"
	PermissionCreditRead         = "billing:credit:read"
	PermissionCreditManage       = "billing:credit:manage"
)

// The audit actions this module contributes: the credit ledger's five
// state-changing operations, per docs/internal's own "financial
// operations belong in the audit trail" security rule. Declaring the
// enumeration is this round's own scope; CreditService does not itself
// call audit.Emit yet -- see AGENTS.md's Known limitations for why, and
// what a later round wiring it needs.
const (
	AuditActionCreditGrant         = "billing.credit.grant"
	AuditActionCreditDeductReserve = "billing.credit.deduct_reserve"
	AuditActionCreditDeductConfirm = "billing.credit.deduct_confirm"
	AuditActionCreditRefund        = "billing.credit.refund"
	AuditActionCreditExpire        = "billing.credit.expire"
)

// Module implements pkgcore.Module for go/billing.
//
// # Wiring
//
// A host constructs one with NewModule and hands it to Kernel.Bootstrap.
// Constructing a Module performs no I/O: db is opened and migrated by the
// host before Register is ever called, exactly like every other module in
// this codebase. Register wires the pkgcore.EventBus onto PlanService and
// SubscriptionService (so a later Create/Update/transition call can
// publish EventPlanChanged/EventSubscriptionStatusChanged) and declares
// this module's permissions, audit actions and published events; it
// performs no I/O of its own.
//
// # No HTTP surface this round
//
// OpenAPISpec returns nil, the same "no fragment yet" answer go/config's,
// go/pki's and go/metering's Module give for their own HTTP-surface-free
// rounds.
type Module struct {
	db *gorm.DB

	plans         *PlanStore
	planService   *PlanService
	subscriptions *SubscriptionRepository
	subService    *SubscriptionService
	invoices      *InvoiceRepository
	credits       *CreditService
	entitlements  *EntitlementsService
}

// NewModule returns a Module whose tables live in db, with
// EntitlementsService's real-time quota reads served by usage (typically a
// host-constructed *metering.Aggregator -- see UsageReader's own doc
// comment for why go/billing importing go/metering directly is sanctioned
// here). Constructing a Module performs no I/O: opening and migrating db
// is the host's responsibility, done before Bootstrap ever calls Register.
func NewModule(db *gorm.DB, usage UsageReader) *Module {
	plans := NewPlanStore(db)
	subscriptions := NewSubscriptionRepository(db)
	subService := NewSubscriptionService(subscriptions, nil)
	return &Module{
		db:            db,
		plans:         plans,
		planService:   NewPlanService(plans, nil),
		subscriptions: subscriptions,
		subService:    subService,
		invoices:      NewInvoiceRepository(db),
		credits:       NewCreditService(db),
		entitlements:  NewEntitlementsService(subService, plans, usage),
	}
}

// Plans returns the module's PlanService, the write-side, event-publishing
// seam for creating and updating Plans. Read-only lookups (e.g.
// EntitlementsService's own) go through the lower-level PlanStore
// directly; a host wanting the same read path calls Plans().Get/Resolve,
// both of which PlanService passes through unchanged.
func (m *Module) Plans() *PlanService { return m.planService }

// Subscriptions returns the module's SubscriptionService.
func (m *Module) Subscriptions() *SubscriptionService { return m.subService }

// Invoices returns the module's InvoiceRepository.
func (m *Module) Invoices() *InvoiceRepository { return m.invoices }

// Credits returns the module's CreditService, the credits ledger's one
// write surface (PreDeduct/Confirm/Refund/Grant/Expire) plus Balance.
func (m *Module) Credits() *CreditService { return m.credits }

// Entitlements returns the module's Entitlements implementation -- the
// single judgment entry point business code calls to learn whether a
// tenant's current subscription permits a feature.
func (m *Module) Entitlements() Entitlements { return m.entitlements }

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module: nothing. billing sits above
// authn/rbac/org/metering in docs/internal/01-architecture.md's graph and
// imports go/metering's Go API directly (UsageReader's own doc comment),
// but this round wires no cross-module event subscription and no consumer
// of any other module's pkgcore.Registry declarations, so DependsOn --
// which enumerates only modules in the bootstrap set billing itself
// requires to have registered first -- returns nil, the same answer
// go/metering's and go/pki's Module give for the identical reason.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: the descriptions of billing's error
// codes, in both supported languages with identical id sets.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module. billing has no HTTP surface this
// round -- every capability is a Go-level API business modules call
// in-process (Entitlements.Check, CreditService's methods,
// SubscriptionService's lifecycle transitions) -- so this returns nil, the
// same "no fragment yet" answer go/config's, go/pki's and go/metering's
// Module give.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's own contract it
// only declares and wires -- no database call, no outbound call, nothing
// that touches m.db. It attaches the registry's EventBus onto
// PlanService and SubscriptionService, and declares this module's
// permissions, audit actions and published events.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if err := reg.Permissions.Add(
		PermissionPlanManage,
		PermissionSubscriptionRead,
		PermissionSubscriptionManage,
		PermissionCreditRead,
		PermissionCreditManage,
	); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(
		AuditActionCreditGrant,
		AuditActionCreditDeductReserve,
		AuditActionCreditDeductConfirm,
		AuditActionCreditRefund,
		AuditActionCreditExpire,
	); err != nil {
		return err
	}
	if err := reg.Events.Publishes(eventDecls...); err != nil {
		return err
	}

	bus := reg.EventBus()
	m.planService.events = bus
	m.subService.events = bus
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)

// compile-time check that *metering.Aggregator satisfies UsageReader
// structurally -- see UsageReader's own doc comment for why this
// package declares a small interface rather than depending on the
// concrete type directly in EntitlementsService's field.
var _ UsageReader = (*metering.Aggregator)(nil)
