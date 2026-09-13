package core

import (
	"fmt"
	"reflect"
)

// capabilityType returns the interface type a capability token designates.
//
// An illegal token panics rather than returning an error. It is a programming
// error at the declaration or the call site, unrelated to data, and it shows
// up deterministically on first execution; expressing it as an error would
// force ResolveAll and Resources to grow an error return for a case only a bug
// can reach.
func capabilityType(t Token, where string) reflect.Type {
	elem := pointerElem(t, where, "capability")
	if elem.Kind() != reflect.Interface {
		panic(fmt.Sprintf("core: %s: capability token must point to an interface, got %s. "+
			"A capability is the set of methods modules take up from each other, "+
			"so a pointer to a concrete type such as (*%s)(nil) cannot designate one", where, elem, elem.Name()))
	}
	return elem
}

// resourceType returns the type a resource token designates. Resources are
// passive data, so any element type is legal.
func resourceType(t Token, where string) reflect.Type {
	return pointerElem(t, where, "resource")
}

func pointerElem(t Token, where, kind string) reflect.Type {
	v := reflect.ValueOf(t)
	if !v.IsValid() {
		panic(fmt.Sprintf("core: %s: %s token is an untyped nil. "+
			"Write it as a typed nil pointer, for example (*Cache)(nil); "+
			"Cache(nil) converts to an untyped nil interface and designates nothing", where, kind))
	}
	if v.Kind() != reflect.Pointer {
		panic(fmt.Sprintf("core: %s: %s token must be a typed nil pointer, got %s. "+
			"Write it as (*%s)(nil)", where, kind, v.Type(), v.Type()))
	}
	if !v.IsNil() {
		panic(fmt.Sprintf("core: %s: %s token must be a nil pointer, got a non-nil %s. "+
			"A token designates a %s, not an instance", where, kind, v.Type(), kind))
	}
	return v.Type().Elem()
}

// typeName renders a type for an error message, keeping the package path so
// the reader can find the declaration.
func typeName(t reflect.Type) string {
	if t.PkgPath() != "" && t.Name() != "" {
		return t.PkgPath() + "." + t.Name()
	}
	return t.String()
}
