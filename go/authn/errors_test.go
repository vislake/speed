package authn

import (
	"net/http"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/vislake/speed/go/authn/locales"
)

// TestErrorCatalog_EveryCodeHasBothLocales is the i18n rule enforced where it
// can actually be enforced: in code, against the embedded files this module
// ships.
//
// tools/check_i18n_keys.py already proves the two files carry identical key
// sets, and pkgcore/i18n's Builder.AddModule fails a bootstrap when they do
// not. Neither of those notices the case that matters most here: an error
// code added to errors.go and to NEITHER file. A client receiving such a code
// has nothing to render and falls back to showing the raw code to a user.
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

// nonErrorMessageIDs lists locale message ids that are not an
// apperr.Error.Code: backend-generated content this module composes and
// sends directly, rather than returning as a structured API error for a
// client to resolve. smsVerificationCodeMessageID (verification.go) is the
// one example today -- the SMS body rendered for a phone-login code.
var nonErrorMessageIDs = []string{
	smsVerificationCodeMessageID,
}

// TestLocales_CarryNoMessageWithoutACode is the other direction: a message id
// nothing returns is dead weight that survives forever because nobody can
// tell it is dead. nonErrorMessageIDs above is the deliberate, named
// exception list -- anything else undeclared is still flagged.
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
// both-languages requirement onto the messages this module renders
// directly (see nonErrorMessageIDs), so a message added to one language and
// forgotten in the other is caught here exactly as an error code's message
// would be by TestErrorCatalog_EveryCodeHasBothLocales.
func TestLocales_NonErrorMessageIDsHaveBothLocales(t *testing.T) {
	t.Parallel()

	for _, language := range []string{"zh-CN", "en-US"} {
		t.Run(language, func(t *testing.T) {
			messages := loadLocale(t, language)
			for _, id := range nonErrorMessageIDs {
				if _, ok := messages[id]; !ok {
					t.Errorf("message id %q has no %s message", id, language)
				}
			}
		})
	}
}

// TestErrorCatalog_CodesArePrefixedWithTheModuleName pins the
// "<module>.<reason>" convention the whole error catalog and its generated
// documentation depend on.
func TestErrorCatalog_CodesArePrefixedWithTheModuleName(t *testing.T) {
	t.Parallel()

	for _, code := range errorCodes {
		if !strings.HasPrefix(code, moduleName+".") {
			t.Errorf("error code %q is not prefixed %q", code, moduleName+".")
		}
		if strings.ToLower(code) != code {
			t.Errorf("error code %q is not lower case", code)
		}
	}
}

// TestErrorCatalog_CodesAreUnique guards against a copy-paste that gives two
// distinct failures one code, which would make them indistinguishable to a
// client for good.
func TestErrorCatalog_CodesAreUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, len(errorCodes))
	for _, code := range errorCodes {
		if seen[code] {
			t.Errorf("error code %q appears twice in the catalog", code)
		}
		seen[code] = true
	}
}

// TestErrorCatalog_StatusCodesMatchTheirMeaning pins the HTTP status of every
// sentinel, because the status is as much a part of the API contract as the
// code and a client's retry logic keys on it.
func TestErrorCatalog_StatusCodesMatchTheirMeaning(t *testing.T) {
	t.Parallel()

	cases := map[string]int{
		ErrInvalidCredentials.Code:          http.StatusUnauthorized,
		ErrIdentifierRequired.Code:          http.StatusBadRequest,
		ErrInvalidEmail.Code:                http.StatusBadRequest,
		ErrInvalidPhone.Code:                http.StatusBadRequest,
		ErrEmailAlreadyRegistered.Code:      http.StatusConflict,
		ErrPhoneAlreadyRegistered.Code:      http.StatusConflict,
		ErrPasswordTooShort.Code:            http.StatusBadRequest,
		ErrPasswordTooLong.Code:             http.StatusBadRequest,
		ErrPasswordTooWeak.Code:             http.StatusBadRequest,
		ErrAuthenticationRequired.Code:      http.StatusUnauthorized,
		ErrTokenInvalid.Code:                http.StatusUnauthorized,
		ErrTokenExpired.Code:                http.StatusUnauthorized,
		ErrSessionRevoked.Code:              http.StatusUnauthorized,
		ErrRefreshTokenInvalid.Code:         http.StatusUnauthorized,
		ErrRefreshTokenReused.Code:          http.StatusUnauthorized,
		ErrTenantMembershipRequired.Code:    http.StatusForbidden,
		ErrTenantMembershipUnavailable.Code: http.StatusForbidden,
		ErrRevocationCheckFailed.Code:       http.StatusInternalServerError,
		ErrInternal.Code:                    http.StatusInternalServerError,
	}

	byCode := map[string]int{}
	for _, err := range []error{
		ErrInvalidCredentials, ErrIdentifierRequired, ErrInvalidEmail, ErrInvalidPhone,
		ErrEmailAlreadyRegistered, ErrPhoneAlreadyRegistered,
		ErrPasswordTooShort, ErrPasswordTooLong, ErrPasswordTooWeak,
		ErrAuthenticationRequired, ErrTokenInvalid, ErrTokenExpired, ErrSessionRevoked,
		ErrRefreshTokenInvalid, ErrRefreshTokenReused,
		ErrTenantMembershipRequired, ErrTenantMembershipUnavailable,
		ErrRevocationCheckFailed, ErrInternal,
	} {
		appErr, ok := asAppError(err)
		if !ok {
			t.Fatalf("%v is not an *apperr.Error", err)
		}
		byCode[appErr.Code] = appErr.Status
	}

	if len(byCode) != len(cases) {
		t.Fatalf("the sentinel list covers %d codes, the expectation table %d", len(byCode), len(cases))
	}
	for code, want := range cases {
		if got := byCode[code]; got != want {
			t.Errorf("%s status = %d, want %d", code, got, want)
		}
	}
}

// loadLocale reads one embedded locale file into a flat id -> text map.
func loadLocale(t *testing.T, language string) map[string]string {
	t.Helper()

	raw, err := locales.FS.ReadFile(language + ".toml")
	if err != nil {
		t.Fatalf("read the embedded %s locale: %v", language, err)
	}
	messages := map[string]string{}
	if err := toml.Unmarshal(raw, &messages); err != nil {
		t.Fatalf("parse the embedded %s locale: %v -- every entry must be one flat top-level message, with no grouping sections", language, err)
	}
	return messages
}
