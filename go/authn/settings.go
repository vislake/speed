package authn

// settings.go carries the seam through which this module's declared dynamic
// configuration (ConfigKey* in module.go, declared on the registry as
// ConfigSchema) actually reaches the behavior it controls.
//
// Before the seam existed those declarations had readers nowhere: every one
// of the values below was wired at construction (WithAccessTokenTTL,
// WithTrustedProviders, WithPasswordPolicy, ...) and the config items were
// inert. The seam is the structurally-typed shape the org FeatureGate and
// sharing TenantConfigReader precedents use -- defined here in terms of the
// config module's own Handle methods, so the host (or this module's
// component descriptor, when a config component is assembled) passes a
// *config.Handle and no adapter exists anywhere: the handle's reads already
// carry the three-tier resolution (tenant row, then system row, then the
// schema default) and the ok=false "no explicit row" answer.
//
// # Reading semantics
//
// Every read below follows one rule: an explicit row wins; when no row
// exists (ok=false) the module falls back to the value its construction
// options carried, so a host that configures nothing dynamically -- the
// reference app today, every unit test -- behaves exactly as before the
// seam existed. A nil reader is a legal configuration meaning "no dynamic
// configuration module at all".
//
// The one deliberate exception is the trusted-provider list, which is a
// security gate: there a READ FAILURE (as opposed to an unset row) fails
// closed to the empty list rather than falling back, because falling back
// on error could resurrect a list an operator had explicitly turned off.
// See trustedProviderList.
//
// # Read timing
//
// Most values are read at the operation that consumes them -- a sign-in, a
// password set, a token mint, an SMS code issue -- so an operator's change
// takes effect on the next operation. The two exceptions are documented at
// their resolution sites: the access-token TTL is resolved on first use and
// frozen for the process (it sizes the signing-key lifecycle, so it cannot
// move under live keys; token.go's effectiveTTL), and the password policy
// is read per validation with a coherence check (passwordPolicyFor).

import (
	"context"
	"strings"
	"time"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/observability"
)

// SettingsReader is the structurally-typed seam through which this module's
// declared config items are read at runtime. Its method set is exactly
// (*config.Handle)'s typed reads, so a host wires it with
//
//	authn.WithSettingsReader(configModule.Handle())
//
// and this module's component descriptor wires the same value when a config
// component is part of the assembly: the descriptor declares the config
// module as an optional requirement and passes its handle (component.go),
// which is where this module's direct go/config dependency comes from. The
// seam stays structural -- it names no config type -- and the assertion at
// the bottom of this file pins that (*config.Handle) satisfies it at
// compile time.
//
// A nil reader is legal and fully supported: every read falls back to the
// value the construction options carried, which is the behavior this module
// had before the seam existed. A wired reader reports ok=false for a key no
// explicit row produced, which reads identically to nil.
type SettingsReader interface {
	// Duration returns key's effective duration value for the tenant the
	// context carries, and whether an explicit row produced it.
	Duration(ctx context.Context, key string) (time.Duration, bool, error)
	// Int returns key's effective integer value for the tenant the context
	// carries, and whether an explicit row produced it.
	Int(ctx context.Context, key string) (int64, bool, error)
	// String returns key's effective string value for the tenant the
	// context carries, and whether an explicit row produced it.
	String(ctx context.Context, key string) (string, bool, error)
}

// WithSettingsReader wires the reader this module's declared dynamic
// configuration is resolved through at runtime. See SettingsReader for the
// reading semantics and the nil contract.
func WithSettingsReader(reader SettingsReader) Option {
	return func(o *options) { o.settings = reader }
}

