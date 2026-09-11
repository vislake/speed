package pkgcore_test

// Runnable documentation for the pkgcore public API. Every example here is
// compiled and executed by `go test`, so an API change that invalidates the
// documented usage fails the build instead of silently rotting.
//
// The three blank imports below register "eventbus.redis", "kv.redis" and
// "objectstore.s3" on pkgcore's shared seam registries as a side effect,
// exactly as a distributed-mode host's own blank import would -- see each
// subpackage's own doc comment. Nothing in pkgcore's root package imports
// them (that would cycle back into the package these subpackages depend on),
// but this external test package can, and doing so here is what makes
// ExampleWithPreset below -- and this same test binary's internal tests in
// preset_test.go and the seam registration tests (eventbus_memory_test.go,
// kv_memory_test.go, objectstore_registry_test.go) that resolve
// PresetDistributed's eventbus/kv/objectstore entries -- exercise the real,
// subpackage implementations instead of failing with ErrUnknownImplementation:
// go test links the internal "pkgcore" test package and this external
// "pkgcore_test" one into one binary, so an init() triggered by an import
// here runs before any test in either package, and the registries it
// populates are the very package-level vars those other tests read.
import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	_ "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	_ "github.com/vislake/speed/go/pkgcore/kv/redis"
	_ "github.com/vislake/speed/go/pkgcore/objectstore/s3"
)

// ExampleParseDeploymentMode shows how a host turns the APP_DEPLOYMENT_MODE
// configuration value into a DeploymentMode, and how an unknown value is
// classified.
func ExampleParseDeploymentMode() {
	mode, err := pkgcore.ParseDeploymentMode(" Distributed\n")
	fmt.Println(mode, err)

	_, err = pkgcore.ParseDeploymentMode("staging")
	fmt.Println(errors.Is(err, pkgcore.ErrInvalidDeploymentMode))

	// Output:
	// distributed <nil>
	// true
}

// ExampleWithTenant shows the tenant context primitives. Data access code
// calls MustTenantFromContext and fails closed when there is no tenant, which
// is why an unscoped context must never reach a repository.
func ExampleWithTenant() {
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("acme"))

	tenant, ok := pkgcore.TenantFromContext(ctx)
	fmt.Println(tenant, ok)

	// An unscoped context is refused rather than silently treated as global.
	_, err := pkgcore.MustTenantFromContext(context.Background())
	fmt.Println(errors.Is(err, pkgcore.ErrNoTenant))

	// Output:
	// acme true
	// true
}

// ExampleWithSystemContext shows the audited escape hatch from tenant
// filtering. The purpose is a closed enumeration: it must be declared with
// RegisterSystemPurpose, from the module's own registration, before it can be
// granted.
func ExampleWithSystemContext() {
	const purpose = pkgcore.SystemPurpose("example.compliance_export")
	pkgcore.RegisterSystemPurpose(purpose)

	ctx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor:   "ops@example.com",
		Purpose: purpose,
		Ticket:  "OPS-1042",
	})
	if err != nil {
		fmt.Println("system context:", err)
		return
	}

	reason, ok := pkgcore.SystemReasonFromContext(ctx)
	fmt.Println(ok, reason.Actor, reason.Purpose, reason.Ticket)

	// An undeclared purpose is refused, and the context comes back unchanged.
	_, err = pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor:   "ops@example.com",
		Purpose: pkgcore.SystemPurpose("example.undeclared"),
	})
	fmt.Println(errors.Is(err, pkgcore.ErrSystemPurposeNotRegistered))

	// An anonymous bypass is refused too: every use must be attributable.
	_, err = pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{Purpose: purpose})
	fmt.Println(errors.Is(err, pkgcore.ErrSystemActorRequired))

	// Output:
	// true ops@example.com example.compliance_export OPS-1042
	// true
	// true
}

// ExampleWithActor shows the impersonation shape an audit trail needs: the
// current Actor and an independently-set OnBehalfOf actor. During an
// impersonated (admin-as-user) session, Actor is the impersonated user and
// OnBehalfOf is the real administrator -- both must remain readable at
// once, which is why WithActor and WithOnBehalfOf never clear one another.
func ExampleWithActor() {
	impersonated := pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-42", DisplayName: "Ada"}
	admin := pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1", DisplayName: "Grace"}

	ctx := pkgcore.WithActor(context.Background(), impersonated)
	ctx = pkgcore.WithOnBehalfOf(ctx, admin)

	actor, ok := pkgcore.ActorFromContext(ctx)
	fmt.Println(ok, actor.Type, actor.ID)

	onBehalfOf, ok := pkgcore.OnBehalfOfFromContext(ctx)
	fmt.Println(ok, onBehalfOf.Type, onBehalfOf.ID)

	// A context with neither set carries no actor at all.
	_, ok = pkgcore.ActorFromContext(context.Background())
	fmt.Println(ok)

	// Output:
	// true user user-42
	// true platform_admin admin-1
	// false
}

// ExampleKVStore shows the four operations every KVStore backend supports.
// NewMemoryKVStore is the standalone-mode implementation and doubles as the
// test double for code written against the interface.
func ExampleKVStore() {
	ctx := context.Background()
	kv := pkgcore.NewMemoryKVStore()

	// A ttl above zero expires the key; a ttl of zero or less stores it forever.
	if err := kv.Set(ctx, "session:u-1", []byte("token"), 5*time.Minute); err != nil {
		fmt.Println("set:", err)
		return
	}

	value, found, err := kv.Get(ctx, "session:u-1")
	fmt.Printf("get %q found=%t err=%v\n", value, found, err)

	// A missing key is not an error.
	_, found, err = kv.Get(ctx, "session:u-2")
	fmt.Printf("missing found=%t err=%v\n", found, err)

	// IncrByFloat, IncrByFloatWithTTL and CompareAndSwap are the only atomic
	// primitives on the interface: build read-modify-write cycles from them,
	// never from Get+Set.
	used, err := kv.IncrByFloat(ctx, "quota:acme:credits", 2.5)
	fmt.Printf("incr %v err=%v\n", used, err)

	// IncrByFloatWithTTL is IncrByFloat plus an expiry attached atomically on
	// the same call that creates the key -- the primitive a rate-limiting
	// window counter needs so a fresh key's ttl can never be lost to a
	// concurrent caller's increment landing between a separate Get and Set.
	windowCount, err := kv.IncrByFloatWithTTL(ctx, "ratelimit:acme:window-1", 1, time.Minute)
	fmt.Printf("incr-with-ttl %v err=%v\n", windowCount, err)

	// Against an empty old value, CompareAndSwap is set-if-absent.
	acquired, err := kv.CompareAndSwap(ctx, "lock:acme:import", nil, []byte("held"))
	fmt.Printf("cas %t err=%v\n", acquired, err)

	// Output:
	// get "token" found=true err=<nil>
	// missing found=false err=<nil>
	// incr 2.5 err=<nil>
	// incr-with-ttl 1 err=<nil>
	// cas true err=<nil>
}

