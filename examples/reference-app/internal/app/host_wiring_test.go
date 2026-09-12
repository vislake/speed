package app

// This file pins the host's wiring components and the composition this host
// builds: the override components carry their modules' declarations
// unchanged, the composition selects the implementations the resolved
// ServerConfig names (and no others), and the whole composition assembles
// over a real database into a closeable application.

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"testing"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	speedapp "github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"

	"gorm.io/gorm"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
)

// overriddenModules names the modules whose descriptors this host overrides
// by copying the module's own registered descriptor: the modules this app
// constructs beyond what a descriptor carries, plus rbac, whose catalog
// attach must run in the post-bootstrap step for this host's Init-stage
// consumers.
var overriddenModules = []string{"authn", "rbac", "notification", "integration", "ai-gateway"}

// TestOverrideComponents_CarryTheirDeclarations pins the override contract
// against the module packages' own descriptors: an override is the
// registered descriptor with only its name, its locale resources (which
// cannot ride a renamed component -- see hostCatalog) and its New callback
// replaced, so every declaration, capability, asset and lifecycle callback
// is the module package's own.
func TestOverrideComponents_CarryTheirDeclarations(t *testing.T) {
	b := newServerBuild(ServerConfig{Port: "8080"})
	reg := pkgcore.NewComponentRegistry()
	components, err := b.hostWiringComponents(reg)
	if err != nil {
		t.Fatalf("hostWiringComponents(): %v", err)
	}
	byName := make(map[string]pkgcore.Component, len(components))
	for _, c := range components {
		if _, dup := byName[c.Name]; dup {
			t.Fatalf("two components share the name %q", c.Name)
		}
		byName[c.Name] = c
	}

	for _, module := range overriddenModules {
		name := hostComponentPrefix + module
		override, ok := byName[name]
		if !ok {
			t.Errorf("no override component %q is registered", name)
			continue
		}
		base := registeredByName(t, reg, module)
		if override.Module != base.Module {
			t.Errorf("override %q declares module %q, want the module's own %q", name, override.Module, base.Module)
		}
		if override.Name != hostComponentPrefix+module {
			t.Errorf("override name = %q, want %q", override.Name, hostComponentPrefix+module)
		}
		if !reflect.DeepEqual(override.Locales, base.Locales) {
			t.Errorf("override %q Locales differs from the module's own resources; the catalog merges them under the MODULE name (hostCatalog), so an override carries them unchanged", name)
		}
		if len(override.Requires) < len(base.Requires) || !reflect.DeepEqual(override.Requires[:len(base.Requires)], base.Requires) {
			t.Errorf("override %q Requires = %v, want the module's own %v (plus the host's own additional edges)", name, override.Requires, base.Requires)
		}
		for _, extra := range override.Requires[len(base.Requires):] {
			if extra.Optional {
				t.Errorf("override %q adds an optional requirement %v; the host's own edges order its construction and must be mandatory", name, extra.Token)
			}
		}
		if !reflect.DeepEqual(override.Provides, base.Provides) {
			t.Errorf("override %q Provides = %v, want the module's own %v", name, override.Provides, base.Provides)
		}
		if !reflect.DeepEqual(override.SystemPurposes, base.SystemPurposes) {
			t.Errorf("override %q SystemPurposes = %v, want the module's own %v", name, override.SystemPurposes, base.SystemPurposes)
		}
		if !reflect.DeepEqual(override.BootstrapKeys, base.BootstrapKeys) {
			t.Errorf("override %q BootstrapKeys = %v, want the module's own %v", name, override.BootstrapKeys, base.BootstrapKeys)
		}
		if override.Capabilities != base.Capabilities {
			t.Errorf("override %q Capabilities = %v, want the module's own %v unchanged", name, override.Capabilities, base.Capabilities)
		}
		if reflect.TypeOf(override.ConfigSchema) != reflect.TypeOf(base.ConfigSchema) {
			t.Errorf("override %q ConfigSchema = %T, want the module's own schema", name, override.ConfigSchema)
		}
		if (override.Prepare == nil) != (base.Prepare == nil) {
			t.Errorf("override %q disagrees with the module's descriptor about declaring a Prepare callback", name)
		}
		if override.Init == nil {
			t.Errorf("override %q must carry the module's Init callback", name)
		}
		if override.New == nil {
			t.Errorf("override %q declares no New callback", name)
		}
	}

	// The database component is the host's own descriptor rather than a
	// copy: it provides the connection product, implements the db module
	// and requires the event bus whose identity the write-capture scope
	// needs.
	db, ok := byName[hostComponentPrefix+"db"]
	if !ok {
		t.Fatal("no host database component is registered")
	}
	if db.Module != "db" || len(db.Provides) != 1 || db.Provides[0] != (*gorm.DB)(nil) {
		t.Errorf("the host database component declares Module %q Provides %v, want the db module's (*gorm.DB) product", db.Module, db.Provides)
	}
	if len(db.Requires) != 1 || db.Requires[0].Token != (*pkgcore.EventBus)(nil) || db.Requires[0].Optional {
		t.Errorf("the host database component Requires = %v, want one mandatory (*pkgcore.EventBus) requirement", db.Requires)
	}
}

