package authn_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
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

// exampleKeySource is a minimal, self-contained authn.KeySource for these
// examples, so they need no live go/pki module: a production deployment
// wires a *pki.Service here instead (docs/internal/22-pki.md's "authn's
// integration" section), satisfying authn.KeySource structurally with zero import
// edge between the two packages -- exactly the property this example
// package's own freedom from a go/pki dependency demonstrates in practice.
type exampleKeySource struct {
	activeKID string
	active    ed25519.PrivateKey
	verify    map[string]ed25519.PublicKey
}

// newExampleKeySource returns an exampleKeySource with one freshly
// generated active key under kid.
func newExampleKeySource(kid string) *exampleKeySource {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return &exampleKeySource{activeKID: kid, active: priv, verify: map[string]ed25519.PublicKey{kid: pub}}
}

// EnsurePurpose implements authn.KeySource. This example source is always
// already provisioned, so there is nothing to do.
func (k *exampleKeySource) EnsurePurpose(context.Context, string, string, time.Duration) error {
	return nil
}

// ActiveSigner implements authn.KeySource.
func (k *exampleKeySource) ActiveSigner(_ context.Context, _ string) (string, string, func(context.Context, []byte) ([]byte, error), error) {
	kid, priv := k.activeKID, k.active
	sign := func(_ context.Context, input []byte) ([]byte, error) {
		return ed25519.Sign(priv, input), nil
	}
	return kid, "ed25519", sign, nil
}

// VerificationKeys implements authn.KeySource.
func (k *exampleKeySource) VerificationKeys(_ context.Context, _ string) ([]struct {
	KID       string
	Algorithm string
	Public    crypto.PublicKey
}, error,
) {
	out := make([]struct {
		KID       string
		Algorithm string
		Public    crypto.PublicKey
	}, 0, len(k.verify))
	for kid, pub := range k.verify {
		out = append(out, struct {
			KID       string
			Algorithm string
			Public    crypto.PublicKey
		}{KID: kid, Algorithm: "ed25519", Public: pub})
	}
	return out, nil
}

// rotate promotes a freshly generated key under newKID to active, keeping
// the previous active key around for verification only -- the same
// pending->active/previous->retiring shape go/pki's Service drives for
// real, illustrated here without depending on go/pki.
func (k *exampleKeySource) rotate(newKID string) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	k.activeKID = newKID
	k.active = priv
	k.verify[newKID] = pub
}

// compile-time check that exampleKeySource satisfies authn.KeySource.
var _ authn.KeySource = (*exampleKeySource)(nil)

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

// ExampleKeySource rotates the access-token signing key through a
// KeySource. The retired key keeps verifying tokens signed before the
// rotation, so outstanding sessions survive it -- the same property
// go/pki's own Service provides for real; this example uses a minimal
// hand-rolled KeySource so it needs no live pki module (see
// exampleKeySource's own doc comment).
func ExampleKeySource() {
	ctx := context.Background()
	keys := newExampleKeySource("2026-01")

	signer, err := authn.NewSigner(keys)
	if err != nil {
		panic(err)
	}
	verifier, err := authn.NewVerifier(keys)
	if err != nil {
		panic(err)
	}

	token, _, err := signer.Issue(ctx, authn.Principal{
		UserID: "user-1", TenantID: pkgcore.TenantID("tenant-a"), SessionID: "session-1",
	})
	if err != nil {
		panic(err)
	}

	// Rotate: the new key signs, the old one is kept for verification --
	// signer and verifier share the same KeySource, so both see the
	// rotation immediately.
	keys.rotate("2026-07")

	principal, err := verifier.Verify(ctx, token)
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
	keys := newExampleKeySource("example")
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

	token, _, err := signer.Issue(context.Background(), authn.Principal{
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

	keys := newExampleKeySource("example")

	module, err := authn.NewModule(db,
		authn.WithKeySource(keys),
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

	keys := newExampleKeySource("example-mfa")

	var sms bytes.Buffer
	module, err := authn.NewModule(db,
		authn.WithKeySource(keys),
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

	enrolled, err := svc.EnrollTOTP(ctx, authn.Principal{UserID: user.ID})
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

// ExampleNewHandler drives authn's HTTP surface end to end: register, sign
// in, then list the caller's own sessions. NewHandler's routing comes from
// the spec-generated api.ServerInterface (api/authn-server.gen.go,
// regenerated from api/openapi.yaml by task api:gen), so this is also a
// compilable proof that the handler still implements every operation the
// fragment declares.
//
// The protected /sessions call only succeeds because it is wrapped in
// authn.Middleware, exactly as a real host must: Handler itself never
// verifies a bearer token -- see requirePrincipal's doc comment in
// handler.go and ExampleNewPrincipalResolver's identical middleware-chain
// wiring above.
func ExampleNewHandler() {
	ctx := context.Background()
	module, _ := exampleModule(ctx)
	svc := module.Service()
	handler := authn.Middleware(svc.Verifier())(authn.NewHandler(svc))

	registerBody, _ := json.Marshal(map[string]string{
		"email": "http-demo@example.com", "password": "a perfectly fine passphrase",
	})
	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/authn/register", bytes.NewReader(registerBody))
	registerRec := httptest.NewRecorder()
	handler.ServeHTTP(registerRec, registerReq)
	fmt.Println("register status:", registerRec.Code)

	loginBody, _ := json.Marshal(map[string]string{
		"identifier": "http-demo@example.com", "password": "a perfectly fine passphrase",
	})
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", bytes.NewReader(loginBody))
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, loginReq)

	var pair struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(loginRec.Body.Bytes(), &pair); err != nil {
		panic(err)
	}
	fmt.Println("login status:", loginRec.Code)

	sessionsReq := httptest.NewRequest(http.MethodGet, "/api/v1/authn/sessions", nil)
	sessionsReq.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	sessionsRec := httptest.NewRecorder()
	handler.ServeHTTP(sessionsRec, sessionsReq)
	fmt.Println("sessions status:", sessionsRec.Code)

	// Output:
	// register status: 201
	// login status: 200
	// sessions status: 200
}