// ExampleEventBus shows how modules stay decoupled: authn publishes a domain
// event and org reacts to it, without either importing the other's types.
func ExampleEventBus() {
	bus := pkgcore.NewMemoryEventBus()

	bus.Subscribe("authn.user_created", func(ctx context.Context, evt pkgcore.Event) error {
		email, _ := evt.Payload.(string)
		fmt.Printf("org: default workspace for %s in tenant %s\n", email, evt.TenantID)
		return nil
	})

	// A failing handler does not stop the handlers registered after it. The
	// in-memory bus reports every failure back to the publisher through
	// Publish's error -- a property of this implementation, not of the
	// EventBus seam, which a broker-backed bus could not honour for a
	// handler it delivers to on another replica.
	bus.Subscribe("authn.user_created", func(ctx context.Context, evt pkgcore.Event) error {
		return errors.New("mailer unavailable")
	})

	err := bus.Publish(context.Background(), pkgcore.Event{
		Type:     "authn.user_created",
		TenantID: pkgcore.TenantID("acme"),
		Payload:  "ada@example.com",
	})
	fmt.Println("publish:", err)

	// Output:
	// org: default workspace for ada@example.com in tenant acme
	// publish: pkgcore: handler 1 for event "authn.user_created" failed: mailer unavailable
}

// exampleTenancyModule is a dependency of exampleBillingModule below.
type exampleTenancyModule struct{}

func (exampleTenancyModule) Name() string         { return "tenancy" }
func (exampleTenancyModule) DependsOn() []string  { return nil }
func (exampleTenancyModule) Migrations() embed.FS { return embed.FS{} }
func (exampleTenancyModule) Locales() embed.FS    { return embed.FS{} }
func (exampleTenancyModule) OpenAPISpec() []byte  { return nil }

func (exampleTenancyModule) Register(reg pkgcore.Registrar) error {
	return reg.PermissionsSeat().Add("tenant:read")
}

// exampleBillingModule is a minimal pkgcore.Module. A module declares its
// identity, its dependencies and its embedded assets, and contributes
// everything else through the single Register call.
type exampleBillingModule struct{}

func (exampleBillingModule) Name() string         { return "billing" }
func (exampleBillingModule) DependsOn() []string  { return []string{"tenancy"} }
func (exampleBillingModule) Migrations() embed.FS { return embed.FS{} }
func (exampleBillingModule) Locales() embed.FS    { return embed.FS{} }
func (exampleBillingModule) OpenAPISpec() []byte  { return nil }

// Register declares; it never performs I/O.
func (exampleBillingModule) Register(reg pkgcore.Registrar) error {
	reg.RoutesSeat().Mount("/api/v1/billing", http.NotFoundHandler())

	if err := reg.ConfigSeat().Add(pkgcore.ConfigItem{
		Key:         "billing.invoice_retry_limit",
		Type:        "int",
		Default:     3,
		Description: "How many times a failed invoice charge is retried.",
	}); err != nil {
		return err
	}
	if err := reg.FeaturesSeat().Add(pkgcore.FeatureFlag{
		Key:         "billing.dunning",
		Default:     false,
		Description: "Chase failed payments on a retry schedule.",
	}); err != nil {
		return err
	}
	if err := reg.PermissionsSeat().Add("billing:read", "billing:write"); err != nil {
		return err
	}
	if err := reg.AuditActionsSeat().Add("billing.subscription_cancelled"); err != nil {
		return err
	}
	if err := reg.EventsSeat().Publishes(pkgcore.EventDecl{
		Type:        "billing.invoice.paid",
		PayloadType: "billing.InvoicePaid",
		Description: "An invoice was paid in full.",
	}); err != nil {
		return err
	}

	reg.EventsSeat().Subscribe("authn.user_created", func(ctx context.Context, evt pkgcore.Event) error {
		fmt.Println("billing: opening a credit ledger for tenant", evt.TenantID)
		return nil
	})
	return nil
}

// ExampleBootstrapRegistrar shows the process-start declaration seat: a module
// states the keys it consumes while registering and never resolves them
// itself, so one declaration is documentation, validation and the generated
// configuration reference's source at once. The host reads the declarations
// back to build the loader target it resolves the values into.
func ExampleBootstrapRegistrar() {
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())

	// A declaration states the contract for the key, never its value: the
	// authoritative default is whatever the host's loader target struct
	// carries, so Default is an operator-facing statement of the fallback
	// behaviour instead of a second runnable default.
	err := reg.Bootstrap.Add(pkgcore.BootstrapKey{
		Key:         "example.pii_cipher_key",
		Format:      "hexkey",
		Default:     "documented non-secret development default",
		Sensitive:   true,
		Description: "Encrypts stored contact details; rotate only through a migration.",
		Group:       "example",
	})
	fmt.Println("add:", err)

	// The host reads the keys back in registration order, as a copy.
	for _, key := range reg.Bootstrap.Keys() {
		fmt.Println(key.Key, key.Format, key.Sensitive)
	}

	// A Sensitive key with no Description is refused: its value never appears
	// in an output, so the contract text is all an operator has to work from.
	err = reg.Bootstrap.Add(pkgcore.BootstrapKey{Key: "example.api_token", Format: "string", Sensitive: true})
	fmt.Println("undocumented secret:", errors.Is(err, pkgcore.ErrInvalidBootstrapKey))

	// Two owners of one key is a bug rather than a merge: there is one process
	// environment, and no registration order makes two readings of it
	// coherent. Neither failed call registered anything, so this collides with
	// the first declaration.
	err = reg.Bootstrap.Add(pkgcore.BootstrapKey{
		Key:         "example.pii_cipher_key",
		Format:      "hexkey",
		Sensitive:   true,
		Description: "A second module claiming the same key.",
	})
	fmt.Println("duplicate:", errors.Is(err, pkgcore.ErrDuplicateBootstrapKey))

	// Output:
	// add: <nil>
	// example.pii_cipher_key hexkey true
	// undocumented secret: true
	// duplicate: true
}

