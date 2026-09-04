package admin

import (
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/vislake/speed/go/admin/locales"
)

// loadLocale decodes the embedded .toml file for language ("zh-CN" or
// "en-US") into a flat id -> text map.
func loadLocale(t *testing.T, language string) map[string]string {
	t.Helper()
	data, err := locales.FS.ReadFile(language + ".toml")
	if err != nil {
		t.Fatalf("read %s.toml: %v", language, err)
	}
	var messages map[string]string
	if err := toml.Unmarshal(data, &messages); err != nil {
		t.Fatalf("parse %s.toml: %v", language, err)
	}
	return messages
}

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

// TestErrorCatalog_EveryCodeHasBothLocales is the i18n rule enforced where
// it can actually be enforced: in code, against the embedded files this
// module ships. See go/authn/errors_test.go's identical test for the full
// rationale.
func TestErrorCatalog_EveryCodeHasBothLocales(t *testing.T) {
	t.Parallel()

	for _, language := range []string{"zh-CN", "en-US"} {
		t.Run(language, func(t *testing.T) {
			messages := loadLocale(t, language)
			for _, code := range errorCodes {
				if _, ok := messages[code]; !ok {
					t.Errorf("error code %q has no %s message; every code must ship text in both languages", code, language)
				}
			}
		})
	}
}

// TestLocales_CarryNoMessageWithoutACode is the other direction: a message
// id nothing returns is dead weight that survives forever because nobody
// can tell it is dead. nonErrorMessageIDs above is the deliberate, named
// exception list.
func TestLocales_CarryNoMessageWithoutACode(t *testing.T) {
	t.Parallel()

	known := make(map[string]bool, len(errorCodes)+len(nonErrorMessageIDs))
	for _, code := range errorCodes {
		known[code] = true
	}
	for _, id := range nonErrorMessageIDs {
		known[id] = true
	}

	for _, language := range []string{"zh-CN", "en-US"} {
		for id := range loadLocale(t, language) {
			if !known[id] {
				t.Errorf("%s carries message %q, which no error code in errors.go returns", language, id)
			}
		}
	}
}

// TestLocales_NonErrorMessageIDsHaveBothLocales pins the same
// both-languages requirement onto the notification templates
// (nonErrorMessageIDs), so a template added to one language and forgotten
// in the other is caught here exactly as an error code's message would be.
func TestLocales_NonErrorMessageIDsHaveBothLocales(t *testing.T) {
	t.Parallel()

	for _, language := range []string{"zh-CN", "en-US"} {
		t.Run(language, func(t *testing.T) {
			messages := loadLocale(t, language)
			for _, id := range nonErrorMessageIDs {
				if _, ok := messages[id]; !ok {
					t.Errorf("template id %q has no %s message", id, language)
				}
			}
		})
	}
}
