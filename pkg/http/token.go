package http

import (
	"fmt"
	"reflect"

	"github.com/vislake/speed/pkg/core"
)

// capabilityKey returns the interface type a capability token designates, and
// with it the key this module builds the middleware graph on. Two tokens
// designating one capability yield one key, whoever wrote them.
//
// An illegal token panics rather than returning an error, for the reason a
// registration returns nothing at all: it is a programming error at the
// declaration site, unrelated to data, and it shows up deterministically the
// first time the declaration is read. The text names the offending token and
// states the fixing action.
//
// The rules restated here are core's own, the ones it applies to the tokens in
// Requires and Provides. core does not export that judgement and will not: its
// exported package-level functions are held to the registry constructor, the
// process registry, the sentinel errors and the wrappers that need a type
// parameter, and a predicate is none of those. This second copy is therefore
// permanent rather than a stopgap, and TestCapabilityRuleAgreesWithCore holds
// it against core's public behaviour so that the two drifting apart shows up as
// a red test. The where argument is what tells the two copies apart in a
// message.
func capabilityKey(t core.Token, where string) reflect.Type {
	v := reflect.ValueOf(t)
	if !v.IsValid() {
		panic(fmt.Sprintf("http: %s: capability token is an untyped nil. "+
			"Write it as a typed nil pointer, for example (*auth.Authenticator)(nil); "+
			"Authenticator(nil) converts to an untyped nil interface and designates nothing", where))
	}
	if v.Kind() != reflect.Pointer {
		panic(fmt.Sprintf("http: %s: capability token must be a typed nil pointer, got %s. "+
			"Write it as (*%s)(nil)", where, v.Type(), v.Type()))
	}
	if !v.IsNil() {
		panic(fmt.Sprintf("http: %s: capability token must be a nil pointer, got a non-nil %s. "+
			"A token designates a capability, not an instance", where, v.Type()))
	}
	elem := v.Type().Elem()
	if elem.Kind() != reflect.Interface {
		panic(fmt.Sprintf("http: %s: capability token must point to an interface, got %s. "+
			"A capability is the set of methods modules take up from each other, "+
			"so a pointer to a concrete type such as (*%s)(nil) cannot designate one",
			where, typeName(elem), elem.Name()))
	}
	return elem
}

// typeName renders a type for an error message, keeping the package path so
// the reader can find the declaration.
func typeName(t reflect.Type) string {
	if t.PkgPath() != "" && t.Name() != "" {
		return t.PkgPath() + "." + t.Name()
	}
	return t.String()
}
