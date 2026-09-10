package main

// host.go composes the schema-only host whose frozen configuration schema is
// what the generated reference documents. The configuration reference must be
// generated from the live config schema, never hand-written (root CLAUDE.md's
// documentation discipline), and a live schema only exists after a real host
// has run Kernel.Bootstrap and config.Module.Attach -- there is no static
// source form to scan, since every module's ConfigItem/FeatureFlag
// declarations only become one merged schema at Attach time. This host is
// that real host, built for the one job of snapshotting the schema: it
// registers the six platform modules whose Register folds items or feature
// flags into the schema (authn, metering, compliance, sharing and pki
// declaring configuration items, org declaring its two feature flags and no
// items -- together the census of reg.Config.Add and reg.Features.Add
// declaration sites in this repository) plus the config module itself,
// freezes the schema with Attach, and hands the resulting *config.Service
// to the caller for Describe.
//
// The composition also registers notification, which declares no schema of
// its own but does declare a process-start key: the bootstrap side of the
// reference is enumerated from the same boot (reg.Bootstrap.Keys()), so the
// composed set is every platform module whose declarations this reference
// renders, on either layer, and no module's declaration can reach the
// reference without being composed here.
//
// The composition deliberately stops at the platform modules. The reference
// app's own notes module registers its own app-owned items (brand.site_name
// and support.reply_email plus its two feature flags); a config reference
// committed at the repository's docs/ root documents the PLATFORM
// configuration surface a speed-based application receives from the modules
// it imports, not one example app's demo items, so notes stays out of the
// composition and its items out of the reference. A host that wants its own
// complete reference (own items included) runs the same two calls against
// its own composition -- go/config's RenderMarkdown doc comment shows the
// shape.
//
// The database is a throwaway in-memory SQLite: every module's Register
// performs no I/O by contract, config's Attach only wires its Service (the
// schema fold is pure), and the anti-loss poller is disabled with
// WithPollInterval(0), so no table needs to exist for the snapshot to be
// exact. The db handle exists because Module constructors and Attach require
// one -- it is never queried. A cipher is mandatory because authn registers
// Sensitive items and Attach refuses a cipher-less schema (ErrCipherRequired
// in module.go); the key below is a fixed literal so the snapshot is
// deterministic, and its value can never leak into any output because
// Describe redacts a Sensitive item's default at the boundary (describe.go).

import (
	"context"
	"crypto"
	"fmt"
	"io"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/sharing"
)

// snapshotKey is the 32-byte AES key the schema-only host seals its cipher
// with, and the HMAC key it hands authn's blind indexer. See the package
// comment for why a fixed literal is safe here.
var snapshotKey = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

// dummyKeySource is an authn.KeySource that is never consulted: its methods
// satisfy the interface structurally (stdlib types only, exactly as
// go/authn/token.go's KeySource contract requires) and fail or no-op instead
// of issuing. authn.NewModule validates the option's presence eagerly;
// Register performs no I/O and never calls the source, so a source that
// refuses every call is all a schema snapshot needs.
type dummyKeySource struct{}

// EnsurePurpose implements authn.KeySource.
func (dummyKeySource) EnsurePurpose(context.Context, string, string, time.Duration) error { return nil }

// ActiveSigner implements authn.KeySource.
func (dummyKeySource) ActiveSigner(context.Context, string) (string, string, func(context.Context, []byte) ([]byte, error), error) {
	return "", "", nil, nil
}

// VerificationKeys implements authn.KeySource, with the exact anonymous
// struct element go/authn/token.go's interface declaration names (its
// Public element is the stdlib crypto.PublicKey alias).
func (dummyKeySource) VerificationKeys(context.Context, string) ([]struct {
	KID       string
	Algorithm string
	Public    crypto.PublicKey
}, error,
) {
	return nil, nil
}

// neverQueue is a jobs.Queue that is never called. compliance.Module.Register
// refuses to proceed without a queue (ErrQueueRequired) even when nothing
// this host does would ever enqueue; the stub exists to satisfy that
// wiring-time requirement, and Register's no-I/O contract means its methods
// are never reached during the snapshot.
type neverQueue struct{}

// Enqueue implements jobs.Queue.
func (neverQueue) Enqueue(context.Context, jobs.Task, ...jobs.EnqueueOption) (jobs.JobID, error) {
	panic("configrefgen: the schema snapshot never enqueues")
}

// Get implements jobs.Queue.
func (neverQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) {
	panic("configrefgen: the schema snapshot never reads a job")
}

// Cancel implements jobs.Queue.
func (neverQueue) Cancel(context.Context, jobs.JobID) error {
	panic("configrefgen: the schema snapshot never cancels a job")
}

// emptyAddressResolver is a notification.UserAddressResolver that knows no
// addresses. Register only requires the seam to exist -- a delivery's
// addresses are resolved at send time, and this host never sends -- so every
// user resolving to no addresses is the honest answer here.
type emptyAddressResolver struct{}

// Resolve implements notification.UserAddressResolver.
func (emptyAddressResolver) Resolve(context.Context, string) (notification.UserAddresses, error) {
	return notification.UserAddresses{}, nil
}