// ExampleBootstrapKeyPurpose shows the derivation convention of the bootstrap
// seat: a declared key path maps to one stable, versioned purpose string,
// which a host composes with dbkit.DeriveKey -- the material at keyPath is
// dbkit.DeriveKey(rootKey, purpose) -- so one root secret can serve every
// declared key. Because the purpose embeds the declared path verbatim and
// the derivation is deterministic, renaming a declared key path is a
// rotation of that key's material, never a plain edit.
func ExampleBootstrapKeyPurpose() {
	purpose, err := pkgcore.BootstrapKeyPurpose("config.cipher_key")
	fmt.Println(purpose, err)

	// A path that is empty or carries an empty segment (a doubled dot here)
	// is refused at the call site, before anything is derived under a
	// purpose nobody meant to freeze.
	_, err = pkgcore.BootstrapKeyPurpose("config..cipher_key")
	fmt.Println(errors.Is(err, pkgcore.ErrInvalidBootstrapKeyPath))

	// Output:
	// speed.config.cipher_key.v1 <nil>
	// true
}

// ExampleKernel_Bootstrap shows the host side of module wiring: hand the
// kernel every module, get one Registry back with everything they contributed.
func ExampleKernel_Bootstrap() {
	kernel := pkgcore.NewKernel()

	// Modules may be listed in any order. Bootstrap sorts them so that each one
	// registers after the modules it depends on.
	reg, err := kernel.Bootstrap(context.Background(), exampleBillingModule{}, exampleTenancyModule{})
	if err != nil {
		fmt.Println("bootstrap:", err)
		return
	}

	fmt.Println("deployment mode:", kernel.DeploymentMode())
	for _, route := range reg.RoutesSeat().Routes() {
		fmt.Println("route:", route.Path)
	}
	fmt.Println("permissions:", reg.PermissionsSeat().Permissions())
	fmt.Println("audit actions:", reg.AuditActionsSeat().Actions())
	for _, decl := range reg.EventsSeat().Published() {
		fmt.Println("publishes:", decl.Type, decl.PayloadType)
	}

	// The host publishes into the same bus the modules subscribed to.
	if err := reg.EventBus().Publish(context.Background(), pkgcore.Event{
		Type:     "authn.user_created",
		TenantID: pkgcore.TenantID("acme"),
	}); err != nil {
		fmt.Println("publish:", err)
	}

	// Output:
	// deployment mode: standalone
	// route: /api/v1/billing
	// permissions: [billing:read billing:write tenant:read]
	// audit actions: [billing.subscription_cancelled]
	// publishes: billing.invoice.paid billing.InvoicePaid
	// billing: opening a credit ledger for tenant acme
}

// ExampleValidateFeatureGraph shows the check Bootstrap runs once every module
// has registered: a flag may depend on a flag owned by a module that registers
// later, so the graph is only resolvable at the end.
func ExampleValidateFeatureGraph() {
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.FeaturesSeat().Add(pkgcore.FeatureFlag{
		Key:         "billing.dunning",
		Description: "Chase failed payments on a retry schedule.",
		DependsOn:   []string{"billing.invoicing"},
	}); err != nil {
		fmt.Println("add:", err)
		return
	}

	err := pkgcore.ValidateFeatureGraph(reg)
	fmt.Println(errors.Is(err, pkgcore.ErrUnresolvedFeatureDependency))
	fmt.Println(err)

	// Output:
	// true
	// pkgcore: unresolved feature flag dependency: "billing.dunning" depends on unregistered flag "billing.invoicing"
}

// ExampleWithEventBus shows the code-injection layer of implementation
// composition and the capability validation that gates assembly:
// DeploymentModeDistributed requires every seam's resolved implementation
// to declare MultiReplicaSafe; the standalone Preset a bare NewKernel()
// would otherwise resolve to does not, so the host injects its own.
func ExampleWithEventBus() {
	// Assembling a distributed-mode kernel with nothing injected resolves
	// every seam through the standalone Preset, whose implementations do not
	// declare MultiReplicaSafe, so the very first seam Bootstrap validates --
	// the event bus -- fails the capability check.
	_, err := pkgcore.NewKernel(pkgcore.WithDeploymentMode(pkgcore.DeploymentModeDistributed)).Bootstrap(context.Background())
	fmt.Println(errors.Is(err, pkgcore.ErrCapabilityUnsatisfied))

	// A real host passes its broker-backed bus here, declaring the
	// capabilities it actually has; the in-memory bus stands in for it in
	// this example, declared MultiReplicaSafe so the composition passes
	// validation -- the capability declaration is what assembly trusts, not
	// what the value actually is. DeploymentModeDistributed requires the same
	// of a KVStore, a Mailer and an ObjectStore too, so all three are wired
	// alongside the bus with their own stand-ins.
	store, cleanup := exampleLocalObjectStore()
	defer cleanup()
	kernel := pkgcore.NewKernel(pkgcore.WithDeploymentMode(pkgcore.DeploymentModeDistributed),
		pkgcore.WithEventBus(pkgcore.NewMemoryEventBus(), pkgcore.MultiReplicaSafe),
		pkgcore.WithKVStore(pkgcore.NewMemoryKVStore(), pkgcore.MultiReplicaSafe),
		pkgcore.WithMailer(pkgcore.NewConsoleMailer(), pkgcore.MultiReplicaSafe),
		pkgcore.WithObjectStore(store, pkgcore.MultiReplicaSafe))
	reg, err := kernel.Bootstrap(context.Background(), exampleTenancyModule{})
	fmt.Println(err, reg.EventBus() != nil)

	// Output:
	// true
	// <nil> true
}

