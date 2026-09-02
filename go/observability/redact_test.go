package observability_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// testSecret and friends are stand-in secret values the tests below log
// and then assert never reach the sink in plaintext. Each is deliberately
// long enough (>= 16 bytes) that value-shape scanning would also engage,
// so a test that logs one under a sensitive key exercises the key rule and
// a test that logs one under a benign key exercises the shape rule.
const (
	testSecret = "sup3r-s3cr3t-v4lue-9876543210"
	testBearer = "Bearer abcDEFgh1234567890XYZmnopQRSTuvWX"
	testJWT    = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ"
)

// logThrough logs msg with args through a FromContext logger bound to buf
// (via textLoggerCtx) and returns everything the sink rendered.
func logThrough(ctx context.Context, buf *bytes.Buffer, msg string, args ...any) string {
	obs.FromContext(ctx).Info(msg, args...)
	return buf.String()
}

// TestRedact_SensitiveKeyValues is the primary contract test: no attribute
// whose key names a secret ever reaches the sink in plaintext, for the
// full documented key family. Each key -- plain, prefixed, dotted
// config-style, mixed-case, or carrying a hyphen -- must survive as a
// field name while its value is replaced wholesale by the deterministic
// RedactedValue. The subtest key is the assertion target: the table
// includes stems from every sensitiveStems entry.
func TestRedact_SensitiveKeyValues(t *testing.T) {
	keys := []string{
		// "token" stem.
		"token", "access_token", "refresh_token", "session_token",
		"id_token", "api_token", "csrf_token", "auth_token",
		// "secret" stem.
		"secret", "client_secret", "api_secret", "webhook_secret",
		// "key" stem.
		"api_key", "apikey", "private_key", "signing_key", "secret_key",
		"stripe_secret_key", "x_api_key", "key",
		// password family.
		"password", "passwd", "pwd", "db_password",
		// "authorization" stem.
		"authorization", "authorization_header",
		// "cookie" stem.
		"cookie", "set_cookie",
		// "credential" stem.
		"credential", "credentials",
		// Case- and separator-tolerance.
		"Token", "X-Api-Key",
		// Dotted config-style keys: every dot-separated segment is checked.
		"billing.stripe_secret_key", "services.github.webhook_secret",
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := textLoggerCtx(context.Background(), &buf)

			out := logThrough(ctx, &buf, "event", key, testSecret)

			if strings.Contains(out, testSecret) {
				t.Errorf("attribute %q leaked its value into the sink; got: %s", key, out)
			}
			if want := key + "=" + obs.RedactedValue; !strings.Contains(out, want) {
				t.Errorf("expected %q to be replaced by %q; got: %s", key, obs.RedactedValue, out)
			}
		})
	}
}

// TestRedact_ScalarKindsUnderSensitiveKey pins the type-consistency rule:
// whatever the value's slog kind -- an int, a duration, a struct -- a
// sensitive key replaces it with the same String-typed RedactedValue.
// Redaction must not depend on the value being a string.
func TestRedact_ScalarKindsUnderSensitiveKey(t *testing.T) {
	var buf bytes.Buffer
	ctx := textLoggerCtx(context.Background(), &buf)

	obs.FromContext(ctx).Info("event",
		"failed_password_attempts", 3,
		"session_token_duration", 5,
		"login_credentials", struct{ Name string }{Name: "bob"},
	)

	out := buf.String()
	for _, key := range []string{"failed_password_attempts", "session_token_duration", "login_credentials"} {
		if want := key + "=" + obs.RedactedValue; !strings.Contains(out, want) {
			t.Errorf("expected %q to be replaced by %q; got: %s", key, obs.RedactedValue, out)
		}
	}
	if strings.Contains(out, "bob") || strings.Contains(out, "=3") {
		t.Errorf("sensitive keys must redact non-string values wholesale; got: %s", out)
	}
}

// TestRedact_CorrelationKeysNeverRedacted is the counterpart guarantee to
// the test above: the correlation field names every module shares --
// tenant_id, user_id, job_id, trace_id, span_id -- always reach the sink
// verbatim, from the context enrichment path and from plain log calls
// alike. The user_id value is deliberately a string with a JWT shape: the
// exemption from value-shape scanning is what keeps an adversarial id
// value intact, so the test must prove even that survives.
func TestRedact_CorrelationKeysNeverRedacted(t *testing.T) {
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("acme"))
	var buf bytes.Buffer
	ctx = textLoggerCtx(ctx, &buf)

	// job_id under a plain key; user_id given a hostile-looking value.
	obs.FromContext(ctx).Info("event",
		"user_id", testJWT,
		"job_id", "job-42",
	)

	out := buf.String()
	if !strings.Contains(out, obs.TenantIDKey+"=acme") {
		t.Errorf("expected the context tenant to survive; got: %s", out)
	}
	if !strings.Contains(out, "user_id="+testJWT) {
		t.Errorf("user_id is exempt from value-shape scanning: a JWT-shaped id must survive verbatim; got: %s", out)
	}
	if !strings.Contains(out, "job_id=job-42") {
		t.Errorf("expected job_id to survive; got: %s", out)
	}
	if strings.Contains(out, obs.RedactedValue) {
		t.Errorf("correlation fields must never be redacted; got: %s", out)
	}
}

