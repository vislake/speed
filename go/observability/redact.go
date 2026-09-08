package observability

// This file implements the PII/secret log-redaction item of the M0
// data-protection milestone described in docs/internal/15-roadmap.md. The
// mandate comes from docs/internal/09-observability.md: plaintext PII,
// secrets, tokens and full prompts never enter logs or traces; redaction
// is on by default and must not be disableable. Coverage against that
// mandate is partial class by class -- credential keys and tokens are
// covered here, plaintext PII and full prompts are caller-declared (see
// the "Coverage against the data-protection mandate" section below), so
// the mandate sentence is not an implementation claim on its own.
// docs/internal/10-compliance-and-audit.md places the mechanism in this
// module's logging layer -- safe by default rather than leaking by default
// -- and defers audit-log redaction to the M1+ compliance milestone.
//
// The mechanism is a slog.Handler wrapper, redactHandler, that wraps the
// sink handler underneath every *slog.Logger FromContext returns. It
// redacts attributes by key name and by value shape before the record ever
// reaches the sink, so the guarantee holds uniformly for every sink a host
// plugs in -- a console handler, a JSON handler feeding Loki, or a future
// slog-to-OTel bridge -- with no per-sink code.
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
//     API; this module's own rules confine that construction site -- the
//     documented escape, not a per-call flag -- to process startup and the
//     other genuinely context-less special cases WithLogger's doc comment
//     names (root CLAUDE.md and backend-coding-standards.md only require
//     the context logger where a context exists).
//  2. Value-shaped, as a fallback net for secrets logged under an
//     unsuspicious key: attribute string values and error texts are scanned
//     for the canonical shapes secrets take -- Authorization-header
//     credentials ("Bearer <token>" and "Basic <base64>"), JWTs
//     (eyJ...), provider-prefixed keys (sk_/pk_/rk_/sk-, AKIA, AIza,
//     ghp_/github_pat_, xox*, glpat-), and credentials embedded in URLs
//     (query parameters such as ?access_token=..., a bare query string
//     that carries no URL at all, and userinfo, scheme case-insensitive).
//     Matched regions are replaced by RedactedValue in place; surrounding
//     text is preserved. Errors are masked, never dropped.
//
// The correlation fields this must never touch are the structured log
// field names every module shares -- tenant_id, user_id, job_id (plus this
// module's trace_id and span_id): they are exempt from value-shape scanning
// by exact name (neverRedactKeys), so an id value can never be mangled no
// matter what it looks like. The exemption is checked before the key-based
// rule and therefore also wins over a sensitive key path: an exempt-named
// attribute whose key path names a secret ("credentials.user_id", or a
// user_id attribute logged under a WithGroup("credentials") context) passes
// untouched all the same, because exemption means no key rule applies
// whatever the surrounding path says.
//
// The one place the exemption does not reach is an inline slog.Group
// attribute whose OWN name is sensitive. There the attribute's key IS the
// group name, so redactAttrWhole replaces the group wholesale before any
// child is visited -- the bucket is the secret -- and
// slog.Group("credentials", "user_id", ...) collapses with its exempt
// child inside it. That is the safe direction (more redaction, never
// less), and it is why this rule is stated as two halves rather than one:
// both are pinned by TestRedact_ExemptKeysUnderSensitivePaths.
//
// # Coverage against the data-protection mandate
//
// docs/internal/09-observability.md's mandatory clause lists four classes
// that never enter logs or traces -- plaintext PII, keys and secrets,
// tokens, and full prompts -- with redaction on by default and not
// disableable. This package's coverage against that clause is deliberately
// partial, and the partiality is stated class by class so the gaps stay
// visible in the doc instead of being inferable only from what is absent:
//
//   - tokens: covered. The "token" key-name stem (word-boundary and
//     terminal-suffix matched, see sensitiveStems) replaces token-named
//     attributes wholesale, and the value-shape net (Bearer/Basic
//     credentials, JWTs, provider-prefixed keys, secret-named URL query
//     parameters) catches token-shaped values logged under any key.
//   - keys and secrets: covered. The key-name stems (secret, key,
//     password/passwd/pwd, credential, authorization, cookie) replace
//     secret-named attributes wholesale, with the same value-shape net as
//     the backstop. This is the class the stem list above is exhaustive
//     over: the stems are the credential and token naming vocabularies,
//     and a future sensitive-named key in this class is a code-review
//     point in redact.go, not a config question.
//   - plaintext PII (email addresses, phone numbers, and the rest): not
//     covered -- a class-level gap recorded here, not an omission from
//     the key list. The PII key-name space is large and grows with
//     business code (a user_email attribute, a contact_phone, an
//     address line, a customer's display name), so key-name matching is
//     inherently incomplete against it, and no value shape exists that
//     separates PII from ordinary identifiers; the caller declares what
//     is PII instead. The declaration mechanism is the explicit
//     sensitive-parameter shape the pkgcore round's apperr work is
//     building (its WithSensitiveParam twin); until that mechanism
//     lands, PII-shaped log content is the logging call site's
//     responsibility, and this layer stays the backstop for the
//     credential classes above, never the main line for PII.
//   - full prompts: not covered, for the same reason and through the same
//     mechanism: an LLM request body can carry anything, so no key-name
//     or shape rule can enumerate it -- prompt text is content-sensitive
//     by the caller's own declaration, with the same dependency on the
//     pkgcore round's declaration mechanism stated above.
//
// The same clause's other exits are covered elsewhere, not here: this
// package's own span attributes are kept free of secret-shaped and
// id-bearing material by construction (see middleware.go), API-response
// redaction belongs to the API layer (apperr), and audit records are the
// M1+ compliance work (docs/internal/10-compliance-and-audit.md).
//
// # Deliberate boundaries
//
// What follows is the mechanism's exclusion list -- the shapes and
// channels the rules above deliberately do not reach. The class-level
// exclusions (plaintext PII and full prompts) live in the coverage table
// above, never claimed covered here by their absence from this list.
//
//   - The record's message is NOT scanned: messages are constant strings
//     per the logging discipline, and a redactor is not a substitute for
//     it.
//   - Attributes holding arbitrary structs (slog.Any with a non-error
//     value) are redacted wholesale under a sensitive key but not
//     introspected under a plain one -- reflection over unknown types is
//     out of scope; log structs field-wise under descriptive keys. The
//     same boundary covers fmt.Stringer values (a *url.URL, say): slog
//     does not unwrap them, so they arrive as KindAny and their rendered
//     text never passes through the value-shape net -- a documented
//     limitation, not a live gap, since no call site logs a Stringer
//     today; log such values as strings (or field-wise) so the shape scan
//     applies.
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
//
// # No redaction-decision trace, by deliberate choice
//
// Redaction here is silent: nothing records which attribute keys were
// redacted, by which stem, on which record. That silence is exactly what
// let the "token" stem's substring match swallow ai-gateway's
// prompt_tokens/completion_tokens fields for a full round with no signal
// anywhere that redaction had even run (the fix: see sensitiveStems' own
// doc comment). Adding a debug-level trace of redaction decisions was
// considered for this same round and deliberately deferred rather than
// built: Handle is on the hot path of every FromContext call site in the
// codebase, and redactHandler.Handle's zero-allocation forwarding of an
// unchanged record (see below) is a documented, tested property this
// package guards deliberately -- a trace call on every redacted attribute,
// gated correctly behind Enabled(LevelDebug) or not, is a real design
// surface (recursive-logging risk if the trace itself goes through
// FromContext, an allocation cost on a path this file measures explicitly)
// that deserves its own round rather than riding in on a stem-matching
// bugfix. Until then, the mitigation is what this round actually shipped:
// narrowing "token" so the false-positive class the silence hid is gone,
// rather than merely making it audible.

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
// as secret-bearing. Matching is case-insensitive and, for every stem but
// "token" (see below), a bare substring match that errs on the safe side:
// an attribute whose name contains any of these is redacted wholesale even
// when its value is not secret-shaped, because the cost of an
// over-redacted field is noise while the cost of a leaked secret is a
// breach -- "apikey" (no separator) and "credentials" (plural) both rely
// on exactly this permissiveness, and both are pinned by
// TestRedact_SensitiveKeyValues. None of the shared snake_case correlation
// keys (tenant_id, user_id, job_id, trace_id, span_id) contains a stem;
// see neverRedactKeys for the belt-and-braces exemption from value
// scanning.
//
// "token" is the stem that cannot use a bare substring match: "token"
// is also a substring of "tokens", the ordinary plural for an LLM/usage
// count (ai-gateway's PromptTokens/CompletionTokens), which the naive
// substring rule swallowed with no warning -- ai-gateway's own gateway.go
// carries a "rename to _units to dodge this redactor" comment as the paper
// trail. stemMatches therefore checks "token" with a word-boundary rule
// (foldContainsWordASCII) instead: a match counts only when a word
// boundary flanks it on both sides -- the segment's own start/end, a
// non-letter byte (an underscore or digit separator), or a
// lowercase-to-uppercase case transition, the marker camelCase leaves
// between words. "access_token" and "session_token_duration" still match
// through the '_' boundary, "token" alone matches at the segment's own
// start/end, and "accessToken"/"tokenValue" match through their case
// transitions (see wordBoundaryASCII); but "tokens"/"prompt_tokens"/
// "completion_tokens" and their camelCase twins "promptTokens"/
// "completionTokens" do not, since the "s" continuing the stem is a
// lowercase letter and a lowercase continuation is the same word, not a
// boundary.
//
// "key" is the second stem with a rule of its own, for the opposite
// false-positive class: a bare substring match over-redacts this
// repository's own correlation fields. "key_id" (go/integration logs an
// API-key row's opaque id under it) and "credit_idempotency_key"
// (examples/reference-app's smilesim logs the derived key of an orphaned
// credit reservation under it -- the value the log line exists for) both
// contain "key" as a bare substring yet hold references or derived
// correlation identifiers, never credential material. keyStemMatches
// (see below) gives "key" the same word-boundary treatment "token" has
// and then narrows the reference-shaped compounds back out of the rule.
//
// One letter-glued join the boundary rule cannot see is nevertheless
// secret-shaped: a run-together compound that ENDS with the stem.
// "apitoken", "accesstoken" and their all-caps-prefix twins "APIToken"/
// "JWTToken" carry no separator and no case transition at the join (an
// all-caps acronym glues through an uppercase-to-uppercase adjacency), so
// wordBoundaryASCII reads each as one word continuing -- yet the stem is
// the compound's last morpheme, and Go compound names place the secret
// word last, the way "accessToken" and "api_token" do. stemMatches
// therefore also matches when the stem is the segment's terminal suffix
// (foldSuffixASCII): nothing continues a trailing stem, so only its left
// side is in question, and a glued left side is exactly the
// modifier+secret-word naming this rule exists for. The rule stops at the
// segment's end -- a stem a lowercase letter continues past ("tokens",
// "tokenizer_version", "detokenize", "subtoken_count", and their
// run-together forms "prompttokens"/"sessiontokens", whose "...tokens"
// ending is not the bare stem) stays a different word, so the
// over-redaction class this paragraph's first half closed stays closed;
// the newly accepted cost is a bare word that literally ends in the
// stem's letters ("subtoken") being redacted, the safe-side direction
// this table already declares.
//
// A field that
// genuinely stores multiple real tokens under a plural key name (e.g. a
// hypothetical "session_tokens") is not caught by the key rule any more --
// the accepted cost of closing the over-redaction gap -- but the
// value-shape net (maskSecretText) still catches a real bearer token, JWT,
// or provider-prefixed key logged under any key, plural or not.
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

