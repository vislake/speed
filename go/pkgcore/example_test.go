package pkgcore_test

// Runnable documentation for the pkgcore public API. Every example here is
// compiled and executed by `go test`, so an API change that invalidates the
// documented usage fails the build instead of silently rotting.

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// ExampleParseDeploymentMode shows how a host turns the SPEED_DEPLOYMENT_MODE
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

	// IncrByFloat and CompareAndSwap are the only atomic primitives on the
	// interface: build read-modify-write cycles from them, never from Get+Set.
	used, err := kv.IncrByFloat(ctx, "quota:acme:credits", 2.5)
	fmt.Printf("incr %v err=%v\n", used, err)

	// Against an empty old value, CompareAndSwap is set-if-absent.
	acquired, err := kv.CompareAndSwap(ctx, "lock:acme:import", nil, []byte("held"))
	fmt.Printf("cas %t err=%v\n", acquired, err)

	// Output:
	// get "token" found=true err=<nil>
	// missing found=false err=<nil>
	// incr 2.5 err=<nil>
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

	// A failing handler does not stop the handlers registered after it; every
	// failure is reported back to the publisher instead.
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

func (exampleTenancyModule) Register(reg *pkgcore.Registry) error {
	return reg.Permissions.Add("tenant:read")
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
func (exampleBillingModule) Register(reg *pkgcore.Registry) error {
	reg.Routes.Mount("/api/v1/billing", http.NotFoundHandler())

	if err := reg.Config.Add(pkgcore.ConfigItem{
		Key:         "billing.invoice_retry_limit",
		Type:        "int",
		Default:     3,
		Description: "How many times a failed invoice charge is retried.",
	}); err != nil {
		return err
	}
	if err := reg.Features.Add(pkgcore.FeatureFlag{
		Key:         "billing.dunning",
		Default:     false,
		Description: "Chase failed payments on a retry schedule.",
	}); err != nil {
		return err
	}
	if err := reg.Permissions.Add("billing:read", "billing:write"); err != nil {
		return err
	}
	if err := reg.AuditActions.Add("billing.subscription_cancelled"); err != nil {
		return err
	}
	if err := reg.Events.Publishes(pkgcore.EventDecl{
		Type:        "billing.invoice.paid",
		PayloadType: "billing.InvoicePaid",
		Description: "An invoice was paid in full.",
	}); err != nil {
		return err
	}

	reg.Events.Subscribe("authn.user_created", func(ctx context.Context, evt pkgcore.Event) error {
		fmt.Println("billing: opening a credit ledger for tenant", evt.TenantID)
		return nil
	})
	return nil
}

// ExampleKernel_Bootstrap shows the host side of module wiring: hand the
// kernel every module, get one Registry back with everything they contributed.
func ExampleKernel_Bootstrap() {
	kernel := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone)

	// Modules may be listed in any order. Bootstrap sorts them so that each one
	// registers after the modules it depends on.
	reg, err := kernel.Bootstrap(context.Background(), exampleBillingModule{}, exampleTenancyModule{})
	if err != nil {
		fmt.Println("bootstrap:", err)
		return
	}

	fmt.Println("deployment mode:", kernel.DeploymentMode())
	for _, route := range reg.Routes.Routes() {
		fmt.Println("route:", route.Path)
	}
	fmt.Println("permissions:", reg.Permissions.Permissions())
	fmt.Println("audit actions:", reg.AuditActions.Actions())
	for _, decl := range reg.Events.Published() {
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
	if err := reg.Features.Add(pkgcore.FeatureFlag{
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

// ExampleWithEventBus shows the distributed-mode wiring seam.
// DeploymentModeDistributed has no built-in EventBus, because falling back to
// the in-memory one would give every replica a private bus, so the host
// injects its own.
func ExampleWithEventBus() {
	// Assembling a distributed-mode kernel without a bus fails at startup. A
	// real distributed-mode host would also need WithKVStore and WithMailer,
	// but their checks run after the EventBus check, so leaving them out here
	// still isolates this example to the EventBus failure.
	_, err := pkgcore.NewKernel(pkgcore.DeploymentModeDistributed).Bootstrap(context.Background())
	fmt.Println(errors.Is(err, pkgcore.ErrMissingDistributedEventBus))

	// A real host passes its broker-backed bus here; the in-memory one stands
	// in for it in this example. DeploymentModeDistributed requires a KVStore
	// and a Mailer too, so both are wired alongside the bus with their own
	// in-memory stand-ins.
	kernel := pkgcore.NewKernel(pkgcore.DeploymentModeDistributed,
		pkgcore.WithEventBus(pkgcore.NewMemoryEventBus()),
		pkgcore.WithKVStore(pkgcore.NewMemoryKVStore()),
		pkgcore.WithMailer(pkgcore.NewConsoleMailer()))
	reg, err := kernel.Bootstrap(context.Background(), exampleTenancyModule{})
	fmt.Println(err, reg.EventBus() != nil)

	// Output:
	// true
	// <nil> true
}

// ExampleWithKVStore shows the distributed-mode wiring seam for the
// key-value seam, mirroring ExampleWithEventBus. DeploymentModeDistributed
// has no built-in KVStore, because falling back to the in-memory one would
// give every replica a private store, so the host injects its own.
func ExampleWithKVStore() {
	// Assembling a distributed-mode kernel without a store fails at startup,
	// once the bus is wired: the bus check runs first, so it is wired here
	// too, to isolate the failure this example is about to the KVStore check.
	_, err := pkgcore.NewKernel(pkgcore.DeploymentModeDistributed, pkgcore.WithEventBus(pkgcore.NewMemoryEventBus())).
		Bootstrap(context.Background())
	fmt.Println(errors.Is(err, pkgcore.ErrMissingDistributedKVStore))

	// A real host passes its Redis-backed store here; the in-memory one
	// stands in for it in this example. The Mailer seam is wired too, because
	// its check runs after the KVStore one and this kernel will succeed.
	kernel := pkgcore.NewKernel(pkgcore.DeploymentModeDistributed,
		pkgcore.WithEventBus(pkgcore.NewMemoryEventBus()),
		pkgcore.WithKVStore(pkgcore.NewMemoryKVStore()),
		pkgcore.WithMailer(pkgcore.NewConsoleMailer()))
	reg, err := kernel.Bootstrap(context.Background(), exampleTenancyModule{})
	fmt.Println(err, reg.KVStore() != nil)

	// Output:
	// true
	// <nil> true
}

// ExampleWithMailer shows the distributed-mode wiring seam for the mail
// seam, mirroring ExampleWithEventBus and ExampleWithKVStore.
// DeploymentModeDistributed has no built-in Mailer, because falling back to
// the console mailer would print every message to a replica's stdout where
// nobody reads it, so the host injects its own.
func ExampleWithMailer() {
	// Assembling a distributed-mode kernel without a mailer fails at startup,
	// once the bus and the store are wired: their checks run first, so both
	// are wired here too, to isolate the failure this example is about to the
	// Mailer check.
	_, err := pkgcore.NewKernel(pkgcore.DeploymentModeDistributed,
		pkgcore.WithEventBus(pkgcore.NewMemoryEventBus()),
		pkgcore.WithKVStore(pkgcore.NewMemoryKVStore())).
		Bootstrap(context.Background())
	fmt.Println(errors.Is(err, pkgcore.ErrMissingDistributedMailer))

	// A real host passes its SMTP-backed mailer here; the console one stands
	// in for it in this example.
	kernel := pkgcore.NewKernel(pkgcore.DeploymentModeDistributed,
		pkgcore.WithEventBus(pkgcore.NewMemoryEventBus()),
		pkgcore.WithKVStore(pkgcore.NewMemoryKVStore()),
		pkgcore.WithMailer(pkgcore.NewConsoleMailer()))
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
	var _ pkgcore.Mailer = mailer // drop-in for the console mailer of ExampleNewConsoleMailer

	fmt.Println("mailer wired; the first Send dials the relay")
	// Output:
	// mailer wired; the first Send dials the relay
}
