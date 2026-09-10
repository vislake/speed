package app

// bootstrap_test.go pins the bootstrap target's shape: the loader key path of
// every leaf field of hostConfig, and the correspondence between those paths
// and the bootstrap surface the app binds -- the keys the platform modules
// declare on the registry's bootstrap seat, plus the host keys the app owns.
//
// The composition-time half of the same correspondence is
// verifyBootstrapBinding, which runs at every boot against the live registry;
// this test is the target-side half, so a field added to hostConfig without a
// key (or a key without a field) fails here even when no boot happens to
// compose the declaring module.

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// platformDeclaredKeys are the bootstrap keys the platform modules declare on
// the registry's bootstrap seat and this app's loader target binds: authn's
// two key materials, org's invitation-address blind-index key, notification's
// contact-address blind-index key, pki's local-key cipher key and config's
// master key. Each module's own bootstrapKeyDecl is the spelling's source;
// verifyBootstrapBinding checks this same set against the live registry, so a
// module-side rename fails the boot rather than silently drifting from this
// fixture.
var platformDeclaredKeys = []string{
	"authn.blind_index_key",
	"authn.pii_cipher_key",
	"config.master_key",
	"notification.contact_index_key",
	"org.invitation_email_index_key",
	"pki.local_key_cipher_key",
}

// hostConfigKeyPaths walks hostConfig the way the loader walks a target: each
// exported field contributes its lowercased name as a key segment, nested
// structs descend into path segments of their own, and every other type is a
// leaf. The fields of this target are strings, ints, bools and the five
// key-group structs, so the walk needs none of the loader's subtler leaf rules
// (maps, scalar structs, unexported fields); the loader's own describe is the
// authority for what it will actually fill.
func hostConfigKeyPaths(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var paths []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.ToLower(field.Name)
		if field.Type.Kind() == reflect.Struct {
			for _, nested := range hostConfigKeyPaths(t, field.Type) {
				paths = append(paths, name+"."+nested)
			}
			continue
		}
		paths = append(paths, name)
	}
	return paths
}

// TestHostConfigBindsExactlyItsBootstrapSurface pins the strict equality the
// binding proof rests on: the target's leaf key set is exactly the host keys
// the app owns plus the keys the platform modules declare -- no key without a
// field (both Verify calls at boot prove that direction too) and no field
// without a key (this half, which no boot can see).
func TestHostConfigBindsExactlyItsBootstrapSurface(t *testing.T) {
	want := append(append([]string{}, hostBootstrapKeys...), platformDeclaredKeys...)
	sort.Strings(want)

	got := hostConfigKeyPaths(t, reflect.TypeOf(hostConfig{}))
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hostConfig key paths = %v, want exactly %v", got, want)
	}
}

// TestHostConfigPinsEveryLeafField pins the other half of the target's shape:
// every leaf field states the exact variable it reads, so no field can fall
// back to the loader's prefix derivation -- which spells the app's flat,
// single-underscored variable names differently -- without failing here.
func TestHostConfigPinsEveryLeafField(t *testing.T) {
	typ := reflect.TypeOf(hostConfig{})
	var unpinned []string
	var walk func(reflect.Type, string)
	walk = func(t2 reflect.Type, prefix string) {
		for i := 0; i < t2.NumField(); i++ {
			field := t2.Field(i)
			if !field.IsExported() {
				continue
			}
			name := prefix + strings.ToLower(field.Name)
			if field.Type.Kind() == reflect.Struct {
				walk(field.Type, name+".")
				continue
			}
			tag := field.Tag.Get("config")
			if !strings.Contains(tag, "env=") {
				unpinned = append(unpinned, name)
			}
		}
	}
	walk(typ, "")
	if len(unpinned) > 0 {
		t.Fatalf("fields without an env pin: %v", unpinned)
	}
}
