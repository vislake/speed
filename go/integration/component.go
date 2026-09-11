package integration

// component.go registers the "integration" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as the "integration" module's single implementation. The component builds
// the same *Module every other caller builds through NewModule; the runtime
// Service stays Attach's product, exactly as it is on the host path.

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/integration/locales"
	"github.com/vislake/speed/go/integration/migrations"
)

// integrationComponentConfig is the "integration" component's configuration
// schema: one field per key a composition block may carry. Decoding is
// strict, so a block naming any other key fails before anything is
// constructed.
//
//	max_api_key_lifetime   duration   expiry ceiling and default for issued keys
type integrationComponentConfig struct {
	MaxAPIKeyLifetime time.Duration `json:"max_api_key_lifetime"`
}

// integrationComponent is the component descriptor for "integration". It
// declares MultiReplicaSafe: the module's state is its rows in the shared
// database, deliveries are drained from the shared queue, and the SSRF
// guards and event mappings hold no per-process state.
//
// Requires the database as its mandatory construction product. The
// permission lister is a required dependency: it is the seam every scoped
// API key is created against, and a security check with nothing to check
// against must fail closed. The webhook queue, the membership checker and
// the subject resolver are optional -- each absence is a documented,
// reduced-but-legal wiring on the host path (no enqueue, no cosmetic
// creator flag, and a closed own-identity path respectively).
//
// Init runs the module's one declaration entry point, Register, then Attach
// -- the runtime Service's construction over the module's freshly declared
// surface: Attach installs no declarations of its own, and it reads the
// same assembly values Register handed the module's handlers, so it can run
// nowhere but the Init stage. The returned *Service is put into the by-type
// context, where a consumer requires and reads it after the assembly.
var integrationComponent = pkgcore.Component{
	Name:   "integration",
	Module: "integration",
	// The construction product is the *Module. The *Service Attach builds
	// over the module's freshly declared surface is a runtime service --
	// Attach installs declarations and reads assembly values, so it can run
	// nowhere but the Init stage and never before construction -- so it
	// stays out of Provides (a service never resolves a token
	// requirement).
	Provides:     []any{(*Module)(nil)},
	Capabilities: pkgcore.MultiReplicaSafe,
	ConfigSchema: (*integrationComponentConfig)(nil),
	Requires: []pkgcore.Requirement{
		{Token: (*gorm.DB)(nil)},
		{Token: (*PermissionLister)(nil)},
		{Token: (*jobs.Queue)(nil), Optional: true},
		{Token: (*MembershipChecker)(nil), Optional: true},
		{Token: (*SubjectResolver)(nil), Optional: true},
	},
	Migrations:  migrations.FS,
	Locales:     locales.FS,
	OpenAPISpec: openAPISpecYAML,
	New: func(_ context.Context, reg *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c integrationComponentConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		var opts []Option
		if c.MaxAPIKeyLifetime > 0 {
			opts = append(opts, WithMaxAPIKeyLifetime(c.MaxAPIKeyLifetime))
		}
		if queue, err := pkgcore.Get[jobs.Queue](reg); err == nil {
			opts = append(opts, WithWebhookQueue(queue))
		}
		if subject, err := pkgcore.Get[SubjectResolver](reg); err == nil {
			opts = append(opts, WithSubjectResolver(subject))
		}
		return NewModule(db, opts...), nil
	},
	Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
		m, ok := instance.(*Module)
		if !ok {
			return fmt.Errorf("integration: component init got a %T instance, want *integration.Module", instance)
		}
		if err := m.Register(reg); err != nil {
			return err
		}
		svc, err := m.Attach(reg)
		if err != nil {
			return err
		}
		reg.Put(svc)
		return nil
	},
}

func init() { pkgcore.MustRegister(integrationComponent) }