// ExampleWithKVStore shows the code-injection layer for the key-value seam,
// mirroring ExampleWithEventBus.
func ExampleWithKVStore() {
	// Assembling a distributed-mode kernel fails on the first seam whose
	// resolved implementation does not satisfy the required capability, in
	// a fixed order: the bus is wired here with MultiReplicaSafe so its check
	// passes, which isolates this example's failure to the KVStore check --
	// still resolving to the standalone Preset's "kv.memory", which lacks it.
	_, err := pkgcore.NewKernel(pkgcore.WithDeploymentMode(pkgcore.DeploymentModeDistributed),
		pkgcore.WithEventBus(pkgcore.NewMemoryEventBus(), pkgcore.MultiReplicaSafe)).
		Bootstrap(context.Background())
	fmt.Println(errors.Is(err, pkgcore.ErrCapabilityUnsatisfied))

	// A real host passes its Redis-backed store here, declaring
	// MultiReplicaSafe; the in-memory store stands in for it in this example.
	// The Mailer and ObjectStore seams are wired too, because every seam must
	// satisfy the same capability for this kernel to bootstrap successfully.
	store, cleanup := exampleLocalObjectStore()
	defer cleanup()
	kernel := pkgcore.NewKernel(pkgcore.WithDeploymentMode(pkgcore.DeploymentModeDistributed),
		pkgcore.WithEventBus(pkgcore.NewMemoryEventBus(), pkgcore.MultiReplicaSafe),
		pkgcore.WithKVStore(pkgcore.NewMemoryKVStore(), pkgcore.MultiReplicaSafe),
		pkgcore.WithMailer(pkgcore.NewConsoleMailer(), pkgcore.MultiReplicaSafe),
		pkgcore.WithObjectStore(store, pkgcore.MultiReplicaSafe))
	reg, err := kernel.Bootstrap(context.Background(), exampleTenancyModule{})
	fmt.Println(err, reg.KVStore() != nil)

	// Output:
	// true
	// <nil> true
}

// ExampleWithMailer shows the code-injection layer for the mail seam,
// mirroring ExampleWithEventBus and ExampleWithKVStore.
func ExampleWithMailer() {
	// Assembling a distributed-mode kernel with the bus and the store wired
	// (both declared MultiReplicaSafe, so their checks pass) isolates this
	// example's failure to the Mailer check: it still resolves to the
	// standalone Preset's "mailer.console", which does not declare the
	// capability.
	_, err := pkgcore.NewKernel(pkgcore.WithDeploymentMode(pkgcore.DeploymentModeDistributed),
		pkgcore.WithEventBus(pkgcore.NewMemoryEventBus(), pkgcore.MultiReplicaSafe),
		pkgcore.WithKVStore(pkgcore.NewMemoryKVStore(), pkgcore.MultiReplicaSafe)).
		Bootstrap(context.Background())
	fmt.Println(errors.Is(err, pkgcore.ErrCapabilityUnsatisfied))

	// A real host passes its SMTP-backed mailer here, declaring
	// MultiReplicaSafe; the console one stands in for it in this example, and
	// the local object store stands in for the S3-backed one, for the same
	// reason the bus and KVStore stand-ins above do: the distributed mode
	// requires the capability of all four seams, so each is wired alongside
	// the one under test.
	store, cleanup := exampleLocalObjectStore()
	defer cleanup()
	kernel := pkgcore.NewKernel(pkgcore.WithDeploymentMode(pkgcore.DeploymentModeDistributed),
		pkgcore.WithEventBus(pkgcore.NewMemoryEventBus(), pkgcore.MultiReplicaSafe),
		pkgcore.WithKVStore(pkgcore.NewMemoryKVStore(), pkgcore.MultiReplicaSafe),
		pkgcore.WithMailer(pkgcore.NewConsoleMailer(), pkgcore.MultiReplicaSafe),
		pkgcore.WithObjectStore(store, pkgcore.MultiReplicaSafe))
	reg, err := kernel.Bootstrap(context.Background(), exampleTenancyModule{})
	fmt.Println(err, reg.Mailer() != nil)

	// Output:
	// true
	// <nil> true
}

// ExampleNewConsoleMailer shows the standalone deployment mode's mailer: it
// prints every message to standard output as one greppable, self-delimiting
// record, instead of delivering it. A module that sends mail through the
// registry's mail seam is exercised identically in tests and in the
// standalone deployment mode.
func ExampleNewConsoleMailer() {
	mailer := pkgcore.NewConsoleMailer()
	err := mailer.Send(context.Background(), pkgcore.Mail{
		From:    "ops@example.com",
		To:      []string{"ada@example.com"},
		Subject: "Your invoice #1042 is ready",
		Text:    "Hello Ada, your invoice is ready to view.",
	})
	fmt.Println(err)

	// A message that fails the shared validity rules is rejected with
	// ErrInvalidMail before anything is printed, so the record above is the
	// whole of this mailer's output.
	err = mailer.Send(context.Background(), pkgcore.Mail{
		From:    "ops@example.com",
		Subject: "no recipients",
		Text:    "this must never print",
	})
	fmt.Println(errors.Is(err, pkgcore.ErrInvalidMail))

	// Output:
	// [mail] from: ops@example.com
	// [mail] to: ada@example.com
	// [mail] subject: Your invoice #1042 is ready
	// [mail] text/plain:
	// Hello Ada, your invoice is ready to view.
	// [mail] end
	// <nil>
	// true
}

// ExampleNewSMTPMailer shows the distributed deployment mode's mailer: an
// SMTP client delivering through the relay in SMTPConfig, the counterpart of
// the console mailer of ExampleNewConsoleMailer. Nothing is dialed at
// construction -- the relay is contacted on the first Send -- so a host can
// wire the mailer at startup whether or not the relay is reachable, and an
// unusable configuration (an empty host, a port outside 1..65535, an unknown
// TLS mode) panics there instead, where the wiring error is visible.
func ExampleNewSMTPMailer() {
	mailer := pkgcore.NewSMTPMailer(pkgcore.SMTPConfig{
		Host:     "smtp.example.com",
		Port:     587, // the submission port: plaintext first, STARTTLS when advertised
		Username: "relay@example.com",
		Password: "s3cret",
		TLSMode:  pkgcore.SMTPTLSModeAuto,
	})
	//nolint:staticcheck // QF1011: the assertion doubles as written doc that
	// this constructor satisfies the Mailer interface -- the console-mailer
	// counterpart of ExampleNewConsoleMailer -- so it is kept rather than
	// inlined, which would leave the value unused.
	var _ pkgcore.Mailer = mailer // drop-in for the console mailer of ExampleNewConsoleMailer

	fmt.Println("mailer wired; the first Send dials the relay")
	// Output:
	// mailer wired; the first Send dials the relay
}

