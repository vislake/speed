package authn

import (
	"context"
	"io/fs"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	// The embedded copy of the IANA timezone database, so a preference
	// value (canonicalTimezone below) validates identically in every
	// deployment shape: a minimal image without system zoneinfo -- a
	// scratch container, a distroless one -- has no /usr/share/zoneinfo,
	// and time.LoadLocation consults that copy instead. The blank import
	// must not be removed: dropping it makes validation environment-
	// dependent, succeeding on a developer machine and refusing every
	// non-UTC zone in the deployments this module ships to.
	_ "time/tzdata"

	"github.com/vislake/speed/go/authn/locales"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore/i18n"
)

// DefaultTimezone is the platform default timezone: the last tier of the
// display chain, applied when the account has chosen none. Stored values
// are IANA names; UTC itself is one of them, so "not chosen" and "chosen
// UTC" are represented the same way once resolved and no consumer needs a
// second spelling.
const DefaultTimezone = "UTC"

// timeZoneLocalPseudoZone is the one name time.LoadLocation answers without
// consulting the IANA database: the process's own local zone. It is
// deliberately not storable -- "Local" means whatever the machine running
// the account's rendering happens to be, so an account carrying it would
// see its times shift with the deployment's host, and no repair is
// possible from the stored value alone.
const timeZoneLocalPseudoZone = "Local"

// PreferencesInput carries an account's stored display preferences: the
// locale is the language backend-generated content addressed to the
// account renders in, and the timezone is the IANA zone its times are
// displayed in. Empty fields mean "not chosen yet" (the platform defaults
// en-US and UTC apply); see the users table's locale and timezone columns
// for the storage contract.
type PreferencesInput struct {
	Locale   string
	Timezone string
}

// PreferencesPatch describes a PARTIAL preference update: a nil field is
// absent from the request and leaves the stored value unchanged, while a
// non-nil field names the new value -- including a pointer to the empty
// string, which clears it. That three-state shape is the PATCH contract
// the API fragment documents; it is why the patch type uses pointers
// rather than treating "" as "unchanged".
type PreferencesPatch struct {
	Locale   *string
	Timezone *string
}

// Preferences returns the account's stored locale and timezone as the
// preferences surface reports them.
//
// The read runs under withConflictRetry: a transient contention conflict on
// it -- under SQLite a reader refused while another connection's write is
// committing -- would otherwise surface as an internal error on a plain
// read that a moment's wait resolves. Retrying a read is unconditionally
// safe: it reads whatever the row holds at the retry's own execution time.
func (s *Service) Preferences(ctx context.Context, userID string) (PreferencesInput, error) {
	var user *User
	err := withConflictRetry(func() error {
		found, findErr := s.users.FindByID(ctx, userID)
		user = found
		return findErr
	})
	if err != nil {
		return PreferencesInput{}, err
	}
	return PreferencesInput{Locale: user.Locale, Timezone: user.Timezone}, nil
}

// UpdatePreferences applies patch to the account's stored preferences and
// returns them as stored afterwards.
//
// Validation here is STRICT -- the PATCH contract's 400: a supplied locale
// must name a language this module ships and a supplied timezone must be a
// known IANA zone (the process-local pseudo-zone is refused), or the whole
// update is refused with authn.invalid_locale/authn.invalid_timezone and
// nothing is written. The registration path validates the same fields
// LENIENTLY instead (registrationLocale and registrationTimeZone): an
// unusable value there must not refuse the registration, and the API
// fragment's field descriptions state both contracts.
func (s *Service) UpdatePreferences(ctx context.Context, userID string, patch PreferencesPatch) (PreferencesInput, error) {
	if patch.Locale != nil {
		canonical, ok := canonicalLocale(*patch.Locale)
		if !ok {
			return PreferencesInput{}, ErrInvalidLocale
		}
		patch.Locale = &canonical
	}
	if patch.Timezone != nil {
		canonical, ok := canonicalTimezone(*patch.Timezone)
		if !ok {
			return PreferencesInput{}, ErrInvalidTimezone
		}
		patch.Timezone = &canonical
	}
	// The write runs under withConflictRetry: the update names only the
	// patched columns and sets them to fixed values, so a retried attempt
	// re-executes the identical statement against the row's current state
	// -- a conflict that outlasts the retry budget surfaces as the same
	// raw store failure this call returns for any other store-side error.
	var user *User
	err := withConflictRetry(func() error {
		updated, updateErr := s.users.UpdatePreferences(ctx, userID, patch.Locale, patch.Timezone)
		user = updated
		return updateErr
	})
	if err != nil {
		return PreferencesInput{}, err
	}
	return PreferencesInput{Locale: user.Locale, Timezone: user.Timezone}, nil
}

// supportedLocales returns the language codes this module ships locale
// files for, sorted -- derived from the embedded locale FS the module
// already owns and tests, never a hardcoded pair, so the module's language
// set has exactly one source: adding a language is one file here and
// nothing else. The listing cannot fail and cannot be empty for the
// build-time-fixed embed.FS the module ships (the locales package's own
// //go:embed set), which is why callers read the answer as the shipped set
// without an error arm.
func supportedLocales() []string {
	entries, err := fs.ReadDir(locales.FS, ".")
	if err != nil {
		return nil
	}
	codes := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".toml") {
			continue
		}
		codes = append(codes, strings.TrimSuffix(name, ".toml"))
	}
	slices.Sort(codes)
	return codes
}

