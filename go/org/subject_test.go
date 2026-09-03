package org

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// headerSubjectResolver resolves the caller from a request header,
// standing in for a real host's authn-backed resolver -- the exact
// structural-satisfaction technique SubjectResolver's own doc comment
// describes.
type headerSubjectResolver struct{ header string }

func (r headerSubjectResolver) Subject(req *http.Request) (string, bool) {
	userID := req.Header.Get(r.header)
	return userID, userID != ""
}

var _ SubjectResolver = headerSubjectResolver{}

func TestSubjectResolver_StructuralSatisfaction(t *testing.T) {
	resolver := headerSubjectResolver{header: "X-Test-User"}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := resolver.Subject(req); ok {
		t.Fatal("Subject reported ok=true with no header set")
	}

	req.Header.Set("X-Test-User", "u-42")
	userID, ok := resolver.Subject(req)
	if !ok || userID != "u-42" {
		t.Fatalf("Subject = (%q, %v), want (%q, true)", userID, ok, "u-42")
	}
}