// stemMatches reports whether seg is marked sensitive by stem, dispatching
// to the word-boundary and terminal-suffix rules for "token" and "key"
// and the permissive substring rule for every other stem. See
// sensitiveStems' doc comment for why the stems need different rules.
func stemMatches(stem, seg string) bool {
	switch stem {
	case "token":
		return foldContainsWordASCII(seg, stem) || foldSuffixASCII(seg, stem)
	case "key":
		return keyStemMatches(seg)
	}
	return foldContainsASCII(seg, stem)
}

// keyStemMatches is the "key" stem's matcher. "key" gets the same
// word-boundary and terminal-suffix treatment "token" has (see
// sensitiveStems' doc comment): a bare substring rule would keep redacting
// words that merely contain the letters as an interior fragment
// ("keyboard", "keycloak" -- and, through the same permissiveness that
// once swallowed "tokens", this repository's own correlation fields),
// while the secret-shaped key names that exist in practice -- api_key,
// x_api_key, private_key, signing_key, secret_key, dotted config segments
// like "stripe_secret_key", run-together "apikey", camelCase "apiKey", and
// bare "key" -- all carry "key" as a whole word or as the segment's
// terminal run-together suffix, which is exactly what the word-boundary
// and suffix rules match. A terminal-glued non-secret word that merely
// ends in the letters ("monkey") stays redacted, the same accepted cost
// the "token" stem's rule documents for "subtoken".
//
// Two reference-shaped compound classes are then exempted from that match,
// because they name correlation identifiers rather than credentials and
// the exemption narrows the rule without opening a hole (their values
// still pass through the value-shape net -- see
// TestRedact_KeyStemDoesNotOverRedactCorrelationReferences):
//
//   - an "_id"-suffixed segment ("key_id", "api_key_id"): the naming
//     convention neverRedactKeys' own user_id/job_id entries follow, and
//     an _id-marked attribute holds a row reference -- go/integration's
//     "key_id" logs an API-key row's opaque id, precisely so an operator
//     can tell which key failed its last-used update.
//
//   - a segment ending in "idempotency_key" ("idempotency_key",
//     "credit_idempotency_key"): the repository-wide idempotency-key
//     naming convention (go/jobs' queue metadata, go/metering's outbox,
//     go/billing's credit ledger and go/notification's delivery key all
//     key one business-operation instance by a deterministic id derived
//     from the operation's own identity -- never random -- a correlation
//     identifier whose value the log line exists to show:
//     examples/reference-app's smilesim logs "credit_idempotency_key"
//     precisely so an operator can reconcile an orphaned reservation.
//
//     The exemption does not rest on the value being safe by
//     construction: of the four conventions only go/notification's
//     delivery key is actually hashed -- go/jobs' is the enqueuing
//     caller's own key text, embedded verbatim in its asynq TaskID, and
//     go/metering's and go/billing's are the calling code's own -- so a
//     caller that embeds credentials or PII in a key puts that text
//     under an exempted name. What the exemption narrows is only the
//     "key" stem, never the value net or the PII boundary: a
//     secret-shaped value logged under a surviving idempotency_key name
//     is still masked in place, exactly as under key_id (pinned by
//     TestRedact_KeyStemDoesNotOverRedactCorrelationReferences), while
//     PII-shaped text is caller-declared content this layer cannot
//     recognize -- the owning module's field doc is the caller's gate
//     (go/jobs' Task.IdempotencyKey warns that its text rides verbatim
//     into the logged JobID).
//
// Everything else the word-boundary match still catches redacts as
// before, and the exemptions apply to the "key" stem only: "id_token",
// "session_token" and friends keep redacting through the "token" stem
// unchanged.
func keyStemMatches(seg string) bool {
	if !foldContainsWordASCII(seg, "key") && !foldSuffixASCII(seg, "key") {
		return false
	}
	if foldSuffixASCII(seg, "_id") || foldSuffixASCII(seg, "idempotency_key") {
		return false
	}
	return true
}