// durationSetting resolves key through the wired reader, falling back to
// fallback when no reader is wired, no explicit row exists, or the read
// fails (logged; availability over strictness for these values -- none of
// them is a security gate, and refusing every session mint because the
// configuration store had a bad minute would be worse than the stale
// value). A resolved value must be positive; a non-positive or nonsensical
// one is logged and the fallback stays.
func durationSetting(ctx context.Context, reader SettingsReader, key string, fallback time.Duration) time.Duration {
	if reader == nil {
		return fallback
	}
	d, ok, err := reader.Duration(ctx, key)
	switch {
	case err != nil:
		observability.FromContext(ctx).Warn("dynamic configuration read failed; using the construction-time value", "key", key, "error", err)
		return fallback
	case !ok:
		return fallback
	case d <= 0:
		observability.FromContext(ctx).Warn("dynamic configuration resolved a non-positive duration; using the construction-time value", "key", key)
		return fallback
	default:
		return d
	}
}

// intSetting resolves key through the wired reader, with the same fallback
// rule as durationSetting. min/max bound the accepted value: a resolved
// value outside the key's declared schema range is logged and the fallback
// stays (defense in depth -- the config schema already refuses such writes).
func intSetting(ctx context.Context, reader SettingsReader, key string, fallback, min, max int64) int64 {
	if reader == nil {
		return fallback
	}
	n, ok, err := reader.Int(ctx, key)
	switch {
	case err != nil:
		observability.FromContext(ctx).Warn("dynamic configuration read failed; using the construction-time value", "key", key, "error", err)
		return fallback
	case !ok:
		return fallback
	case n < min || n > max:
		observability.FromContext(ctx).Warn("dynamic configuration resolved a value outside the schema range; using the construction-time value", "key", key, "value", n)
		return fallback
	default:
		return n
	}
}

// trustedProviderList resolves the effective trusted-provider list for the
// request. An explicit row wins (whitespace-delimited channel names, empty
// meaning "trust nothing"); an unset row falls back to the construction-time
// WithTrustedProviders list; and a READ FAILURE fails closed to the empty
// list with a logged warning.
//
// The fail-closed direction on error is deliberate and is the one place
// this file deviates from the fall-back rule: the fallback list is the
// deployment's bootstrap-era baseline, so treating an unreadable override as
// "no override" could resurrect auto-linking after an operator explicitly
// disabled it, while refusing a link only costs the person a manual bind.
func (s *Service) trustedProviderList(ctx context.Context) []string {
	if s.settings == nil {
		return s.trustedProviders
	}
	raw, ok, err := s.settings.String(ctx, ConfigKeyTrustedProviders)
	switch {
	case err != nil:
		observability.FromContext(ctx).Warn("trusted-provider override could not be read; automatic account linking stays off", "key", ConfigKeyTrustedProviders, "error", err)
		return nil
	case !ok:
		return s.trustedProviders
	default:
		return strings.Fields(raw)
	}
}

// passwordPolicyFor resolves the password policy every new password is
// validated against. Both length bounds are read per validation; each falls
// back to the construction-time policy (WithPasswordPolicy) independently.
// A pair that is incoherent -- minimum above maximum, either length below
// the schema's own floors -- is logged and the construction-time policy
// applies as a whole, because a half-applied policy would refuse every
// password instead of protecting them.
func (s *Service) passwordPolicyFor(ctx context.Context) PasswordPolicy {
	if s.settings == nil {
		return s.policy
	}
	policy := s.policy
	minN, ok, minErr := s.settings.Int(ctx, ConfigKeyPasswordMinLength)
	if minErr != nil {
		observability.FromContext(ctx).Warn("dynamic configuration read failed; using the construction-time value", "key", ConfigKeyPasswordMinLength, "error", minErr)
	} else if ok {
		policy.MinLength = int(minN)
	}
	maxN, ok, maxErr := s.settings.Int(ctx, ConfigKeyPasswordMaxLength)
	if maxErr != nil {
		observability.FromContext(ctx).Warn("dynamic configuration read failed; using the construction-time value", "key", ConfigKeyPasswordMaxLength, "error", maxErr)
	} else if ok {
		policy.MaxLength = int(maxN)
	}
	if policy.MinLength < 1 || policy.MaxLength < 1 || policy.MinLength > policy.MaxLength {
		observability.FromContext(ctx).Warn("resolved password policy is incoherent; keeping the construction-time policy", "min_length", policy.MinLength, "max_length", policy.MaxLength)
		return s.policy
	}
	return policy
}

