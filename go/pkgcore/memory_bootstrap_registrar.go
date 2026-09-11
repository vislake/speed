package pkgcore

import (
	"slices"
	"sync"
)

type memoryBootstrapRegistrar struct {
	mu   sync.Mutex
	keys map[string]struct{}
	all  []BootstrapKey
}

func (r *memoryBootstrapRegistrar) Add(keys ...BootstrapKey) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Validation comes before the duplicate check, for the same reason
	// memoryConfigRegistrar documents: a contradictory declaration reports
	// itself as invalid rather than as a collision with whatever an earlier
	// caller registered under the same key. Either way the whole call
	// registers nothing.
	for _, key := range keys {
		if err := validateBootstrapKey(key); err != nil {
			return err
		}
	}
	keyOf := func(key BootstrapKey) string { return key.Key }
	if err := checkUnique(r.keys, keys, keyOf, ErrDuplicateBootstrapKey); err != nil {
		return err
	}
	for _, key := range keys {
		r.keys[key.Key] = struct{}{}
		r.all = append(r.all, key)
	}
	return nil
}

func (r *memoryBootstrapRegistrar) Keys() []BootstrapKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.all)
}