// neverRedactKeys are the correlation field names
// docs/internal/09-observability.md's logging rule fixes as shared and
// queryable: they must survive redaction verbatim so a log line stays
// joinable to its trace and tenant. None of them can match sensitiveStems
// today, but they are exempted explicitly -- not by luck -- so a future
// stem addition or an adversarial id value (tenant names are user-chosen
// strings) can never start mangling correlation fields. Exemption means
// the whole attribute is passed through untouched: no key rule, no
// value-shape scan. What the value contains is the logging call site's
// responsibility, not this map's: on the asynq-backed queue the job_id
// values embed the enqueuing caller's raw Task.IdempotencyKey text
// (go/jobs' own field doc warns key builders of exactly that), and such a
// value passes through untouched by this exemption's design.
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
	// An empty key matches no key-name rule: there is no segment to exempt
	// and no segment that could be sensitive. It must not therefore bypass
	// the value rules -- an empty key is exactly what slog's inline-group
	// idiom (slog.Group("", ...)) attaches, and the built-in sinks render
	// that group's children, and an empty-key scalar, normally, so an
	// early return here was a total redaction bypass for values logged
	// under an empty key (pinned by
	// TestRedact_EmptyKeyAttributes_ValueRulesStillApply). The value rules
	// below apply exactly as they do under a benign key: a group's
	// children are still visited (the empty segment the key contributes to
	// their path is inert -- it can neither be exempted nor match a stem),
	// and a string or error value is still scanned for secret shapes.
	//
	// Nor may the branch bypass the PATH rules: groups is an independent
	// input the branch already holds, and a sensitive logger-level group
	// (WithGroup("credentials")) redacts every attribute logged under it
	// wholesale, whatever the attribute's own key -- an empty-key scalar
	// under that group used to slip past the group-name rule to the value
	// rules alone and render verbatim when its value had no recognizable
	// secret shape (pinned by
	// TestRedact_EmptyKeyScalarUnderSensitiveGroupPath_RedactedWholesale).
	// The empty key contributes nothing to the path; the segments already
	// in groups contribute everything, so the check runs before the value
	// rules exactly as it does on the sibling branches below.
	if a.Key == "" {
		if pathSensitive(groups) {
			return redactAttrWhole(a)
		}
		return redactAttrValue(a, groups, nil)
	}

	// The overwhelmingly common attribute key is a single segment: the
	// correlation fields and every module's shared snake_case attribute
	// names carry no dot. Segmenting such a key must not allocate --
	// strings.Split always heap-allocates the segment slice it returns, and
	// one allocation per attribute is exactly what redactHandler.Handle's
	// zero-allocation forwarding of unchanged records (documented on the
	// handler below) rules out. A dot-free key IS its one segment: the
	// neverRedactKeys lookup keys on the whole key, the sensitivity check
	// scans the key directly, and no segment slice is ever built. Only a
	// genuinely dotted key (config-style names such as
	// "billing.stripe_secret_key") pays for Split.
	if strings.IndexByte(a.Key, '.') < 0 {
		if _, exempt := neverRedactKeys[a.Key]; exempt {
			return a, false
		}
		if pathSensitive(groups) || segmentSensitive(a.Key) {
			return redactAttrWhole(a)
		}
		return redactAttrValue(a, groups, nil)
	}

	segs := strings.Split(a.Key, ".")
	if _, exempt := neverRedactKeys[segs[len(segs)-1]]; exempt {
		return a, false
	}
	if pathSensitive(groups) || pathSensitive(segs) {
		return redactAttrWhole(a)
	}
	return redactAttrValue(a, groups, segs)
}

