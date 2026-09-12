package main

// bootstrap.go owns the reference's bootstrap layer: the process-start keys a
// speed-based application resolves once, before anything else is wired.
//
// The layer's source is the modules themselves. Every platform module that
// consumes process-start input declares it on its component descriptor, as a
// pkgcore.BootstrapKey on the BootstrapKeys seat or as a derive-tagged field
// of its ConfigSchema, and the generator renders those declarations straight
// from the census -- what the key protects, its value type, whether it is
// secret material, and the fallback an operator should expect when it is
// unset -- never from a hand-kept copy.
// A host's own bootstrap variables are the host's business: they belong to
// the assembling application and are documented where that host lives, so
// this repository-wide reference stays the platform surface: the keys the
// modules declare, and the mechanism a host drives to resolve them.
//
// One rule spans the two layers, and it is enforced here rather than rendered:
// a key belongs to exactly one layer, so a bootstrap key that is also a
// runtime configuration item fails the generator instead of being documented
// twice.

import (
	"sort"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/pkgcore"
)

// overlappingKeys reports every key declared on both configuration layers:
// as a module's bootstrap key and as a runtime configuration item in the
// schema the reference renders. One dotted key cannot mean both, so a reference
// that would print the same identifier twice, once per layer, fails instead.
func overlappingKeys(descriptors []config.ConfigItemDescriptor, declared []pkgcore.BootstrapKey) []string {
	runtime := make(map[string]struct{}, len(descriptors))
	for _, d := range descriptors {
		runtime[d.Key] = struct{}{}
	}
	var overlaps []string
	for _, key := range declared {
		if _, both := runtime[key.Key]; both {
			overlaps = append(overlaps, key.Key)
		}
	}
	sort.Strings(overlaps)
	return overlaps
}
