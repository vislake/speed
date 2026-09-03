package pkgcore_test

// Runnable documentation for the pkgcore public API. Every example here is
// compiled and executed by `go test`, so an API change that invalidates the
// documented usage fails the build instead of silently rotting.

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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
	kernel := pkgcore.NewKernel()

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

// ExampleWithEventBus shows the code-injection layer of implementation
// composition, and the capability validation that replaced the old
// mode-keyed "distributed mode has no built-in implementation" checks.
// DeploymentModeDistributed requires every seam's resolved implementation to
// declare MultiReplicaSafe; the standalone Preset a bare NewKernel() would
// otherwise resolve to does not, so the host injects its own.
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

// ExampleNewS3ObjectStore shows the distributed deployment mode's object
// store: an S3-compatible service (MinIO, Aliyun OSS or AWS S3) reached
// through the bucket and credentials in S3Config, the counterpart of the
// local store of ExampleNewLocalObjectStore. Nothing is dialed at
// construction -- the service is contacted on the first operation -- so a
// host can wire the store at startup whether or not the service is
// reachable, and an unusable configuration (an empty endpoint, bucket or
// credential) panics there instead, where the wiring error is visible.
func ExampleNewS3ObjectStore() {
	store := pkgcore.NewS3ObjectStore(pkgcore.S3Config{
		Endpoint:  "s3.example.com:9000",
		Bucket:    "objects",
		AccessKey: "access-key",
		SecretKey: "secret-key",
		Region:    "us-east-1",
		UseSSL:    true, // HTTPS: the setting for anything beyond a local MinIO
	})
	//nolint:staticcheck // QF1011: the assertion doubles as written doc that
	// this constructor satisfies the ObjectStore interface -- the local-store
	// counterpart of ExampleNewLocalObjectStore -- so it is kept rather than
	// inlined, which would leave the value unused.
	var _ pkgcore.ObjectStore = store // drop-in for the local store of ExampleNewLocalObjectStore

	fmt.Println("store wired; the first operation contacts the service")
	// Output:
	// store wired; the first operation contacts the service
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
// (EventBusRegistry and its siblings) with the built-in implementations
// listed in this package's AGENTS.md, and a host registers its own the same
// way this example registers a test one.
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
// has not injected directly. WithPreset replaces the whole map; injecting a
// seam directly (WithMailer, here) still wins over the Preset for that one
// seam, regardless of option order.
func ExampleWithPreset() {
	// A host that wants pkgcore's own Redis-backed EventBus and KVStore, but
	// keeps the console Mailer, composes a Preset from the two built-in ones:
	// PresetDistributed's eventbus/kv entries, PresetStandalone's mailer
	// entry. Nothing is dialed by resolving "eventbus.redis"/"kv.redis" here
	// (see NewRedisEventBus and NewRedisKVStore's own doc comments), so this
	// example needs no real Redis to run.
	custom := pkgcore.Preset{
		"eventbus":    pkgcore.PresetDistributed["eventbus"],
		"kv":          pkgcore.PresetDistributed["kv"],
		"mailer":      pkgcore.PresetStandalone["mailer"],
		"objectstore": pkgcore.PresetStandalone["objectstore"],
	}
	reg, err := pkgcore.NewKernel(pkgcore.WithPreset(custom)).Bootstrap(context.Background())
	fmt.Println(err, reg.EventBus() != nil, reg.KVStore() != nil, reg.Mailer() != nil)

	// Output:
	// <nil> true true true
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
// default host, bucket or credential, so resolving either through a Preset
// with an empty Config fails instead of building an unusable mailer or
// store. A host that wants real credentials injects the implementation
// directly with WithMailer or WithObjectStore instead (see
// ExampleNewSMTPMailer and ExampleNewS3ObjectStore).
func ExampleErrMissingSeamConfig() {
	_, _, err := pkgcore.MailerRegistry.Build("mailer.smtp", pkgcore.Config{})
	fmt.Println(errors.Is(err, pkgcore.ErrMissingSeamConfig))

	_, _, err = pkgcore.ObjectStoreRegistry.Build("objectstore.s3", pkgcore.Config{})
	fmt.Println(errors.Is(err, pkgcore.ErrMissingSeamConfig))

	// Output:
	// true
	// true
}
