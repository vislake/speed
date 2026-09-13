package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type redisOptions struct {
	Addr     string
	PoolSize int
}

type cacheOptions struct {
	Addr   string
	TTL    time.Duration
	Labels map[string]string
	Nodes  []redisOptions
}

// cacheManifest declares one module under the cache namespace with a scalar,
// a duration, a map and a list of structs, which covers every shape a layer
// has to deal with.
func cacheManifest(t *testing.T, items map[string]Item) *manifest {
	t.Helper()
	defaults := cacheOptions{
		Addr:   "localhost:6379",
		TTL:    5 * time.Minute,
		Labels: map[string]string{"tier": "hot"},
	}
	return collect(t, declaring("cache", Schema{
		Namespace: "cache",
		Mounts:    []Mount{{Value: &defaults}},
		Items:     items,
	}))
}

// TestPrimaryOverridesLeafWiseNotSubtree pins that the primary source
// overrides declared leaves one by one: the key set of a section comes from
// the declarations, so giving one key must not knock its siblings back to
// their zero values.
func TestPrimaryOverridesLeafWiseNotSubtree(t *testing.T) {
	m := cacheManifest(t, nil)
	d := newData(m)
	if err := d.applyPrimary(m, map[string]any{
		"cache": map[string]any{"addr": "redis:6379"},
	}); err != nil {
		t.Fatalf("applying the primary source failed: %v", err)
	}
	if got := d.values["cache.addr"].value; got != "redis:6379" {
		t.Fatalf("cache.addr holds %#v, want the value the primary source gave", got)
	}
	if got := d.values["cache.ttl"].value; got != 5*time.Minute {
		t.Fatalf("cache.ttl holds %#v, want the declared default: a sibling key nobody gave "+
			"must not be knocked back", got)
	}
	if got := d.values["cache.ttl"].layer; got != layerDefault {
		t.Fatalf("cache.ttl records %v, want the default layer", got)
	}
}

// TestContainerReplacedWholesale pins the other half of the rule: a map and a
// list of structs have no keys in the manifest, so there is no leaf-wise merge
// to make and the value is replaced entirely.
func TestContainerReplacedWholesale(t *testing.T) {
	m := cacheManifest(t, nil)
	d := newData(m)
	if err := d.applyPrimary(m, map[string]any{
		"cache": map[string]any{
			"labels": map[string]any{"tier": "cold", "zone": "b"},
			"nodes":  []any{map[string]any{"addr": "n1:1", "pool-size": float64(3)}},
		},
	}); err != nil {
		t.Fatalf("applying the primary source failed: %v", err)
	}
	want := map[string]string{"tier": "cold", "zone": "b"}
	if got := d.values["cache.labels"].value; !reflect.DeepEqual(got, want) {
		t.Fatalf("cache.labels holds %#v, want %#v: the default entry is replaced, not merged "+
			"under the new one", got, want)
	}
	wantNodes := []redisOptions{{Addr: "n1:1", PoolSize: 3}}
	if got := d.values["cache.nodes"].value; !reflect.DeepEqual(got, wantNodes) {
		t.Fatalf("cache.nodes holds %#v, want %#v", got, wantNodes)
	}
}

// TestKeysInsideContainerLeafNotValidated pins that the keys inside a
// container do not take part in the unknown-key check. The same fact decides
// that the value is replaced as a whole; using it a second time to judge the
// keys would make every map-typed item unusable at its first key.
func TestKeysInsideContainerLeafNotValidated(t *testing.T) {
	m := cacheManifest(t, nil)
	d := newData(m)
	err := d.applyPrimary(m, map[string]any{
		"cache": map[string]any{
			"labels": map[string]any{"anything-at-all": "x", "cache.addr": "y"},
		},
	})
	if err != nil {
		t.Fatalf("keys inside a map were checked against the manifest: %v", err)
	}
}

func TestUnknownKeyNotInManifest(t *testing.T) {
	m := cacheManifest(t, nil)
	d := newData(m)
	err := d.applyPrimary(m, map[string]any{"cache": map[string]any{"addres": "typo"}})
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("a misspelled key returned %v, want ErrUnknownKey", err)
	}
	if !strings.Contains(err.Error(), "no module declares") {
		t.Fatalf("the rejection reads %q, which does not say the manifest has no such key", err)
	}
	if !strings.Contains(err.Error(), "cache.addres") {
		t.Fatalf("the rejection reads %q, which does not name the key", err)
	}
}

