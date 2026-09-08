package observability_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
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
	// testJWE is a five-segment JOSE compact token (JWE:
	// header.encrypted_key.iv.ciphertext.tag): testJWT's three segments
	// plus the two trailing segments a JWE carries for ciphertext and
	// authentication tag. testJWETail names those trailing segments on
	// their own, so a test can assert that no fragment of them survives
	// masking -- the leak a shape rule stopping at three segments leaves
	// in plaintext after the redaction marker.
	testJWETail = ".abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ab" +
		".abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789cd"
	testJWE = testJWT + testJWETail
)

// testBasic is a real Basic-authorization credential value: the base64 of
// "dummy:supersecretpassword", computed through the stdlib encoder here
// (not a hand-written literal) so the fixture cannot drift from what a
// genuine Authorization header would carry.
var testBasic = "Basic " + base64.StdEncoding.EncodeToString([]byte("dummy:supersecretpassword"))

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

// TestRedact_TokenStemDoesNotOverRedactUnrelatedWords is the regression for
// the "token" stem's over-redaction bug: a legitimate, non-secret
// diagnostic key that merely contains "token" as a substring of a
// different word ("tokens", the ordinary plural for an LLM/usage count --
// ai-gateway's own prompt_tokens/completion_tokens fields, which its
// gateway.go renamed to prompt_units/completion_units specifically to
// dodge this redactor) must survive verbatim, both key and value.
func TestRedact_TokenStemDoesNotOverRedactUnrelatedWords(t *testing.T) {
	keys := []string{"prompt_tokens", "completion_tokens", "tokens", "tokenizer_version"}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := textLoggerCtx(context.Background(), &buf)

			obs.FromContext(ctx).Info("event", key, 42)

			out := buf.String()
			if !strings.Contains(out, key+"=42") {
				t.Errorf("expected %q=42 to survive unredacted; got: %s", key, out)
			}
			if strings.Contains(out, obs.RedactedValue) {
				t.Errorf("attribute %q was wrongly redacted; got: %s", key, out)
			}
		})
	}
}

