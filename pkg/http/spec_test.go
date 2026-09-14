package http

import (
	"errors"
	"strings"
	"testing"

	"github.com/vislake/speed/pkg/core"
)

// validFragment is the smallest document that passes the shape check: an
// object carrying the openapi field.
const validFragment = `{"openapi":"3.1.0","paths":{"/things":{}}}`

// badFragments are the five shapes the design rules out, one per class. Each
// is a startup failure, and the caller tells them from every other startup
// failure by ErrInvalidSpec.
func TestSpecRejectsEmptyDocument(t *testing.T) {
	for _, document := range []string{"", "   \n\t"} {
		err := validateSpec("billing", Spec{Endpoint: "public", Document: []byte(document)})
		assertInvalidSpec(t, err, "billing", "empty")
	}
	err := validateSpec("billing", Spec{Endpoint: "public"})
	assertInvalidSpec(t, err, "billing", "empty")
}

func TestSpecRejectsNonUTF8(t *testing.T) {
	// The illegal bytes sit inside a JSON string, which is where
	// encoding/json replaces them with U+FFFD instead of failing. A check
	// that leans on the decode alone lets this through and the corruption
	// surfaces in whoever merges the fragments.
	document := []byte(`{"openapi":"3.1.0","info":{"title":"` + "\xff\xfe" + `"}}`)
	err := validateSpec("billing", Spec{Endpoint: "public", Document: document})
	assertInvalidSpec(t, err, "billing", "UTF-8")
}

func TestSpecRejectsMalformedJSON(t *testing.T) {
	err := validateSpec("billing", Spec{Endpoint: "public", Document: []byte(`{"openapi":`)})
	assertInvalidSpec(t, err, "billing", "JSON")
}

func TestSpecRejectsNonObjectTopLevel(t *testing.T) {
	for _, document := range []string{`["openapi"]`, `"3.1.0"`, `42`, `null`} {
		err := validateSpec("billing", Spec{Endpoint: "public", Document: []byte(document)})
		assertInvalidSpec(t, err, "billing", "object at the top level")
	}
}

func TestSpecRejectsMissingOpenAPIField(t *testing.T) {
	err := validateSpec("billing", Spec{Endpoint: "public", Document: []byte(`{"paths":{"/things":{}}}`)})
	assertInvalidSpec(t, err, "billing", "openapi")
}

// TestSpecAcceptsAWellShapedFragment is the positive half: the five refusals
// above would all hold in an implementation that refuses everything.
func TestSpecAcceptsAWellShapedFragment(t *testing.T) {
	if err := validateSpec("billing", Spec{Endpoint: "public", Document: []byte(validFragment)}); err != nil {
		t.Fatalf("a well-shaped fragment was refused: %v", err)
	}
}

// TestSpecErrorNamesDeclaringModule pins the fixing action. A module declares
// its fragment with //go:embed, so the document alone says nothing about where
// to go and fix it.
func TestSpecErrorNamesDeclaringModule(t *testing.T) {
	err := validateSpecs([]core.Resource[Spec]{
		{Module: "billing", Value: Spec{Endpoint: "public", Document: []byte(`{"paths":{}}`)}},
		{Module: "tenancy", Value: Spec{Endpoint: "public", Document: []byte(`nonsense`)}},
		{Module: "healthy", Value: Spec{Endpoint: "public", Document: []byte(validFragment)}},
	})
	if err == nil {
		t.Fatal("two unusable fragments produced no failure")
	}
	if !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("the failure does not match ErrInvalidSpec: %v", err)
	}
	for _, module := range []string{"billing", "tenancy"} {
		if !strings.Contains(err.Error(), module) {
			t.Errorf("the failure %q does not name %q, whose fragment is unusable", err, module)
		}
	}
	if strings.Contains(err.Error(), "healthy") {
		t.Errorf("the failure %q names healthy, whose fragment is fine", err)
	}
}

// TestSpecUnknownEndpointIsNotAFailure pins one of the design's accepted
// costs. Spec and the actual registrations are separately maintained facts and
// are allowed to drift; a check here would turn "this API is not enabled in
// this assembly" into a startup failure.
func TestSpecUnknownEndpointIsNotAFailure(t *testing.T) {
	err := validateSpecs([]core.Resource[Spec]{
		{Module: "billing", Value: Spec{Endpoint: "an-endpoint-nobody-declared", Document: []byte(validFragment)}},
	})
	if err != nil {
		t.Fatalf("a fragment pointing at an undeclared endpoint failed the startup: %v. "+
			"The design accepts that Spec and the registrations drift apart", err)
	}
}

// TestSpecsWithNothingDeclaredIsNotAFailure covers the ordinary host that
// declares no fragment at all.
func TestSpecsWithNothingDeclaredIsNotAFailure(t *testing.T) {
	if err := validateSpecs(nil); err != nil {
		t.Fatalf("an assembly declaring no fragment failed: %v", err)
	}
}

func assertInvalidSpec(t *testing.T, err error, module, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("an unusable fragment (%s) was accepted", reason)
	}
	if !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("the failure for %s does not match ErrInvalidSpec: %v", reason, err)
	}
	if !strings.Contains(err.Error(), module) {
		t.Errorf("the failure %q does not name the declaring module %q", err, module)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("the failure %q does not say what is wrong (%q)", err, reason)
	}
}
