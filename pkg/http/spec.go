package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/vislake/speed/pkg/core"
)

// openAPIField is the one top-level field a fragment has to carry. Its
// presence is what tells an OpenAPI document apart from some other JSON that
// ended up in the declaration.
const openAPIField = "openapi"

// validateSpecs checks the shape of every declared fragment and reports every
// offending one at once. A single fragment's failure aborts the startup
// anyway, so reporting them one run at a time would only make the host fix
// them one run at a time.
func validateSpecs(specs []core.Resource[Spec]) error {
	var failures []error
	for _, res := range specs {
		if err := validateSpec(res.Module, res.Value); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// validateSpec checks that one fragment has a usable shape. It does not check
// the fragment against the OpenAPI grammar: that needs a parsing library, and
// this package's surface carries no third-party type. What is caught here is
// the class that would otherwise break a reader's merge long after startup,
// where nothing connects the crash back to the module that declared the
// fragment.
//
// Nor does it check the fragment against what the module actually registered.
// The two are separately maintained facts and are allowed to drift; a mismatch
// produces no failure signal at all.
func validateSpec(module string, s Spec) error {
	if len(bytes.TrimSpace(s.Document)) == 0 {
		return fmt.Errorf("%w: module %q declared a Spec with an empty Document. "+
			"Either embed the module's openapi.json into it or drop the declaration",
			ErrInvalidSpec, module)
	}
	// utf8.Valid comes before the JSON decoding on purpose: encoding/json
	// replaces an illegal byte inside a string with U+FFFD instead of
	// failing, so a fragment carrying mis-encoded text would pass the decode
	// and reach the reader's merge as silently corrupted content.
	if !utf8.Valid(s.Document) {
		return fmt.Errorf("%w: module %q declared a Spec whose Document is not valid UTF-8. "+
			"JSON is UTF-8 by definition; re-encode the file the fragment came from",
			ErrInvalidSpec, module)
	}
	if !json.Valid(s.Document) {
		return fmt.Errorf("%w: module %q declared a Spec whose Document is not valid JSON. "+
			"The Document is the JSON encoding of the fragment, not YAML",
			ErrInvalidSpec, module)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(s.Document, &top); err != nil {
		return fmt.Errorf("%w: module %q declared a Spec whose Document is not a JSON object at the "+
			"top level. An OpenAPI document is an object; wrap or replace the fragment: %w",
			ErrInvalidSpec, module, err)
	}
	// A literal null decodes into a nil map without an error, so it reaches
	// here as a document with no fields rather than as a decoding failure.
	// Reported as a missing openapi field it would send the reader looking
	// for a field to add to a document that has no top-level object to add
	// it to.
	if top == nil {
		return fmt.Errorf("%w: module %q declared a Spec whose Document is null, not a JSON object at "+
			"the top level. An OpenAPI document is an object", ErrInvalidSpec, module)
	}
	if _, ok := top[openAPIField]; !ok {
		return fmt.Errorf("%w: module %q declared a Spec whose Document has no %q field at the top "+
			"level. Add it with the OpenAPI version the fragment is written against, "+
			"for example %q: \"3.1.0\"", ErrInvalidSpec, module, openAPIField, openAPIField)
	}
	return nil
}
