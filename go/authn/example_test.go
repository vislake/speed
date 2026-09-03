package authn_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/vislake/speed/go/authn"
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
	// published events: 6
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