// oauthStateTTLFor, smsCodeTTLFor and smsCodeMaxAttemptsFor resolve the
// remaining per-operation values; each is read at the operation that
// consumes it and falls back to its construction-time value, per
// durationSetting/intSetting. (The refresh and session TTLs resolve through
// the SessionManager's own pickups, session.go's refreshTTLFor/sessionTTLFor,
// because the manager is what consumes them.)
func (s *Service) oauthStateTTLFor(ctx context.Context) time.Duration {
	return durationSetting(ctx, s.settings, ConfigKeyOAuthStateTTL, s.oauthStateTTL)
}

func (s *Service) smsCodeTTLFor(ctx context.Context) time.Duration {
	return durationSetting(ctx, s.settings, ConfigKeySMSCodeTTL, s.smsCodeTTL)
}

func (s *Service) smsCodeMaxAttemptsFor(ctx context.Context) int {
	return int(intSetting(ctx, s.settings, ConfigKeySMSCodeMaxAttempts, int64(s.smsCodeMaxAttempts), 1, 1000))
}

// channelCredentialKeys maps a shipped channel to its declared
// client-id/secret config keys. A channel this module did not ship (a
// host-registered SocialProvider) has no declared keys and reports no
// mapping, so its provider keeps its construction-time credentials.
func channelCredentialKeys(channel string) (idKey, secretKey string, ok bool) {
	switch channel {
	case ProviderGoogle:
		return ConfigKeyGoogleClientID, ConfigKeyGoogleClientSecret, true
	case ProviderGitHub:
		return ConfigKeyGitHubClientID, ConfigKeyGitHubClientSecret, true
	case ProviderWeChat:
		return ConfigKeyWeChatClientID, ConfigKeyWeChatClientSecret, true
	case ProviderDingTalk:
		return ConfigKeyDingTalkClientID, ConfigKeyDingTalkClientSecret, true
	case ProviderFeishu:
		return ConfigKeyFeishuClientID, ConfigKeyFeishuClientSecret, true
	default:
		return "", "", false
	}
}

// providerCredentialsFor resolves a channel's dynamically configured client
// credentials. Both keys must carry explicit rows for the pair to win: a
// half-configured channel (one key set, one not) is a wiring mistake, and
// mixing a dynamic id with a construction-time secret would authenticate as
// neither -- so it is logged and the provider keeps its own credentials.
// The read failures follow the durationSetting rule (logged, provider
// keeps its own credentials); the SECRET's value is never logged.
func (s *Service) providerCredentialsFor(ctx context.Context, channel string) (id, secret string, ok bool) {
	if s.settings == nil {
		return "", "", false
	}
	idKey, secretKey, mapped := channelCredentialKeys(channel)
	if !mapped {
		return "", "", false
	}
	id, idSet, idErr := s.settings.String(ctx, idKey)
	secret, secretSet, secretErr := s.settings.String(ctx, secretKey)
	if idErr != nil || secretErr != nil {
		observability.FromContext(ctx).Warn("social channel credential override could not be read; the channel keeps its construction-time credentials",
			"channel", channel, "error", firstErr(idErr, secretErr))
		return "", "", false
	}
	switch {
	case !idSet && !secretSet:
		return "", "", false
	case idSet != secretSet || id == "" || secret == "":
		observability.FromContext(ctx).Warn("social channel credentials are half-configured; the channel keeps its construction-time credentials", "channel", channel)
		return "", "", false
	default:
		return id, secret, true
	}
}

// firstErr returns the first non-nil error of the pair, for one log line
// naming whichever read failed.
func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// The compile-time proof that a config module's handle satisfies this
// seam: if either shape drifts, this module no longer compiles.
var _ SettingsReader = (*config.Handle)(nil)