// TestRedact_TokenStemWordBoundary_AdversarialVocabulary is a broader
// adversarial pass on P2-5's word-boundary fix, checked in both
// directions at once against a wider vocabulary than the original
// regression test:
//
//  1. legitimate diagnostic keys where "token" is merely a substring of a
//     longer, different word must survive unredacted (the over-redaction
//     class the fix closed), and
//  2. every genuinely secret-shaped "token" key from the existing
//     TestRedact_SensitiveKeyValues vocabulary, plus several additional
//     realistic secret-shaped names built the same way (an underscore-
//     joined "token" segment), must still redact (the fix must not have
//     narrowed the word-boundary check so far that it stops matching
//     "token" as a whole segment).
//
// The vocabulary spans both separator styles the boundary rule must treat
// identically. The underscore-joined forms below ("access_token") mark
// their boundaries with a non-letter; the camelCase forms ("accessToken",
// "tokenValue") mark the same boundaries with a lowercase-to-uppercase
// case transition instead -- the regression class this round closes, after
// the original fix's letter-only boundary check stopped treating "Token"
// following a lowercase letter as a whole word and let exactly these key
// names leak their values again. Both styles appear in both directions:
// prompt_tokens and its camelCase plural "promptTokens" are equally
// legitimate usage counts that must survive, and a bare "token" segment
// joined either way is equally secret-bearing.
//
// A third class is terminal-position compounds: a segment that ENDS with
// the bare stem -- all-lowercase run-together ("apitoken", "accesstoken")
// or acronym-glued ("APIToken", "JWTToken") -- has nothing continuing the
// stem, so the stem is the segment's last word and the compound is
// secret-shaped whatever the glue before it (the terminal-suffix rule in
// stemMatches). Run-together plurals ("prompttokens", "sessiontokens")
// end in "...tokens", not in the bare stem, and stay benign like their
// separated and camelCase twins.
func TestRedact_TokenStemWordBoundary_AdversarialVocabulary(t *testing.T) {
	benign := []string{
		// Already covered by the original regression test; repeated here
		// so this table is a self-contained adversarial pass.
		"tokens", "prompt_tokens", "completion_tokens", "tokenizer_version",
		// "token" glued to a preceding letter with no separator at all --
		// the class the original regression test did not exercise.
		"detokenize", "retokenized", "subtoken_count",
		// "token" glued to a following letter with no separator, a
		// different word shape than the "...tokens" plural.
		"tokenized_length", "tokenify",
		// "token" as an interior fragment of an unrelated compound word,
		// letters on both sides.
		"autotokenizer",
		// camelCase plurals: a lowercase-to-uppercase transition marks the
		// boundary before "Token", but the lowercase "s" continuing the
		// stem is still the plural -- the same usage-count field as
		// prompt_tokens, spelled camelCase, and equally legitimate.
		"promptTokens", "completionTokens", "sessionTokens",
		// Run-together lowercase plurals: the same usage-count family with
		// no separator and no case transition anywhere. The plural "s"
		// continues the stem, so the segment ends in "...tokens", never in
		// the bare "...token" the terminal-suffix rule keys on.
		"prompttokens", "sessiontokens",
	}
	for _, key := range benign {
		t.Run("benign/"+key, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := textLoggerCtx(context.Background(), &buf)

			obs.FromContext(ctx).Info("event", key, 7)

			out := buf.String()
			if !strings.Contains(out, key+"=7") {
				t.Errorf("expected %q=7 to survive unredacted; got: %s", key, out)
			}
			if strings.Contains(out, obs.RedactedValue) {
				t.Errorf("attribute %q was wrongly redacted; got: %s", key, out)
			}
		})
	}

	secretShaped := []string{
		// From the existing 34-row table (TestRedact_SensitiveKeyValues) --
		// re-verified here so this adversarial pass is self-contained.
		"token", "access_token", "refresh_token", "session_token",
		"id_token", "api_token", "csrf_token", "auth_token", "Token",
		// Additional realistic secret-shaped names built the same way
		// (an underscore-joined "token" segment), not present verbatim in
		// the 34-row table, to widen the net past exactly what was already
		// pinned.
		"oauth_token", "bearer_token", "reset_token", "verification_token",
		"TOKEN", "x_auth_token",
		// Separator-free camelCase compounds, both boundary directions:
		// "Token" following a lowercase letter (the accessToken family --
		// the security regression this round fixes: the word-boundary
		// check's letter-only left boundary rejected these, so the key
		// rule stopped redacting them entirely and only the weaker
		// value-shape net remained) and "token" followed by an uppercase
		// letter starting the next word (tokenValue). Each was redacted
		// before the word-boundary narrowing and must redact again.
		"accessToken", "sessionToken", "refreshToken", "apiToken",
		"userToken", "idToken", "tokenValue",
		// Run-together compounds whose last morpheme IS the stem, with no
		// separator and no case transition at the join -- the terminal
		// residue class. The all-lowercase forms glue the stem to a
		// lowercase prefix ("apitoken" through the "i"->"t" pair, which the
		// boundary rule reads as one word continuing); the acronym forms
		// glue it to an all-caps prefix ("APIToken", "JWTToken"), an
		// uppercase-to-uppercase join no case transition marks. Neither is
		// a whole word to wordBoundaryASCII -- each is caught because the
		// stem sits at the segment's own end, where the terminal-suffix
		// rule treats a trailing stem as the secret word whatever the glue
		// before it.
		"apitoken", "accesstoken", "authtoken", "sessiontoken", "refreshtoken",
		"APIToken", "JWTToken",
	}
	for _, key := range secretShaped {
		t.Run("secret/"+key, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := textLoggerCtx(context.Background(), &buf)

			out := logThrough(ctx, &buf, "event", key, testSecret)

			if strings.Contains(out, testSecret) {
				t.Errorf("attribute %q leaked its value into the sink; got: %s", key, out)
			}
			if want := key + "=" + obs.RedactedValue; !strings.Contains(out, want) {
				t.Errorf("expected %q to still be redacted after the word-boundary narrowing; got: %s", key, out)
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
			name:     "jwt as whole value",
			value:    testJWT,
			secret:   testJWT,
			wantMask: []string{obs.RedactedValue},
		},
		{
			// A JWE has five dot-separated segments; the trailing
			// ciphertext and tag segments must not survive masking.
			name:     "jwe five segments as whole value",
			value:    testJWE,
			secret:   testJWETail,
			wantMask: []string{obs.RedactedValue},
		},
		{
			name:     "jwe five segments embedded in text",
			value:    "inbound " + testJWE + " rejected by policy",
			secret:   testJWETail,
			wantKeep: []string{"inbound ", " rejected by policy"},
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
		{
			name:     "bare query string as whole value",
			value:    "access_token=abcDEFgh1234567890&scope=read",
			secret:   "abcDEFgh1234567890",
			wantKeep: []string{"&scope=read"},
			wantMask: []string{"access_token=" + obs.RedactedValue},
		},
		{
			name:     "bare query string as first parameter",
			value:    "password=sup3rSecretPw123456789&username=ops",
			secret:   "sup3rSecretPw123456789",
			wantKeep: []string{"&username=ops"},
			wantMask: []string{"password=" + obs.RedactedValue},
		},
		{
			name:     "bare benign query string untouched",
			value:    "expand=charge&limit=25",
			secret:   "", // no secret parameter name anywhere in the run
			wantKeep: []string{"expand=charge&limit=25"},
		},
		{
			name:     "password in url userinfo with uppercase scheme",
			value:    "HTTPS://ops:sup3rSecretPw12345@db.internal:5432/pg",
			secret:   "sup3rSecretPw12345",
			wantKeep: []string{"HTTPS://ops:", "@db.internal:5432/pg"},
			wantMask: []string{"HTTPS://ops:" + obs.RedactedValue + "@db.internal"},
		},
		{
			name:     "basic auth credentials as whole value",
			value:    "Basic " + base64.StdEncoding.EncodeToString([]byte("dummy:supersecretpassword")),
			secret:   base64.StdEncoding.EncodeToString([]byte("dummy:supersecretpassword")),
			wantKeep: []string{"Basic "},
			wantMask: []string{"Basic " + obs.RedactedValue},
		},
		{
			name:     "basic auth credentials embedded in text",
			value:    "caller presented " + testBasic + " and was rejected",
			secret:   testBasic,
			wantKeep: []string{"caller presented", "and was rejected"},
			wantMask: []string{"Basic " + obs.RedactedValue},
		},
		{
			name:     "basic-auth prose untouched",
			value:    "basic authentication is enabled on this endpoint",
			secret:   "", // the word "basic" in prose is not a credential; the run after it is 15 chars, below secret strength
			wantKeep: []string{"basic authentication is enabled on this endpoint"},
		},
		{
			// The three compact spellings the pattern's alternation admits
			// (api[_-]?key, client[_-]?secret, session[_-]?key) heading a
			// bare query string. These rows exercise the leading-anchor
			// branch of the gate, whose querySecretParamNames mirror must
			// name each of them; a compact spelling forgotten there fails
			// its own row here (both clientsecret and sessionkey once were,
			// while the alternation kept matching them).
			name:     "apikey heading a bare query string",
			value:    "apikey=abCdefgh1234567890&scope=read",
			secret:   "abCdefgh1234567890",
			wantKeep: []string{"&scope=read"},
			wantMask: []string{"apikey=" + obs.RedactedValue},
		},
		{
			name:     "clientsecret heading a bare query string",
			value:    "clientsecret=abCdefgh1234567890&grant_type=refresh_token",
			secret:   "abCdefgh1234567890",
			wantKeep: []string{"&grant_type=refresh_token"},
			wantMask: []string{"clientsecret=" + obs.RedactedValue},
		},
		{
			name:     "sessionkey heading a bare query string",
			value:    "sessionkey=abCdefgh1234567890&scope=read",
			secret:   "abCdefgh1234567890",
			wantKeep: []string{"&scope=read"},
			wantMask: []string{"sessionkey=" + obs.RedactedValue},
		},
		{
			// The same three names mid-string, behind a benign first
			// parameter: the gate reaches a non-leading parameter only
			// through its any-anchor name probe, so these rows pin the
			// gate/regexp equivalence for the non-leading anchor -- the
			// shape this round's regression is about (bare form bodies
			// carry neither '?' nor '://', and a secret parameter that is
			// not first used to leave the gate false and skip the
			// pattern entirely).
			name:     "apikey mid-string in a bare form body",
			value:    "scope=read&apikey=abCdefgh1234567890",
			secret:   "abCdefgh1234567890",
			wantKeep: []string{"scope=read&"},
			wantMask: []string{"apikey=" + obs.RedactedValue},
		},
		{
			name:     "clientsecret mid-string in a bare form body",
			value:    "username=ops&clientsecret=abCdefgh1234567890&scope=read",
			secret:   "abCdefgh1234567890",
			wantKeep: []string{"username=ops&", "&scope=read"},
			wantMask: []string{"clientsecret=" + obs.RedactedValue},
		},
		{
			name:     "sessionkey mid-string in a bare form body",
			value:    "username=ops&sessionkey=abCdefgh1234567890&scope=read",
			secret:   "abCdefgh1234567890",
			wantKeep: []string{"username=ops&", "&scope=read"},
			wantMask: []string{"sessionkey=" + obs.RedactedValue},
		},
		{
			// Two literal alternation names at the same anchors, so the
			// backstop is not limited to the compact classes: token is
			// the alternation's catch-all (and the value of grant_type in
			// the regression body above, which must stay readable), and
			// access_token is its most common member.
			name:     "token heading a bare query string",
			value:    "token=abCdefgh1234567890&scope=read",
			secret:   "abCdefgh1234567890",
			wantKeep: []string{"&scope=read"},
			wantMask: []string{"token=" + obs.RedactedValue},
		},
		{
			name:     "token mid-string in a bare form body",
			value:    "scope=read&token=abCdefgh1234567890",
			secret:   "abCdefgh1234567890",
			wantKeep: []string{"scope=read&"},
			wantMask: []string{"token=" + obs.RedactedValue},
		},
		{
			name:     "access_token mid-string in a bare form body",
			value:    "scope=read&access_token=abCdefgh1234567890",
			secret:   "abCdefgh1234567890",
			wantKeep: []string{"scope=read&"},
			wantMask: []string{"access_token=" + obs.RedactedValue},
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

// TestRedact_OpaqueRefreshTokenNonFirstInBareFormBody is the regression
// test for the gate/regexp inequivalence that let a mid-string secret
// parameter reach the sink in plaintext: the URL-query pattern's regexp
// anchors on (?:^|[?&]) and CAN hit a parameter that is not first, while
// the pattern's gate only opened on '://', '?' or a leading secret
// parameter name -- so a bare form body (no '?', no '://') whose secret
// parameter is not first never ran the regexp at all. The input is
// deliberately the shape that would prove nothing by accident:
//
//   - neither '?' nor '://' anywhere (an application/x-www-form-urlencoded
//     body, or an upstream error echoing one);
//   - the secret parameter is NOT first -- OAuth2 form bodies put
//     grant_type first by convention, and an upstream error echoing the
//     request that failed is exactly the "credentials and all" scenario
//     the KindAny error path exists for;
//   - an opaque refresh token (this module family's refresh tokens are
//     random values, not JWTs): no eyJ header, no provider prefix, so no
//     shape class other than the parameter name can identify it.
//
// A test case carrying a '?' or '://', or putting the secret parameter
// first, would pass the old gate and prove nothing.
func TestRedact_OpaqueRefreshTokenNonFirstInBareFormBody(t *testing.T) {
	const body = "grant_type=refresh_token&refresh_token=a1b2c3d4e5f6g7h8i9j0"
	const refreshToken = "a1b2c3d4e5f6g7h8i9j0"
	const maskedForm = "refresh_token=" + obs.RedactedValue

	for _, tc := range []struct {
		name string
		val  any
	}{
		{name: "string attribute value", val: body},
		{name: "error text", val: errors.New(body)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := textLoggerCtx(context.Background(), &buf)

			out := logThrough(ctx, &buf, "event", "details", tc.val)

			if strings.Contains(out, refreshToken) {
				t.Errorf("refresh token reached the sink in plaintext; got: %s", out)
			}
			if !strings.Contains(out, maskedForm) {
				t.Errorf("expected the masked form %q; got: %s", maskedForm, out)
			}
			if !strings.Contains(out, "grant_type=refresh_token&") {
				t.Errorf("the surrounding form body must survive masking; got: %s", out)
			}
		})
	}
}

// TestRedact_MaskedOutputIsStable asserts the redacted form is
// deterministic and idempotent: text that already carries the marker is
// not masked into anything else (masking is a fixed point -- a re-scan can
// re-match the marker where a value class admits its brackets, but only to
// reproduce it exactly -- so re-scanning cannot multiply it), and a value
// that is masked once always renders as the single fixed marker.
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

// TestRedact_NoPerAttributeAllocationOnBenignRecord pins the documented
// fast path of redactHandler.Handle (see redact.go): a record with nothing
// sensitive is forwarded after a single streaming scan, with no allocation
// per attribute. The shared correlation and module attribute keys are
// dot-free, and segmenting a dot-free key must not allocate -- before the
// dot-free fast path existed, every attribute's key went through
// strings.Split, one heap allocation per attribute, so a benign record's
// allocation count scaled 1:1 with its attribute count (1 attribute = 1
// alloc, 6 = 7). The test measures allocation counts with
// testing.AllocsPerRun and asserts the count does not scale: a six-
// attribute record must cost the same as a one-attribute one, within the
// small record-level noise (slog's own attribute-storage allocation, which
// can differ by one between record sizes). This test fails on the
// per-attribute-split code and passes on the dot-free fast path.
func TestRedact_NoPerAttributeAllocationOnBenignRecord(t *testing.T) {
	ctx := obs.WithLogger(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	logger := obs.FromContext(ctx)

	logWith := func(attrs int) func() {
		args := make([]any, 0, 2*attrs)
		for i := 0; i < attrs; i++ {
			args = append(args, "metric_id", "m-42")
		}
		return func() { logger.Info("benign event", args...) }
	}

	one := testing.AllocsPerRun(200, logWith(1))
	six := testing.AllocsPerRun(200, logWith(6))

	if extra := six - one; extra > 2 {
		t.Errorf("allocation count scales with attribute count: 1 benign attribute cost %.0f allocs, 6 cost %.0f (expected within 2)", one, six)
	}
}

// TestRedact_ExemptKeysUnderSensitivePaths pins the interaction the
// exemption's own documentation used to overstate: what happens when a
// never-redact correlation key sits under a path that names a secret.
// TestRedact_CorrelationKeysNeverRedacted above logs correlation keys
// under no sensitive path at all, so nothing exercised this intersection
// and the class comment drifted from the code unnoticed.
//
// The code checks the exemption before the key-based rule, so exemption
// wins over a sensitive key PATH in both of its forms: a dotted key whose
// leading segment names a secret, and an exempt attribute logged under a
// WithGroup context named for a secret. It does NOT reach an inline
// slog.Group attribute whose own name is sensitive: that attribute's key
// IS the group name, so redactAttrWhole replaces the group before any
// child is visited -- the bucket is the secret. The divergence is toward
// more redaction, so it leaks nothing; it is pinned here because it is
// the half a reader of the exemption rule would not predict.
func TestRedact_ExemptKeysUnderSensitivePaths(t *testing.T) {
	t.Run("WithGroup named for a secret leaves an exempt child intact", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := textLoggerCtx(context.Background(), &buf)
		obs.FromContext(ctx).WithGroup("credentials").Info("event",
			"user_id", testJWT,
		)
		out := buf.String()
		if want := "credentials.user_id=" + testJWT; !strings.Contains(out, want) {
			t.Errorf("expected the exempt user_id to survive under a sensitive group verbatim as %q; got: %s", want, out)
		}
		if strings.Contains(out, obs.RedactedValue) {
			t.Errorf("an exempt key must not be redacted by the surrounding group name; got: %s", out)
		}
	})

	t.Run("dotted key under a secret-named segment stays intact", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := textLoggerCtx(context.Background(), &buf)
		obs.FromContext(ctx).Info("event", "credentials.user_id", "u-1")
		out := buf.String()
		if !strings.Contains(out, "credentials.user_id=u-1") {
			t.Errorf("expected the exempt trailing segment to survive a sensitive path; got: %s", out)
		}
		if strings.Contains(out, obs.RedactedValue) {
			t.Errorf("an exempt trailing segment must not be redacted; got: %s", out)
		}
	})

	t.Run("inline group named for a secret collapses, exempt child included", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := textLoggerCtx(context.Background(), &buf)
		obs.FromContext(ctx).Info("event",
			slog.Group("credentials", "user_id", "u-1", "password", testSecret),
		)
		out := buf.String()
		if want := "credentials=" + obs.RedactedValue; !strings.Contains(out, want) {
			t.Errorf("expected the sensitive group to collapse wholesale to %q; got: %s", want, out)
		}
		if strings.Contains(out, "u-1") {
			t.Errorf("the exempt child must not survive a collapsed sensitive group; got: %s", out)
		}
		if strings.Contains(out, testSecret) {
			t.Errorf("the secret must never reach the sink; got: %s", out)
		}
	})
}

// TestRedact_EmptyKeyAttributes_ValueRulesStillApply is the regression
// for a total redaction bypass: redactAttr used to return an attribute
// with an empty key untouched, on the theory that an empty key names no
// secret. But slog's inline-group idiom -- slog.Group("", ...) -- attaches
// an attribute whose key is empty while its VALUE is a group of named
// children, and the built-in sinks render those children normally (an
// empty group key adds no qualification, so the children appear inline,
// and an empty-key scalar renders as ""=value), which made the early
// return a live leak path: a password logged inside an empty-key group
// reached the sink verbatim, and an empty-key string carrying a bearer
// token skipped the value-shape scan entirely. An empty key must skip
// only what is genuinely absent -- a key-name rule has nothing to match,
// and an empty segment can neither be exempted nor sensitive -- while the
// value rules still apply exactly as they do under a benign key: a
// group's children are still visited, and a string or error value is
// still scanned for secret shapes. Fails before the fix (verified): the
// text sink renders "password=hunter2-super-secret" and the full bearer
// token verbatim; passes after.
func TestRedact_EmptyKeyAttributes_ValueRulesStillApply(t *testing.T) {
	t.Run("empty-key group is recursed into", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := textLoggerCtx(context.Background(), &buf)

		out := logThrough(ctx, &buf, "event",
			slog.Group("", "username", "ops", "password", testSecret),
		)

		if strings.Contains(out, testSecret) {
			t.Errorf("an empty-key group leaked its sensitive child's value into the sink; got: %s", out)
		}
		if !strings.Contains(out, obs.RedactedValue) {
			t.Errorf("expected the sensitive child of the empty-key group to be redacted; got: %s", out)
		}
		if !strings.Contains(out, "username=ops") {
			t.Errorf("expected the benign child of the empty-key group to survive; got: %s", out)
		}
	})

	t.Run("empty-key string is value-scanned", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := textLoggerCtx(context.Background(), &buf)

		out := logThrough(ctx, &buf, "event", slog.String("", testBearer))

		if strings.Contains(out, testBearer) {
			t.Errorf("an empty-key attribute leaked its secret-shaped value into the sink; got: %s", out)
		}
		if want := "Bearer " + obs.RedactedValue; !strings.Contains(out, want) {
			t.Errorf("expected the bearer token under the empty key to be masked in place (%q); got: %s", want, out)
		}
	})

	t.Run("benign empty-key values pass untouched", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := textLoggerCtx(context.Background(), &buf)

		out := logThrough(ctx, &buf, "event",
			slog.String("", "harmless-correlation-value-here"),
			slog.Int("", 7),
		)

		if !strings.Contains(out, "harmless-correlation-value-here") {
			t.Errorf("a benign empty-key string must survive value scanning; got: %s", out)
		}
		if !strings.Contains(out, "=7") {
			t.Errorf("a numeric empty-key attribute must pass untouched; got: %s", out)
		}
		if strings.Contains(out, obs.RedactedValue) {
			t.Errorf("nothing in this record is secret-shaped; got: %s", out)
		}
	})
}

