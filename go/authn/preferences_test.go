package authn

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/vislake/speed/go/authn/locales"
)

// fakeTimeZoneResolver is the unit double for the TimeZoneResolver seam:
// a fixed answer per IP, or one error for every call. A test that needs
// "wired but answering" uses answers; one that needs "wired but broken"
// uses err.
type fakeTimeZoneResolver struct {
	answers map[string]string
	err     error
}

func (r fakeTimeZoneResolver) TimeZoneForIP(_ context.Context, ip string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	return r.answers[ip], nil
}

// TestSupportedLocales_MatchesTheShippedLocaleFiles pins the single-source
// claim supportedLocales makes: the list it derives must be exactly the
// .toml files the embedded locale FS ships, so a language added as a file
// (and nowhere else) is picked up by validation, negotiation and the SMS
// loader at once.
func TestSupportedLocales_MatchesTheShippedLocaleFiles(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(locales.FS, ".")
	if err != nil {
		t.Fatalf("ReadDir(locales.FS) error = %v", err)
	}
	want := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".toml") {
			want = append(want, strings.TrimSuffix(entry.Name(), ".toml"))
		}
	}
	slices.Sort(want)

	got := supportedLocales()
	if !slices.Equal(got, want) {
		t.Errorf("supportedLocales() = %v, want the shipped locale files %v", got, want)
	}
	if !slices.Contains(got, DefaultLocale) {
		t.Errorf("supportedLocales() = %v, want it to carry the platform default language %q", got, DefaultLocale)
	}
}