// ExampleMail_replyTo shows the optional Reply-To field: replies to a message
// sent from a no-reply address can be pointed at an inbox a person reads. The
// console record prints one extra line for it, between the to and subject
// lines, and the SMTP mailer accepts the same address as a config-level
// default (SMTPConfig.ReplyTo) for every message whose Mail leaves ReplyTo
// empty.
func ExampleMail_replyTo() {
	mailer := pkgcore.NewConsoleMailer()
	err := mailer.Send(context.Background(), pkgcore.Mail{
		From:    "notifications@example.com",
		To:      []string{"ada@example.com"},
		ReplyTo: "support@example.com",
		Subject: "Your invoice #1042 is ready",
		Text:    "Hello Ada, your invoice is ready to view.",
	})
	fmt.Println(err)

	// A Mail.ReplyTo always wins over the SMTP mailer's config-level
	// default; an empty one falls back to it. Nothing is dialed here -- the
	// relay is contacted on the first Send.
	notifications := pkgcore.NewSMTPMailer(pkgcore.SMTPConfig{
		Host:    "smtp.example.com",
		Port:    587,
		ReplyTo: "support@example.com",
	})
	fmt.Println(notifications != nil)

	// Output:
	// [mail] from: notifications@example.com
	// [mail] to: ada@example.com
	// [mail] reply-to: support@example.com
	// [mail] subject: Your invoice #1042 is ready
	// [mail] text/plain:
	// Hello Ada, your invoice is ready to view.
	// [mail] end
	// <nil>
	// true
}

// ExampleNewConsoleSMSSender shows the zero-external-dependency SMS
// transport: it prints every message to the writer it was given as one
// record per send, instead of delivering it -- the standalone deployment
// mode's sender, and the test double code written against SMSSender asserts
// on. go/authn's phone-login flow and go/notification's sms channel both
// receive it through their own sender options.
func ExampleNewConsoleSMSSender() {
	sender := pkgcore.NewConsoleSMSSender(os.Stdout)

	err := sender.Send(context.Background(), pkgcore.SMS{
		To:   "+8613800000000",
		Text: "your verification code is 123456",
	})
	fmt.Println(err)

	// Output:
	// SMS to +8613800000000: your verification code is 123456
	// <nil>
}

// ExampleNewHTTPSMSSender shows the operator-gateway SMS transport: a JSON
// POST to the endpoint in the argument, the counterpart of the console
// sender of ExampleNewConsoleSMSSender. Nothing is dialed at construction --
// the gateway is contacted on the first Send -- and the endpoint must be
// https: a plaintext one is refused by the first Send, before any request
// leaves the process (the pkgcore/safehttp scheme policy).
func ExampleNewHTTPSMSSender() {
	sender := pkgcore.NewHTTPSMSSender("https://sms-gateway.example.com/send")
	//nolint:staticcheck // QF1011: the assertion doubles as written doc that
	// this constructor satisfies the SMSSender interface -- the console
	// sender's counterpart in ExampleNewConsoleSMSSender -- so it is kept
	// rather than inlined, which would leave the value unused.
	var _ pkgcore.SMSSender = sender

	fmt.Println("sender wired; the first Send posts to the gateway")
	// Output:
	// sender wired; the first Send posts to the gateway
}

// exampleLocalObjectStore creates a throwaway local object store for the
// examples that need one, along with the cleanup that removes it. The
// standalone implementation doubles as the test double for the ObjectStore
// interface, so it stands in for the S3-backed store of a real
// distributed-mode host exactly as the in-memory bus and KVStore stand in for
// their broker-backed counterparts.
func exampleLocalObjectStore() (pkgcore.ObjectStore, func()) {
	directory, err := os.MkdirTemp("", "pkgcore-example-object-store-*")
	if err != nil {
		fmt.Println("object store temp dir:", err)
		return nil, func() {}
	}
	return pkgcore.NewLocalObjectStore(directory), func() {
		if err := os.RemoveAll(directory); err != nil {
			fmt.Println("object store cleanup:", err)
		}
	}
}

// ExampleNewLocalObjectStore shows the standalone deployment mode's object
// store: objects are files below one directory, and the store doubles as the
// test double for code written against ObjectStore, the way NewConsoleMailer
// doubles for Mailer. The directory is the durable home of the objects, so a
// standalone-mode host keeps them across restarts by opening a store over the
// same directory again.
func ExampleNewLocalObjectStore() {
	directory, err := os.MkdirTemp("", "pkgcore-example-object-store-*")
	if err != nil {
		fmt.Println("temp dir:", err)
		return
	}
	defer os.RemoveAll(directory)

	store := pkgcore.NewLocalObjectStore(directory)
	err = store.PutObject(context.Background(), "invoices/2026/1042", strings.NewReader("invoice bytes"))
	fmt.Println("put:", err)

	// A later store over the same directory reads what the earlier one wrote:
	// objects survive the store, only the process hosting it is throwaway.
	later := pkgcore.NewLocalObjectStore(directory)
	reader, err := later.GetObject(context.Background(), "invoices/2026/1042")
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	body, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	fmt.Printf("object: %s (err=%v)\n", body, err)

	// A missing key is ErrObjectNotFound, the same sentinel every backend
	// reports, never a backend-specific error.
	_, err = later.GetObject(context.Background(), "invoices/2026/0001")
	fmt.Println(errors.Is(err, pkgcore.ErrObjectNotFound))

	// Deleting a missing key is a success: DeleteObject is idempotent, so a
	// failed cleanup can always be retried.
	err = later.DeleteObject(context.Background(), "invoices/2026/0001")
	fmt.Println("delete:", err)

	// Output:
	// put: <nil>
	// object: invoice bytes (err=<nil>)
	// true
	// delete: <nil>
}

