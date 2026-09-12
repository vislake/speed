package admin

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/vislake/speed/go/admin/locales"
	"github.com/vislake/speed/go/pkgcore/i18n"
)

// nonErrorMessageIDs lists locale message ids that are not an
// apperr.Error.Code -- the admin.impersonation_started notification
// type's bilingual templates and directory description, mirroring
// go/authn/errors_test.go's identical nonErrorMessageIDs exception list.
var nonErrorMessageIDs = []string{
	NotificationTypeImpersonationStarted + ".in_app.title",
	NotificationTypeImpersonationStarted + ".in_app.body",
	NotificationTypeImpersonationStarted + ".email.subject",
	NotificationTypeImpersonationStarted + ".email.body_text",
	NotificationTypeImpersonationStarted + ".description",
}

// newAdminCatalog builds admin's REAL locale bundle through the same
// i18n.Builder the assembly uses, so the tests render from the exact text
// the shipped files carry: a code the catalog cannot render fails here, in
// the test, rather than in production.
func newAdminCatalog(t *testing.T) *i18n.Catalog {
	t.Helper()

	builder := i18n.NewBuilder()
	if err := builder.AddModule(moduleName, locales.FS); err != nil {
		t.Fatalf("build the admin message catalog: %v", err)
	}
	return builder.Build()
}

// TestErrorCatalog_EveryCodeHasBothLocales is the i18n rule enforced where
// it can actually be enforced: in code, against the merged catalog built
// from the embedded files this module ships through the same i18n.Builder
// the assembly drives. See go/authn/errors_test.go's identical test for the
// full rationale.
func TestErrorCatalog_EveryCodeHasBothLocales(t *testing.T) {
	t.Parallel()

	catalog := newAdminCatalog(t)
	for _, language := range []string{"zh-CN", "en-US"} {
		t.Run(language, func(t *testing.T) {
			t.Parallel()

			for _, code := range errorCodes {
				message, err := catalog.Lookup(language, code, map[string]any{})
				if err != nil {
					t.Errorf("error code %q has no %s message: %v; every code must ship text in both languages", code, language, err)
					continue
				}
				if message == "" {
					t.Errorf("error code %q renders as an empty %s message", code, language)
				}
			}
		})
	}
}

// TestLocales_CarryNoMessageWithoutACode is the other direction: a message
// id nothing returns is dead weight that survives forever because nobody
// can tell it is dead. The census runs through the mechanism the assembly
// uses: for each language, the module's real file is merged beside a
// synthetic file for the other language carrying EXACTLY the ids this
// package claims (errorCodes plus nonErrorMessageIDs above), so AddModule's
// own key-parity rule -- which compares both directions and names every
// differing id in its error -- is the comparison. An id shipped but
// unclaimed surfaces as a real id the synthetic file lacks; a claimed id
// with no text to render surfaces as a synthetic id the real file lacks.
// Either drift fails here; nonErrorMessageIDs is the deliberate, named
// exception list -- anything else undeclared is still flagged.
func TestLocales_CarryNoMessageWithoutACode(t *testing.T) {
	t.Parallel()

	claimed := make([]string, 0, len(errorCodes)+len(nonErrorMessageIDs))
	claimed = append(claimed, errorCodes...)
	claimed = append(claimed, nonErrorMessageIDs...)

	for _, language := range []string{"zh-CN", "en-US"} {
		t.Run(language, func(t *testing.T) {
			t.Parallel()

			other := "en-US"
			if language == other {
				other = "zh-CN"
			}
			real, err := locales.FS.ReadFile(language + ".toml")
			if err != nil {
				t.Fatalf("read the embedded %s locale: %v", language, err)
			}
			fsys := fstest.MapFS{
				language + ".toml": {Data: real},
				other + ".toml":    {Data: censusLocaleFile(claimed)},
			}
			builder := i18n.NewBuilder()
			if err := builder.AddModule(moduleName, fsys); err != nil {
				t.Errorf("the %s id set drifted from the ids this package claims: %v", language, err)
			}
		})
	}
}

// censusLocaleFile renders ids as one flat locale file of placeholder
// messages, the exact shape the locale-file contract prescribes: quoted
// top-level ids, non-empty string values, every id inside the module's own
// prefix.
func censusLocaleFile(ids []string) []byte {
	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, "%q = \"census\"\n", id)
	}
	return []byte(b.String())
}

// TestLocales_NonErrorMessageIDsHaveBothLocales pins the same
// both-languages requirement onto the notification templates
// (nonErrorMessageIDs), so a template added to one language and forgotten
// in the other is caught here exactly as an error code's message would be
// by TestErrorCatalog_EveryCodeHasBothLocales.
func TestLocales_NonErrorMessageIDsHaveBothLocales(t *testing.T) {
	t.Parallel()

	catalog := newAdminCatalog(t)
	for _, language := range []string{"zh-CN", "en-US"} {
		t.Run(language, func(t *testing.T) {
			t.Parallel()

			for _, id := range nonErrorMessageIDs {
				if _, err := catalog.Lookup(language, id, map[string]any{}); err != nil {
					t.Errorf("template id %q has no %s message: %v", id, language, err)
				}
			}
		})
	}
}
