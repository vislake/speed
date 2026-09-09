package pkgcore

// EventBusRegistry is the package-level SeamRegistry every Kernel resolves
// the "eventbus" Preset key against, pre-populated below with pkgcore's
// in-process built-in implementation. A host registers its own
// implementation -- including pkgcore's own Redis-backed one, which
// registers "eventbus.redis" through the eventbus/redis subpackage's own
// init(), not here -- by calling EventBusRegistry.Register before
// bootstrapping a Kernel whose Preset names it.
var EventBusRegistry = newBuiltinEventBusRegistry()

func newBuiltinEventBusRegistry() *SeamRegistry[EventBus] {
	r := NewSeamRegistry[EventBus]()
	mustRegister(r, Registration[EventBus]{
		Name:         "eventbus.memory",
		Capabilities: 0,
		New:          func(Config) (EventBus, error) { return NewMemoryEventBus(), nil },
	})
	return r
}