// hostSnapshot is what the composed schema-only host hands back: the frozen
// schema snapshot (the dynamic layer's source), every bootstrap key the
// composed modules declared on the registry's Bootstrap seat (the platform
// bootstrap keys' source), and the names of the modules composed, so the
// reference can state which of them declared no bootstrap key at all.
type hostSnapshot struct {
	service         *config.Service
	declaredKeys    []pkgcore.BootstrapKey
	composedModules []string
}

// schemaHost composes the schema-only host and freezes its configuration
// schema.
func schemaHost(ctx context.Context) (*hostSnapshot, error) {
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:configrefgen?mode=memory&cache=shared",
	})
	if err != nil {
		return nil, err
	}

	cipher, err := dbkit.NewCipher(snapshotKey)
	if err != nil {
		return nil, err
	}

	authnModule, err := authn.NewModule(db,
		authn.WithKeySource(dummyKeySource{}),
		authn.WithBlindIndexKey(snapshotKey),
	)
	if err != nil {
		return nil, err
	}

	// orgModule carries the three options org's Register validates even
	// though a schema snapshot neither indexes a real address nor sends
	// mail: the mandatory invitation-address blind indexer
	// (ErrEmailIndexerRequired without one) and -- with the
	// org.invitation_email flag at its declared default of on -- a sender
	// address and a link builder (ErrInvitationMailRequired without both).
	// The indexer key is the same fixed snapshotKey the cipher and authn's
	// indexer reuse: a real host derives a separate APP_ORG_INDEX_KEY
	// material, but nothing here encrypts or indexes real rows and Describe
	// never renders a key, so the reuse leaks nothing (the package comment
	// above states the fixed-literal reasoning).
	orgEmailIndexer, err := dbkit.NewBlindIndexer(org.EmailIndexColumn, snapshotKey, dbkit.NormalizeEmail)
	if err != nil {
		return nil, err
	}
	orgModule := org.NewModule(db,
		org.WithEmailIndexer(orgEmailIndexer),
		org.WithMailFrom("invitations@configrefgen.invalid"),
		org.WithInvitationLinkBuilder(func(_ context.Context, token string) (string, error) {
			return "https://configrefgen.invalid/invitations/accept?token=" + token, nil
		}),
	)

	// notification is here for its bootstrap-key declaration, not for a
	// schema: its Register declares no items and no flags, so it changes
	// nothing the dynamic layer renders. Its constructor and Register demand
	// the five seams below; each is a value this snapshot never exercises --
	// a console SMS sender writing to io.Discard, the same two blind indexers
	// a real host builds from one key, the never-called queue, and a resolver
	// that knows no addresses.
	contactEmailIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn, snapshotKey, dbkit.NormalizeEmail)
	if err != nil {
		return nil, err
	}
	contactPhoneIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn, snapshotKey, dbkit.NormalizePhoneE164)
	if err != nil {
		return nil, err
	}
	notificationModule := notification.NewModule(db,
		notification.WithSMSSender(pkgcore.NewConsoleSMSSender(io.Discard)),
		notification.WithMailFrom("notifications@configrefgen.invalid"),
		notification.WithContactEmailIndexer(contactEmailIndexer),
		notification.WithContactPhoneIndexer(contactPhoneIndexer),
		notification.WithDeliveryQueue(neverQueue{}),
		notification.WithUserAddressResolver(emptyAddressResolver{}),
	)

	modules := []pkgcore.Module{
		authnModule,
		orgModule,
		notificationModule,
		pki.NewModule(db),
		metering.NewModule(db),
		sharing.NewModule(db),
		compliance.NewModule(audit.NewRepository(db), compliance.WithQueue(neverQueue{})),
		config.NewModule(db,
			config.WithCipher(cipher),
			// The anti-loss poller would query a configs table this
			// throwaway database never migrates; zero disables it, which is
			// exactly what a snapshot needs (WithPollInterval's doc comment:
			// "Zero disables the poller entirely").
			config.WithPollInterval(0),
		),
	}

	reg, err := pkgcore.NewKernel().Bootstrap(ctx, modules...)
	if err != nil {
		return nil, err
	}

	// Attach freezes the schema snapshot from what every module registered
	// during Bootstrap -- reg.Config.Items() and reg.Features.Flags() -- and
	// returns the Service whose Describe is the reference's source. Exactly
	// one module in the set is the config module (the last one passed in);
	// the comma-ok assertion is deliberate so a reordering of the module
	// list above fails here with a nameable error instead of panicking.
	configModule, ok := modules[len(modules)-1].(*config.Module)
	if !ok {
		return nil, fmt.Errorf("configrefgen: last module in the bootstrap set is %T, want *config.Module", modules[len(modules)-1])
	}
	svc, err := configModule.Attach(reg)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(modules))
	for _, module := range modules {
		names = append(names, module.Name())
	}
	return &hostSnapshot{service: svc, declaredKeys: reg.Bootstrap.Keys(), composedModules: names}, nil
}
