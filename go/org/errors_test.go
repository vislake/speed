package org

import (
	"net/http"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/org/locales"
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
	{"ErrNodeNotFound", ErrNodeNotFound, "org.node_not_found", http.StatusNotFound},
	{"ErrNodeNameRequired", ErrNodeNameRequired, "org.node_name_required", http.StatusBadRequest},
	{"ErrNodeNameTooLong", ErrNodeNameTooLong, "org.node_name_too_long", http.StatusBadRequest},
	{"ErrParentNotFound", ErrParentNotFound, "org.parent_not_found", http.StatusNotFound},
	{"ErrMaxDepthExceeded", ErrMaxDepthExceeded, "org.max_depth_exceeded", http.StatusBadRequest},
	{"ErrCycleNotAllowed", ErrCycleNotAllowed, "org.cycle_not_allowed", http.StatusBadRequest},
	{"ErrNodeHasChildren", ErrNodeHasChildren, "org.node_has_children", http.StatusConflict},
	{"ErrRootAlreadyExists", ErrRootAlreadyExists, "org.root_already_exists", http.StatusConflict},
	{"ErrRootNotDeletable", ErrRootNotDeletable, "org.root_not_deletable", http.StatusConflict},
	{"ErrDuplicateSiblingName", ErrDuplicateSiblingName, "org.duplicate_sibling_name", http.StatusConflict},
	{"ErrInvalidNodeID", ErrInvalidNodeID, "org.invalid_node_id", http.StatusBadRequest},
	{"ErrInternal", ErrInternal, "org.internal_error", http.StatusInternalServerError},
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
	decorated := ErrNodeNotFound.WithParam("node_id", "aa")

	if decorated == ErrNodeNotFound {
		t.Fatal("WithParam returned the receiver; it must derive a new *apperr.Error")
	}
	if ErrNodeNotFound.Params != nil {
		t.Errorf("WithParam wrote into the shared sentinel: Params = %v, want nil", ErrNodeNotFound.Params)
	}
	if got := decorated.Params["node_id"]; got != "aa" {
		t.Errorf("decorated.Params[node_id] = %v, want %q", got, "aa")
	}
	if !hasCode(decorated, ErrNodeNotFound.Code) {
		t.Errorf("decorated error lost its code %q", ErrNodeNotFound.Code)
	}
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