// redactAttrWhole returns a replaced wholesale by the deterministic
// RedactedValue marker, reporting that it changed. The attribute's key --
// or any group name above it -- names a secret, so none of its value may
// reach the sink whatever the value's kind; a group whose own name is
// sensitive collapses in its entirety, because the bucket is the secret.
func redactAttrWhole(a slog.Attr) (slog.Attr, bool) {
	return slog.Attr{Key: a.Key, Value: slog.StringValue(RedactedValue)}, true
}

// redactAttrValue applies the value-kind rules to an attribute whose key
// has already passed the exemption and sensitivity checks. keySegments
// holds the dot-separated segments of a.Key, or nil when the key is
// dot-free -- its single segment is the key itself, which is then appended
// when a group value's children inherit this attribute's key path.
func redactAttrValue(a slog.Attr, groups, keySegments []string) (slog.Attr, bool) {
	switch v := a.Value.Resolve(); v.Kind() {
	case slog.KindGroup:
		children := v.Group()
		if len(children) == 0 {
			return a, false
		}
		if keySegments == nil {
			keySegments = []string{a.Key}
		}
		childPath := appendPath(groups, keySegments)
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
// compared ASCII-case-insensitively (see stemMatches for the per-stem match
// rule).
func segmentSensitive(seg string) bool {
	for _, stem := range sensitiveStems {
		if stemMatches(stem, seg) {
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

// secretShapePatterns is scanned in order; the order is immaterial to
// correctness because masking is a fixed point: re-scanning masked output
// leaves it unchanged. That fixed point is not because the marker is
// unmatchable -- the URL query and userinfo value classes admit '[' and
// ']', so a re-scan can re-match "access_token=[REDACTED]" and rewrite it
// to itself -- but because every class that can admit the marker
// reproduces it exactly, never a new region.
var secretShapePatterns = []secretShapePattern{
	{
		// "Bearer <token>" and friends: the Authorization-header idiom, in
		// attribute values and inside error texts alike. The credential
		// itself is masked; the "Bearer "/"Basic " marker survives,
		// matching how the URL patterns below keep their parameter names
		// and scheme -- the reader still learns WHAT was masked (a bearer
		// credential or a basic-auth pair, not, say, a session id) without
		// ever seeing the credential. "Basic" shares the shape class
		// because an upstream error echoes a basic-auth header the same
		// way it echoes a bearer one, and the base64 body of an
		// Authorization: Basic header is exactly the secret its name
		// says; the run class covers base64's alphabet (including the '='
		// padding a bearer token never carries).
		gate: func(s string) bool {
			return foldContainsASCII(s, "bearer") || foldContainsASCII(s, "basic")
		},
		re:   regexp.MustCompile(`(?i)(\b(?:bearer|basic)\s+)([a-z0-9._~+/=-]{16,})`),
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
		// Credentials embedded in a URL query string, and in a bare query
		// string that carries no URL at all. Only parameter names that are
		// unambiguous secrets are recognized (access_token, api_key,
		// client_secret, password, refresh_token, secret, session
		// keys/tokens, signatures, bare "token"); deliberately not the
		// ambiguous ones ("auth", "key", "code") whose values are often
		// harmless. The parameter name survives, the value does not.
		//
		// The parameter may sit at the very start of the scanned text, not
		// only after a '?' or '&': the most natural way for a handler to
		// log a query is the raw r.URL.RawQuery, which carries no leading
		// '?', and captured form bodies and error echoes of them start
		// with whatever the first parameter is -- so the anchor is
		// (?:^|[?&]). The gate must be no narrower than that anchor set,
		// but it must also stay cheap: opening on any bare '&' would send
		// every benign parameter list through the regexp, and a
		// no-match regexp run costs microseconds and allocations per log
		// value (measured -- the module's allocation-free no-hit promise
		// lives or dies on gates like this one). So instead the gate
		// keeps its broad '://' and '?' checks for URL-shaped text and
		// answers every other anchor with querySecretParamAnywhere, the
		// allocation-free mirrored-name probe: a string the regexp can
		// match always contains '=' and carries one of the secret
		// parameter names directly at the start of the string, after a
		// '?', or after a '&' -- so no regexp match exists that the gate
		// does not open for. (This pattern's shape is still only
		// "a run of k=v pairs", never "a secret-looking word anywhere":
		// a secret parameter in the middle of running prose still needs a
		// '?' or '&' immediately before it.)
		gate: func(s string) bool {
			return strings.Contains(s, "=") &&
				(strings.Contains(s, "://") || strings.Contains(s, "?") || querySecretParamAnywhere(s))
		},
		re: regexp.MustCompile(
			`(?i)((?:^|[?&])(?:access_token|access-token|api[_-]?key|apikey|authorization|` +
				`client[_-]?secret|password|passwd|refresh_token|refresh-token|secret|` +
				`session[_-]?key|session_token|session-token|sig|signature|token)=)([^&#"\s<>]+)`,
		),
		repl: "${1}" + RedactedValue,
	},
	{
		// userinfo embedded in a URL (https://user:password@host): the
		// password half is masked, the username and the scheme survive.
		// The (?i) fold exists because schemes are case-insensitive in the
		// real world -- "HTTPS://ops:secret@host" is the same URL as its
		// lowercase twin, and an error text echoing it may carry either
		// spelling; every other shape class that needs case-insensitivity
		// already carries (?i), this one was missed. The negated classes
		// (username and password alphabets) contain no letters, so the
		// fold changes nothing about them.
		gate: func(s string) bool {
			return strings.Contains(s, "://") && strings.Contains(s, "@")
		},
		re:   regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^/@\s:]+:)([^@\s/]+)(@)`),
		repl: "${1}" + RedactedValue + "${3}",
	},
}

// querySecretParamNames mirrors the parameter-name alternation of the
// URL-query pattern above -- the exact names a query parameter can
// carry, compared ASCII-case-insensitively by nameAtQueryAnchor (the
// pattern's own alternation also admits the compact regex classes
// api[_-]?key / client[_-]?secret / session[_-]?key, which this list
// spells out in their concrete forms). It must be kept in step with that
// alternation by hand; the provider-prefix pattern's gate already
// carries the same established duplication. The cost of drift is bounded
// and one-directional: a forgotten name here makes the gate skip a
// string the regexp would have masked (never the reverse -- the gate
// only decides whether the regexp runs, and the regexp is the sole
// masking authority). What holds the pair in step is the value-shape
// test table in redact_test.go, not this list's own prose: the table
// logs the compact spellings the alternation's regex classes admit
// (apikey, clientsecret, sessionkey) plus two literal alternation names
// (token, access_token) at the two anchors that decide through this list
// -- heading a bare query string, and mid-string after a '&' in a bare
// form body -- so a name forgotten on either side of the pair fails the
// row that spells it. The drift this round fixed (clientsecret and
// sessionkey missing here while the alternation kept matching them)
// failed exactly those head-anchor rows, which is how the backstop is
// meant to work. Names not rowed at an anchor stay covered on URL-shaped
// text through the gate's '://' and '?' branches, which open without
// consulting this list.
var querySecretParamNames = []string{
	"access_token", "access-token", "api_key", "api-key", "apikey",
	"authorization", "client_secret", "client-secret", "clientsecret",
	"password", "passwd",
	"refresh_token", "refresh-token", "secret", "session_key", "session-key",
	"sessionkey", "session_token", "session-token", "sig", "signature",
	"token",
}

// querySecretParamAnywhere reports whether a querySecretParamNames entry
// stands directly after an anchor the URL-query pattern's regexp can hit
// -- the start of s, a '?', or a '&' -- with '=' immediately after the
// name. It is the leading-anchor test generalized to every anchor of the
// pattern's (?:^|[?&]) alternation, which is how a secret parameter that
// is not first in a bare form body (no '?', no '://') opens the gate
// without paying for a regexp run. Allocation-free: every candidate name
// is a sub-slice of s.
func querySecretParamAnywhere(s string) bool {
	for start := 0; start < len(s); {
		eq := strings.IndexByte(s[start:], '=')
		if eq < 0 {
			return false
		}
		// A '?' or '&' between the anchor and that '=' ends the current
		// parameter before any value and starts the next candidate right
		// after itself -- a name not directly after an anchor cannot be
		// what the pattern matches.
		if rel := strings.IndexAny(s[start:start+eq], "?&"); rel >= 0 {
			start += rel + 1
			continue
		}
		if nameAtQueryAnchor(s[start : start+eq]) {
			return true
		}
		// Skip the parameter's value: the next candidate can only start
		// at the next '?' or '&' (a '=' without an intervening anchor
		// begins nothing the pattern could match).
		rel := strings.IndexAny(s[start+eq+1:], "?&")
		if rel < 0 {
			return false
		}
		start += eq + 1 + rel + 1
	}
	return false
}

// nameAtQueryAnchor reports whether name is one of querySecretParamNames,
// compared ASCII-case-insensitively -- the same fold under which the
// pattern's own (?i) alternation matches it.
func nameAtQueryAnchor(name string) bool {
	for _, n := range querySecretParamNames {
		if len(name) == len(n) && foldEqualASCII(name, n) {
			return true
		}
	}
	return false
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
//     is forwarded untouched when nothing changed. The zero-allocation
//     claim is scoped to scalar attribute values: a benign slog.Group is
//     rebuilt on every visit (redactAttrValue allocates a fresh child-path
//     slice and a fresh result slice per nesting level, on the order of
//     three allocations a level) because its children must be checked
//     before the record can be forwarded untouched.
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

// foldSuffixASCII reports whether s ends with sub, comparing ASCII letters
// case-insensitively and everything else byte-exactly. Allocates nothing,
// so it is safe on the logging hot path. This is the "token" stem's
// terminal-suffix matcher (see sensitiveStems and stemMatches): a segment
// that ends with the bare stem has nothing continuing it, so the stem is
// the segment's last word and the segment is secret-shaped whatever glue
// runs it together with its prefix -- "apitoken" and "APIToken" carry no
// separator and no case transition at the join, but each ends with
// "...token". A segment ending in the plural ("...tokens", "...Tokens")
// does not end with the bare stem and stays out of this rule's reach.
func foldSuffixASCII(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	return foldEqualASCII(s[len(s)-len(sub):], sub)
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

// isASCIILetter reports whether b is an ASCII letter.
func isASCIILetter(b byte) bool {
	return ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

// isLowerCaseASCII and isUpperCaseASCII report whether b is an ASCII
// lowercase / uppercase letter.
func isLowerCaseASCII(b byte) bool { return 'a' <= b && b <= 'z' }
func isUpperCaseASCII(b byte) bool { return 'A' <= b && b <= 'Z' }

// wordBoundaryASCII reports whether a word boundary separates the two
// adjacent bytes s[left] and s[right] (right == left+1). A boundary is
// present when one side lies outside the string (the segment's own start
// or end), when either byte is not an ASCII letter (an underscore or digit
// separator), or when the pair is a lowercase-to-uppercase case transition
// -- the marker camelCase leaves between words, so "accessToken" splits
// between the 's' and the 'T' exactly where "access_token" splits at the
// '_', and "tokenValue" splits between the 'n' and the 'V'. Every other
// letter adjacency is one word continuing -- "tokens"'s 'n'-'s' and
// "tokenizer"'s 'n'-'i' are the same word, not a boundary -- which is what
// keeps the plural and the "-izer" suffix out of the "token" stem's net.
// An all-caps acronym glued directly to a word ("APIToken", "JWTToken")
// carries no case transition at the join, so the join is not a boundary
// to this matcher either. When such a compound ends at the segment's own
// end, the terminal-suffix rule (foldSuffixASCII, see stemMatches) still
// catches it -- nothing continues a trailing stem -- so this matcher's
// blindness is confined to a mid-segment acronym join ("JWTTokenValue"),
// a shape no call site uses; spelling such a compound with a separator or
// with mixed case at the join is the convention this matcher keys on.
func wordBoundaryASCII(s string, left, right int) bool {
	if left < 0 || right >= len(s) {
		return true
	}
	if !isASCIILetter(s[left]) || !isASCIILetter(s[right]) {
		return true
	}
	return isLowerCaseASCII(s[left]) && isUpperCaseASCII(s[right])
}

// foldContainsWordASCII reports whether s contains sub as a whole word:
// every occurrence of sub (ASCII case-insensitive) is checked, and a match
// only counts when a word boundary flanks it on both sides (see
// wordBoundaryASCII for what counts as a boundary). This is the "token"
// stem's own matcher (see sensitiveStems and stemMatches): "access_token"
// and "accessToken" both match, the first through the '_' and the second
// through the 's'->'T' case transition; "tokenValue" matches through the
// 'n'->'V' transition on the other side. The plurals "tokens"/
// "prompt_tokens", camelCase or not ("promptTokens"), do not, because the
// "s" continuing the stem is a lowercase letter and a lowercase
// continuation is the same word, not a boundary.
func foldContainsWordASCII(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if !foldEqualASCII(s[i:i+len(sub)], sub) {
			continue
		}
		if !wordBoundaryASCII(s, i-1, i) {
			continue
		}
		if !wordBoundaryASCII(s, i+len(sub)-1, i+len(sub)) {
			continue
		}
		return true
	}
	return false
}
