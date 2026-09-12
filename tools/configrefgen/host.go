package main

// host.go composes the schema-only host whose frozen configuration schema is
// what the generated reference documents. The configuration reference must be
// generated from the live config schema, never hand-written (root CLAUDE.md's
// documentation discipline), and a live schema only exists after a real host
// has driven a component assembly and called config.Module.Attach -- there is
// no static source form to scan, since every module's ConfigItem/FeatureFlag
// declarations only become one merged schema at Attach time. This host is
// that real host, built for the one job of snapshotting the schema: its
// composition selects the six platform modules whose Register folds items or
// feature flags into the schema (authn, metering, compliance, sharing and pki
// declaring configuration items, org declaring its two feature flags and no
// items -- together the census of the Config and Features declaration sites
// in this repository) plus the config module itself, a host step component
// freezes the schema with Attach once every module's declaration turn has
// run, and the host hands the resulting *config.Service to the caller for
// Describe.
//
// The composition also selects notification, which declares no runtime
// schema of its own but does declare a process-start key: the bootstrap side
// of the reference is enumerated from the same composed set (each module's
// component descriptor carries its keys, on the BootstrapKeys seat or as
// ConfigSchema derive fields), so the composed set is
// every platform module whose declarations this reference renders, on either
// layer, and no module's declaration can reach the reference without being
// composed here.
//
// The composition deliberately stops at the platform modules. A
// config reference committed at the repository's docs/ root documents the
// PLATFORM configuration surface a speed-based application receives from the
// modules it imports, not any one application's own items, so an
// application's declarations stay out of the composition and out of the
// reference. A host that wants its own complete reference (own items
// included) runs the same assembly against its own composition -- go/config's
// RenderMarkdown doc comment shows the shape.
//
// The database is a throwaway in-memory SQLite: every module's Register
// performs no I/O by contract, config's Attach only wires its Service (the
// schema fold is pure), the composition disables the anti-loss poller with
// its poll_interval block, and the assembly stops after the Init stage (no
// Start: nothing here serves, so no listener or worker is begun), so no
// table needs to exist for the snapshot to be exact. The db handle exists
// because Module constructors and Attach require one -- it is never queried.
// The declared key material every selected module consumes resolves from the
// fixed, non-secret snapshotKey table below, so the snapshot is
// deterministic, and no key can leak into any output because Describe
// redacts a Sensitive item's default at the boundary (describe.go).

import (
	"context"
	"embed"
	"fmt"
	"io"

	"gorm.io/gorm"

	speedapp "github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"

	// The composed platform modules' own packages are blank imports: each
	// one's component descriptor registers itself from its package init, so
	// importing the package is what puts the module into this binary, and
	// the assembly's composition (not this file) selects it.
	_ "github.com/vislake/speed/go/authn"
	_ "github.com/vislake/speed/go/compliance"
	_ "github.com/vislake/speed/go/dbkit/audit"
	_ "github.com/vislake/speed/go/metering"
	_ "github.com/vislake/speed/go/pki"
	_ "github.com/vislake/speed/go/sharing"
)

// envPrefix is the loader prefix this host's own environment surface is
// derived under, deliberately different from the platform default: the
// snapshot must not pick up a variable meant for a real application, and
// naming a dedicated prefix makes that a property of the composition rather
// than of whatever environment the generator happens to run in.
const envPrefix = "CONFIGREFGEN_"

// platformModules names the composed platform modules in the order the
// reference renders their declared keys: every module whose declarations
// reach either configuration layer of the reference. The audit module is a
// required construction dependency of compliance rather than a declaring
// module, so it is selected by the composition but stays out of this list.
var platformModules = []string{
	"authn",
	"org",
	"notification",
	"pki",
	"metering",
	"sharing",
	"compliance",
	"config",
}

// snapshotKey is the 32-byte AES key the schema-only host seals its cipher
// with, and the fixed material every declared key of the composed modules
// resolves to. See the package comment for why a fixed literal is safe here.
var snapshotKey = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

// neverQueue is a jobs.Queue that is never called. The selected modules'
// components declare the queue as a construction requirement even when
// nothing this host does would ever enqueue; the stub exists to satisfy that
// requirement, and Register's no-I/O contract means its methods are never
// reached during the snapshot.
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
// addresses. The selected modules' components require the module to exist -- a
// delivery's addresses are resolved at send time, and this host never sends
// -- so every user resolving to no addresses is the honest answer here.
type emptyAddressResolver struct{}

// Resolve implements notification.UserAddressResolver.
func (emptyAddressResolver) Resolve(context.Context, string) (notification.UserAddresses, error) {
	return notification.UserAddresses{}, nil
}