// canonicalLocale returns the storable form of a caller-supplied locale:
// the trimmed value when it names a language this module ships, "" for an
// empty (cleared or unset) value, and ok=false for any other value. The
// empty case reports ok=true because empty is a legitimate stored state --
// "not chosen yet" -- not a refusal; the two write paths decide what
// ok=false means for them (registration stores empty, PATCH refuses).
//
// Matching is exact and case-sensitive against the canonical file-name
// spellings: a stored locale is compared by string equality along every
// chain that reads it (User.Locale, the locale files' own names), so
// admitting a non-canonical spelling here would store a value no renderer
// resolves.
func canonicalLocale(locale string) (string, bool) {
	locale = strings.TrimSpace(locale)
	if locale == "" {
		return "", true
	}
	if slices.Contains(supportedLocales(), locale) {
		return locale, true
	}
	return "", false
}

// canonicalTimezone returns the storable form of a caller-supplied
// timezone name: the trimmed value when the IANA database knows it, "" for
// an empty (cleared or unset) value, and ok=false for anything else --
// including the process-local pseudo-zone and any name longer than the
// column the value would be written to.
func canonicalTimezone(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", true
	}
	if name == timeZoneLocalPseudoZone {
		return "", false
	}
	if _, err := time.LoadLocation(name); err != nil {
		return "", false
	}
	if utf8.RuneCountInString(name) > timezoneColumnWidth {
		return "", false
	}
	return name, true
}

// registrationLocale applies the registration-time locale chain to the
// value the account's creator supplied -- the registration body's locale
// for a direct sign-up, the provider-reported profile locale for a social
// one: the declared value when it names a shipped language, else the
// request's Accept-Language negotiated against the shipped languages, else
// empty ("not chosen yet"; the platform default en-US applies). A declared
// value that cannot be stored is SKIPPED rather than refused, per the
// registration path's lenient validation; the Accept-Language header is a
// transport channel only -- the frontend's own language chain resolves the
// language, and this call is the backend reading what that chain sent.
func registrationLocale(declared, acceptLanguage string) string {
	if locale, ok := canonicalLocale(declared); ok && locale != "" {
		return locale
	}
	locale, _ := i18n.Negotiate(acceptLanguage, supportedLocales())
	return locale
}

// TimeZoneResolver resolves a client IP address to an IANA timezone name,
// the registration chain's IP tier. It is declared here, next to the chain
// that reads it, the way MembershipReader is declared beside its own
// consumers: the signature is built from stdlib types only, so a host
// adapter for any geo-IP service satisfies it without this module learning
// which service runs. The module ships no implementation and the reference
// app wires none -- a real resolver needs a geo-IP database and its
// licence review -- so the tier is present in the chain, absent in
// practice, and recorded as such in this module's own docs.
//
// The seam is NOT fail-closed, deliberately, following the
// MembershipReader/UserAddressResolver precedent's inverse: a resolver
// that is nil, errors, or answers a name that is not a known IANA zone
// yields the empty string -- "not chosen yet" -- and the registration
// proceeds. A registration must not fail because an optional convenience
// tier was unavailable, and an empty timezone is a first-class state the
// account can later change; the failure worth avoiding is a WRONG stored
// zone, which the canonicalTimezone re-validation of the resolver's answer
// prevents.
type TimeZoneResolver interface {
	// TimeZoneForIP returns the IANA timezone name best associated with
	// ip, or an error when it cannot answer.
	TimeZoneForIP(ctx context.Context, ip string) (string, error)
}

// registrationTimeZone applies the registration-time timezone chain to the
// value the account's creator declared -- the registration form's browser
// report, or a provider's profile field for a social sign-up (none of the
// five shipped social channels reports one today): the declared value when
// it is a known IANA zone, else the IP-resolution seam's answer, else
// empty ("not chosen yet"; the platform default UTC applies). Lenient like
// registrationLocale: a declared value that cannot be stored is skipped,
// never a refusal, and the seam's own failure modes all degrade to the
// next tier rather than failing the registration.
//
// ip empty skips the seam outright: an unattributable request has no
// address to resolve, and consulting a resolver with the empty string
// would only produce the same empty answer through a wasted round trip.
func (s *Service) registrationTimeZone(ctx context.Context, declared, ip string) string {
	if timezone, ok := canonicalTimezone(declared); ok && timezone != "" {
		return timezone
	}
	if s.timezoneResolver == nil || ip == "" {
		return ""
	}
	resolved, err := s.timezoneResolver.TimeZoneForIP(ctx, ip)
	if err != nil {
		obs.FromContext(ctx).Warn("timezone resolution failed during registration; storing no timezone",
			"ip", ip, "error", err)
		return ""
	}
	timezone, ok := canonicalTimezone(resolved)
	if !ok {
		obs.FromContext(ctx).Warn("timezone resolver answered a name that is not a known IANA zone; storing no timezone",
			"ip", ip, "timezone", resolved)
		return ""
	}
	return timezone
}
