package authn

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/vislake/speed/go/authn/locales"
	"github.com/vislake/speed/go/pkgcore/i18n"
)

// TestErrorCatalog_EveryCodeHasBothLocales is the i18n rule enforced where it
// can actually be enforced: in code, against the merged catalog built from
// the embedded files this module ships through the same i18n.Builder the
// assembly drives.
//
// tools/check_i18n_keys.py already proves the two files carry identical key
// sets, and pkgcore/i18n's Builder.AddModule fails a bootstrap when they do
// not. Neither of those notices the case that matters most here: an error
// code added to errors.go and to NEITHER file. A client receiving such a code
// has nothing to render and falls back to showing the raw code to a user.
func TestErrorCatalog_EveryCodeHasBothLocales(t *testing.T) {
	t.Parallel()

	catalog := newAuthnCatalog(t)
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
// tell it is dead. The census runs through the mechanism the assembly uses:
// for each language, the module's real file is merged beside a synthetic
// file for the other language carrying EXACTLY the ids this package claims
// (errorCodes plus nonErrorMessageIDs above), so AddModule's own key-parity
// rule -- which compares both directions and names every differing id in its
// error -- is the comparison. An id shipped but unclaimed surfaces as a real
// id the synthetic file lacks; a claimed id with no text to render surfaces
// as a synthetic id the real file lacks. Either drift fails here;
// nonErrorMessageIDs is the deliberate, named exception list -- anything
// else undeclared is still flagged.
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
// both-languages requirement onto the messages this module renders
// directly (see nonErrorMessageIDs), so a message added to one language and
// forgotten in the other is caught here exactly as an error code's message
// would be by TestErrorCatalog_EveryCodeHasBothLocales.
func TestLocales_NonErrorMessageIDsHaveBothLocales(t *testing.T) {
	t.Parallel()

	catalog := newAuthnCatalog(t)
	for _, language := range []string{"zh-CN", "en-US"} {
		t.Run(language, func(t *testing.T) {
			t.Parallel()

			for _, id := range nonErrorMessageIDs {
				if _, err := catalog.Lookup(language, id, map[string]any{}); err != nil {
					t.Errorf("message id %q has no %s message: %v", id, language, err)
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
