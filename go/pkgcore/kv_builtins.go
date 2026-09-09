package pkgcore

// KVStoreRegistry mirrors EventBusRegistry for the "kv" seam ("kv.redis"
// registers through the kv/redis subpackage).
var KVStoreRegistry = newBuiltinKVStoreRegistry()

func newBuiltinKVStoreRegistry() *SeamRegistry[KVStore] {
	r := NewSeamRegistry[KVStore]()
	mustRegister(r, Registration[KVStore]{
		Name:         "kv.memory",
		Capabilities: 0,
		New:          func(Config) (KVStore, error) { return NewMemoryKVStore(), nil },
	})
	return r
}
