package observability

// This file implements the PII/secret log-redaction item of the M0
// data-protection milestone described in docs/internal/15-roadmap.md. The
// mandate comes from docs/internal/09-observability.md: plaintext PII,
// secrets, tokens and full prompts never enter logs or traces; redaction
// is on by default and must not be disableable.
// docs/internal/10-compliance-and-audit.md places the mechanism in this
// module's logging layer -- safe by default rather than leaking by default
// -- and defers audit-log redaction to the M1+ compliance milestone.
//
// The mechanism is a slog.Handler wrapper, redactHandler, that wraps the
// sink handler underneath every *slog.Logger FromContext returns. It
// redacts attributes by key name and by value shape before the record ever
// reaches the sink, so the guarantee holds uniformly for every sink a host
// plugs in -- the standalone deployment mode's console handler, a JSON
// handler feeding Loki, or a future slog-to-OTel bridge -- with no
// per-sink code.
//
// # Why key naming, not a registry marker
//
// Redaction keys off the attribute's own name, not off pkgcore's
// ConfigItem.Sensitive declaration. A Sensitive marker lives on the config
// Registry, which is a host-wiring object assembled at startup and not
// reachable from a log call; the logger must also redact correctly for a
// record emitted before any module registered, and for keys no module ever
// declared. Key naming aligns with this repository's logging discipline
// (root CLAUDE.md: attribute keys are shared, snake_case names), so
// "an attribute whose name says it is a secret is redacted" needs no
// registry lookup. ConfigItem.Sensitive continues to mark config items for
// the generated configuration reference and the admin console; the two
// mechanisms are complementary and this package does not read the marker.
//
// # What is redacted
//
//  1. Key-based, default-on, no per-call opt-out: an attribute whose key
//     path contains a sensitive segment (see sensitiveStems) is replaced
//     wholesale by RedactedValue, whatever the value's type. A group whose
//     own name is sensitive collapses entirely. Attribute keys are matched
//     on every dot-separated segment so dotted config-style keys
//     ("billing.stripe_secret_key") and slog.Group names ("credentials")
//     are covered identically. There is deliberately NO way to tag one
//     attribute "do not redact me": the sanctioned logger API is FromContext,
//     and everything logged through it is redacted. The only unredacted
//     output left is a *slog.Logger a host constructs by hand outside this
//     API, which root CLAUDE.md's logging rule already confines to process
//     startup -- that construction site is the documented escape, not a
//     per-call flag.
//  2. Value-shaped, as a fallback net for secrets logged under an
//     unsuspicious key: attribute string values and error texts are scanned
//     for the canonical shapes secrets take -- "Bearer <token>", JWTs
//     (eyJ...), provider-prefixed keys (sk_/pk_/rk_/sk-, AKIA, AIza,
//     ghp_/github_pat_, xox*, glpat-), and credentials embedded in URLs
//     (query parameters such as ?access_token=..., and userinfo). Matched
//     regions are replaced by RedactedValue in place; surrounding text is
//     preserved. Errors are masked, never dropped.
//
// The correlation fields this must never touch are the structured log
// field names every module shares -- tenant_id, user_id, job_id (plus this
// module's trace_id and span_id): they are exempt from value-shape scanning
// by exact name (neverRedactKeys), so an id value can never be mangled no
// matter what it looks like.
//
// # Deliberate boundaries
//
//   - The record's message is NOT scanned: messages are constant strings
//     per the logging discipline, and a redactor is not a substitute for
//     it.
//   - Attributes holding arbitrary structs (slog.Any with a non-error
//     value) are redacted wholesale under a sensitive key but not
//     introspected under a plain one -- reflection over unknown types is
//     out of scope; log structs field-wise under descriptive keys.
//   - Short values (< 16 bytes) are never value-scanned; anything that
//     short is below secret strength and key-based rules still apply.
//   - There is deliberately no generic "long random string" shape: normal
//     IDs (UUIDs and similar) are long random strings too, and redacting
//     correlation values would be its own outage. High-precision shapes
//     only; a secret logged under a benign key in an exotic format is out
//     of this net's reach, which is why the key-based rule is the primary
//     defense.
//   - API responses and audit logs are out of scope here: response
//     redaction belongs to the API layer, and audit-log redaction is the
//     M1+ compliance work (docs/internal/10-compliance-and-audit.md); this
//     package guards the ops-logging and span-attribute channel only.

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
)

