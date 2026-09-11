package aigateway

// component.go registers the "ai-gateway" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as the "ai-gateway" module's single implementation. The component builds
// the same *Module every other caller builds through NewModule, so the
// module's credential service, gateway and HTTP surface are one
// implementation reachable two ways. Its Init runs the module's one
// declaration entry point, Register, inside the assembly's Init stage -- the
// one stage whose seats accept writes.

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/storage"

	"github.com/vislake/speed/go/ai-gateway/migrations"
)

// aiGatewayComponent is the component descriptor for "ai-gateway". It
// declares MultiReplicaSafe: the module's state is its credential rows in
// the shared database, and the rate limiter counts on the shared key-value
// store.
//
// Requires the database and optionally the products that reach only through
// host wiring on the reference-app path: the queue and the storage module
// (image generation is wired only when both are present -- the job handler
// reads input images and writes generated ones through the storage
// service), and the billing/metering-shaped Entitlements and UsageRecorder
// seams, whose absence is the module's own documented default (no gate, no
// recording). The key-value store backs the per-tenant rate limiter and
// fails closed when absent.
//
// Init runs the module's one declaration entry point, Register, inside the
// assembly's Init stage -- the one stage whose seats accept writes -- so the
// component world declares exactly what the module's Register declares.
//
// SystemPurposes declares SystemPurposeCredentialWrite: the audited purpose
// a host names when it builds the system context authorizing a platform-wide
// credential write.
//
// Model routes are deliberately not part of this descriptor: the logical
// model to vendor model mapping is a construction-time concern of the
// deploying application, not something a composition block carries.
//
// Prepare is deliberately not declared: the credential API-key serializer
// is registered today by the host's pre-database path from material the
// host holds.
var aiGatewayComponent = pkgcore.Component{
	Name:           "ai-gateway",
	Module:         "ai-gateway",
	Provides:       []any{(*Module)(nil)},
	Capabilities:   pkgcore.MultiReplicaSafe,
	SystemPurposes: []pkgcore.SystemPurpose{SystemPurposeCredentialWrite},
	Requires: []pkgcore.Requirement{
		{Token: (*gorm.DB)(nil)},
		{Token: (*jobs.Queue)(nil), Optional: true},
		{Token: (*storage.Module)(nil), Optional: true},
		{Token: (*Entitlements)(nil), Optional: true},
		{Token: (*UsageRecorder)(nil), Optional: true},
		{Token: (*pkgcore.KVStore)(nil), Optional: true},
	},
	Migrations:  migrations.FS,
	OpenAPISpec: openAPISpecYAML,
	New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		var opts []GatewayOption
		// Image generation is wired exactly when both products it runs on
		// are selected: the queue its job handler drains and the storage
		// service whose bytes it reads and writes. A gateway with either
		// absent stays chat-only, the WithImageGeneration-omitted shape.
		queue, queueErr := pkgcore.Get[jobs.Queue](reg)
		storageModule, storageErr := pkgcore.Get[*storage.Module](reg)
		if queueErr == nil && storageErr == nil {
			opts = append(opts, WithImageGeneration(queue, storageModule.ObjectService()))
		}
		if entitlements, err := pkgcore.Get[Entitlements](reg); err == nil {
			opts = append(opts, WithEntitlements(entitlements))
		}
		if recorder, err := pkgcore.Get[UsageRecorder](reg); err == nil {
			opts = append(opts, WithUsageRecorder(recorder))
		}
		return NewModule(db, opts...), nil
	},
	Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
		m, ok := instance.(*Module)
		if !ok {
			return fmt.Errorf("ai-gateway: component init got a %T instance, want *ai-gateway.Module", instance)
		}
		return m.Register(reg)
	},
}

func init() { pkgcore.MustRegister(aiGatewayComponent) }
