package authn

// token_bench_test.go benchmarks the access-token hot paths: Issue mints a
// signed Ed25519 token on every sign-in and every tenant switch, and Verify
// runs on every authenticated request, resolving the verification keys
// through the KeySource seam each time (the per-call resolution is part of
// the measured cost -- a KeySource whose lookups cache or hit a database
// moves that number). Part of the module-owned benchmark set
// docs/internal/20-quality-and-security.md's performance plan calls for;
// the KeySource is the module's in-memory test double, no external services.
//
// Run: go test -bench=BenchmarkToken -benchmem .

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
)

// tokenBenchSink keeps the last issue/verify outcome reachable so the
// compiler cannot elide either call.
var tokenBenchSink any

// benchTokenPrincipal is the realistic principal an issued token carries: a
// signed-in user inside one tenant with a session established by password
// and an MFA step-up.
var benchTokenPrincipal = Principal{
	UserID:    "user-1042",
	TenantID:  pkgcore.TenantID("tenant-acme"),
	SessionID: "session-77e1a9",
	AMR:       []string{"password", "mfa:totp"},
}

// BenchmarkTokenIssue measures one Signer.Issue call: the ensure-signing-key
// gate (a no-op after the first call), the ActiveSigner resolution through
// the KeySource, the claim construction and the Ed25519 signature.
func BenchmarkTokenIssue(b *testing.B) {
	keys := testutil.NewKeySource(b, "kid-active")
	signer, err := NewSigner(keys)
	if err != nil {
		b.Fatalf("NewSigner() error = %v", err)
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	var last string
	for i := 0; i < b.N; i++ {
		raw, _, err := signer.Issue(ctx, benchTokenPrincipal)
		if err != nil {
			b.Fatalf("Issue() error = %v", err)
		}
		last = raw
	}
	tokenBenchSink = last
}

// BenchmarkTokenVerify measures one Verifier.Verify call against a token
// issued under the same KeySource: the JWT parse, the key lookup through
// KeySource.VerificationKeys (called afresh on every verification), the
// Ed25519 signature check and the claim validation -- the steady-state cost
// of authenticating one request.
func BenchmarkTokenVerify(b *testing.B) {
	keys := testutil.NewKeySource(b, "kid-active")
	signer, err := NewSigner(keys)
	if err != nil {
		b.Fatalf("NewSigner() error = %v", err)
	}
	verifier, err := NewVerifier(keys)
	if err != nil {
		b.Fatalf("NewVerifier() error = %v", err)
	}
	ctx := context.Background()

	raw, _, err := signer.Issue(ctx, benchTokenPrincipal)
	if err != nil {
		b.Fatalf("Issue() error = %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	var last Principal
	for i := 0; i < b.N; i++ {
		p, err := verifier.Verify(ctx, raw)
		if err != nil {
			b.Fatalf("Verify() error = %v", err)
		}
		last = p
	}
	tokenBenchSink = last
}
