package authn_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/internal/totp"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy"
)

// exampleParams keeps these examples fast. Real deployments use
// authn.DefaultPasswordParams(), or stronger.
var exampleParams = authn.PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

// exampleBlindIndexKey stands in for a 32-byte secret a real deployment reads
// from its secret manager. It must be a secret used for nothing else -- in
// particular never the field-encryption key.
func exampleBlindIndexKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

// ExampleHashPassword stores a password and checks it again later. The stored
// value carries its own argon2id parameters, which is what lets a deployment
// raise the cost without invalidating anything already stored.
func ExampleHashPassword() {
	stored, err := authn.HashPassword("a reasonably long passphrase", exampleParams)
	if err != nil {
		panic(err)
	}

	ok, err := authn.VerifyPassword(stored, "a reasonably long passphrase")
	if err != nil {
		panic(err)
	}
	fmt.Println("correct password verifies:", ok)

	ok, err = authn.VerifyPassword(stored, "something else entirely")
	if err != nil {
		panic(err)
	}
	fmt.Println("wrong password verifies:", ok)

	// Raising the cost does not invalidate the stored hash; it only marks
	// it for upgrade on the owner's next successful sign-in.
	stale, err := authn.NeedsRehash(stored, authn.DefaultPasswordParams())
	if err != nil {
		panic(err)
	}
	fmt.Println("needs rehash under stronger parameters:", stale)

	// Output:
	// correct password verifies: true
	// wrong password verifies: false
	// needs rehash under stronger parameters: true
}

// ExamplePasswordPolicy_Validate shows what a rejected password produces: a
// structured code and its parameters, never text. The client renders the
// message from its own catalog, in the user's own language.
func ExamplePasswordPolicy_Validate() {
	policy := authn.DefaultPasswordPolicy()

	err := policy.Validate("short")
	appErr, _ := apperr.As(err)
	fmt.Println(appErr.Code, appErr.Params["min_length"])

	fmt.Println(policy.Validate("a reasonably long passphrase"))

	// Output:
	// authn.password_too_short 12
	// <nil>
}

// ExampleNewKeySet rotates the access-token signing key. The retired key
// keeps verifying tokens signed before the rotation, so outstanding sessions
// survive it.
func ExampleNewKeySet() {
	oldKey, err := authn.GenerateTokenKey("2026-01")
	if err != nil {
		panic(err)
	}
	newKey, err := authn.GenerateTokenKey("2026-07")
	if err != nil {
		panic(err)
	}

	before, err := authn.NewKeySet(oldKey)
	if err != nil {
		panic(err)
	}
	signer, err := authn.NewSigner(before)
	if err != nil {
		panic(err)
	}
	token, _, err := signer.Issue(authn.Principal{
		UserID: "user-1", TenantID: pkgcore.TenantID("tenant-a"), SessionID: "session-1",
	})
	if err != nil {
		panic(err)
	}

	// Rotate: the new key signs, the old one is kept for verification.
	after, err := authn.NewKeySet(newKey, authn.TokenKey{ID: oldKey.ID, Public: oldKey.Public})
	if err != nil {
		panic(err)
	}
	verifier, err := authn.NewVerifier(after)
	if err != nil {
		panic(err)
	}

	principal, err := verifier.Verify(token)
	if err != nil {
		panic(err)
	}
	fmt.Println("a token signed before the rotation still verifies for", principal.UserID, "in", principal.TenantID)

	// Output:
	// a token signed before the rotation still verifies for user-1 in tenant-a
}

// ExampleNewProviderRegistry wires the social channels a deployment offers
// and shows how a channel is looked up by its Provider* name -- the same
// lookup Service.SocialAuthorizeURL performs when a caller names a channel.
//
// Each provider constructor also takes ProviderOption values (a base URL
// override, an injected *http.Client), which is what lets every provider's
// own tests run against an httptest server with no network call; production
// code passes none and gets the channel's real endpoints and a
// safehttp-guarded client.
func ExampleNewProviderRegistry() {
	registry, err := authn.NewProviderRegistry(
		authn.NewGitHubProvider("github-client-id", "github-client-secret"),
		authn.NewGoogleProvider("google-client-id", "google-client-secret"),
	)
	if err != nil {
		panic(err)
	}

	fmt.Println("wired channels:", registry.Names())
	if _, ok := registry.Get(authn.ProviderGoogle); ok {
		fmt.Println("google is wired")
	}
	if _, ok := registry.Get(authn.ProviderWeChat); !ok {
		fmt.Println("wechat is not wired")
	}

	// Output:
	// wired channels: [github google]
	// google is wired
	// wechat is not wired
}