// hostSnapshot is what the composed schema-only host hands back: the frozen
// schema snapshot (the dynamic layer's source) and every bootstrap key the
// composed modules declared on their component descriptors (the platform
// bootstrap keys' source).
type hostSnapshot struct {
	service      *config.Service
	declaredKeys []pkgcore.BootstrapKey
}

// snapshotHostConfig is this host's own bootstrap configuration target: the
// tool declares no host keys of its own, so the loader fills an empty struct
// while resolving the selected modules' declared keys through the same
// load.
type snapshotHostConfig struct{}

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

	reg := pkgcore.NewComponentRegistry()
	for _, c := range hostComponents(db, cipher) {
		if err = reg.Register(c); err != nil {
			return nil, err
		}
	}
	// notification is a host override: its shipped descriptor deliberately
	// leaves the contact-address indexers to the host, so the descriptor
	// this host selects is the module's own declaration with this host's
	// construction (see notificationComponent).
	notificationComp, err := notificationComponent(reg)
	if err != nil {
		return nil, err
	}
	if err = reg.Register(notificationComp); err != nil {
		return nil, err
	}
	// config is a register-only override: the shipped descriptor attaches
	// its schema from its own Init, which would freeze a snapshot missing
	// every module ordered after it (see configComponent).
	configComp, err := configComponent(reg)
	if err != nil {
		return nil, err
	}
	if err = reg.Register(configComp); err != nil {
		return nil, err
	}
	// authn's KeySource requirement is answered by the composition itself:
	// the selected pki component's New delivers its *pki.Service, the signer
	// lifecycle authn's construction reads, so the by-type context already
	// carries exactly one value for the token and this host adds none.

	spec := speedapp.LoadSpec{
		Host: &snapshotHostConfig{},
		// The loader's sources are pinned for determinism: no flag source
		// (empty argument lists on both scans, so the generator's own --check
		// flag never reaches a load), a dedicated environment prefix, and
		// the fixed material table every declared key falls back to.
		Options: []speedapp.ConfigOption{
			speedapp.ConfigArgs([]string{}),
			speedapp.ConfigEnvPrefix(envPrefix),
			speedapp.ConfigDevDefaults(snapshotDevDefaults()),
		},
		Args:      []string{},
		Overrides: &speedapp.CompositionOverrides{Config: composition()},
	}
	if err = speedapp.Load(ctx, reg, spec); err != nil {
		return nil, err
	}

	// The first four stages, deliberately not Start: the snapshot needs the
	// declaration seats folded (Init) and nothing begun -- a Start callback
	// would launch the selected modules' workers against a database this
	// host never migrates.
	for _, stage := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"prepare", reg.Prepare},
		{"construct", reg.Construct},
		{"verify", reg.Verify},
		{"init", reg.Init},
	} {
		if err = stage.run(ctx); err != nil {
			return nil, fmt.Errorf("configrefgen: assembly stage %s: %w", stage.name, err)
		}
	}

	// The frozen schema snapshot: the host step component's Init ran
	// config.Module.Attach once every module's declaration turn had passed
	// and published the Service, which is the reference's source.
	svc, err := pkgcore.Get[*config.Service](reg)
	if err != nil {
		return nil, fmt.Errorf("configrefgen: read the attached config service: %w", err)
	}
	return &hostSnapshot{service: svc, declaredKeys: declaredBootstrapKeys(reg)}, nil
}

// hostComponents returns this host's own components: one provider per
// contract token the selected module components require but nothing else
// selected provides -- the database handle, the configuration cipher, the
// SMS transport, the job queue and the notification address resolver -- and
// the step component that freezes the schema snapshot (attachComponentName).
// The values behind the providers are the same fixed stand-ins the host
// always used; only their wiring moved from constructor options to the
// by-type context.
func hostComponents(db *gorm.DB, cipher *dbkit.Cipher) []pkgcore.Component {
	return []pkgcore.Component{
		providerComponent("configrefgen.db", (*gorm.DB)(nil), func() any { return db }),
		providerComponent("configrefgen.cipher", (*dbkit.Cipher)(nil), func() any { return cipher }),
		providerComponent("configrefgen.sms", (*pkgcore.SMSSender)(nil), func() any { return pkgcore.NewConsoleSMSSender(io.Discard) }),
		providerComponent("configrefgen.queue", (*jobs.Queue)(nil), func() any { return neverQueue{} }),
		providerComponent("configrefgen.addresses", (*notification.UserAddressResolver)(nil), func() any { return emptyAddressResolver{} }),
		providerComponent("configrefgen.linkbuilder", (*org.InvitationLinkBuilder)(nil), func() any {
			return org.InvitationLinkBuilder(func(_ context.Context, token string) (string, error) {
				return "https://configrefgen.invalid/invitations/accept?token=" + token, nil
			})
		}),
		attachStepComponent(),
	}
}