// RedactedValue is the deterministic replacement the redactor substitutes
// for every sensitive attribute value, and the in-place marker it inserts
// where a secret-shaped region was found inside a longer string. It is a
// fixed constant -- never derived from the value, so identical input
// always redacts to identical output, and never empty, so a redacted line
// is unmistakable when read or queried.
const RedactedValue = "[REDACTED]"

// sensitiveStems are the substrings that mark a key-path segment (an
// attribute key, a dotted config-style key segment, or a slog group name)
// as secret-bearing. Matching is case-insensitive and errs on the safe
// side: an attribute whose name contains any of these is redacted
// wholesale even when its value is not secret-shaped, because the cost of
// an over-redacted field is noise while the cost of a leaked secret is a
// breach. None of the shared snake_case correlation keys (tenant_id,
// user_id, job_id, trace_id, span_id) contains a stem; see neverRedactKeys
// for the belt-and-braces exemption from value scanning.
var sensitiveStems = []string{
	"token",
	"secret",
	"password",
	"passwd",
	"pwd",
	"authorization",
	"cookie",
	"credential",
	"key",
}

// neverRedactKeys are the correlation field names
// docs/internal/09-observability.md's logging rule fixes as shared and
// queryable: they must survive redaction verbatim so a log line stays
// joinable to its trace and tenant. None of them can match sensitiveStems
// today, but they are exempted explicitly -- not by luck -- so a future
// stem addition or an adversarial id value (tenant names are user-chosen
// strings) can never start mangling correlation fields. Exemption means
// the whole attribute is passed through untouched: no key rule, no
// value-shape scan.
var neverRedactKeys = map[string]struct{}{
	TraceIDKey:  {},
	SpanIDKey:   {},
	TenantIDKey: {},
	"user_id":   {},
	"job_id":    {},
}

// minSecretScanLen is the shortest string worth value-scanning. Every
// secret shape this package recognizes is longer than 16 bytes (the
// shortest, an AWS access key id, is 20), so anything shorter is skipped
// without touching a regexp. Key-based redaction is unaffected.
const minSecretScanLen = 16

// ---------------------------------------------------------------------------
// Attribute rules
// ---------------------------------------------------------------------------

// redactAttr returns a's redacted form, reporting whether it changed.
// groups is the key-path context (slog group names opened by the logger
// via WithGroup) the attribute sits under; both groups and the segments of
// a's own key participate in the sensitive-segment check, mirroring how
// slog's ReplaceAttr is handed the open groups of the attribute it rewrites.
//
// redactAttr must not panic: every path that touches a value supplied by
// the caller (Resolve on a LogValuer, Error on an error) is reached
// through safeRedactAttr's recover.
func redactAttr(groups []string, a slog.Attr) (slog.Attr, bool) {
	if a.Key == "" {
		return a, false
	}
	segs := strings.Split(a.Key, ".")
	if _, exempt := neverRedactKeys[segs[len(segs)-1]]; exempt {
		return a, false
	}

	if pathSensitive(groups) || pathSensitive(segs) {
		// Key- (or group-)named as a secret: replace the whole value,
		// whatever its type. A group whose own name is sensitive is
		// collapsed in its entirety -- the bucket is the secret.
		return slog.Attr{Key: a.Key, Value: slog.StringValue(RedactedValue)}, true
	}

	childPath := appendPath(groups, segs)
	switch v := a.Value.Resolve(); v.Kind() {
	case slog.KindGroup:
		children := v.Group()
		if len(children) == 0 {
			return a, false
		}
		out := make([]slog.Attr, 0, len(children))
		changed := false
		for _, child := range children {
			ra, ch := redactAttr(childPath, child)
			out = append(out, ra)
			if ch {
				changed = true
			}
		}
		if !changed {
			return a, false
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}, true
	case slog.KindString:
		masked, changed := maskSecretText(v.String())
		if !changed {
			return a, false
		}
		return slog.String(a.Key, masked), true
	case slog.KindAny:
		// Errors are the one common Any whose text this package can and
		// must inspect: an upstream error often echoes the request that
		// failed, credentials and all. The message is masked in place --
		// the rest of the error survives -- rather than the whole error
		// being dropped, so the failure stays diagnosable. Only the text
		// is preserved (a new error carrying the masked message): sinks
		// render error attributes from Error() alone, and nothing
		// downstream of a sink can unwrap an attribute anyway.
		if err, ok := v.Any().(error); ok {
			text := err.Error()
			masked, changed := maskSecretText(text)
			if !changed {
				return a, false
			}
			return slog.Any(a.Key, errors.New(masked)), true
		}
		return a, false
	default:
		// Numeric, boolean, duration and time values carry no textual
		// content to leak.
		return a, false
	}
}

