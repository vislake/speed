package app

// The descriptor-level delivery compositions: module packages' own
// descriptors, assembled together WITHOUT this app's override copies, so the
// plans are the ones a host that selects the modules as shipped gets. They
// pin the construction-time delivery promise a Provides declaration makes --
// every declared token must be delivered by the end of the constructing
// component's New, including values New puts itself -- over the two real
// module pairs whose declarations and deliveries had drifted apart.

import (
	"context"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pki"
)

// deliveryDBComponent is a stand-in for the database component the assembly
// selects: it declares the *gorm.DB product and constructs the test's open
// handle, so the module descriptors' declared database dependency resolves
// exactly as it will against the real db component.
func deliveryDBComponent(db *gorm.DB) pkgcore.Component {
	return pkgcore.Component{
		Name:     "test.delivery.db",
		Module:   "test",
		Provides: []any{(*gorm.DB)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return db, nil
		},
	}
}

// deliveryDB opens the throwaway SQLite handle the descriptor compositions
// construct over. Construction performs no I/O against it; the handle only
// has to exist and be non-nil.
func deliveryDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     filepath.Join(t.TempDir(), name),
	})
	if err != nil {
		t.Fatalf("opening the throwaway database: %v", err)
	}
	t.Cleanup(func() { _ = dbkit.Close(db) })
	return db
}

// deliveryMaterial publishes the bootstrap material authn's descriptor
// declares and reads: the PII cipher key and the blind-index key. The real
// assembly resolves these on the loader's five-source chain; this drive
// publishes the resolved form directly, which is the same value layer.
func deliveryMaterial() *pkgcore.BootstrapMaterial {
	key := func() []byte {
		b := make([]byte, 32)
		for i := range b {
			b[i] = byte(i + 1)
		}
		return b
	}
	return pkgcore.NewBootstrapMaterial([]pkgcore.BootstrapMaterialEntry{
		{KeyPath: "authn.pii_cipher_key", Value: key()},
		{KeyPath: "authn.blind_index_key", Value: key()},
	})
}

// deliveryComposition selects exactly the named components, strictly.
func deliveryComposition(names ...string) pkgcore.ComponentConfig {
	block := pkgcore.ComponentConfig{}
	for _, name := range names {
		block = block.With(name, nil)
	}
	return pkgcore.NewComponentConfig(nil).
		With("components", block).
		With("strict", true)
}

// runDeliveryStages drives Prepare and Construct over reg, failing t with
// the stage and the error.
func runDeliveryStages(t *testing.T, reg *pkgcore.ComponentRegistry) {
	t.Helper()
	ctx := context.Background()
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct: %v", err)
	}
}

// TestDescriptorComposition_PkiAuthn_KeySourceIsDelivered assembles the pki
// and authn module descriptors together over a real database. Plan-time
// resolution matches authn's (*authn.KeySource) requirement against pki's
// (*pki.Service)(nil) declaration and orders pki first, so the construct is
// where the promise must be kept: pki's New puts the *pki.Service it
// declares alongside the *pki.Module it returns.
//
// The failure this pins out: with a declaration that was never delivered,
// the plan resolved -- order and all -- from a promise nothing kept, and the
// construct then failed inside authn's New with an ErrMissingRequirement
// naming the consumer, never the undelivered declaration; nothing in the
// assembly connected the two.
func TestDescriptorComposition_PkiAuthn_KeySourceIsDelivered(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(deliveryDBComponent(deliveryDB(t, "pki-authn.db"))); err != nil {
		t.Fatalf("registering the database stand-in: %v", err)
	}
	reg.Put(deliveryMaterial())
	reg.Put(deliveryComposition("test.delivery.db", "pki", "authn"))

	runDeliveryStages(t, reg)

	keySource, err := pkgcore.Get[authn.KeySource](reg)
	if err != nil {
		t.Fatalf("Get[authn.KeySource] after construction = %v, want the *pki.Service pki's New delivered", err)
	}
	pkiModule, err := pkgcore.Get[*pki.Module](reg)
	if err != nil {
		t.Fatalf("Get[*pki.Module] = %v", err)
	}
	if _, ok := keySource.(*pki.Service); !ok {
		t.Fatalf("the KeySource value is a %T, want the pki module's own *pki.Service", keySource)
	}
	if keySource.(*pki.Service) != pkiModule.Service() {
		t.Error("the delivered KeySource is not the Service of the constructed pki module")
	}
}

// TestDescriptorComposition_MeteringBilling_UsageReaderIsDelivered
// assembles the metering and billing module descriptors together over a real
// database. billing's optional (*billing.UsageReader) requirement matches
// metering's (*metering.Aggregator)(nil) declaration at plan time and orders
// metering first; metering's New puts the Aggregator it declares, so the
// reader billing's New reads is the real one.
//
// The failure this pins out: with the declaration never delivered, billing's
// Get found nothing, treated it as the documented "no reader" absence, and
// constructed silently with a nil reader -- every quota read from then on
// answered billing.usage_reader_unconfigured, with nothing at startup saying
// the composition had promised a reader it never delivered.
func TestDescriptorComposition_MeteringBilling_UsageReaderIsDelivered(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(deliveryDBComponent(deliveryDB(t, "metering-billing.db"))); err != nil {
		t.Fatalf("registering the database stand-in: %v", err)
	}
	reg.Put(deliveryComposition("test.delivery.db", "metering", "billing"))

	runDeliveryStages(t, reg)

	reader, err := pkgcore.Get[billing.UsageReader](reg)
	if err != nil {
		t.Fatalf("Get[billing.UsageReader] after construction = %v, want the *metering.Aggregator metering's New delivered", err)
	}
	meteringModule, err := pkgcore.Get[*metering.Module](reg)
	if err != nil {
		t.Fatalf("Get[*metering.Module] = %v", err)
	}
	if got, ok := reader.(*metering.Aggregator); !ok || got != meteringModule.Aggregator() {
		t.Fatalf("the delivered UsageReader is a %T, want the constructed metering module's own *metering.Aggregator", reader)
	}
}