// ExampleValidateObjectKey shows the shared key grammar every ObjectStore
// implementation enforces before an operation touches its backend --
// exported so that objectstore/s3 can share it without importing this
// package's local store implementation back into itself.
func ExampleValidateObjectKey() {
	fmt.Println(pkgcore.ValidateObjectKey("invoices/2026/1042.pdf"))
	fmt.Println(errors.Is(pkgcore.ValidateObjectKey(""), pkgcore.ErrInvalidObjectKey))
	fmt.Println(errors.Is(pkgcore.ValidateObjectKey("a/../b"), pkgcore.ErrInvalidObjectKey))

	// Output:
	// <nil>
	// true
	// true
}

// ExampleObjectStore shows the three operations of the object seam as a
// module's code sees them, on the standalone implementation. Objects are
// whole streams under keys: there is no metadata, no listing and no
// server-side operation, because no backend the interface must support has
// all of those.
func ExampleObjectStore() {
	store, cleanup := exampleLocalObjectStore()
	defer cleanup()

	ctx := context.Background()

	// PutObject replaces the object at the key atomically; the write never
	// leaves a partial object behind for a concurrent reader.
	err := store.PutObject(ctx, "scans/panoramic/2026-08-31", strings.NewReader("cbct bytes"))
	fmt.Println("put:", err)

	// GetObject returns a reader over the object as it was when the request
	// started. The caller owns it and must Close it.
	reader, err := store.GetObject(ctx, "scans/panoramic/2026-08-31")
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	body, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	fmt.Printf("read: %s (err=%v)\n", body, err)

	// A key that names nothing is ErrObjectNotFound.
	_, err = store.GetObject(ctx, "scans/panoramic/2026-08-30")
	fmt.Println("missing:", errors.Is(err, pkgcore.ErrObjectNotFound))

	// A key that breaks the shared grammar is ErrInvalidObjectKey before any
	// backend is touched, so a key accepted by one backend is accepted by all.
	err = store.PutObject(ctx, "../escape", strings.NewReader("no"))
	fmt.Println("invalid:", errors.Is(err, pkgcore.ErrInvalidObjectKey))

	// Output:
	// put: <nil>
	// read: cbct bytes (err=<nil>)
	// missing: true
	// invalid: true
}

// ExampleWithObjectStore shows the code-injection layer for the object-store
// seam, mirroring ExampleWithEventBus and its counterparts.
func ExampleWithObjectStore() {
	// Assembling a distributed-mode kernel with the bus, the key-value store
	// and the mailer wired (all three declared MultiReplicaSafe, so their
	// checks pass) isolates this example's failure to the ObjectStore check:
	// it still resolves to the standalone Preset's "objectstore.local", which
	// does not declare the capability.
	_, err := pkgcore.NewKernel(pkgcore.WithDeploymentMode(pkgcore.DeploymentModeDistributed),
		pkgcore.WithEventBus(pkgcore.NewMemoryEventBus(), pkgcore.MultiReplicaSafe),
		pkgcore.WithKVStore(pkgcore.NewMemoryKVStore(), pkgcore.MultiReplicaSafe),
		pkgcore.WithMailer(pkgcore.NewConsoleMailer(), pkgcore.MultiReplicaSafe)).
		Bootstrap(context.Background())
	fmt.Println(errors.Is(err, pkgcore.ErrCapabilityUnsatisfied))

	// A real host passes its S3-backed store here, declaring MultiReplicaSafe;
	// the local one stands in for it in this example, and every module's
	// ObjectStore() calls reach the store the host wired in.
	store, cleanup := exampleLocalObjectStore()
	defer cleanup()
	kernel := pkgcore.NewKernel(pkgcore.WithDeploymentMode(pkgcore.DeploymentModeDistributed),
		pkgcore.WithEventBus(pkgcore.NewMemoryEventBus(), pkgcore.MultiReplicaSafe),
		pkgcore.WithKVStore(pkgcore.NewMemoryKVStore(), pkgcore.MultiReplicaSafe),
		pkgcore.WithMailer(pkgcore.NewConsoleMailer(), pkgcore.MultiReplicaSafe),
		pkgcore.WithObjectStore(store, pkgcore.MultiReplicaSafe))
	reg, err := kernel.Bootstrap(context.Background(), exampleTenancyModule{})
	fmt.Println(err, reg.ObjectStore() != nil)

	// Output:
	// true
	// <nil> true
}

// ExampleCapability shows the bitmask deployment mode and implementation
// composition compare: an implementation declares what it Has, a deployment
// mode declares what it requires, and Kernel.Bootstrap checks the two
// against each other. See ExampleWithEventBus and its siblings for that
// check running for real, inside Bootstrap.
func ExampleCapability() {
	redisLike := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart
	inProcess := pkgcore.Capability(0)

	fmt.Println(redisLike.Has(pkgcore.MultiReplicaSafe))
	fmt.Println(inProcess.Has(pkgcore.MultiReplicaSafe))
	// A DeploymentModeStandalone requirement is the zero Capability, which
	// every implementation satisfies, including one declaring nothing.
	fmt.Println(inProcess.Has(0))

	fmt.Println(redisLike)
	fmt.Println(inProcess)

	// Output:
	// true
	// false
	// true
	// MultiReplicaSafe|SurvivesRestart
	// none
}

// ExampleSeamRegistry shows the name-to-constructor mechanism a Preset
// resolves through: pkgcore pre-populates one SeamRegistry per seam
// (EventBusRegistry and its siblings) with the built-in implementations it
// ships, and a host registers its own the same way this example registers
// a test one.
func ExampleSeamRegistry() {
	registry := pkgcore.NewSeamRegistry[pkgcore.KVStore]()

	err := registry.Register(pkgcore.Registration[pkgcore.KVStore]{
		Name:         "kv.example",
		Capabilities: pkgcore.MultiReplicaSafe,
		New:          func(pkgcore.Config) (pkgcore.KVStore, error) { return pkgcore.NewMemoryKVStore(), nil },
	})
	fmt.Println("register:", err)

	store, caps, err := registry.Build("kv.example", pkgcore.Config{})
	fmt.Println("build:", err, store != nil, caps)

	// Registering the same name twice is rejected; the original registration
	// is left in place.
	err = registry.Register(pkgcore.Registration[pkgcore.KVStore]{Name: "kv.example"})
	fmt.Println("duplicate:", errors.Is(err, pkgcore.ErrDuplicateImplementation))

	// Building a name nothing registered is rejected too.
	_, _, err = registry.Build("kv.nonexistent", pkgcore.Config{})
	fmt.Println("unknown:", errors.Is(err, pkgcore.ErrUnknownImplementation))

	// Output:
	// register: <nil>
	// build: <nil> true MultiReplicaSafe
	// duplicate: true
	// unknown: true
}

