package authn

// password_bench_test.go benchmarks the password-verification hot path at
// the module's argon2id cost floor (DefaultPasswordParams, OWASP's first
// recommended configuration): every password sign-in pays one
// VerifyPassword, and every new hash pays one HashPassword, so these two
// numbers are the CPU cost of one credential check and one credential
// issuance on the current hardware. They are what a deployment owner weighs
// against raising Memory/Iterations -- the measured trade-off behind the
// "prefer raising Memory" guidance on PasswordParams. Part of the
// module-owned benchmark set docs/internal/20-quality-and-security.md's
// performance plan calls for; pure CPU and memory, no external services.
//
// Each operation derives a full argon2id digest (~19 MiB of memory at the
// floor), so a default one-second benchmark run completes only a handful of
// iterations -- that small count is itself the answer the benchmark
// reports. Run: go test -bench=BenchmarkPassword -benchmem .

import "testing"

// passwordBenchSink keeps the last verification outcome reachable so the
// compiler cannot elide the derivation.
var passwordBenchSink bool

// benchmarkPassword is the realistic credential the benchmarks hash and
// verify: a passphrase-shaped value, not an empty string.
const benchmarkPassword = "Bench-Passw0rd!2026"

// BenchmarkPasswordHash measures one HashPassword call: drawing the salt
// and deriving the argon2id digest under the module's cost floor.
func BenchmarkPasswordHash(b *testing.B) {
	params := DefaultPasswordParams()
	b.ReportAllocs()
	b.ResetTimer()
	var last string
	for i := 0; i < b.N; i++ {
		encoded, err := HashPassword(benchmarkPassword, params)
		if err != nil {
			b.Fatalf("HashPassword() error = %v", err)
		}
		last = encoded
	}
	passwordBenchSink = last != ""
}

// BenchmarkPasswordVerify measures one VerifyPassword call against a hash
// produced under the module's cost floor -- the per-sign-in check. The
// "wrong-password" variant costs the same derivation and reports the same
// number, which is the point of the constant-time discipline the verify
// path keeps.
func BenchmarkPasswordVerify(b *testing.B) {
	encoded, err := HashPassword(benchmarkPassword, DefaultPasswordParams())
	if err != nil {
		b.Fatalf("HashPassword() error = %v", err)
	}

	b.ReportAllocs()
	b.Run("correct-password", func(b *testing.B) {
		b.ResetTimer()
		var last bool
		for i := 0; i < b.N; i++ {
			ok, err := VerifyPassword(encoded, benchmarkPassword)
			if err != nil {
				b.Fatalf("VerifyPassword() error = %v", err)
			}
			last = ok
		}
		passwordBenchSink = last
	})
	b.Run("wrong-password", func(b *testing.B) {
		b.ResetTimer()
		var last bool
		for i := 0; i < b.N; i++ {
			ok, err := VerifyPassword(encoded, "Bench-Wrong-Passw0rd!2026")
			if err != nil {
				b.Fatalf("VerifyPassword() error = %v", err)
			}
			last = ok
		}
		passwordBenchSink = last
	})
}