// safeRedactAttr is the recover-guarded entry point for caller-supplied
// values: a LogValuer whose LogValue panics, or an error whose Error panics,
// must never take down the logging path (root CLAUDE.md: logging must never
// take down a request). Where the underlying slog sink would propagate such
// a panic, this layer converts it into a wholesale redaction of the
// attribute, which is also the safe direction.
func safeRedactAttr(groups []string, a slog.Attr) (out slog.Attr, changed bool) {
	defer func() {
		if recover() != nil {
			out = slog.Attr{Key: a.Key, Value: slog.StringValue(RedactedValue)}
			changed = true
		}
	}()
	return redactAttr(groups, a)
}

// pathSensitive reports whether any segment of segs contains a
// sensitiveStems entry. A path made of dotted key segments is the unit the
// redactor reasons about: a dotted config key like
// "billing.stripe_secret_key" is sensitive through its final segment, a
// slog group named "credentials" is sensitive through its own.
func pathSensitive(segs []string) bool {
	for _, seg := range segs {
		if segmentSensitive(seg) {
			return true
		}
	}
	return false
}

// segmentSensitive reports whether seg contains any sensitiveStems entry,
// compared ASCII-case-insensitively.
func segmentSensitive(seg string) bool {
	for _, stem := range sensitiveStems {
		if foldContainsASCII(seg, stem) {
			return true
		}
	}
	return false
}

// appendPath returns path followed by more, in a fresh slice.
func appendPath(path, more []string) []string {
	out := make([]string, 0, len(path)+len(more))
	out = append(out, path...)
	return append(out, more...)
}

// ---------------------------------------------------------------------------
// Value-shape scanning
// ---------------------------------------------------------------------------

// maskSecretText returns s with every recognized secret-shaped region
// replaced in place by RedactedValue, reporting whether anything changed.
// The shapes are the canonical forms secrets take in text -- high
// precision by design, so ordinary identifiers (UUIDs, opaque row ids)
// never match; there is deliberately no generic "long random string"
// heuristic, which would redact exactly the correlation values this
// package must preserve. Keys are case-insensitive only where the real
// world is ("Bearer" is conventional, provider prefixes are fixed).
//
// The scan is bounded and allocation-free on the common no-match path: a
// length gate first, then one cheap substring gate per shape class, and
// only a gate hit runs the corresponding regexp.
func maskSecretText(s string) (string, bool) {
	if len(s) < minSecretScanLen {
		return s, false
	}
	changed := false
	out := s
	for i := range secretShapePatterns {
		p := &secretShapePatterns[i]
		if !p.gate(out) {
			continue
		}
		replaced := p.re.ReplaceAllString(out, p.repl)
		if replaced != out {
			out = replaced
			changed = true
		}
	}
	return out, changed
}

// secretShapePattern couples one secret-shape regexp with the cheap gate
// that decides whether running it is worth it, and the replacement
// template applied to each match (either the bare RedactedValue, or a
// template keeping the identifying prefix -- the parameter name in a URL,
// the user part of userinfo -- and masking only the secret itself).
type secretShapePattern struct {
	gate func(string) bool
	re   *regexp.Regexp
	repl string
}

