package core

// The generic free functions below exist because Go does not allow methods to
// carry type parameters. They build the capability or resource token
// themselves and complete the assertion, and carry no semantics beyond the
// method they wrap: what they buy is type safety at the call site, not fewer
// characters.

// Resolve returns the instance of the one module that delivers capability T.
// It is the type-safe form of (*Registry).Resolve, written core.Resolve[Cache](reg).
//
// T must be an interface: a capability is a set of methods, so core.Resolve[int]
// panics just as a concrete token in a declaration does.
func Resolve[T any](r *Registry) (T, error) {
	var zero T
	v, err := r.Resolve(token[T]())
	if err != nil {
		return zero, err
	}
	return v.(T), nil
}

// ResolveAll returns every constructed instance that delivers capability T, in
// module-name order. T must be an interface.
func ResolveAll[T any](r *Registry) []T {
	values := r.ResolveAll(token[T]())
	out := make([]T, 0, len(values))
	for _, v := range values {
		out = append(out, v.(T))
	}
	return out
}

// Resources returns every declared resource assignable to T, in module-name
// order. T designates a resource type, so any type is legal here.
func Resources[T any](r *Registry) []Resource[T] {
	found := r.Resources(token[T]())
	out := make([]Resource[T], 0, len(found))
	for _, res := range found {
		out = append(out, Resource[T]{Module: res.Module, Value: res.Value.(T)})
	}
	return out
}

// token builds the typed nil pointer that designates T.
func token[T any]() Token {
	var p *T
	return p
}