// registeredByName returns the module package's own registered descriptor.
func registeredByName(t *testing.T, reg *pkgcore.ComponentRegistry, module string) pkgcore.Component {
	t.Helper()
	c, ok := registeredModuleComponent(reg, module)
	if !ok {
		t.Fatalf("module %q has no registered component descriptor", module)
	}
	return c
}

// TestHostWiringComponents_ProvideOneSubjectResolverValue pins the family
// constraint the structurally identical seam interfaces impose: org,
// notification, integration and notes declare resolvers with the same
// method set, so any two provided values in that family would make every
// read of it ambiguous. Exactly one selected component may provide it.
func TestHostWiringComponents_ProvideOneSubjectResolverValue(t *testing.T) {
	b := newServerBuild(ServerConfig{})
	components, err := b.hostWiringComponents(pkgcore.NewComponentRegistry())
	if err != nil {
		t.Fatalf("hostWiringComponents(): %v", err)
	}
	token := reflect.TypeFor[org.SubjectResolver]()
	providers := 0
	for _, c := range components {
		for _, provided := range c.Provides {
			// A Provides entry is a typed nil pointer; the family check is
			// whether the type it points at is the seam interface. One
			// component declaring several of the family's tokens counts
			// once.
			if reflect.TypeOf(provided).Elem().AssignableTo(token) {
				providers++
				break
			}
		}
	}
	if providers != 1 {
		t.Fatalf("%d host components provide a value in the subject-resolver family, want exactly one", providers)
	}
}

// TestHostWiringComponents_ConditionalHostImplementations pins the two
// components that stand in for a registered implementation only when the
// boot supplies one: the in-process mailer capture double and the SMS
// sender writing to the host's declared output.
func TestHostWiringComponents_ConditionalHostImplementations(t *testing.T) {
	plain := newServerBuild(ServerConfig{})
	components, err := plain.hostWiringComponents(pkgcore.NewComponentRegistry())
	if err != nil {
		t.Fatalf("hostWiringComponents() without a mailer or an SMS output: %v", err)
	}
	for _, c := range components {
		if c.Name == hostComponentPrefix+"mailer" || c.Name == hostComponentPrefix+"sms" {
			t.Errorf("component %q is registered for a boot that supplies neither a mailer nor an SMS output", c.Name)
		}
	}

	supplied := newServerBuild(ServerConfig{Mailer: pkgcore.NewConsoleMailer(), SMSOutput: &bytes.Buffer{}})
	components, err = supplied.hostWiringComponents(pkgcore.NewComponentRegistry())
	if err != nil {
		t.Fatalf("hostWiringComponents() with a mailer and an SMS output: %v", err)
	}
	names := make(map[string]bool, len(components))
	for _, c := range components {
		names[c.Name] = true
	}
	for _, want := range []string{hostComponentPrefix + "mailer", hostComponentPrefix + "sms"} {
		if !names[want] {
			t.Errorf("component %q is missing for a boot that supplies it", want)
		}
	}
}