// ExampleWithPreset shows the middle composition layer: a Preset names, per
// seam, which registered implementation Bootstrap builds for a seam the host
// has not injected directly -- and with which Config. WithPreset replaces
// the whole map; Preset.With derives one from a built-in preset without
// mutating it, and injecting a seam directly still wins over the Preset for
// that one seam, regardless of option order.
func ExampleWithPreset() {
	// A host that wants pkgcore's own Redis-backed EventBus and KVStore, but
	// keeps the console Mailer, derives its Preset from the standalone one by
	// overriding the two entries PresetDistributed names. Nothing is dialed by
	// resolving "eventbus.redis"/"kv.redis" here (see eventbus/redis's
	// NewEventBus and kv/redis's NewKVStore, whose packages this file's own
	// blank imports register onto the shared registries), so this example
	// needs no real Redis to run; see ExampleSeamPreset for an entry that
	// carries the implementation's own configuration.
	custom := pkgcore.PresetStandalone.
		With("eventbus", pkgcore.PresetDistributed["eventbus"]).
		With("kv", pkgcore.PresetDistributed["kv"])
	reg, err := pkgcore.NewKernel(pkgcore.WithPreset(custom)).Bootstrap(context.Background())
	fmt.Println(err, reg.EventBus() != nil, reg.KVStore() != nil, reg.Mailer() != nil)

	// Output:
	// <nil> true true true
}

// ExampleSeamPreset shows a preset entry carrying configuration: the Config
// on a SeamPreset is what the named Registration.New is called with, so a
// host names an implementation and hands over the settings its own
// configuration layer resolved -- without building anything itself. A
// setting the implementation cannot default (the SMTP host here) fails
// Bootstrap with ErrMissingSeamConfig when the entry leaves it out, instead
// of silently resolving to an implementation nothing configured.
func ExampleSeamPreset() {
	preset := pkgcore.PresetStandalone.With("mailer", pkgcore.SeamPreset{
		Implementation: "mailer.smtp",
		Config:         pkgcore.Config{"host": "smtp.example.com", "port": "587"},
	})
	reg, err := pkgcore.NewKernel(pkgcore.WithPreset(preset)).Bootstrap(context.Background())
	fmt.Println(err, reg.Mailer() != nil)

	hostless := pkgcore.PresetStandalone.With("mailer", pkgcore.SeamPreset{Implementation: "mailer.smtp"})
	_, err = pkgcore.NewKernel(pkgcore.WithPreset(hostless)).Bootstrap(context.Background())
	fmt.Println(errors.Is(err, pkgcore.ErrMissingSeamConfig))

	// Output:
	// <nil> true
	// true
}

// ExampleDeploymentMode_RequiredCapabilities shows what Kernel.Bootstrap
// compares every resolved seam's declared Capability against.
func ExampleDeploymentMode_RequiredCapabilities() {
	fmt.Println(pkgcore.DeploymentModeStandalone.RequiredCapabilities())
	fmt.Println(pkgcore.DeploymentModeDistributed.RequiredCapabilities())

	// Output:
	// none
	// MultiReplicaSafe
}

// ExampleErrMissingSeamConfig shows the built-in "mailer.smtp" and
// "objectstore.s3" implementations' one deliberate gap: neither has a safe
// default host, bucket or credential, so building either with a Config
// missing one fails instead of producing an unusable mailer or store. A host
// supplies them through a preset entry carrying the settings its own
// configuration resolved (see ExampleSeamPreset), or injects a value it
// built itself (see ExampleNewSMTPMailer and the objectstore/s3
// subpackage's own ExampleNewObjectStore).
func ExampleErrMissingSeamConfig() {
	_, _, err := pkgcore.MailerRegistry.Build("mailer.smtp", pkgcore.Config{})
	fmt.Println(errors.Is(err, pkgcore.ErrMissingSeamConfig))

	_, _, err = pkgcore.ObjectStoreRegistry.Build("objectstore.s3", pkgcore.Config{})
	fmt.Println(errors.Is(err, pkgcore.ErrMissingSeamConfig))

	// Output:
	// true
	// true
}

// ExampleKernel_Shutdown shows the seam lifecycle a successful Bootstrap
// hands over to the Kernel: Shutdown releases the resources of every seam
// implementation the Bootstrap resolved from a Preset and that implements
// the Registration-level Close() error contract (a dialed connection, a
// client built from the Config, a throwaway temporary directory), and a
// Bootstrap that fails after resolving such a seam closes it before
// returning the error. The default standalone composition's in-process
// implementations own nothing closable, so Shutdown here is a no-op that
// reports no error; a host calls it when the assembled application is
// stopping, after the bootstrapped Registry's seams are no longer in use.
func ExampleKernel_Shutdown() {
	kernel := pkgcore.NewKernel()
	reg, err := kernel.Bootstrap(context.Background())
	if err != nil {
		fmt.Println("unexpected bootstrap error:", err)
		return
	}
	fmt.Println(reg != nil, kernel.Shutdown())

	// Output:
	// true <nil>
}

// ExampleEventPayloadString shows a cross-module subscriber reading a field
// out of an event payload it cannot type-assert: a same-process publish hands
// it the publisher's own struct, a broker-backed bus hands it the decoded
// wire shape, and the probe accepts both by trying the spellings a field may
// carry, in the order given. An unusable spelling is reported, never guessed.
func ExampleEventPayloadString() {
	// The publisher's own value, as a same-process delivery hands it over:
	// this subscriber cannot name the type (it lives in another module), only
	// the JSON spelling it documents.
	published := struct {
		UserID string `json:"user_id"`
	}{UserID: "u-42"}

	// The wire shape the same publish arrives as over a broker-backed bus.
	wire := map[string]any{"user_id": "u-42"}

	for _, payload := range []any{published, wire} {
		id, ok := pkgcore.EventPayloadString(payload, "user_id", "userId", "UserID")
		fmt.Println(id, ok)
	}

	// A payload whose value is not the expected shape is reported as absent.
	_, ok := pkgcore.EventPayloadString(map[string]any{"user_id": 42}, "user_id")
	fmt.Println(ok)

	// Output:
	// u-42 true
	// u-42 true
	// false
}