// TestRedact_LoggerWithAttrsCannotLeak closes the slog trap this package
// is built around: slog carries logger-level With attributes in the
// HANDLER, not in the record, so a wrapper that only redacted Handle's
// record would let logger.With("token", ...) sail past it. Attributes
// attached to a FromContext logger must be redacted just like call-site
// ones, while benign static attributes attached the same way survive.
func TestRedact_LoggerWithAttrsCannotLeak(t *testing.T) {
	var buf bytes.Buffer
	logger := obs.FromContext(textLoggerCtx(context.Background(), &buf))

	logger.With("note_id", "n-1").With("access_token", testSecret).Info("event")

	out := buf.String()
	if !strings.Contains(out, "note_id=n-1") {
		t.Errorf("expected the benign static attribute to survive; got: %s", out)
	}
	if strings.Contains(out, testSecret) {
		t.Errorf("static attributes attached via With must be redacted too; got: %s", out)
	}
	if want := "access_token=" + obs.RedactedValue; !strings.Contains(out, want) {
		t.Errorf("expected %q; got: %s", want, out)
	}
}

// TestRedact_Groups covers slog group semantics: a group whose own name is
// sensitive collapses in its entirety (the bucket is the secret), a group
// with a benign name is recursed into so its sensitive children redact
// while its benign children survive, and a logger-level WithGroup named
// like a secret -- whose name only lives in the handler's group context --
// must still redact what is logged under it.
func TestRedact_Groups(t *testing.T) {
	t.Run("sensitive group name collapses the whole group", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := textLoggerCtx(context.Background(), &buf)

		out := logThrough(ctx, &buf, "event",
			slog.Group("credentials", "username", "ops", "password", testSecret),
		)

		if want := "credentials=" + obs.RedactedValue; !strings.Contains(out, want) {
			t.Errorf("expected the whole group to collapse to %q; got: %s", want, out)
		}
		if strings.Contains(out, testSecret) || strings.Contains(out, "username=ops") {
			t.Errorf("a sensitive group must not leak any child; got: %s", out)
		}
	})

	t.Run("benign group recursed into", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := textLoggerCtx(context.Background(), &buf)

		out := logThrough(ctx, &buf, "event",
			slog.Group("session", "id", "s-1", "password", testSecret),
		)

		if !strings.Contains(out, "session.id=s-1") {
			t.Errorf("expected the benign child to survive; got: %s", out)
		}
		if want := "session.password=" + obs.RedactedValue; !strings.Contains(out, want) {
			t.Errorf("expected %q; got: %s", want, out)
		}
		if strings.Contains(out, testSecret) {
			t.Errorf("the sensitive child leaked; got: %s", out)
		}
	})

	t.Run("logger-level group named like a secret", func(t *testing.T) {
		var buf bytes.Buffer
		logger := obs.FromContext(textLoggerCtx(context.Background(), &buf))

		logger.WithGroup("token").With("value", testSecret).Info("event")

		out := buf.String()
		if want := "token.value=" + obs.RedactedValue; !strings.Contains(out, want) {
			t.Errorf("expected %q: a group named like a secret must redact what is logged under it; got: %s", want, out)
		}
	})
}