// selectedEntry reports the value a composition block carries for name, and
// whether the block selects it (nil or a block value selects, false
// deselects).
func selectedEntry(block pkgcore.ComponentConfig, name string) (any, bool) {
	value, ok := block.Get(name)
	if !ok {
		return nil, false
	}
	if off, isBool := value.(bool); isBool && !off {
		return nil, false
	}
	return value, true
}

// componentsBlock returns the composition's components subtree.
func componentsBlock(t *testing.T, whole pkgcore.ComponentConfig) pkgcore.ComponentConfig {
	t.Helper()
	value, ok := whole.Get("components")
	if !ok {
		t.Fatal("the composition carries no components block")
	}
	block, isBlock := value.(pkgcore.ComponentConfig)
	if !isBlock {
		t.Fatalf("the components entry carries a %T, want a ComponentConfig", value)
	}
	return block
}

// TestComposition_SelectsTheResolvedImplementations pins the composition
// block: the deployment mode and strict mode, the in-process
// implementations for an unset ServerConfig, the configured implementations
// for a resolved one, and the observability selection that differs between
// the two drives.
func TestComposition_SelectsTheResolvedImplementations(t *testing.T) {
	t.Run("the plain boot", func(t *testing.T) {
		b := newServerBuild(ServerConfig{DeploymentMode: pkgcore.DeploymentModeStandalone})
		whole := b.composition(false)
		if got, _ := whole.Get("deployment"); got != string(pkgcore.DeploymentModeStandalone) {
			t.Errorf("deployment = %v, want %q", got, pkgcore.DeploymentModeStandalone)
		}
		if got, _ := whole.Get("strict"); got != true {
			t.Errorf("strict = %v, want true", got)
		}
		block := componentsBlock(t, whole)
		for _, want := range []string{
			"eventbus.memory", "kv.memory", "mailer.console", "objectstore.local", "sms.console",
			"queue.standalone", "notes", "demo",
			"compliance", "storage", "sharing",
			"config", hostComponentPrefix + "rbac", "admin",
			"pki", hostComponentPrefix + "signer.local",
			"billing", "metering", "audit",
			hostComponentPrefix + "db", hostComponentPrefix + "crypto", hostComponentPrefix + "authn",
			hostComponentPrefix + "org",
			hostComponentPrefix + "notification", hostComponentPrefix + "integration", hostComponentPrefix + "ai-gateway",
			hostComponentPrefix + "subject-resolvers", hostComponentPrefix + "attestation",
			hostComponentPrefix + "app", hostComponentPrefix + "worker", hostComponentPrefix + "pre_serve",
			hostComponentPrefix + "post_bootstrap", hostComponentPrefix + "post_attach",
		} {
			if _, ok := selectedEntry(block, want); !ok {
				t.Errorf("the plain boot does not select %q", want)
			}
		}
		for _, off := range []string{
			"db.sqlite", "authn", "org", "rbac", "notification", "integration", "ai-gateway",
			"signer.local",
			"observability",
		} {
			value, ok := block.Get(off)
			if !ok || value != false {
				t.Errorf("entry %q = %v, want the explicit deselection of the built-in the host stands in for", off, value)
			}
		}
		// The ai-gateway provider members are credential-conditioned: a
		// plain boot configures no provider credential, so neither member is
		// selected and the provider names stay on the package-level
		// registries -- the module's documented fallback.
		for _, absent := range []string{"chat.openai-compatible", "image.openai-compatible"} {
			if value, ok := block.Get(absent); ok {
				t.Errorf("the plain boot carries entry %q = %v, want it absent: the member is selected only when its platform credential is configured", absent, value)
			}
		}
	})

	t.Run("the provider-configured boot", func(t *testing.T) {
		b := newServerBuild(ServerConfig{
			AIGatewayBaseURL:      "https://chat-upstream.example.test/v1",
			AIGatewayAPIKey:       "sk-assembly-chat",
			AIGatewayImageBaseURL: "https://image-upstream.example.test/v1",
			AIGatewayImageAPIKey:  "sk-assembly-image",
		})
		block := componentsBlock(t, b.composition(false))
		for _, tc := range []struct {
			name    string
			baseURL string
			apiKey  string
		}{
			{"chat.openai-compatible", b.cfg.AIGatewayBaseURL, b.cfg.AIGatewayAPIKey},
			{"image.openai-compatible", b.cfg.AIGatewayImageBaseURL, b.cfg.AIGatewayImageAPIKey},
		} {
			value, ok := selectedEntry(block, tc.name)
			if !ok {
				t.Fatalf("a boot with the %s platform credential does not select its provider component", tc.name)
			}
			entry, isBlock := value.(pkgcore.ComponentConfig)
			if !isBlock {
				t.Fatalf("%q's entry carries a %T, want a ComponentConfig", tc.name, value)
			}
			if got, _ := entry.Get("base_url"); got != tc.baseURL {
				t.Errorf("%s block base_url = %v, want %q", tc.name, got, tc.baseURL)
			}
			if got, _ := entry.Get("api_key"); got != tc.apiKey {
				t.Errorf("%s block api_key = %v, want %q", tc.name, got, tc.apiKey)
			}
		}
	})

	t.Run("the configured boot", func(t *testing.T) {
		b := newServerBuild(ServerConfig{
			RedisAddr:          "127.0.0.1:6379",
			SMTPHost:           "smtp.example.test",
			SMTPPort:           2525,
			S3Endpoint:         "minio.example.test:9000",
			S3Bucket:           "clinic-media",
			SMSGatewayURL:      "https://sms-gateway.example.test/send",
			OTLPEndpoint:       "collector.example.test:4317",
			DisableQueueWorker: true,
		})
		block := componentsBlock(t, b.composition(true))
		for _, want := range []string{"eventbus.redis", "kv.redis", "mailer.smtp", "objectstore.s3", "sms.http"} {
			if _, ok := selectedEntry(block, want); !ok {
				t.Errorf("the configured boot does not select %q", want)
			}
		}
		for _, off := range []string{"eventbus.memory", "kv.memory", "mailer.console", "objectstore.local", "sms.console"} {
			if value, ok := block.Get(off); !ok || value != false {
				t.Errorf("the configured boot keeps the in-process default %q selected (value %v)", off, value)
			}
		}
		queue, ok := selectedEntry(block, "queue.standalone")
		if !ok {
			t.Fatal("the configured boot selects no queue implementation")
		}
		queueBlock, isBlock := queue.(pkgcore.ComponentConfig)
		if !isBlock {
			t.Fatalf("the queue block carries a %T, want a ComponentConfig", queue)
		}
		if worker, _ := queueBlock.Get("worker"); worker != false {
			t.Errorf("queue worker = %v under DisableQueueWorker, want false", worker)
		}
		observability, ok := selectedEntry(block, "observability")
		if !ok {
			t.Fatal("the live drive selects no observability component")
		}
		observabilityBlock, isBlock := observability.(pkgcore.ComponentConfig)
		if !isBlock {
			t.Fatalf("the observability block carries a %T, want a ComponentConfig", observability)
		}
		if endpoint, _ := observabilityBlock.Get("otlp_endpoint"); endpoint != b.cfg.OTLPEndpoint {
			t.Errorf("observability otlp_endpoint = %v, want %q", endpoint, b.cfg.OTLPEndpoint)
		}

		// A Redis-configured boot keeps both Redis-backed implementations
		// selected for their pairs; a mailer-less, SMS-less boot selects the
		// host's own components instead of the registered defaults.
		hostMailer := newServerBuild(ServerConfig{Mailer: pkgcore.NewConsoleMailer()})
		mailerBlock := componentsBlock(t, hostMailer.composition(false))
		if _, ok := selectedEntry(mailerBlock, hostComponentPrefix+"mailer"); !ok {
			t.Error("a boot supplying its own mailer does not select the host's mailer component")
		}
		if value, ok := mailerBlock.Get("mailer.console"); !ok || value != false {
			t.Errorf("a boot supplying its own mailer keeps mailer.console selected (value %v)", value)
		}
	})
}

