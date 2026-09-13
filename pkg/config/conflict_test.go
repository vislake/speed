package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/vislake/speed/pkg/core"
)

// declaring returns a module that carries one input item declaration, which is
// how a module joins the manifest.
func declaring(name string, s Schema) core.Module {
	return core.Module{Name: name, Resources: []any{s}}
}

// identifying returns a module that carries the host identity.
func identifying(name string, h HostIdentity) core.Module {
	return core.Module{Name: name, Resources: []any{h}}
}

func collect(t *testing.T, modules ...core.Module) *manifest {
	t.Helper()
	reg := core.New()
	for _, m := range modules {
		reg.Register(m)
	}
	m, err := newManifest(reg)
	if err != nil {
		t.Fatalf("collecting the manifest failed: %v", err)
	}
	return m
}

// collectRejects collects a manifest that is expected to be refused as a
// conflict, and checks that both parties are named.
func collectRejects(t *testing.T, parties []string, modules ...core.Module) error {
	t.Helper()
	reg := core.New()
	for _, m := range modules {
		reg.Register(m)
	}
	m, err := newManifest(reg)
	if err == nil {
		t.Fatalf("collecting the manifest produced %d items, want a conflict", len(m.items))
	}
	if !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("collection returned %v, want ErrConfigConflict", err)
	}
	for _, party := range parties {
		if !strings.Contains(err.Error(), party) {
			t.Fatalf("conflict reads %q, which does not name %q", err, party)
		}
	}
	return err
}

type addrCarrier struct {
	Addr string
}

type ttlCarrier struct {
	TTL string
}

func TestPathPrefixIntersectionConflict(t *testing.T) {
	scalar := struct{ Redis string }{}
	nested := struct{ PoolSize int }{}
	err := collectRejects(t, []string{"leaf", "tree", "cache.redis", "cache.redis.pool-size"},
		declaring("leaf", Schema{Namespace: "cache", Mounts: []Mount{{Value: &scalar}}}),
		declaring("tree", Schema{Namespace: "cache", Mounts: []Mount{{Path: "redis", Value: &nested}}}),
	)
	if !strings.Contains(err.Error(), "prefix") {
		t.Fatalf("conflict reads %q, which does not say the paths intersect", err)
	}
}

// TestPathPrefixIntersectionConflictEitherWayRound puts the deeper path in the
// module that sorts first, so the intersection is found with the ancestor
// already in the manifest rather than the other way round.
func TestPathPrefixIntersectionConflictEitherWayRound(t *testing.T) {
	nested := struct{ PoolSize int }{}
	scalar := struct{ Redis string }{}
	collectRejects(t, []string{"a-tree", "b-leaf", "cache.redis", "cache.redis.pool-size"},
		declaring("a-tree", Schema{Namespace: "cache", Mounts: []Mount{{Path: "redis", Value: &nested}}}),
		declaring("b-leaf", Schema{Namespace: "cache", Mounts: []Mount{{Value: &scalar}}}),
	)
}

func TestIdenticalPathConflict(t *testing.T) {
	collectRejects(t, []string{"alpha", "beta", "cache.addr"},
		declaring("alpha", Schema{Namespace: "cache", Mounts: []Mount{{Value: &addrCarrier{}}}}),
		declaring("beta", Schema{Namespace: "cache", Mounts: []Mount{{Value: &addrCarrier{}}}}),
	)
}

// TestMutuallyExclusiveImplementationsSharingAPathConflict pins the reach of
// the manifest: it is collected from every registered module, including the
// ones this run will not enable, so two implementations of one capability
// conflict even though only one of them ever runs.
func TestMutuallyExclusiveImplementationsSharingAPathConflict(t *testing.T) {
	collectRejects(t, []string{"cache.memory", "cache.remote", "cache.ttl"},
		declaring("cache.memory", Schema{Namespace: "cache", Mounts: []Mount{{Value: &ttlCarrier{}}}}),
		declaring("cache.remote", Schema{Namespace: "cache", Mounts: []Mount{{Value: &ttlCarrier{}}}}),
	)
}

func TestSameNamespaceDisjointPathsAllowed(t *testing.T) {
	m := collect(t,
		declaring("alpha", Schema{Namespace: "cache", Mounts: []Mount{{Value: &addrCarrier{}}}}),
		declaring("beta", Schema{Namespace: "cache", Mounts: []Mount{{Value: &ttlCarrier{}}}}),
	)
	for _, path := range []string{"cache.addr", "cache.ttl"} {
		if m.byPath[path] == nil {
			t.Errorf("manifest has no item at %q", path)
		}
	}
}

// TestOverlappingMountPathsAllowed holds the criteria to the paths items
// expand to: a root mount beside a named one is the ordinary shape of several
// mounts, not a conflict.
func TestOverlappingMountPathsAllowed(t *testing.T) {
	root := addrCarrier{}
	nested := struct{ PoolSize int }{}
	m := collect(t, declaring("cache", Schema{
		Namespace: "cache",
		Mounts:    []Mount{{Value: &root}, {Path: "redis", Value: &nested}},
	}))
	for _, path := range []string{"cache.addr", "cache.redis.pool-size"} {
		if m.byPath[path] == nil {
			t.Errorf("manifest has no item at %q", path)
		}
	}
}