// TestRedact_EmptyKeyScalarUnderSensitiveGroupPath_RedactedWholesale is
// the regression for a path-rule bypass in redactAttr's empty-key branch
// (P1-2): the branch used to skip the pathSensitive(groups) check every
// sibling branch runs, reasoning only about what the empty key ITSELF
// contributes to an attribute's key path -- true for the key's own empty
// segment, over-broad for the branch, which already holds the segments in
// groups as an independent input. A logger-level WithGroup("credentials")
// context therefore redacted a non-empty benign key wholesale (the sibling
// branches' group-name rule) while an empty-key scalar under that same
// context reached only the value rules -- a value with no recognizable
// secret shape passed through the group-name rule entirely. The empty key
// contributes nothing to the path; the group context contributes
// everything, so the empty-key branch must run the same pathSensitive
// check as its siblings and collapse wholesale when it fires. Fails before
// the fix (verified): the sink renders the empty-key scalar's value
// verbatim with no redaction marker; passes after: the attribute collapses
// to RedactedValue exactly as a non-empty key under the same group does.
func TestRedact_EmptyKeyScalarUnderSensitiveGroupPath_RedactedWholesale(t *testing.T) {
	var buf bytes.Buffer
	logger := obs.FromContext(textLoggerCtx(context.Background(), &buf))

	logger.WithGroup("credentials").Info("event", slog.String("", testSecret))

	out := buf.String()
	if strings.Contains(out, testSecret) {
		t.Errorf("an empty-key scalar under a sensitive logger-level group leaked its value into the sink; got: %s", out)
	}
	if !strings.Contains(out, obs.RedactedValue) {
		t.Errorf("expected the group-name rule to replace the empty-key scalar wholesale (as it does a non-empty key under the same group); got: %s", out)
	}
}