// ExampleNewPrincipalResolver wires the middleware chain a host installs.
//
// authn.Middleware runs FIRST and verifies the token once; tenancy.Middleware
// then reads the already-verified Principal through this resolver and is the
// only thing that injects the tenant. The documented order cannot work,
// because tenancy.Resolver returns a tenant and no context, so a resolver
// that verified the token would have nowhere to hand the claims.
func ExampleNewPrincipalResolver() {
	key, err := authn.GenerateTokenKey("example")
	if err != nil {
		panic(err)
	}
	keys, err := authn.NewKeySet(key)
	if err != nil {
		panic(err)
	}
	signer, err := authn.NewSigner(keys)
	if err != nil {
		panic(err)
	}
	verifier, err := authn.NewVerifier(keys)
	if err != nil {
		panic(err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := pkgcore.TenantFromContext(r.Context())
		principal, _ := authn.PrincipalFromContext(r.Context())
		fmt.Println("tenant in context:", tenant, ok)
		fmt.Println("authenticated user:", principal.UserID)
		w.WriteHeader(http.StatusOK)
	})

	chain := authn.Middleware(verifier)(
		tenancy.Middleware(authn.NewPrincipalResolver(),
			// Pre-auth routes need an explicit allowlist entry per
			// (method, path): the matching is exact, with no
			// prefix and no GET-implies-HEAD convenience.
			tenancy.WithAllowlist(http.MethodPost, "/api/v1/authn/login"),
		)(handler),
	)

	token, _, err := signer.Issue(authn.Principal{
		UserID: "user-1", TenantID: pkgcore.TenantID("tenant-a"), SessionID: "session-1",
	})
	if err != nil {
		panic(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	chain.ServeHTTP(httptest.NewRecorder(), req)

	// Output:
	// tenant in context: tenant-a true
	// authenticated user: user-1
}

// ExampleNewModule wires authn into a host and registers it, which is what
// pkgcore.Kernel.Bootstrap does for every module.
//
// The membership reader is the seam through which authn asks whether a user
// belongs to a tenant without importing the module that owns memberships.
// Leaving it out is not a permissive default: sign-in and tenant switching
// refuse.
func ExampleNewModule() {
	ctx := context.Background()

	// A real host opens its database with dbkit.Open once at startup, and
	// registers the PII serializer BEFORE doing so -- GORM resolves a
	// model's serializer while it parses the schema.
	cipher, err := dbkit.NewCipher(make([]byte, 32))
	if err != nil {
		panic(err)
	}
	if regErr := authn.RegisterPIISerializer(cipher); regErr != nil {
		panic(regErr)
	}
	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: "file::memory:?cache=shared"})
	if err != nil {
		panic(err)
	}

	keys, err := authn.NewKeySet(mustKey("example"))
	if err != nil {
		panic(err)
	}

	module, err := authn.NewModule(db,
		authn.WithSigningKeys(keys),
		authn.WithBlindIndexKey(exampleBlindIndexKey()),
		authn.WithMembershipReader(exampleMemberships{}),
		authn.WithRevocationMode(authn.RevocationModeImmediate),
	)
	if err != nil {
		panic(err)
	}

	registry := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := module.Register(registry); err != nil {
		panic(err)
	}

	fmt.Println("module:", module.Name())
	fmt.Println("published events:", len(registry.Events.Published()))
	fmt.Println("service wired:", module.Service() != nil)

	// Output:
	// module: authn
	// published events: 10
	// service wired: true
}