// TestRedact_SecretShapesInValues is the fallback net: a secret logged
// under a benign key must still not reach the sink, because its value has
// one of the canonical secret shapes. Matched regions are replaced in
// place -- surrounding text survives -- and each case also carries its
// negative control: lookalike benign values pass through untouched, so the
// net does not redact ordinary identifiers.
func TestRedact_SecretShapesInValues(t *testing.T) {
	cases := []struct {
		name     string
		value    string
		secret   string // fragment that must never reach the sink
		wantKeep []string
		wantMask []string
	}{
		{
			name:     "bearer token embedded in text",
			value:    "caller presented " + testBearer + " and was rejected",
			secret:   testBearer,
			wantKeep: []string{"caller presented"},
			wantMask: []string{obs.RedactedValue},
		},
		{
			name:   "jwt as whole value",
			value:  testJWT,
			secret: testJWT,
			wantMask: []string{obs.RedactedValue},
		},
		{
			name:     "stripe secret key",
			value:    "sk_live_51H3xYt9KqLmNpQrStUvWxYz8AbCdEfGh2JkLmN4PqR6sT8uV",
			secret:   "sk_live_51H3xYt9KqLmNpQrStUvWxYz8AbCdEfGh2JkLmN4PqR6sT8uV",
			wantMask: []string{obs.RedactedValue},
		},
		{
			name:     "aws access key id",
			value:    "AKIAIOSFODNN7EXAMPLE",
			secret:   "AKIAIOSFODNN7EXAMPLE",
			wantMask: []string{obs.RedactedValue},
		},
		{
			name:     "google api key",
			value:    "AIza" + strings.Repeat("x", 35),
			secret:   strings.Repeat("x", 35),
			wantMask: []string{obs.RedactedValue},
		},
		{
			name:     "github token",
			value:    "ghp_" + strings.Repeat("a", 36),
			secret:   strings.Repeat("a", 36),
			wantMask: []string{obs.RedactedValue},
		},
		{
			name:     "slack bot token",
			value:    "xoxb-123456789012-ABCDEFGHIJKLMNOPQRST",
			secret:   "xoxb-123456789012-ABCDEFGHIJKLMNOPQRST",
			wantMask: []string{obs.RedactedValue},
		},
		{
			name:     "gitlab personal token",
			value:    "glpat-" + strings.Repeat("x", 20),
			secret:   strings.Repeat("x", 20),
			wantMask: []string{obs.RedactedValue},
		},
		{
			name:     "secret in url query parameter",
			value:    "https://api.example.com/v1/charges?access_token=abcDEFgh1234567890&expand=charge",
			secret:   "abcDEFgh1234567890",
			wantKeep: []string{"https://api.example.com/v1/charges?", "&expand=charge"},
			wantMask: []string{"access_token=" + obs.RedactedValue},
		},
		{
			name:     "password in url userinfo",
			value:    "https://ops:sup3rSecretPw12345@db.internal:5432/pg",
			secret:   "sup3rSecretPw12345",
			wantKeep: []string{"https://ops:", "@db.internal:5432/pg"},
			wantMask: []string{"https://ops:" + obs.RedactedValue + "@db.internal"},
		},
		{
			name:     "uuid untouched",
			value:    "550e8400-e29b-41d4-a716-446655440000",
			secret:   "", // nothing may be masked
			wantKeep: []string{"550e8400-e29b-41d4-a716-446655440000"},
		},
		{
			name:     "url with benign parameters untouched",
			value:    "https://api.example.com/v1/charges?expand=charge&limit=25",
			secret:   "", // nothing may be masked
			wantKeep: []string{"https://api.example.com/v1/charges?expand=charge&limit=25"},
		},
		{
			name:     "short bearer-like text untouched",
			value:    "says Bearer abc to everyone",
			secret:   "", // below minSecretScanLen, and below secret strength
			wantKeep: []string{"says Bearer abc to everyone"},
		},
		{
			name:     "vendor prefix without key length untouched",
			value:    "the sk_ prefix alone is not a key",
			secret:   "", // gate hits but the shape requires 16+ key characters
			wantKeep: []string{"the sk_ prefix alone is not a key"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := textLoggerCtx(context.Background(), &buf)

			out := logThrough(ctx, &buf, "event", "details", tc.value)

			if tc.secret != "" && strings.Contains(out, tc.secret) {
				t.Errorf("secret-shaped value leaked into the sink; got: %s", out)
			}
			for _, keep := range tc.wantKeep {
				if !strings.Contains(out, keep) {
					t.Errorf("expected surrounding text %q to survive masking; got: %s", keep, out)
				}
			}
			for _, mask := range tc.wantMask {
				if !strings.Contains(out, mask) {
					t.Errorf("expected the masked form %q; got: %s", mask, out)
				}
			}
		})
	}
}

// TestRedact_MaskedOutputIsStable asserts the redacted form is
// deterministic and idempotent: text that already carries the marker is
// not masked again (the marker contains no character any shape accepts,
// so re-scanning cannot multiply it), and a value that is masked once
// always renders as the single fixed marker.
func TestRedact_MaskedOutputIsStable(t *testing.T) {
	var buf bytes.Buffer
	ctx := textLoggerCtx(context.Background(), &buf)

	obs.FromContext(ctx).Info("event", "note", "see "+obs.RedactedValue+" earlier")

	if got := strings.Count(buf.String(), obs.RedactedValue); got != 1 {
		t.Errorf("expected the pre-masked marker to pass through exactly once; got %d in: %s", got, buf.String())
	}
}

