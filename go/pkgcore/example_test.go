package pkgcore_test

// Runnable documentation for the pkgcore public API. Every example here is
// compiled and executed by `go test`, so an API change that invalidates the
// documented usage fails the build instead of silently rotting.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/vislake/speed/go/pkgcore"
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

// ExampleErrTransportPermanent shows the one classification a delivery
// caller reads out of a transport failure: a cause wrapping the sentinel is
// the destination's own permanent refusal -- terminal, never retried --
// while a transient failure travels unwrapped and stays retryable. The
// transports themselves wrap it (an SMTP relay's 5xx answer to RCPT, a
// carrier's invalid-number verdict); a caller only ever matches it with
// errors.Is, and go/notification answers the marked failure by marking its
// external contact bounced.
func ExampleErrTransportPermanent() {
	// What a transport returns for a permanently refused destination.
	refused := fmt.Errorf("smtp: %w: 550 5.1.1 No such user", pkgcore.ErrTransportPermanent)
	fmt.Println(errors.Is(refused, pkgcore.ErrTransportPermanent))

	// What it returns for a transient failure: no sentinel, retryable.
	transient := errors.New("smtp: 421 4.3.2 service not available")
	fmt.Println(errors.Is(transient, pkgcore.ErrTransportPermanent))

	// Output:
	// true
	// false
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

// ExampleCapability shows the bitmask deployment mode and implementation
// composition compare: an implementation declares what it Has, a deployment
// mode declares what it requires, and the assembly checks the two
// against each other. See the sibling Example functions for that check
// running for real, inside the assembly.
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

// ExampleSeamRegistry shows the name-to-constructor directory mechanism a
// module keeps its own implementations in -- the shape go/ai-gateway's
// provider registries and go/pki's signer registry use to build a member
// chosen by a name their own configuration carries.
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

// ExampleDeploymentMode_RequiredCapabilities shows what the assembly's
// Prepare stage compares every selected component's declared Capability
// against.
func ExampleDeploymentMode_RequiredCapabilities() {
	fmt.Println(pkgcore.DeploymentModeStandalone.RequiredCapabilities())
	fmt.Println(pkgcore.DeploymentModeDistributed.RequiredCapabilities())

	// Output:
	// none
	// MultiReplicaSafe
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

// ExampleGetOptional shows the optional-dependency reading of the by-type
// context: an absent value is a fact the caller branches on -- the
// dependency's documented default applies -- never an error, while a true
// lookup error is the caller's to handle, conventionally by propagating it.
// Get stays the reading for a required dependency, where absence is itself
// the error.
func ExampleGetOptional() {
	reg := pkgcore.NewComponentRegistry()

	// The composition wired no mailer: absence, reported as a fact.
	mailer, ok, err := pkgcore.GetOptional[pkgcore.Mailer](reg)
	fmt.Println(mailer != nil, ok, err)

	// A component construction put a console mailer: the value resolves.
	reg.Put(pkgcore.NewConsoleMailer())
	mailer, ok, err = pkgcore.GetOptional[pkgcore.Mailer](reg)
	fmt.Println(mailer != nil, ok, err)

	// Output:
	// false false <nil>
	// true true <nil>
}
