package pki

// settings.go carries the seam through which this module's declared dynamic
// configuration (the ConfigKey* items in module.go, declared on the
// registry's ConfigSeat) reaches the behavior it controls: the CA and
// end-entity validity bounds and the CRL distribution point default
// (ca.go), the CRL validity period (crl.go), and the propagation window and
// renewal lead time the expiry scan resolves (lifecycle.go).
//
// The seam is the structurally-typed shape the authn and metering
// SettingsReader precedents use -- defined here in terms of the config
// module's own Handle reads, so the host (or this module's component
// descriptor, when a config component is assembled) passes a
// *config.Handle and no adapter exists anywhere: the handle's reads already
// carry the three-tier resolution (tenant row, then system row, then the
// schema default) and the ok=false "no explicit row" answer.
//
// # Reading semantics
//
// Every read follows one rule: an explicit row wins; when no row exists
// (ok=false), no reader is wired, or the read fails, the consumer falls
// back to the value its construction carried -- a NewService/NewCAService
// argument, a caller-supplied CAParams/CertificateParams field, or the
// package constant backing the declared schema default -- so a host that
// configures nothing dynamically behaves exactly as one with no config
// component at all. A nil reader is a legal configuration meaning "no
// dynamic configuration module".
//
// The CA/certificate validity pair is the one place the fallback is not the
// whole story: a zero NotAfter request takes the declared default, and the
// result is then clamped to the declared maximum -- see boundedNotAfter.

import (
	"context"
	"time"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/observability"
)

// SettingsReader is the structurally-typed seam through which this module's
// declared config items are read at runtime. Its method set is exactly the
// subset of (*config.Handle)'s typed reads pki consumes, so a host wires it
// with
//
//	pki.WithSettingsReader(configModule.Handle())
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
// value the construction carried, which is the behavior this module has
// with no config component assembled. A wired reader reports ok=false for a
// key no explicit row produced, which reads identically to nil.
type SettingsReader interface {
	// Duration returns key's effective duration value for the tenant the
	// context carries, and whether an explicit row produced it.
	Duration(ctx context.Context, key string) (time.Duration, bool, error)
	// String returns key's effective string value for the tenant the
	// context carries, and whether an explicit row produced it.
	String(ctx context.Context, key string) (string, bool, error)
}

// WithSettingsReader wires the reader this module's declared dynamic
// configuration is resolved through at runtime. See SettingsReader for the
// reading semantics and the nil contract.
func WithSettingsReader(reader SettingsReader) Option {
	return func(m *Module) { m.settings = reader }
}

// durationSetting resolves key through the wired reader, falling back to
// fallback when no reader is wired, no explicit row exists, or the read
// fails (logged; availability over strictness for these values -- refusing
// every issuance because the configuration store had a bad minute would be
// worse than the stale value). A resolved value must be positive; a
// non-positive one is logged and the fallback stays.
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

// stringSetting resolves key through the wired reader, falling back to
// fallback when no reader is wired, no explicit row exists, or the read
// fails (logged). An explicit row wins even when empty: empty is a
// meaningful value for the one string item this module reads, meaning "no
// default".
func stringSetting(ctx context.Context, reader SettingsReader, key, fallback string) string {
	if reader == nil {
		return fallback
	}
	value, ok, err := reader.String(ctx, key)
	switch {
	case err != nil:
		observability.FromContext(ctx).Warn("dynamic configuration read failed; using the construction-time value", "key", key, "error", err)
		return fallback
	case !ok:
		return fallback
	default:
		return value
	}
}

// propagationWindowFor resolves the propagation window a lifecycle
// operation applies. The caller's per-call override wins; otherwise the
// declared pki.propagation_window row wins over the Service's
// construction-time value (NewService, or Module's WithPropagationWindow
// option); a chain that still resolves nothing falls back to
// DefaultPropagationWindow.
func (s *Service) propagationWindowFor(ctx context.Context, override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	if d := durationSetting(ctx, s.settings, ConfigPropagationWindow, s.propagationWindow); d > 0 {
		return d
	}
	return DefaultPropagationWindow
}

// renewalLeadTimeFor resolves the renewal lead time an expiry scan applies,
// with the identical chain propagationWindowFor documents, ending at
// DefaultRenewalLeadTime.
func (s *Service) renewalLeadTimeFor(ctx context.Context, override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	if d := durationSetting(ctx, s.settings, ConfigRenewalLeadTime, s.renewalLeadTime); d > 0 {
		return d
	}
	return DefaultRenewalLeadTime
}

// crlValidityFor resolves how long a generated CRL claims to be current:
// GenerateCRL's own positive validity argument wins; otherwise the declared
// pki.crl_validity row wins over DefaultCRLValidity.
func (s *CAService) crlValidityFor(ctx context.Context, requested time.Duration) time.Duration {
	if requested > 0 {
		return requested
	}
	return durationSetting(ctx, s.settings, ConfigCRLValidity, DefaultCRLValidity)
}

// crlDistributionPointFor resolves the value recorded as a NEW authority's
// CRL distribution point: the caller's own CAParams value wins; otherwise
// the declared pki.crl_distribution_point row applies. A read failure falls
// back to the empty value -- no extension into child certificates -- the
// same result an unset row produces. Existing authority rows are never
// rewritten by this resolution.
func (s *CAService) crlDistributionPointFor(ctx context.Context, requested string) string {
	if requested != "" {
		return requested
	}
	return stringSetting(ctx, s.settings, ConfigCRLDistributionPoint, "")
}

// caCertificateNotAfter returns the effective NotAfter of a CA certificate
// creation request (CreateRootCA, CreateIntermediateCA), per
// boundedNotAfter.
func (s *CAService) caCertificateNotAfter(ctx context.Context, notBefore, requested time.Time) time.Time {
	return s.boundedNotAfter(ctx, notBefore, requested,
		ConfigCADefaultValidity, ConfigCAMaxValidity,
		defaultCADefaultValidity, defaultCAMaxValidity)
}

// endEntityCertificateNotAfter returns the effective NotAfter of an
// end-entity issuance request (IssueCertificate), per boundedNotAfter.
func (s *CAService) endEntityCertificateNotAfter(ctx context.Context, notBefore, requested time.Time) time.Time {
	return s.boundedNotAfter(ctx, notBefore, requested,
		ConfigCertificateDefaultValidity, ConfigCertificateMaxValidity,
		defaultCertificateDefaultValidity, defaultCertificateMaxValidity)
}

// boundedNotAfter normalizes a requested NotAfter under one declared
// default/max pair. A zero request takes the default -- the declared
// defaultKey row when an explicit one applies, the defaultValidity package
// constant otherwise. The result, defaulted or requested, is then clamped
// to the max validity (the maxKey row, or its package default), bounding
// the span measured from notBefore: a caller's request beyond the ceiling
// is clamped to it, matching the declared item's own description
// ("regardless of what the caller requests"). A request within the bounds
// passes through verbatim.
func (s *CAService) boundedNotAfter(ctx context.Context, notBefore, requested time.Time, defaultKey, maxKey string, defaultValidity, maxValidity time.Duration) time.Time {
	if requested.IsZero() {
		requested = notBefore.Add(durationSetting(ctx, s.settings, defaultKey, defaultValidity))
	}
	if max := durationSetting(ctx, s.settings, maxKey, maxValidity); requested.Sub(notBefore) > max {
		return notBefore.Add(max)
	}
	return requested
}

// The compile-time proof that a config module's handle satisfies this
// seam: if either shape drifts, this module no longer compiles.
var _ SettingsReader = (*config.Handle)(nil)