// ExampleMountRoutes mounts two module routes the one correct way: each
// Handler answers at its exact Path and at everything nested below it,
// which is the contract MountedRoute's own doc comment states. The
// alternative -- registering only the subtree pattern -- leaves the exact
// path to ServeMux's redirect-on-missing-slash behavior, which does not
// preserve a POST's method or body.
func ExampleMountRoutes() {
	mux := http.NewServeMux()
	mount := func(mark string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, mark)
		})
	}
	pkgcore.MountRoutes(mux,
		pkgcore.MountedRoute{Path: "/api/v1/notes", Handler: mount("notes")},
		pkgcore.MountedRoute{Path: "/api/v1/authn", Handler: mount("authn")},
	)

	for _, path := range []string{"/api/v1/notes", "/api/v1/notes/42", "/api/v1/authn/login"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		fmt.Println(path, rec.Code, rec.Body.String())
	}

	// Output:
	// /api/v1/notes 200 notes
	// /api/v1/notes/42 200 notes
	// /api/v1/authn/login 200 authn
}

// ExampleMountedRoute_access shows the two authorization decisions a route
// can carry. A module's own mount records none -- the zero RouteAccess --
// and the host's route table resolves it before anything serves: an
// explicit Public declaration, or the permission each request must hold,
// selected from the request's own route (a read on GET, a write otherwise).
func ExampleMountedRoute_access() {
	notes := pkgcore.MountedRoute{
		Path:    "/api/v1/notes",
		Handler: http.NotFoundHandler(),
		Access: pkgcore.RouteAccess{Permission: func(r *http.Request) string {
			if r.Method == http.MethodGet {
				return "notes:read"
			}
			return "notes:write"
		}},
	}
	public := pkgcore.MountedRoute{
		Path:    "/api/v1/config/public",
		Handler: http.NotFoundHandler(),
		Access:  pkgcore.RouteAccess{Public: true},
	}

	fmt.Println(pkgcore.MountedRoute{}.Access.Public, pkgcore.MountedRoute{}.Access.Permission == nil)
	fmt.Println(notes.Path, notes.Handler != nil, notes.Access.Public, notes.Access.Permission(httptest.NewRequest(http.MethodGet, notes.Path, nil)))
	fmt.Println(public.Path, public.Handler != nil, public.Access.Public, public.Access.Permission == nil)

	// Output:
	// false true
	// /api/v1/notes true false notes:read
	// /api/v1/config/public true true true
}

// ExamplePeriodicTask shows the periodic-schedule seat. A module declares
// every periodic task it owns here, exactly where it registers the task's
// handler: declaring means scheduled, so a host that runs a jobs.Scheduler
// over the finished registry's declarations enqueues them all, and the
// host's single switch is whether a scheduler runs. Each declaration
// carries the window size, the tenant scope, and the idempotency-key
// prefix the scheduler composes a window-scoped key from.
func ExamplePeriodicTask() {
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())

	// A per-tenant task: once per window for every tenant the host's
	// tenant lister returns.
	err := reg.SchedulesSeat().Add(pkgcore.PeriodicTask{
		Type:      "storage.expiry_sweep",
		Every:     time.Hour,
		Scope:     pkgcore.PeriodicScopePerTenant,
		KeyPrefix: "storage.sweep:",
	})
	fmt.Println("add:", err)

	// A platform task runs once per window for the platform as a whole: its
	// task belongs to no single tenant, so it carries the sentinel tenant
	// its Job rows are enqueued under.
	err = reg.SchedulesSeat().Add(pkgcore.PeriodicTask{
		Type:           "pki.expiry_scan",
		Every:          time.Hour,
		Scope:          pkgcore.PeriodicScopePlatform,
		KeyPrefix:      "pki.expiry_scan:",
		PlatformTenant: pkgcore.TenantID("_pki_platform_scan"),
	})
	fmt.Println("add platform:", err)

	for _, decl := range reg.SchedulesSeat().Declarations() {
		fmt.Println(decl.Type, decl.Every, decl.Scope)
	}

	// A platform-scope declaration with no sentinel tenant could never be
	// enqueued, so it is refused at registration -- as is a second
	// declaration of an already owned task type.
	err = reg.SchedulesSeat().Add(pkgcore.PeriodicTask{Type: "pki.crl_regenerate", Every: time.Hour, Scope: pkgcore.PeriodicScopePlatform, KeyPrefix: "pki.crl_regenerate:"})
	fmt.Println("tenantless platform task:", errors.Is(err, pkgcore.ErrInvalidPeriodicTask))
	err = reg.SchedulesSeat().Add(pkgcore.PeriodicTask{Type: "pki.expiry_scan", Every: time.Hour, Scope: pkgcore.PeriodicScopePlatform, KeyPrefix: "pki.expiry_scan:", PlatformTenant: "_pki_platform_scan"})
	fmt.Println("duplicate:", errors.Is(err, pkgcore.ErrDuplicatePeriodicTask))

	// Output:
	// add: <nil>
	// add platform: <nil>
	// storage.expiry_sweep 1h0m0s per_tenant
	// pki.expiry_scan 1h0m0s platform
	// tenantless platform task: true
	// duplicate: true
}

// ExampleRegistrar shows the declaration face at work: a declaration body
// written against the Registrar view reaches the seats of whichever registry
// it is handed. Here it is a module Registry, whose seats accept a write the
// moment it is made; handed the component assembly's registry instead, the
// same body's writes would go through that assembly's Init-stage gate.
func ExampleRegistrar() {
	declare := func(reg pkgcore.Registrar) error {
		return reg.PermissionsSeat().Add("reports:read", "reports:export")
	}

	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	fmt.Println("declare:", declare(reg))
	fmt.Println(reg.Permissions.Permissions())

	// Output:
	// declare: <nil>
	// [reports:export reports:read]
}
