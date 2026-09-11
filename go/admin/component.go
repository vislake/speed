package admin

// component.go registers the "admin" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as the "admin" module's single implementation. The component builds the
// same *Module every other caller builds through NewModule, reading the
// products of the modules the console fans in on.

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"

	"github.com/vislake/speed/go/admin/locales"
	"github.com/vislake/speed/go/admin/migrations"
)

// adminComponent is the component descriptor for "admin". It declares
// MultiReplicaSafe: the module's state is its platform rows in the shared
// database, and every cross-tenant operation it performs is a read or an
// audited write over shared state.
//
// Requires the products of every module the console reads: authn (whose
// Service admin's declaration reads, so the dependency also orders this
// component strictly after authn's own declaration stage), org, compliance
// and notification as mandatory dependencies -- Register refuses without
// each -- and metering and billing as optional ones, the module's own
// documented absent-with-no-dashboard-dimension wiring. rbac is a required
// dependency of the provider's product: the *rbac.Service is a runtime
// service published after rbac's declaration stage, and the requirement is
// what orders admin's declaration stage strictly after it. The jobs queue
// is mandatory: the audit-export leg enqueues onto it (ErrQueueRequired).
//
// SystemPurposes declares SystemPurposeAdminCrossTenant: the one audited
// purpose every cross-tenant operation this module performs acts under. The
// assembly registers it when it closes its Init stage, after every Init
// callback has run.
//
// Init runs the module's one declaration entry point, Register, then
// AttachRBAC -- the *rbac.Service whose catalog snapshot is already frozen
// when rbac's own Init turn published it into the by-type context, so the
// RoleService and impersonation pipeline wired here read a complete
// catalog, and the roles fail closed exactly as they do on the host path
// until this call runs.
var adminComponent = pkgcore.Component{
	Name:           "admin",
	Module:         "admin",
	Provides:       []any{(*Module)(nil)},
	Capabilities:   pkgcore.MultiReplicaSafe,
	SystemPurposes: []pkgcore.SystemPurpose{SystemPurposeAdminCrossTenant},
	Requires: []pkgcore.Requirement{
		{Token: (*gorm.DB)(nil)},
		{Token: (*jobs.Queue)(nil)},
		{Token: (*authn.Module)(nil)},
		{Token: (*org.Module)(nil)},
		{Token: (*compliance.Module)(nil)},
		{Token: (*notification.Module)(nil)},
		{Token: (*rbac.Service)(nil)},
		{Token: (*metering.Module)(nil), Optional: true},
		{Token: (*billing.Module)(nil), Optional: true},
	},
	Migrations:  migrations.FS,
	Locales:     locales.FS,
	OpenAPISpec: openAPISpecYAML,
	New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		queue, err := pkgcore.Get[jobs.Queue](reg)
		if err != nil {
			return nil, err
		}
		authnModule, err := pkgcore.Get[*authn.Module](reg)
		if err != nil {
			return nil, err
		}
		orgModule, err := pkgcore.Get[*org.Module](reg)
		if err != nil {
			return nil, err
		}
		complianceModule, err := pkgcore.Get[*compliance.Module](reg)
		if err != nil {
			return nil, err
		}
		notificationModule, err := pkgcore.Get[*notification.Module](reg)
		if err != nil {
			return nil, err
		}
		opts := []Option{
			WithAuthn(authnModule),
			WithOrg(orgModule),
			WithCompliance(complianceModule),
			WithNotification(notificationModule),
			WithQueue(queue),
		}
		if meteringModule, err := pkgcore.Get[*metering.Module](reg); err == nil {
			opts = append(opts, WithMetering(meteringModule))
		}
		if billingModule, err := pkgcore.Get[*billing.Module](reg); err == nil {
			opts = append(opts, WithBilling(billingModule))
		}
		return NewModule(db, opts...), nil
	},
	Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
		m, ok := instance.(*Module)
		if !ok {
			return fmt.Errorf("admin: component init got a %T instance, want *admin.Module", instance)
		}
		if err := m.Register(reg); err != nil {
			return err
		}
		svc, err := pkgcore.Get[*rbac.Service](reg)
		if err != nil {
			return fmt.Errorf("admin: read the published rbac Service: %w", err)
		}
		m.AttachRBAC(svc)
		return nil
	},
}

func init() { pkgcore.MustRegister(adminComponent) }