// providerComponent builds a host component that declares one contract token
// and produces the value it stands for.
func providerComponent(name string, token any, product func() any) pkgcore.Component {
	return pkgcore.Component{
		Name:     name,
		Provides: []any{token},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return product(), nil
		},
	}
}

// notificationComponent returns notification's descriptor with this host's
// own construction: the same declaration the module ships -- Requires,
// Provides, assets -- with the two blind indexers over its encrypted
// contact addresses built from the fixed snapshot key. Those indexers are
// the piece the module's shipped descriptor deliberately leaves to the host
// (its docs: a module built from it alone fails Register's
// ErrContactEmailIndexerRequired), and they are what this host supplies
// here, exactly as the pre-assembly host supplied them through
// WithContactEmailIndexer/WithContactPhoneIndexer.
func notificationComponent(reg *pkgcore.ComponentRegistry) (pkgcore.Component, error) {
	var base pkgcore.Component
	for _, c := range pkgcore.RegisteredComponents(reg) {
		if c.Name == "notification" {
			base = c
			break
		}
	}
	if base.Name == "" {
		return pkgcore.Component{}, fmt.Errorf("configrefgen: component %q has no registered descriptor to override", "notification")
	}
	c := base
	c.Name = "configrefgen.notification"
	// The override carries a host name, and a component's locale resources
	// must be prefixed with its own name, so the module's locales stay with
	// the module's descriptor (the reference renders none of them anyway).
	c.Locales = embed.FS{}
	c.New = func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		queue, err := pkgcore.Get[jobs.Queue](reg)
		if err != nil {
			return nil, err
		}
		sms, err := pkgcore.Get[pkgcore.SMSSender](reg)
		if err != nil {
			return nil, err
		}
		resolver, err := pkgcore.Get[notification.UserAddressResolver](reg)
		if err != nil {
			return nil, err
		}
		emailIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn, snapshotKey, dbkit.NormalizeEmail)
		if err != nil {
			return nil, err
		}
		phoneIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn, snapshotKey, dbkit.NormalizePhoneE164)
		if err != nil {
			return nil, err
		}
		return notification.NewModule(db,
			notification.WithSMSSender(sms),
			notification.WithMailFrom("notifications@configrefgen.invalid"),
			notification.WithContactEmailIndexer(emailIndexer),
			notification.WithContactPhoneIndexer(phoneIndexer),
			notification.WithDeliveryQueue(queue),
			notification.WithUserAddressResolver(resolver),
		), nil
	}
	return c, nil
}

// configComponent returns config's descriptor declaring only: its schema
// snapshot is frozen later, in the attach step (attachStepComponent), after
// every module's declaration turn. The shipped descriptor would call Attach
// from its own Init, freezing whatever the schema held at that point of the
// dependency order -- a snapshot that misses the declarations of every
// module ordered after config.
func configComponent(reg *pkgcore.ComponentRegistry) (pkgcore.Component, error) {
	var base pkgcore.Component
	for _, c := range pkgcore.RegisteredComponents(reg) {
		if c.Name == "config" {
			base = c
			break
		}
	}
	if base.Name == "" {
		return pkgcore.Component{}, fmt.Errorf("configrefgen: component %q has no registered descriptor to override", "config")
	}
	c := base
	c.Name = "configrefgen.config"
	c.Locales = embed.FS{}
	c.Init = func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
		m, ok := instance.(*config.Module)
		if !ok {
			return fmt.Errorf("configrefgen: the config component was handed a %T instance, want *config.Module", instance)
		}
		return m.Register(reg)
	}
	return c, nil
}

// attachStepComponent returns the host step that freezes the schema snapshot:
// it requires the config module, so the assembly constructs it after the
// config component, and the composition lists it last, so its Init runs
// config.Module.Attach after every module's declaration turn -- the one
// moment the fold sees a complete schema. It publishes the attached
// *config.Service for the caller.
func attachStepComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:     "configrefgen.attach",
		Requires: []pkgcore.Requirement{{Token: (*config.Module)(nil)}},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return new(int), nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			configModule, err := pkgcore.Get[*config.Module](reg)
			if err != nil {
				return err
			}
			svc, err := configModule.Attach(reg)
			if err != nil {
				return err
			}
			reg.Put(svc)
			return nil
		},
	}
}