// TestRedact_ErrorValues covers error attributes: an error whose message
// embeds a secret is masked in place -- the diagnosis survives, the secret
// does not -- and the whole error is replaced when its key is sensitive.
// Benign errors pass through byte-for-byte.
func TestRedact_ErrorValues(t *testing.T) {
	t.Run("secret embedded in an error message", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := textLoggerCtx(context.Background(), &buf)

		out := logThrough(ctx, &buf, "event", "error",
			errors.New("provider auth failed: "+testBearer))

		if strings.Contains(out, testBearer) {
			t.Errorf("error-embedded secret reached the sink; got: %s", out)
		}
		if !strings.Contains(out, "provider auth failed") {
			t.Errorf("the diagnosis must survive masking; got: %s", out)
		}
		if !strings.Contains(out, obs.RedactedValue) {
			t.Errorf("expected the masked marker in the error text; got: %s", out)
		}
	})

	t.Run("sensitive key replaces the whole error", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := textLoggerCtx(context.Background(), &buf)

		out := logThrough(ctx, &buf, "event", "api_secret",
			errors.New("could not connect: secret is stale"))

		if strings.Contains(out, "could not connect") || strings.Contains(out, "stale") {
			t.Errorf("an error under a sensitive key must be replaced wholesale; got: %s", out)
		}
		if want := "api_secret=" + obs.RedactedValue; !strings.Contains(out, want) {
			t.Errorf("expected %q; got: %s", want, out)
		}
	})

	t.Run("benign error passes through unchanged", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := textLoggerCtx(context.Background(), &buf)

		out := logThrough(ctx, &buf, "event", "error", errors.New("disk full"))

		if !strings.Contains(out, "disk full") {
			t.Errorf("a benign error must pass through unchanged; got: %s", out)
		}
		if strings.Contains(out, obs.RedactedValue) {
			t.Errorf("a benign error must not be masked; got: %s", out)
		}
	})
}

// TestRedact_ReWrappingIsIdempotent covers the WithLogger(FromContext(...))
// composition: a context carrying a logger that is already redacted must
// not double-wrap it, and one log call must render exactly one marker.
func TestRedact_ReWrappingIsIdempotent(t *testing.T) {
	var buf bytes.Buffer
	first := obs.FromContext(textLoggerCtx(context.Background(), &buf))
	ctx := obs.WithLogger(context.Background(), first)

	out := logThrough(ctx, &buf, "event", "token", testSecret)

	if strings.Contains(out, testSecret) {
		t.Errorf("token value leaked; got: %s", out)
	}
	if got := strings.Count(out, obs.RedactedValue); got != 1 {
		t.Errorf("expected exactly one redaction marker; got %d in: %s", got, out)
	}
}

// TestRedact_FallsBackToSlogDefault pins redaction onto the fallback path
// too: a context that never went through WithLogger still logs through a
// redacted slog.Default().
func TestRedact_FallsBackToSlogDefault(t *testing.T) {
	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))

	out := logThrough(context.Background(), &buf, "via default", "token", testSecret)

	if strings.Contains(out, testSecret) {
		t.Errorf("the slog.Default() fallback path must redact too; got: %s", out)
	}
	if want := "token=" + obs.RedactedValue; !strings.Contains(out, want) {
		t.Errorf("expected %q; got: %s", want, out)
	}
}

// TestRedact_JSONSink proves the guarantee is sink-independent: redaction
// happens before the sink formats, so a JSON handler (the shape the
// distributed deployment mode feeds Loki) redacts identically to the text
// handler every other test uses.
func TestRedact_JSONSink(t *testing.T) {
	var buf bytes.Buffer
	ctx := obs.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))

	out := logThrough(ctx, &buf, "event", "token", testSecret)

	if strings.Contains(out, testSecret) {
		t.Errorf("token value leaked through the JSON sink; got: %s", out)
	}
	if !strings.Contains(out, obs.RedactedValue) {
		t.Errorf("expected the JSON sink to carry the redaction marker; got: %s", out)
	}
}

// TestRedact_ConcurrentLogging exercises the slog.Handler concurrency
// contract: one redacted logger shared by many goroutines, each logging a
// secret-bearing line, must render every line redacted with no race (this
// test exists for the -race run) and no lost or double-masked record.
func TestRedact_ConcurrentLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := obs.FromContext(textLoggerCtx(context.Background(), &buf))

	const (
		goroutines = 16
		linesEach  = 50
	)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < linesEach; j++ {
				logger.Info("parallel event", "note_id", "n-1", "token", testSecret)
			}
		}()
	}
	wg.Wait()

	out := buf.String()
	if strings.Contains(out, testSecret) {
		t.Errorf("a concurrent log call leaked the token value; got: %s", out)
	}
	if got := strings.Count(out, obs.RedactedValue); got != goroutines*linesEach {
		t.Errorf("expected %d redacted records, found %d", goroutines*linesEach, got)
	}
	if got := strings.Count(out, "note_id=n-1"); got != goroutines*linesEach {
		t.Errorf("expected %d intact benign attributes, found %d", goroutines*linesEach, got)
	}
}