// TestAssemble_ComposesAndClosesTheWholeApplication drives the flip end to
// end over a real database: the loader resolves the composition, the
// assembly constructs every module and step, Verify applies the selected
// migration sets and the ledger keys them by module, the app component
// composes the face without listening (BuildServer's drive), and the
// two-phase shutdown releases everything. The composition's strict mode
// makes this the completeness check: a token no selected component provides
// fails the assembly by name. The boot carries a configured provider pair,
// so the credential-conditioned ai-gateway member selection is part of this
// check too.
func TestAssemble_ComposesAndClosesTheWholeApplication(t *testing.T) {
	cfg := ServerConfig{
		DeploymentMode: pkgcore.DeploymentModeStandalone,
		Port:           "0",
		SQLitePath:     filepath.Join(t.TempDir(), "assembly.db"),
		HostTenants:    demo.DemoHostTenants,
		Memberships:    NewSignInMemberships(),
		// The provider pair: a configured chat and image credential, under
		// the same condition the boot's platform-credential write uses.
		AIGatewayBaseURL:      "https://chat-upstream.example.test/v1",
		AIGatewayAPIKey:       "sk-assembly-chat",
		AIGatewayImageBaseURL: "https://image-upstream.example.test/v1",
		AIGatewayImageAPIKey:  "sk-assembly-image",
	}
	b := newServerBuild(cfg)
	reg, err := b.assemble(context.Background(), false)
	if err != nil {
		t.Fatalf("assemble(): %v", err)
	}
	face, err := pkgcore.Get[*hostFace](reg)
	if err != nil {
		t.Fatalf("read the composed face: %v", err)
	}
	if face.Handler() == nil {
		t.Fatal("the composed face carries no handler")
	}
	if face.server != nil {
		t.Fatal("BuildServer's drive started a listener, want the face composed without one")
	}

	// The provider members are selected members of the real assembly and
	// construct through the component face: the names the module's routes
	// carry resolve as the selected components (ai-gateway's buildChat/
	// buildImage prefer a selected member over the package-level registry
	// for exactly these names), and the per-call construction shape works
	// over the assembled plan. The two faces construct the same
	// implementation, so this is the selection half of the switch; the
	// preference itself is pinned in ai-gateway's own suite.
	if got := pkgcore.MemberNames(reg, "chat"); !reflect.DeepEqual(got, []string{aigateway.ProviderOpenAICompatible}) {
		t.Errorf("MemberNames(chat) = %v, want [%s]", got, aigateway.ProviderOpenAICompatible)
	}
	if got := pkgcore.MemberNames(reg, "image"); !reflect.DeepEqual(got, []string{aigateway.ProviderOpenAICompatibleImage}) {
		t.Errorf("MemberNames(image) = %v, want [%s]", got, aigateway.ProviderOpenAICompatibleImage)
	}
	provider, err := pkgcore.Build[aigateway.ChatProvider](context.Background(), reg, aigateway.ProviderOpenAICompatible, nil)
	if err != nil {
		t.Fatalf("building %s through the component face: %v", aigateway.ProviderOpenAICompatible, err)
	}
	if provider == nil {
		t.Fatal("the component face built no provider")
	}
	// The migration ledger keys every set by the module its component
	// implements, never by the host-prefixed component name the assembly
	// selected: the set below is exactly the migration-carrying modules this
	// composition selects, and a "reference-app."-prefixed key here would
	// mean the boot replayed -- or, once every set is idempotent, silently
	// forked -- the log the migration command recorded under module names.
	gdb, err := pkgcore.Get[*gorm.DB](reg)
	if err != nil {
		t.Fatalf("read the assembled database: %v", err)
	}
	var modules []string
	if err := gdb.Table("schema_migrations").Distinct("module").Order("module").Pluck("module", &modules).Error; err != nil {
		t.Fatalf("read the migration ledger: %v", err)
	}
	wantModules := []string{
		"admin", "ai-gateway", "attestation", "audit", "authn", "billing",
		"cases", "config", "integration", "metering", "notes",
		"notification", "org", "pki", "rbac", "sharing", "smilesim",
		"storage",
	}
	if !reflect.DeepEqual(modules, wantModules) {
		t.Errorf("schema_migrations module keys = %v, want %v", modules, wantModules)
	}

	if err := speedapp.Shutdown(context.Background(), reg); err != nil {
		t.Fatalf("Shutdown(): %v", err)
	}
}