// secretShapePatterns is scanned in order; masked output is never
// re-matchable (RedactedValue contains characters none of the shape
// classes accepts), so the order is immaterial to correctness.
var secretShapePatterns = []secretShapePattern{
	{
		// "Bearer <token>" and friends: the Authorization-header idiom, in
		// attribute values and inside error texts alike. The token itself
		// is masked; the "Bearer " marker survives, matching how the URL
		// patterns below keep their parameter names and scheme -- the
		// reader still learns WHAT was masked (a bearer credential, not,
		// say, a session id) without ever seeing the credential.
		gate: func(s string) bool { return foldContainsASCII(s, "bearer") },
		re:   regexp.MustCompile(`(?i)(\bbearer\s+)([a-z0-9._~+/=-]{16,})`),
		repl: "${1}" + RedactedValue,
	},
	{
		// JSON Web Tokens: the base64url header of a real JWT always
		// starts with "eyJ" ({"...), which is a far more precise anchor
		// than any generic "dot-separated segments" rule. JOSE compact
		// tokens carry three (JWS) or five (JWE) dot-separated segments,
		// so the shape consumes the whole run of segments after the
		// header -- at least two of them, and any number more -- rather
		// than stopping at the first three, which would leave a JWE's
		// ciphertext and tag segments in plaintext behind the marker.
		gate: func(s string) bool { return strings.Contains(s, "eyJ") },
		re:   regexp.MustCompile(`\beyJ[a-zA-Z0-9_-]{6,}(?:\.[a-zA-Z0-9_-]{6,}){2,}`),
		repl: RedactedValue,
	},
	{
		// Provider-prefixed keys: the families whose literal prefixes make
		// them recognizable at a glance -- Stripe/OpenAI-style (sk_/pk_/
		// rk_/sk-), AWS access key ids (AKIA), Google API keys (AIza),
		// GitHub (ghp_/gho_/ghu_/ghs_/ghr_ and github_pat_),
		// Slack (xox*), GitLab (glpat-).
		gate: func(s string) bool {
			return strings.Contains(s, "sk_") || strings.Contains(s, "sk-") ||
				strings.Contains(s, "pk_") || strings.Contains(s, "pk-") ||
				strings.Contains(s, "rk_") || strings.Contains(s, "rk-") ||
				strings.Contains(s, "AKIA") || strings.Contains(s, "AIza") ||
				strings.Contains(s, "ghp_") || strings.Contains(s, "gho_") ||
				strings.Contains(s, "ghu_") || strings.Contains(s, "ghs_") ||
				strings.Contains(s, "ghr_") || strings.Contains(s, "github_pat_") ||
				strings.Contains(s, "glpat-") || strings.Contains(s, "xoxa-") ||
				strings.Contains(s, "xoxb-") || strings.Contains(s, "xoxo-") ||
				strings.Contains(s, "xoxp-") || strings.Contains(s, "xoxr-") ||
				strings.Contains(s, "xoxs-")
		},
		re: regexp.MustCompile(
			`\b(?:sk|pk|rk)[_-][a-zA-Z0-9_-]{16,}` +
				`|\bAKIA[0-9A-Z]{16}\b` +
				`|\bAIza[0-9A-Za-z_-]{35}\b` +
				`|\bgh[pousr]_[A-Za-z0-9]{36}\b` +
				`|\bgithub_pat_[A-Za-z0-9_]{20,}\b` +
				`|\bglpat-[a-zA-Z0-9_-]{20,}\b` +
				`|\bxox[baprs]-[a-zA-Z0-9-]{10,}\b` +
				`|\bxoxo-[a-zA-Z0-9-]{10,}\b`,
		),
		repl: RedactedValue,
	},
	{
		// Credentials embedded in a URL query string. Only parameter names
		// that are unambiguous secrets are recognized (access_token,
		// api_key, client_secret, password, refresh_token, secret,
		// session keys/tokens, signatures, bare "token"); deliberately not
		// the ambiguous ones ("auth", "key", "code") whose values are
		// often harmless. The parameter name survives, the value does not.
		gate: func(s string) bool {
			return strings.Contains(s, "=") &&
				(strings.Contains(s, "://") || strings.Contains(s, "?"))
		},
		re: regexp.MustCompile(
			`(?i)([?&](?:access_token|access-token|api[_-]?key|apikey|authorization|` +
				`client[_-]?secret|password|passwd|refresh_token|refresh-token|secret|` +
				`session[_-]?key|session_token|session-token|sig|signature|token)=)([^&#"\s<>]+)`,
		),
		repl: "${1}" + RedactedValue,
	},
	{
		// userinfo embedded in a URL (https://user:password@host): the
		// password half is masked, the username and the scheme survive.
		gate: func(s string) bool {
			return strings.Contains(s, "://") && strings.Contains(s, "@")
		},
		re:   regexp.MustCompile(`([a-z][a-z0-9+.-]*://[^/@\s:]+:)([^@\s/]+)(@)`),
		repl: "${1}" + RedactedValue + "${3}",
	},
}

// ---------------------------------------------------------------------------
// The slog handler layer
// ---------------------------------------------------------------------------

