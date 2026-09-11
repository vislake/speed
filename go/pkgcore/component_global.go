package pkgcore

// component_global.go carries the read side of the package-level global
// registration: the enumeration of "which components does this binary
// contain", the input to roster checks (a module pinning its golden component
// list) and to host tooling that reports what an import set made available.
// Which of those components an application is made of stays a configuration
// question, answered per assembly.

// GlobalComponents returns a snapshot of the package-level global
// registration, in registration order: every component an imported package
// self-registered through its init, plus every component a host added with
// Register before creating a registry instance. The returned slice is a
// fresh copy -- mutating it never touches the registration -- and a
// component registered after the call does not appear in an earlier
// snapshot.
func GlobalComponents() []Component {
	globalComponents.mu.RLock()
	defer globalComponents.mu.RUnlock()

	out := make([]Component, 0, len(globalComponents.order))
	for _, name := range globalComponents.order {
		out = append(out, globalComponents.byName[name])
	}
	return out
}