func TestCanonicalLocale_Table(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"empty is the clearing value", "", "", true},
		{"blank trims to the clearing value", "  ", "", true},
		{"shipped locale in canonical form", "zh-CN", "zh-CN", true},
		{"the other shipped locale", "en-US", "en-US", true},
		{"surrounding whitespace is trimmed", "  en-US ", "en-US", true},
		{"non-canonical case is not a shipped spelling", "zh-cn", "", false},
		{"unshipped language is refused", "de-DE", "", false},
		{"language prefix alone is not a stored value", "zh", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := canonicalLocale(tc.input)
			if got != tc.want || ok != tc.ok {
				t.Errorf("canonicalLocale(%q) = (%q, %v), want (%q, %v)", tc.input, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestCanonicalTimezone_Table(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"empty is the clearing value", "", "", true},
		{"blank trims to the clearing value", "  ", "", true},
		{"UTC is storable", "UTC", "UTC", true},
		{"a zone city", "Asia/Shanghai", "Asia/Shanghai", true},
		{"a fixed-offset zone", "Etc/GMT+12", "Etc/GMT+12", true},
		{"whitespace is trimmed", "  Europe/Berlin ", "Europe/Berlin", true},
		{"the process-local pseudo-zone is refused", "Local", "", false},
		{"an unknown zone name is refused", "Mars/Olympus", "", false},
		{"an empty-looking path is refused", "/", "", false},
		{"A zone name over the column width is refused", "Area/" + strings.Repeat("x", timezoneColumnWidth), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := canonicalTimezone(tc.input)
			if got != tc.want || ok != tc.ok {
				t.Errorf("canonicalTimezone(%q) = (%q, %v), want (%q, %v)", tc.input, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestRegistrationLocale_Chain drives the registration locale chain's tier
// order: a declared value that names a shipped language wins outright, a
// declared value that cannot be stored is SKIPPED (lenient -- never a
// refusal), the header answers next, and nothing anywhere leaves the empty
// stored value whose effective default is the platform's.
func TestRegistrationLocale_Chain(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		declared       string
		acceptLanguage string
		want           string
	}{
		{"declared value wins over the header", "zh-CN", "en-US", "zh-CN"},
		{"declared value is trimmed", " en-US ", "", "en-US"},
		{"unusable declared value falls to the header", "de-DE", "zh-CN,en;q=0.8", "zh-CN"},
		{"header language prefix matches", "", "en", "en-US"},
		{"header with no shipped language stores empty", "", "fr-FR, de;q=0.5", ""},
		{"no declared value and no header stores empty", "", "", ""},
		{"unusable declared value and unusable header store empty", "de-DE", "fr-FR", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := registrationLocale(tc.declared, tc.acceptLanguage); got != tc.want {
				t.Errorf("registrationLocale(%q, %q) = %q, want %q", tc.declared, tc.acceptLanguage, got, tc.want)
			}
		})
	}
}

// TestService_Register_ResolvesInitialPreferencesAtTheInitialization pins
// the registration write path: both fields land on the row's single insert
// (no second write), the declared tiers win when valid, and each chain's
// lenient behavior holds end to end -- an unusable declared value never
// refuses the registration, it stores empty (or the next tier's answer).
func TestService_Register_ResolvesInitialPreferencesAtTheInitialization(t *testing.T) {
	t.Parallel()

	t.Run("valid declared values are stored", func(t *testing.T) {
		t.Parallel()
		f := newServiceFixture(t)
		user, err := f.svc.Register(t.Context(), RegisterInput{
			Email: "prefs-valid@example.com", Password: testPassword, DisplayName: "Prefs",
			Locale: "zh-CN", AcceptLanguage: "en-US", Timezone: "Asia/Shanghai",
		})
		if err != nil {
			t.Fatalf("Register() error = %v", err)
		}
		if user.Locale != "zh-CN" {
			t.Errorf("stored Locale = %q, want the declared %q", user.Locale, "zh-CN")
		}
		if user.Timezone != "Asia/Shanghai" {
			t.Errorf("stored Timezone = %q, want the declared %q", user.Timezone, "Asia/Shanghai")
		}
	})

	t.Run("unusable declared locale is lenient and the header decides", func(t *testing.T) {
		t.Parallel()
		f := newServiceFixture(t)
		user, err := f.svc.Register(t.Context(), RegisterInput{
			Email: "prefs-lenient-locale@example.com", Password: testPassword, DisplayName: "Prefs",
			Locale: "de-DE", AcceptLanguage: "zh-CN",
		})
		if err != nil {
			t.Fatalf("Register() error = %v (an unusable locale must never refuse a registration)", err)
		}
		if user.Locale != "zh-CN" {
			t.Errorf("stored Locale = %q, want the header's %q", user.Locale, "zh-CN")
		}
	})

	t.Run("unusable values everywhere store empty", func(t *testing.T) {
		t.Parallel()
		f := newServiceFixture(t)
		user, err := f.svc.Register(t.Context(), RegisterInput{
			Email: "prefs-lenient-empty@example.com", Password: testPassword, DisplayName: "Prefs",
			Locale: "de-DE", AcceptLanguage: "fr-FR", Timezone: "Mars/Olympus",
		})
		if err != nil {
			t.Fatalf("Register() error = %v", err)
		}
		if user.Locale != "" || user.Timezone != "" {
			t.Errorf("stored preferences = (%q, %q), want both empty (the platform defaults en-US/UTC apply)", user.Locale, user.Timezone)
		}
		// The empty stored values are a real state, not a dropped write:
		// the preferences read must answer them as such.
		prefs, err := f.svc.Preferences(t.Context(), user.ID)
		if err != nil {
			t.Fatalf("Preferences() error = %v", err)
		}
		if prefs.Locale != "" || prefs.Timezone != "" {
			t.Errorf("Preferences() = %+v, want the stored empties", prefs)
		}
	})
}

// TestService_Register_TimeZoneResolverTier pins the timezone chain's IP
// tier: it answers only when the declared value could not be stored, its
// answers are re-validated (an unknown name degrades to empty), and its
// failure modes -- nil seam, error, unusable answer -- all store empty
// rather than failing or storing a wrong zone.
func TestService_Register_TimeZoneResolverTier(t *testing.T) {
	t.Parallel()

	t.Run("resolver answers when nothing was declared", func(t *testing.T) {
		t.Parallel()
		f := newServiceFixture(t, WithTimeZoneResolver(fakeTimeZoneResolver{
			answers: map[string]string{"203.0.113.7": "Asia/Tokyo"},
		}))
		user, err := f.svc.Register(t.Context(), RegisterInput{
			Email: "tz-resolved@example.com", Password: testPassword, DisplayName: "TZ", IP: "203.0.113.7",
		})
		if err != nil {
			t.Fatalf("Register() error = %v", err)
		}
		if user.Timezone != "Asia/Tokyo" {
			t.Errorf("stored Timezone = %q, want the resolver's %q", user.Timezone, "Asia/Tokyo")
		}
	})

	t.Run("a declared value outranks the resolver", func(t *testing.T) {
		t.Parallel()
		f := newServiceFixture(t, WithTimeZoneResolver(fakeTimeZoneResolver{
			answers: map[string]string{"203.0.113.7": "Asia/Tokyo"},
		}))
		user, err := f.svc.Register(t.Context(), RegisterInput{
			Email: "tz-declared@example.com", Password: testPassword, DisplayName: "TZ",
			Timezone: "Europe/Berlin", IP: "203.0.113.7",
		})
		if err != nil {
			t.Fatalf("Register() error = %v", err)
		}
		if user.Timezone != "Europe/Berlin" {
			t.Errorf("stored Timezone = %q, want the declared %q", user.Timezone, "Europe/Berlin")
		}
	})

	t.Run("unusable declared value falls through to the resolver", func(t *testing.T) {
		t.Parallel()
		f := newServiceFixture(t, WithTimeZoneResolver(fakeTimeZoneResolver{
			answers: map[string]string{"203.0.113.7": "Asia/Tokyo"},
		}))
		user, err := f.svc.Register(t.Context(), RegisterInput{
			Email: "tz-lenient@example.com", Password: testPassword, DisplayName: "TZ",
			Timezone: "Mars/Olympus", IP: "203.0.113.7",
		})
		if err != nil {
			t.Fatalf("Register() error = %v (an unusable timezone must never refuse a registration)", err)
		}
		if user.Timezone != "Asia/Tokyo" {
			t.Errorf("stored Timezone = %q, want the resolver's %q", user.Timezone, "Asia/Tokyo")
		}
	})

	t.Run("resolver answering an unknown zone stores empty", func(t *testing.T) {
		t.Parallel()
		f := newServiceFixture(t, WithTimeZoneResolver(fakeTimeZoneResolver{
			answers: map[string]string{"203.0.113.7": "Mars/Olympus"},
		}))
		user, err := f.svc.Register(t.Context(), RegisterInput{
			Email: "tz-bad-answer@example.com", Password: testPassword, DisplayName: "TZ", IP: "203.0.113.7",
		})
		if err != nil {
			t.Fatalf("Register() error = %v", err)
		}
		if user.Timezone != "" {
			t.Errorf("stored Timezone = %q, want empty for an unvalidatable resolver answer", user.Timezone)
		}
	})

	t.Run("resolver error stores empty without failing", func(t *testing.T) {
		t.Parallel()
		f := newServiceFixture(t, WithTimeZoneResolver(fakeTimeZoneResolver{err: errors.New("geo source down")}))
		user, err := f.svc.Register(t.Context(), RegisterInput{
			Email: "tz-error@example.com", Password: testPassword, DisplayName: "TZ", IP: "203.0.113.7",
		})
		if err != nil {
			t.Fatalf("Register() error = %v (the IP tier is not fail-closed)", err)
		}
		if user.Timezone != "" {
			t.Errorf("stored Timezone = %q, want empty", user.Timezone)
		}
	})

	t.Run("no resolver wired stores empty", func(t *testing.T) {
		t.Parallel()
		f := newServiceFixture(t)
		user, err := f.svc.Register(t.Context(), RegisterInput{
			Email: "tz-unwired@example.com", Password: testPassword, DisplayName: "TZ", IP: "203.0.113.7",
		})
		if err != nil {
			t.Fatalf("Register() error = %v", err)
		}
		if user.Timezone != "" {
			t.Errorf("stored Timezone = %q, want empty", user.Timezone)
		}
	})
}

// TestService_Preferences_RoundTripAndPartialUpdate drives the preferences
// surface's core semantics: the read reflects what was stored; an update
// with one field absent leaves the other alone; a cleared field (empty
// string) is stored empty and read back as such; and both invalid values
// are refused with their own coded error while the stored pair stays
// untouched.
func TestService_Preferences_RoundTripAndPartialUpdate(t *testing.T) {
	t.Parallel()
	f := newServiceFixture(t)
	user := f.registerUser(t, "prefs-service@example.com", testTenantA)

	prefs, err := f.svc.Preferences(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("Preferences() error = %v", err)
	}
	if prefs.Locale != "" || prefs.Timezone != "" {
		t.Fatalf("fresh account preferences = %+v, want both empty", prefs)
	}

	locale := "zh-CN"
	prefs, err = f.svc.UpdatePreferences(t.Context(), user.ID, PreferencesPatch{Locale: &locale})
	if err != nil {
		t.Fatalf("UpdatePreferences(locale) error = %v", err)
	}
	if prefs.Locale != "zh-CN" || prefs.Timezone != "" {
		t.Fatalf("after locale update, preferences = %+v, want locale set and timezone untouched", prefs)
	}

	timezone := "  Asia/Shanghai "
	prefs, err = f.svc.UpdatePreferences(t.Context(), user.ID, PreferencesPatch{Timezone: &timezone})
	if err != nil {
		t.Fatalf("UpdatePreferences(timezone) error = %v", err)
	}
	if prefs.Locale != "zh-CN" || prefs.Timezone != "Asia/Shanghai" {
		t.Fatalf("after timezone update, preferences = %+v, want the trimmed timezone and the locale unchanged", prefs)
	}

	// A read goes through the row, not any in-memory state: re-reading
	// must answer the same pair.
	reRead, err := f.svc.Preferences(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("Preferences() after updates error = %v", err)
	}
	if reRead != prefs {
		t.Fatalf("re-read preferences = %+v, want %+v", reRead, prefs)
	}

	cleared := ""
	prefs, err = f.svc.UpdatePreferences(t.Context(), user.ID, PreferencesPatch{Timezone: &cleared})
	if err != nil {
		t.Fatalf("UpdatePreferences(clear timezone) error = %v", err)
	}
	if prefs.Timezone != "" || prefs.Locale != "zh-CN" {
		t.Fatalf("after clearing the timezone, preferences = %+v, want an empty timezone and the locale kept", prefs)
	}

	// Refusals: the whole update is refused and NOTHING is written.
	badLocale := "de-DE"
	if _, err := f.svc.UpdatePreferences(t.Context(), user.ID, PreferencesPatch{Locale: &badLocale, Timezone: &timezone}); !errors.Is(err, ErrInvalidLocale) {
		t.Errorf("UpdatePreferences(unshipped locale) error = %v, want ErrInvalidLocale", err)
	}
	badTimezone := "Mars/Olympus"
	if _, err := f.svc.UpdatePreferences(t.Context(), user.ID, PreferencesPatch{Timezone: &badTimezone, Locale: &locale}); !errors.Is(err, ErrInvalidTimezone) {
		t.Errorf("UpdatePreferences(unknown timezone) error = %v, want ErrInvalidTimezone", err)
	}
	localPseudoZone := "Local"
	if _, err := f.svc.UpdatePreferences(t.Context(), user.ID, PreferencesPatch{Timezone: &localPseudoZone}); !errors.Is(err, ErrInvalidTimezone) {
		t.Errorf("UpdatePreferences(process-local pseudo-zone) error = %v, want ErrInvalidTimezone", err)
	}
	after, err := f.svc.Preferences(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("Preferences() after refusals error = %v", err)
	}
	if after.Locale != "zh-CN" || after.Timezone != "" {
		t.Errorf("preferences after refused updates = %+v, want the pre-refusal pair (a refused update must write nothing)", after)
	}

	// A patch naming no field is a read: it answers the stored pair.
	if prefs, err = f.svc.UpdatePreferences(t.Context(), user.ID, PreferencesPatch{}); err != nil {
		t.Fatalf("UpdatePreferences(empty patch) error = %v", err)
	}
	if prefs.Locale != "zh-CN" || prefs.Timezone != "" {
		t.Errorf("empty-patch preferences = %+v, want the stored pair", prefs)
	}
}

// TestService_UpdatePreferences_UnknownUser_ReturnsNotFound pins the
// repository's RowsAffected=0 translation: a patch against an id no row
// carries is a not-found, not a silent success.
func TestService_UpdatePreferences_UnknownUser_ReturnsNotFound(t *testing.T) {
	t.Parallel()
	f := newServiceFixture(t)
	locale := "en-US"
	if _, err := f.svc.UpdatePreferences(t.Context(), "no-such-user", PreferencesPatch{Locale: &locale}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdatePreferences(unknown user) error = %v, want ErrNotFound", err)
	}
	if _, err := f.svc.Preferences(t.Context(), "no-such-user"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Preferences(unknown user) error = %v, want ErrNotFound", err)
	}
}
