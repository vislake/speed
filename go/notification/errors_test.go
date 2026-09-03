package notification

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/notification/locales"
)

// errorCatalog is every error this module exports, paired with the HTTP
// status its class implies. Adding an error without adding it here fails
// TestErrorCatalog_IsComplete below.
var errorCatalog = []struct {
	name string
	err  *apperr.Error
	code string
	want int
}{
	// The preference-matrix group and the module-wide internal error.
	{"ErrRecipientRequired", ErrRecipientRequired, "notification.recipient_required", http.StatusBadRequest},
	{"ErrTypeNotFound", ErrTypeNotFound, "notification.type_not_found", http.StatusNotFound},
	{"ErrPreferenceInvalidChannels", ErrPreferenceInvalidChannels, "notification.preference_invalid_channels", http.StatusBadRequest},
	{"ErrPreferenceOptoutNotAllowed", ErrPreferenceOptoutNotAllowed, "notification.preference_optout_not_allowed", http.StatusBadRequest},
	{"ErrInternal", ErrInternal, "notification.internal_error", http.StatusInternalServerError},

	// The consent-ledger group (contact.go).
	{"ErrContactNotFound", ErrContactNotFound, "notification.contact_not_found", http.StatusNotFound},
	{"ErrContactInvalidChannel", ErrContactInvalidChannel, "notification.contact_invalid_channel", http.StatusBadRequest},
	{"ErrContactInvalidAddress", ErrContactInvalidAddress, "notification.contact_invalid_address", http.StatusBadRequest},
	{"ErrContactCodeInvalid", ErrContactCodeInvalid, "notification.contact_code_invalid", http.StatusBadRequest},
	{"ErrContactCodeDeliveryFailed", ErrContactCodeDeliveryFailed, "notification.contact_code_delivery_failed", http.StatusInternalServerError},
	{"ErrContactUnsubscribed", ErrContactUnsubscribed, "notification.contact_unsubscribed", http.StatusConflict},
	{"ErrContactBounced", ErrContactBounced, "notification.contact_bounced", http.StatusConflict},
	{"ErrContactNotVerified", ErrContactNotVerified, "notification.contact_not_verified", http.StatusConflict},
	{"ErrContactRateLimited", ErrContactRateLimited, "notification.contact_rate_limited", http.StatusTooManyRequests},

	// The Register-time wiring group (module.go).
	{"ErrSMSSenderRequired", ErrSMSSenderRequired, "notification.sms_sender_required", http.StatusInternalServerError},
	{"ErrMailFromRequired", ErrMailFromRequired, "notification.mail_from_required", http.StatusInternalServerError},
	{"ErrContactEmailIndexerRequired", ErrContactEmailIndexerRequired, "notification.contact_email_indexer_required", http.StatusInternalServerError},
	{"ErrContactPhoneIndexerRequired", ErrContactPhoneIndexerRequired, "notification.contact_phone_indexer_required", http.StatusInternalServerError},
}

// TestErrorCatalog_IsComplete is what makes the promise at the top of
// errorCatalog true -- every exported Err* sentinel errors.go declares must
// appear in the table above, so a new error cannot be added without also
// gaining its status assertion and its two translations.
//
// It reads the source file rather than reflecting, because Go offers no way
// to enumerate a package's own variables at run time.
func TestErrorCatalog_IsComplete(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "errors.go", nil, 0)
	if err != nil {
		t.Fatalf("parse errors.go: %v", err)
	}

	catalogued := make(map[string]bool, len(errorCatalog))
	for _, tc := range errorCatalog {
		catalogued[tc.name] = true
	}

	declared := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				if !strings.HasPrefix(name.Name, "Err") {
					continue
				}
				declared++
				if !catalogued[name.Name] {
					t.Errorf("errors.go declares %s, which errorCatalog does not list", name.Name)
				}
			}
		}
	}
	if declared != len(errorCatalog) {
		t.Errorf("errors.go declares %d exported errors, errorCatalog lists %d", declared, len(errorCatalog))
	}
}

func TestErrorCatalog_CodesAndStatuses(t *testing.T) {
	seen := make(map[string]string, len(errorCatalog))
	for _, tc := range errorCatalog {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Code != tc.code {
				t.Errorf("%s.Code = %q, want %q", tc.name, tc.err.Code, tc.code)
			}
			if !strings.HasPrefix(tc.err.Code, moduleName+".") {
				t.Errorf("%s.Code = %q, want the %q. prefix the <module>.<reason> convention requires",
					tc.name, tc.err.Code, moduleName)
			}
			if tc.err.Status != tc.want {
				t.Errorf("%s.Status = %d, want %d", tc.name, tc.err.Status, tc.want)
			}
		})
		if other, dup := seen[tc.err.Code]; dup {
			t.Errorf("code %q is declared by both %s and %s", tc.err.Code, other, tc.name)
		}
		seen[tc.err.Code] = tc.name
	}
}

// TestErrorCatalog_EveryCodeHasBothTranslations is the in-code half of the
// i18n rule: new text ships in zh-CN AND en-US. tools/check_i18n_keys.py
// proves the two files agree with each other; only this test proves they
// actually cover the codes this module can return, which is the failure a
// user would experience as an untranslated error.
func TestErrorCatalog_EveryCodeHasBothTranslations(t *testing.T) {
	for _, language := range []string{"zh-CN", "en-US"} {
		raw, err := locales.FS.ReadFile(language + ".toml")
		if err != nil {
			t.Fatalf("read %s.toml: %v", language, err)
		}
		bundle := string(raw)
		for _, tc := range errorCatalog {
			if !strings.Contains(bundle, `"`+tc.err.Code+`"`) {
				t.Errorf("%s.toml has no entry for %q", language, tc.err.Code)
			}
		}
	}
}

// TestErrorCatalog_WithParamDerivesRatherThanMutates guards the sharing
// contract these package-level sentinels rely on: decorating one must not
// write into the value every other request is holding.
func TestErrorCatalog_WithParamDerivesRatherThanMutates(t *testing.T) {
	decorated := ErrPreferenceInvalidChannels.
		WithParam("type_key", "note.shared").
		WithParam("channels", "push")

	if decorated == ErrPreferenceInvalidChannels {
		t.Fatal("WithParam returned the receiver; it must derive a new *apperr.Error")
	}
	if ErrPreferenceInvalidChannels.Params != nil {
		t.Errorf("WithParam wrote into the shared sentinel: Params = %v, want nil", ErrPreferenceInvalidChannels.Params)
	}
	if got := decorated.Params["type_key"]; got != "note.shared" {
		t.Errorf("decorated.Params[type_key] = %v, want %q", got, "note.shared")
	}
	if got := decorated.Params["channels"]; got != "push" {
		t.Errorf("decorated.Params[channels] = %v, want %q", got, "push")
	}
	assertCode(t, decorated, ErrPreferenceInvalidChannels.Code)
}

// assertCode fails t unless err carries the given apperr code. Codes are
// compared rather than pointers because WithParam and WithCause derive a new
// *apperr.Error every time (see apperr's own doc comment).
func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v is not an *apperr.Error, want code %s", err, want)
	}
	if appErr.Code != want {
		t.Fatalf("error code = %s, want %s", appErr.Code, want)
	}
}
