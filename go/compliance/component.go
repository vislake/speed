package compliance

// component.go registers the "compliance" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as the "compliance" module's single implementation. The component builds
// the same *Module every other caller builds through NewModule, including
// the audit repository the module's four services read and write through.

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/compliance/locales"
)

// complianceComponent is the component descriptor for "compliance". It
// declares MultiReplicaSafe: the module's state is the participants' rows
// and the audit table in the shared database, and every orchestration it
// runs is a cross-tenant sweep over that shared state.
//
// Requires the audit module (the module whose table and event capture the
// whole surface is built on), the database its repository is built over, the
// configuration module (whose lazy handle backs the export-delivery expiry
// reader) and the queue its retention sweep task is drained from -- Register
// refuses to boot without one. The sharing creator (the export delivery
// seam) and the tenant lister (the sweep-all-tenants discovery seam) are
// optional, each with the module's own documented call-time refusal when
// absent.
//
// SystemPurposes declares the two audited purposes the module acts under:
// the retention sweep and the right-to-erasure execution. The assembly
// registers them at the Init stage's entry, before any Init callback runs.
//
// Init runs the module's one declaration entry point, Register, inside the
// assembly's Init stage -- the one stage whose seats accept writes -- so the
// component world declares exactly what the module's Register declares.
var complianceComponent = pkgcore.Component{
	Name:           "compliance",
	Module:         "compliance",
	Provides:       []any{(*Module)(nil)},
	Capabilities:   pkgcore.MultiReplicaSafe,
	SystemPurposes: []pkgcore.SystemPurpose{SystemPurposeRetentionSweep, SystemPurposeRightToErasure},
	Requires: []pkgcore.Requirement{
		{Token: (*gorm.DB)(nil)},
		{Token: (*audit.Module)(nil)},
		{Token: (*config.Module)(nil)},
		{Token: (*jobs.Queue)(nil)},
		{Token: (*SharingCreator)(nil), Optional: true},
		{Token: (*TenantLister)(nil), Optional: true},
	},
	Locales: locales.FS,
	New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		queue, err := pkgcore.Get[jobs.Queue](reg)
		if err != nil {
			return nil, err
		}
		cfgModule, err := pkgcore.Get[*config.Module](reg)
		if err != nil {
			return nil, err
		}
		opts := []Option{
			WithQueue(queue),
			WithExportConfigReader(NewConfigReader(cfgModule.Handle())),
		}
		if creator, err := pkgcore.Get[SharingCreator](reg); err == nil {
			opts = append(opts, WithSharing(creator))
		}
		if lister, err := pkgcore.Get[TenantLister](reg); err == nil {
			opts = append(opts, WithTenantLister(lister))
		}
		// The repository is built over the shared connection exactly as the
		// host path builds it, so the module reads and writes the same
		// audit_events table the audit module's persister captures into.
		return NewModule(audit.NewRepository(db), opts...), nil
	},
	Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
		m, ok := instance.(*Module)
		if !ok {
			return fmt.Errorf("compliance: component init got a %T instance, want *compliance.Module", instance)
		}
		return m.Register(reg)
	},
}

func init() { pkgcore.MustRegister(complianceComponent) }