// TestRedact_KeyStemDoesNotOverRedactCorrelationReferences is the
// regression for the "key" stem's false-positive class: the stem used a
// bare substring match, so any attribute whose name merely contained
// "key" was redacted wholesale -- including correlation-only fields that
// must stay queryable, exactly the class the "token" stem's word-boundary
// treatment already closes. Two real call sites were damaged: the
// reference-app integration module logs "key_id" holding an opaque API-key
// row id (go/integration/authenticate.go -- an operator needs it to tell
// which key failed its last-used update), and examples/reference-app's
// smilesim service logs "credit_idempotency_key" holding the derived key
// of an orphaned credit reservation precisely so an operator can reconcile
// it (smilesim/service.go -- the value the log line exists for was masked).
// Both names are references or correlation identifiers whose values the
// log line exists to show: "key_id" is an _id-suffixed row reference (the
// naming convention neverRedactKeys' own user_id/job_id entries follow),
// and a terminal "idempotency_key" compound is this repository's
// idempotency-key naming convention (go/jobs, go/metering, go/billing and
// go/notification all key one business-operation instance by a
// deterministic id derived from the operation's own identity -- never
// random -- with only go/notification's actually hashed: go/jobs' is the
// enqueuing caller's own key text, embedded verbatim in the asynq
// TaskID), so both fall through the "key" stem to the value-shape net. A
// genuinely secret-shaped key field -- api_key and its family -- still
// redacts wholesale, and a secret-shaped VALUE logged under a surviving
// reference name is still masked by the value net. What no exemption here
// can do is tell PII-shaped text (an email inside a caller-chosen key,
// say) apart from an ordinary identifier -- that is caller-declared
// content, and go/jobs' Task.IdempotencyKey doc comment, the key
// builder's own gate, warns of exactly that.
// Fails before the fix (verified): "key_id" and "credit_idempotency_key"
// both render "[REDACTED]" and the values never reach the sink.
func TestRedact_KeyStemDoesNotOverRedactCorrelationReferences(t *testing.T) {
	benign := []struct {
		key, value string
	}{
		{"key_id", "550e8400-e29b-41d4-a716-446655440000"},
		{"credit_idempotency_key", "credit-reservation-42-settlement-key"},
		{"idempotency_key", "delivery-run-7-dedupe-key"},
	}
	for _, tc := range benign {
		t.Run("reference/"+tc.key, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := textLoggerCtx(context.Background(), &buf)

			out := logThrough(ctx, &buf, "event", tc.key, tc.value)

			if want := tc.key + "=" + tc.value; !strings.Contains(out, want) {
				t.Errorf("correlation reference %q must render its value verbatim; got: %s", tc.key, out)
			}
			if strings.Contains(out, obs.RedactedValue) {
				t.Errorf("attribute %q was wrongly redacted; got: %s", tc.key, out)
			}
		})
	}

	t.Run("secret-shaped key values still redact wholesale", func(t *testing.T) {
		for _, key := range []string{"api_key", "signing_key", "x_api_key"} {
			var buf bytes.Buffer
			ctx := textLoggerCtx(context.Background(), &buf)

			out := logThrough(ctx, &buf, "event", key, testSecret)

			if strings.Contains(out, testSecret) {
				t.Errorf("attribute %q leaked its value into the sink; got: %s", key, out)
			}
			if want := key + "=" + obs.RedactedValue; !strings.Contains(out, want) {
				t.Errorf("expected %q to be replaced by %q; got: %s", key, obs.RedactedValue, out)
			}
		}
	})

	t.Run("secret-shaped value under a surviving reference name is still masked", func(t *testing.T) {
		// None of the reference exemptions opens a hole: key_id and the
		// idempotency_key-suffixed names are exempt from the "key" stem's
		// key-name rule only -- they are not neverRedactKeys entries, so
		// their values still pass through the value-shape net, which
		// masks a secret-shaped value in place exactly as under any
		// benign key. (What the exemptions do not catch is PII-shaped
		// text, which no shape rule can tell from an ordinary identifier
		// -- that boundary is the caller's own, per the test header
		// above.)
		for _, key := range []string{"key_id", "credit_idempotency_key", "idempotency_key"} {
			var buf bytes.Buffer
			ctx := textLoggerCtx(context.Background(), &buf)

			out := logThrough(ctx, &buf, "event", key, testJWT)

			if strings.Contains(out, testJWT) {
				t.Errorf("a secret-shaped value under surviving reference key %q leaked; got: %s", key, out)
			}
			if want := key + "=" + obs.RedactedValue; !strings.Contains(out, want) {
				t.Errorf("expected the JWT-shaped value under %q to be masked in place (%q); got: %s", key, want, out)
			}
		}
	})
}