// composition returns the host's composition configuration: the platform
// modules, the construction dependency compliance requires (audit), the
// host's own providers and the attach step, in this order -- the block's
// order is the plan's tie-break, so the step that freezes the schema is
// listed after every module and runs its Init last.
func composition() pkgcore.ComponentConfig {
	components := pkgcore.ComponentConfig{}.
		// The engine's observability component participates by default;
		// this host deselects it -- the snapshot needs no telemetry, and
		// the default would otherwise initialize exporters and write their
		// output to this command's stdout.
		With("observability", false).
		// The four in-process infrastructure implementations a bare
		// standalone host runs: the audit persister subscribes on the bus
		// during its Register, and the remaining three are the old host's
		// own zero-configuration defaults, kept so the composed object
		// graph matches what the snapshot was taken over.
		With("eventbus.memory", nil).
		With("kv.memory", nil).
		With("mailer.console", nil).
		With("objectstore.local", nil).
		With("configrefgen.db", nil).
		With("configrefgen.cipher", nil).
		With("configrefgen.sms", nil).
		With("configrefgen.queue", nil).
		With("configrefgen.addresses", nil).
		With("configrefgen.linkbuilder", nil).
		With("audit", nil).
		With("authn", nil).
		With("org", pkgcore.ComponentConfig{}.
			With("mail_from", "invitations@configrefgen.invalid")).
		// notification is selected as this host's override (see
		// notificationComponent): the shipped descriptor alone cannot boot
		// without the host-supplied contact indexers.
		With("notification", false).
		With("configrefgen.notification", nil).
		With("pki", nil).
		With("metering", nil).
		With("sharing", nil).
		With("compliance", nil).
		// config is selected as this host's register-only override; the
		// poller is disabled (zero interval), the throwaway database is
		// never migrated and never queried.
		With("config", false).
		With("configrefgen.config", pkgcore.ComponentConfig{}.
			With("poll_interval", 0)).
		With("configrefgen.attach", nil)
	return pkgcore.ComponentConfig{}.
		With("deployment", "standalone").
		With("strict", true).
		With("components", components)
}

// snapshotDevDefaults returns the declared defaults table the loader
// resolves the composed modules' declared key material through. Every entry
// is the same fixed snapshotKey: nothing here encrypts or indexes real rows
// and Describe never renders key material, so one deterministic value serves
// every declared key (the old direct-construction host reused the same
// literal for the same reason).
func snapshotDevDefaults() map[string][]byte {
	return map[string][]byte{
		"authn.blind_index_key":          snapshotKey,
		"authn.pii_cipher_key":           snapshotKey,
		"config.cipher_key":              snapshotKey,
		"notification.contact_index_key": snapshotKey,
		"org.invitation_email_index_key": snapshotKey,
		"pki.local_key_cipher_key":       snapshotKey,
	}
}

// declaredBootstrapKeys returns the composed set's declared process-start
// keys: each module states its keys on its component descriptor, either as a
// BootstrapKeys declaration or as ConfigSchema derive fields, and the
// assembly's loader resolves both before anything is constructed. The census
// reads the descriptors of exactly the modules this composition selects, in
// platformModules order, and renders a schema-declared key at the final key
// path its field resolves at -- the address an operator supplies it by.
func declaredBootstrapKeys(reg *pkgcore.ComponentRegistry) []pkgcore.BootstrapKey {
	descriptors := make(map[string]pkgcore.Component)
	for _, c := range pkgcore.RegisteredComponents(reg) {
		if c.Name == c.Module && c.Module != "" {
			descriptors[c.Name] = c
		}
	}
	var keys []pkgcore.BootstrapKey
	for _, name := range platformModules {
		descriptor := descriptors[name]
		keys = append(keys, descriptor.BootstrapKeys...)
		keys = append(keys, schemaDeclaredKeys(name, descriptor.ConfigSchema)...)
	}
	return keys
}

// schemaDeclaredKeys returns the key material a component's ConfigSchema
// declares, one BootstrapKey-shaped entry per derive-tagged field, keyed by
// the field's final key path (the component's namespace prefix, then the
// field's local path -- the same path the assembly's loader resolves the
// field at). The field's ConfigDocs entry supplies the rendered contract
// text and unset fallback; a schema that declares no key material (or
// fails to describe, which the assembly's own validation would refuse)
// contributes nothing.
func schemaDeclaredKeys(componentName string, schema any) []pkgcore.BootstrapKey {
	fields, err := pkgcore.DescribeComponentSchema(componentName, schema)
	if err != nil {
		return nil
	}
	var keys []pkgcore.BootstrapKey
	for _, field := range fields {
		if !field.Derive {
			continue
		}
		keys = append(keys, pkgcore.BootstrapKey{
			Key:         field.Key,
			Format:      "hexkey",
			Default:     field.Doc.Default,
			Sensitive:   field.Sensitive,
			Description: field.Doc.Description,
			Group:       field.Group,
		})
	}
	return keys
}
