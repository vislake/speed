// Package componenttest provides the contract assertions a component's tests
// run to prove the descriptor it registers honours the assembly's contract.
// It is test-support code: a component package's unit tests call
// AssertWellFormed on the descriptor its init self-registration builds, so a
// descriptor that would fail the assembly's own registration checks -- or
// that would only surface as an assembly failure in a consumer's process --
// fails in the component's own suite instead.
package componenttest

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// componentNamePattern is the naming convention a component name follows:
// lowercase dot-separated segments of letters, digits, underscores and
// hyphens, such as "authn", "mailer.smtp" or "ai-gateway". Hyphens are legal
// so a component of a hyphenated module can carry the module's own name, as
// a single-implementation module's component does.
var componentNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*)*$`)

// WellFormed reports why c does not satisfy the component descriptor
// contract, or nil when it does. It checks the conventions the assembly's
// own registration enforces (a non-empty name, a New callback) plus the ones
// the assembly relies on the descriptors honouring:
//
//   - the name follows the component naming convention (lowercase,
//     dot-separated segments);
//   - a component implementing a module is named after it: the name equals
//     the module name (single-implementation modules) or extends it with a
//     dot segment ("mailer.smtp" for module "mailer");
//   - ConfigSchema is nil or a pointer to a struct that decodes an empty
//     configuration cleanly, the form the assembly decodes a component's
//     configuration block with;
//   - Requires tokens and Provides entries are typed pointers to the
//     contract types they name.
//
// All problems are reported together, so one call tells a component author
// everything the descriptor still has to fix.
func WellFormed(c pkgcore.Component) error {
	var problems []string

	switch {
	case c.Name == "":
		problems = append(problems, "the component has no name; a name is the selection key a composition configuration addresses the component by")
	case !componentNamePattern.MatchString(c.Name):
		problems = append(problems, fmt.Sprintf("the name %q does not follow the component naming convention: lowercase dot-separated segments of letters, digits, underscores and hyphens (\"authn\", \"mailer.smtp\", \"ai-gateway\")", c.Name))
	case c.Module != "" && c.Name != c.Module && !strings.HasPrefix(c.Name, c.Module+"."):
		problems = append(problems, fmt.Sprintf("the name %q does not reflect the module %q it implements; a component of a module is named after it (\"authn\" for module \"authn\") or extends it with a dot segment (\"mailer.smtp\" for module \"mailer\")", c.Name, c.Module))
	}

	if c.New == nil {
		problems = append(problems, "the component declares no New callback, and New is the one required callback")
	}

	if c.ConfigSchema != nil {
		t := reflect.TypeOf(c.ConfigSchema)
		switch {
		case t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct:
			problems = append(problems, fmt.Sprintf("ConfigSchema has type %s, want a pointer to a config struct such as (*mailerConfig)(nil), or nil for a component that takes no configuration", t))
		default:
			target := reflect.New(t.Elem())
			if err := pkgcore.NewComponentConfig(nil).Decode(target.Interface()); err != nil {
				problems = append(problems, fmt.Sprintf("ConfigSchema type %s does not decode cleanly: %v", t, err))
			}
		}
	}

	for i, req := range c.Requires {
		if problem := tokenProblem(req.Token); problem != "" {
			problems = append(problems, fmt.Sprintf("Requires entry %d: %s", i, problem))
		}
	}
	for i, provided := range c.Provides {
		if problem := tokenProblem(provided); problem != "" {
			problems = append(problems, fmt.Sprintf("Provides entry %d: %s", i, problem))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return errors.New("component " + c.Name + ": " + strings.Join(problems, "; "))
}

// tokenProblem reports why a Requirement.Token or Provides entry is not a
// usable contract token, or "" when it is.
func tokenProblem(tok any) string {
	t := reflect.TypeOf(tok)
	if t == nil || t.Kind() != reflect.Pointer {
		return fmt.Sprintf("%s is not a contract token; a token is a typed pointer to the contract type, such as (*pkgcore.Mailer)(nil)", displayType(t))
	}
	return ""
}

// displayType renders a type for error text, naming the null type.
func displayType(t reflect.Type) string {
	if t == nil {
		return "an untyped nil"
	}
	return t.String()
}

// AssertWellFormed fails t -- without stopping the test -- for every problem
// WellFormed reports about c. A component package calls it on the descriptor
// its registration builds, from a unit test, so a contract violation fails
// in the suite that owns the descriptor.
func AssertWellFormed(t *testing.T, c pkgcore.Component) {
	t.Helper()
	if err := WellFormed(c); err != nil {
		t.Errorf("component descriptor is not well formed: %v", err)
	}
}