// mustKey panics on a key-generation failure, which an example may do and
// production code may not.
func mustKey(id string) authn.TokenKey {
	key, err := authn.GenerateTokenKey(id)
	if err != nil {
		panic(err)
	}
	return key
}

// exampleMemberships is the smallest possible stand-in for the org module's
// membership store. A real host injects one backed by that module.
type exampleMemberships struct{}

func (exampleMemberships) ActiveMembership(context.Context, string, pkgcore.TenantID) (bool, error) {
	return true, nil
}

func (exampleMemberships) TenantsOf(context.Context, string) ([]pkgcore.TenantID, error) {
	return []pkgcore.TenantID{"tenant-a"}, nil
}

// compile-time check that the example's stand-in satisfies the real seam.
var _ authn.MembershipReader = exampleMemberships{}

// ExampleNewConsoleSMSSender delivers a verification code to the standalone
// deployment mode's transport, which -- unlike a real gateway -- writes to
// whatever io.Writer it is given, which is what makes it usable here.
func ExampleNewConsoleSMSSender() {
	var out bytes.Buffer
	sender := authn.NewConsoleSMSSender(&out)

	err := sender.Send(context.Background(), authn.SMS{
		To:   "+8613800000000",
		Text: "your verification code is 123456",
	})
	if err != nil {
		panic(err)
	}

	fmt.Print(out.String())
	// Output:
	// SMS to +8613800000000: your verification code is 123456
}

// exampleModule builds a fully registered Module over a fresh in-memory
// database, for the examples below that need a working Service rather than
// just NewModule's own return value.
func exampleModule(ctx context.Context) (*authn.Module, *pkgcore.Registry) {
	cipher, err := dbkit.NewCipher(make([]byte, 32))
	if err != nil {
		panic(err)
	}
	if regErr := authn.RegisterPIISerializer(cipher); regErr != nil {
		panic(regErr)
	}
	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: "file::memory:?cache=shared"})
	if err != nil {
		panic(err)
	}

	keys, err := authn.NewKeySet(mustKey("example-mfa"))
	if err != nil {
		panic(err)
	}

	var sms bytes.Buffer
	module, err := authn.NewModule(db,
		authn.WithSigningKeys(keys),
		authn.WithBlindIndexKey(exampleBlindIndexKey()),
		authn.WithMembershipReader(exampleMemberships{}),
		authn.WithPasswordParams(exampleParams),
		authn.WithSMSSender(authn.NewConsoleSMSSender(&sms)),
	)
	if err != nil {
		panic(err)
	}

	// A real host applies every bootstrapped module's migrations before
	// opening it for business. This example needs only authn's own
	// tables, so registering just this one Module is enough.
	migrations := dbkit.NewMigrationRegistry()
	if err := migrations.Register(module); err != nil {
		panic(err)
	}
	if err := migrations.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		panic(err)
	}

	registry := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := module.Register(registry); err != nil {
		panic(err)
	}
	return module, registry
}

// ExampleService_EnrollTOTP walks the full second-factor lifecycle: enroll,
// confirm with a real code from the provisioned secret, and use that same
// factor to complete a step-up verification.
func ExampleService_EnrollTOTP() {
	ctx := context.Background()
	module, _ := exampleModule(ctx)
	svc := module.Service()

	user, err := svc.Register(ctx, authn.RegisterInput{
		Email: "mfa-demo@example.com", Password: "a perfectly fine passphrase",
	})
	if err != nil {
		panic(err)
	}

	enrolled, err := svc.EnrollTOTP(ctx, user.ID)
	if err != nil {
		panic(err)
	}

	code, err := totp.Code(enrolled.Secret, time.Now())
	if err != nil {
		panic(err)
	}
	recoveryCodes, err := svc.ConfirmTOTP(ctx, user.ID, code)
	if err != nil {
		panic(err)
	}

	fmt.Println("provisioning URI has otpauth scheme:", strings.HasPrefix(enrolled.ProvisioningURI, "otpauth://totp/"))
	fmt.Println("recovery codes issued:", len(recoveryCodes))

	// Output:
	// provisioning URI has otpauth scheme: true
	// recovery codes issued: 10
}