// redactHandler is the slog.Handler that guarantees redaction for every
// record logged through a logger FromContext returned: it wraps the sink
// handler underneath and rewrites each record's attributes before the sink
// ever sees them. It is not exported -- redaction is a property of the
// sanctioned FromContext API, not an optional wrapper a caller picks up.
//
// How the slog mechanics are kept honest:
//
//   - slog carries logger-level attributes and groups in the HANDLER, not
//     the record: Logger.With and Logger.WithGroup call the handler's
//     WithAttrs and WithGroup, and only the call-site arguments of a log
//     statement arrive in Handle's record. Delegating WithAttrs/WithGroup
//     to the sink unchanged would therefore let "static" attributes logged
//     as logger.With("token", t) bypass this layer entirely (the built-in
//     TextHandler/JSONHandler even pre-format With-attrs into bytes at
//     WithAttrs time). This handler therefore redacts in WithAttrs --
//     static attributes are fixed values, so redacting once, when they are
//     attached, is as strong as redacting per record -- and only then hands
//     them to the sink's own WithAttrs, preserving the sink's formatting
//     fast path. WithGroup is mirrored: the group name chain is kept here
//     as the key-path context redaction runs under, and delegated so the
//     sink qualifies keys exactly as it would unmodified.
//
//   - Handle therefore only ever needs to process the record's own
//     attributes (plus the mirrored group context), which keeps the common
//     path -- a record with nothing sensitive, the overwhelming majority --
//     at a single streaming scan with zero allocations: the original record
//     is forwarded untouched when nothing changed.
//
// redactHandler is safe for concurrent use: it carries no mutable state
// (WithAttrs/WithGroup return new handlers), matching the slog.Handler
// concurrency contract.
type redactHandler struct {
	// next is the sink handler, possibly carrying its own WithAttrs/
	// WithGroup state built from already-redacted attributes.
	next slog.Handler
	// groups mirrors the WithGroup chain opened on this handler, so key
	// redaction runs with the same group context the sink will qualify
	// keys with.
	groups []string
}

// Enabled delegates to the sink.
func (h *redactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// WithAttrs redacts attrs under the current group context, then stores
// them in the sink's own WithAttrs state -- the sink formats them (and, for
// the built-in handlers, pre-formats them) exactly as it would have without
// this wrapper, from already-redacted values.
func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	redacted := make([]slog.Attr, len(attrs))
	changed := false
	for i, a := range attrs {
		ra, ch := safeRedactAttr(h.groups, a)
		redacted[i] = ra
		if ch {
			changed = true
		}
	}
	h2 := *h
	if changed {
		h2.next = h.next.WithAttrs(redacted)
	} else {
		h2.next = h.next.WithAttrs(attrs)
	}
	return &h2
}

// WithGroup mirrors the group name into this handler's key-path context and
// delegates to the sink so key qualification stays identical to an
// unwrapped handler.
func (h *redactHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	h2.groups = append(append([]string(nil), h.groups...), name)
	h2.next = h.next.WithGroup(name)
	return &h2
}

// Handle rewrites r's attributes through redactAttr, then forwards the
// record to the sink. When nothing changed -- the common case -- r itself
// is forwarded, so the fast path costs one streaming scan and no
// allocations.
func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	groups := h.groups

	changed := false
	r.Attrs(func(a slog.Attr) bool {
		_, ch := safeRedactAttr(groups, a)
		if ch {
			changed = true
		}
		return true
	})
	if !changed {
		return h.next.Handle(ctx, r)
	}

	rec := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		ra, _ := safeRedactAttr(groups, a)
		rec.AddAttrs(ra)
		return true
	})
	return h.next.Handle(ctx, rec)
}

// ---------------------------------------------------------------------------
// ASCII folding helpers
// ---------------------------------------------------------------------------

// foldContainsASCII reports whether s contains sub, comparing ASCII
// letters case-insensitively and everything else byte-exactly. Allocated
// nothing, so it is safe on the logging hot path.
func foldContainsASCII(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if foldEqualASCII(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

// foldEqualASCII reports whether a and b are equal under ASCII
// case-folding, assuming len(a) == len(b).
func foldEqualASCII(a, b string) bool {
	for i := 0; i < len(a); i++ {
		if foldByte(a[i]) != foldByte(b[i]) {
			return false
		}
	}
	return true
}

// foldByte lowercases an ASCII letter and leaves every other byte alone.
func foldByte(b byte) byte {
	if 'A' <= b && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
