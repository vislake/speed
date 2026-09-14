package http

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// sentinels is the design's error table, by name. A caller tells the classes
// apart with errors.Is, so two sentinels that matched each other would leave a
// failure unclassifiable.
var sentinels = map[string]error{
	"ErrUnknownEndpoint": ErrUnknownEndpoint,
	"ErrRouteConflict":   ErrRouteConflict,
	"ErrMiddlewareCycle": ErrMiddlewareCycle,
	"ErrChainAssembly":   ErrChainAssembly,
	"ErrInvalidSpec":     ErrInvalidSpec,
	"ErrListen":          ErrListen,
	"ErrDrainTimeout":    ErrDrainTimeout,
	"ErrMalformedBody":   ErrMalformedBody,
	"ErrBodyTooLarge":    ErrBodyTooLarge,
	"ErrValidation":      ErrValidation,
}

// TestSentinelsAreDistinct checks that no two of them are interchangeable,
// wrapped or bare, and that each carries a message.
func TestSentinelsAreDistinct(t *testing.T) {
	for name, err := range sentinels {
		if err == nil {
			t.Fatalf("%s is nil", name)
		}
		if !strings.HasPrefix(err.Error(), "http: ") {
			t.Errorf("%s reads %q; a sentinel names its package so a caller can tell "+
				"whose failure it is reading", name, err.Error())
		}
		wrapped := fmt.Errorf("assembling endpoint %q: %w", "public", err)
		for otherName, other := range sentinels {
			match := errors.Is(wrapped, other)
			if otherName == name && !match {
				t.Errorf("a wrapped %s does not match %s", name, name)
			}
			if otherName != name && match {
				t.Errorf("a wrapped %s also matches %s", name, otherName)
			}
		}
	}
}

// TestSentinelTableHasNoStrayMember reads the sentinels back out of the source
// and checks the table above accounts for every one of them, in both
// directions. The count is never written down here: it is a snapshot of what
// errors.go happens to declare today, and pinning it would make this test
// disagree with the source the moment a class is added. A sentinel added to
// errors.go and to nothing else would otherwise never be checked for
// distinctness, and two overlapping sentinels are only found by checking.
func TestSentinelTableHasNoStrayMember(t *testing.T) {
	_, files := parseProductionFiles(t)
	var declared []string
	for _, file := range files {
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
					if strings.HasPrefix(name.Name, "Err") && name.IsExported() {
						declared = append(declared, name.Name)
					}
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no exported Err* variable was found in the source, so the check below proved nothing")
	}
	for _, name := range declared {
		if _, ok := sentinels[name]; !ok {
			t.Errorf("the source declares sentinel %s, which the table here does not list. "+
				"Add it, so that it is checked against every other sentinel", name)
		}
	}
	for name := range sentinels {
		if !slices.Contains(declared, name) {
			t.Errorf("the table lists %s, which the source no longer declares", name)
		}
	}
}