// TestKeyNotAcceptedFromPrimary is the second kind of unknown key, and the
// error text has to tell it from the first: the manifest has this key, and the
// item does not take the primary source. Letting it through would swallow a
// value somebody wrote on purpose.
func TestKeyNotAcceptedFromPrimary(t *testing.T) {
	m := cacheManifest(t, map[string]Item{"addr": {Origins: OriginFlag, FlagName: "cache-addr"}})
	d := newData(m)
	err := d.applyPrimary(m, map[string]any{"cache": map[string]any{"addr": "redis:6379"}})
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("a key the item does not take from the primary source returned %v, want ErrUnknownKey", err)
	}
	if !strings.Contains(err.Error(), "does not accept from the primary source") {
		t.Fatalf("the rejection reads %q, which does not tell this case from a key the manifest "+
			"has never heard of", err)
	}
}

// TestSectionGivenAValue covers the shape error the other way round: the path
// is a section holding input items, and the source put a value on it.
func TestSectionGivenAValue(t *testing.T) {
	m := cacheManifest(t, nil)
	d := newData(m)
	err := d.applyPrimary(m, map[string]any{"cache": "just a string"})
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("a value on a section returned %v, want ErrUnknownKey", err)
	}
	if !strings.Contains(err.Error(), "section") {
		t.Fatalf("the rejection reads %q, which does not say the path is a section", err)
	}
}

func TestPrimaryValueTypeMismatch(t *testing.T) {
	m := cacheManifest(t, nil)
	d := newData(m)
	err := d.applyPrimary(m, map[string]any{"cache": map[string]any{"ttl": "abc"}})
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("an unconvertible value returned %v, want ErrTypeMismatch", err)
	}
	if !strings.Contains(err.Error(), "cache.ttl") {
		t.Fatalf("the rejection reads %q, which does not name the path", err)
	}
}

// TestProvenanceRecordsLayer pins the record the required rule stands on: a
// leaf remembers which layer gave it its value, so a value nobody gave and a
// value that happens to be the zero one stay apart.
func TestProvenanceRecordsLayer(t *testing.T) {
	m := cacheManifest(t, nil)
	d := newData(m)
	if got := d.values["cache.addr"].layer; got != layerDefault {
		t.Fatalf("before any source is applied cache.addr records %v, want the default layer", got)
	}
	if d.given("cache.addr") {
		t.Fatal("the default layer counts as somebody having given a value")
	}
	if err := d.applyPrimary(m, map[string]any{"cache": map[string]any{"addr": "redis:6379"}}); err != nil {
		t.Fatalf("applying the primary source failed: %v", err)
	}
	if got := d.values["cache.addr"].layer; got != layerPrimary {
		t.Fatalf("after the primary source cache.addr records %v, want the primary layer", got)
	}
	if !d.given("cache.addr") {
		t.Fatal("a value the primary source gave does not count as given")
	}
}

// TestNoPrimarySourceStillYieldsDefaults pins that configuration exists
// without a primary source: assembly is driven by the configuration, not by
// the config source.
func TestNoPrimarySourceStillYieldsDefaults(t *testing.T) {
	m := cacheManifest(t, nil)
	d := newData(m)
	if got, want := len(d.values), len(m.items); got != want {
		t.Fatalf("the base layer holds %d leaves, want %d", got, want)
	}
	if got := d.values["cache.ttl"].value; got != 5*time.Minute {
		t.Fatalf("cache.ttl holds %#v with no primary source, want the declared default", got)
	}
}

// TestDefaultsDoNotAliasThePrototype pins that the config data cannot be used
// to write back into the struct a module declared, which two registries may
// share.
func TestDefaultsDoNotAliasThePrototype(t *testing.T) {
	defaults := cacheOptions{Labels: map[string]string{"tier": "hot"}}
	m := collect(t, declaring("cache", Schema{Namespace: "cache", Mounts: []Mount{{Value: &defaults}}}))
	held := newData(m).values["cache.labels"].value.(map[string]string)
	held["tier"] = "changed"
	if defaults.Labels["tier"] != "hot" {
		t.Fatal("writing through the config data reached the prototype the module declared")
	}
}