func TestFlagLongNameConflict(t *testing.T) {
	collectRejects(t, []string{"alpha", "beta", "listen"},
		declaring("alpha", Schema{Namespace: "a", Mounts: []Mount{{Value: &addrCarrier{}}},
			Items: map[string]Item{"addr": {Origins: OriginFlag, FlagName: "listen"}}}),
		declaring("beta", Schema{Namespace: "b", Mounts: []Mount{{Value: &addrCarrier{}}},
			Items: map[string]Item{"addr": {Origins: OriginFlag, FlagName: "listen"}}}),
	)
}

func TestFlagShortNameConflict(t *testing.T) {
	collectRejects(t, []string{"alpha", "beta", "l"},
		declaring("alpha", Schema{Namespace: "a", Mounts: []Mount{{Value: &addrCarrier{}}},
			Items: map[string]Item{"addr": {Origins: OriginFlag, FlagName: "listen", FlagShort: "l"}}}),
		declaring("beta", Schema{Namespace: "b", Mounts: []Mount{{Value: &addrCarrier{}}},
			Items: map[string]Item{"addr": {Origins: OriginFlag, FlagName: "local", FlagShort: "l"}}}),
	)
}

func TestEnvNameConflictDerivedVsPinned(t *testing.T) {
	collectRejects(t, []string{"alpha", "beta", "MYAPP_A__ADDR"},
		identifying("host", HostIdentity{Prefix: "MYAPP"}),
		declaring("alpha", Schema{Namespace: "a", Mounts: []Mount{{Value: &addrCarrier{}}}}),
		declaring("beta", Schema{Namespace: "b", Mounts: []Mount{{Value: &addrCarrier{}}},
			Items: map[string]Item{"addr": {EnvName: "MYAPP_A__ADDR"}}}),
	)
}

func TestEnvNameConflictTwoPinned(t *testing.T) {
	collectRejects(t, []string{"alpha", "beta", "NO_COLOR"},
		declaring("alpha", Schema{Namespace: "a", Mounts: []Mount{{Value: &addrCarrier{}}},
			Items: map[string]Item{"addr": {EnvName: "NO_COLOR"}}}),
		declaring("beta", Schema{Namespace: "b", Mounts: []Mount{{Value: &addrCarrier{}}},
			Items: map[string]Item{"addr": {EnvName: "NO_COLOR"}}}),
	)
}

func TestReservedFlagConfig(t *testing.T) {
	collectRejects(t, []string{"alpha", "config"},
		declaring("alpha", Schema{Namespace: "a", Mounts: []Mount{{Value: &addrCarrier{}}},
			Items: map[string]Item{"addr": {Origins: OriginFlag, FlagName: "config"}}}),
	)
}

func TestReservedFlagHelp(t *testing.T) {
	collectRejects(t, []string{"alpha", "help"},
		declaring("alpha", Schema{Namespace: "a", Mounts: []Mount{{Value: &addrCarrier{}}},
			Items: map[string]Item{"addr": {Origins: OriginFlag, FlagName: "help"}}}),
	)
}

func TestReservedEnvConfig(t *testing.T) {
	carrier := struct{ Config string }{}
	collectRejects(t, []string{"alpha", "MYAPP_CONFIG"},
		identifying("host", HostIdentity{Prefix: "MYAPP"}),
		declaring("alpha", Schema{Mounts: []Mount{{Value: &carrier}}}),
	)
}

func TestDuplicateHostIdentityConflict(t *testing.T) {
	collectRejects(t, []string{"host", "second-host"},
		identifying("host", HostIdentity{Prefix: "MYAPP"}),
		identifying("second-host", HostIdentity{Prefix: "OTHER"}),
	)
}

func TestHostIdentityReachesDerivation(t *testing.T) {
	m := collect(t,
		identifying("host", HostIdentity{Prefix: "MYAPP", DefaultLocator: "file:///etc/myapp.yaml"}),
		declaring("alpha", Schema{Namespace: "cache", Mounts: []Mount{{Value: &addrCarrier{}}}}),
	)
	if !m.hasHost || m.host.DefaultLocator != "file:///etc/myapp.yaml" {
		t.Fatalf("manifest carries host identity %+v (declared %v), want the declared one", m.host, m.hasHost)
	}
	if got := m.byEnv["MYAPP_CACHE__ADDR"]; got == nil {
		t.Fatalf("manifest indexes the environment names %v, want MYAPP_CACHE__ADDR", envNames(m))
	}
}

// TestConflictMessageStableAcrossRegistrationOrders holds the two parties of a
// conflict to a fixed order: resources arrive in module-name order, so the
// message does not depend on which module happened to register first.
func TestConflictMessageStableAcrossRegistrationOrders(t *testing.T) {
	alpha := declaring("alpha", Schema{Namespace: "cache", Mounts: []Mount{{Value: &addrCarrier{}}}})
	zeta := declaring("zeta", Schema{Namespace: "cache", Mounts: []Mount{{Value: &addrCarrier{}}}})

	first := collectRejects(t, []string{"alpha", "zeta"}, alpha, zeta)
	second := collectRejects(t, []string{"alpha", "zeta"}, zeta, alpha)
	if first.Error() != second.Error() {
		t.Fatalf("registration order changed the conflict message:\n%v\n%v", first, second)
	}
	if strings.Index(first.Error(), "alpha") > strings.Index(first.Error(), "zeta") {
		t.Fatalf("conflict reads %q, want the parties in module-name order", first)
	}
}

func envNames(m *manifest) []string {
	out := make([]string, 0, len(m.byEnv))
	for name := range m.byEnv {
		out = append(out, name)
	}
	return out
}
