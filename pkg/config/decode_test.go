package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type decodeOptions struct {
	Addr string
	TTL  time.Duration
}

func newReader(t *testing.T, m *manifest) (*reader, *data) {
	t.Helper()
	d := newData(m)
	return &reader{manifest: m, data: d}, d
}

// TestDecodeYieldsDefaultsIntoZeroStruct pins that the defaults travel through
// the config data's base layer: the caller passes a zero struct and still gets
// what the module declared.
func TestDecodeYieldsDefaultsIntoZeroStruct(t *testing.T) {
	defaults := decodeOptions{Addr: "localhost:6379", TTL: 5 * time.Minute}
	m := collect(t, declaring("cache", Schema{Namespace: "cache", Mounts: []Mount{{Value: &defaults}}}))
	r, _ := newReader(t, m)

	var opts decodeOptions
	if err := r.Decode("cache", &opts); err != nil {
		t.Fatalf("decoding failed: %v", err)
	}
	if opts.TTL != 5*time.Minute || opts.Addr != "localhost:6379" {
		t.Fatalf("a zero struct decoded to %#v, want the declared defaults", opts)
	}
}

// TestDecodeDoesNotConsultMountPrototype pins that collection is the only time
// the prototype is read. Changing it afterwards must not change what a module
// decodes, or two registries sharing one prototype would see each other.
func TestDecodeDoesNotConsultMountPrototype(t *testing.T) {
	defaults := decodeOptions{Addr: "localhost:6379"}
	m := collect(t, declaring("cache", Schema{Namespace: "cache", Mounts: []Mount{{Value: &defaults}}}))
	r, _ := newReader(t, m)

	defaults.Addr = "changed-after-collection"
	var opts decodeOptions
	if err := r.Decode("cache", &opts); err != nil {
		t.Fatalf("decoding failed: %v", err)
	}
	if opts.Addr != "localhost:6379" {
		t.Fatalf("the decoded value is %q, want the value the prototype held when it was "+
			"collected", opts.Addr)
	}
}

// TestPrototypeSharedAcrossRegistriesIsIsolated pins that two registries using
// one prototype decode independently: the prototype carries the shape and the
// defaults, never the values of a run.
func TestPrototypeSharedAcrossRegistriesIsIsolated(t *testing.T) {
	defaults := decodeOptions{Addr: "localhost:6379"}
	declaration := declaring("cache", Schema{Namespace: "cache", Mounts: []Mount{{Value: &defaults}}})

	first := collect(t, declaration)
	firstReader, firstData := newReader(t, first)
	if err := firstData.applyPrimary(first, map[string]any{"cache": map[string]any{"addr": "one:1"}}); err != nil {
		t.Fatalf("applying the primary source failed: %v", err)
	}

	second := collect(t, declaration)
	secondReader, _ := newReader(t, second)

	var a, b decodeOptions
	if err := firstReader.Decode("cache", &a); err != nil {
		t.Fatalf("decoding the first registry failed: %v", err)
	}
	if err := secondReader.Decode("cache", &b); err != nil {
		t.Fatalf("decoding the second registry failed: %v", err)
	}
	if a.Addr != "one:1" {
		t.Fatalf("the first registry decoded %q, want the value its own source gave", a.Addr)
	}
	if b.Addr != "localhost:6379" {
		t.Fatalf("the second registry decoded %q, want the declared default: the two registries "+
			"share a prototype and nothing else", b.Addr)
	}
}

// TestDecodeUnknownPathLeavesTargetUntouched pins that a path nobody declared
// is not an error. A declared path always exists, the base layer having built
// it, so this only happens when a caller decodes something it never declared.
func TestDecodeUnknownPathLeavesTargetUntouched(t *testing.T) {
	defaults := decodeOptions{Addr: "localhost:6379"}
	m := collect(t, declaring("cache", Schema{Namespace: "cache", Mounts: []Mount{{Value: &defaults}}}))
	r, _ := newReader(t, m)

	opts := decodeOptions{Addr: "put here by the caller", TTL: time.Second}
	if err := r.Decode("nowhere", &opts); err != nil {
		t.Fatalf("decoding a path nobody declared returned %v, want no error", err)
	}
	if opts.Addr != "put here by the caller" || opts.TTL != time.Second {
		t.Fatalf("the target became %#v, want what the caller put in it", opts)
	}
}

type requiredOptions struct {
	Addr  string
	Token string
}

// requiredSetup declares one required item and one ordinary one.
func requiredSetup(t *testing.T) *manifest {
	t.Helper()
	defaults := requiredOptions{Addr: "unset-but-not-empty"}
	return collect(t,
		identifying("host", HostIdentity{Prefix: "MYAPP"}),
		declaring("remote", Schema{
			Namespace: "remote",
			Mounts:    []Mount{{Value: &defaults}},
			Items: map[string]Item{
				"addr": {Origins: OriginPrimary | OriginEnv | OriginFlag, Required: true, FlagName: "remote-addr"},
			},
		}),
	)
}

func TestRequiredMissingAcrossThreeLayers(t *testing.T) {
	m := requiredSetup(t)
	r, _ := newReader(t, m)

	var opts requiredOptions
	err := r.Decode("remote", &opts)
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("a required item nobody gave returned %v, want ErrMissingRequired", err)
	}
	if !strings.Contains(err.Error(), "remote.addr") {
		t.Fatalf("the rejection reads %q, which does not name the path", err)
	}
}

// TestDefaultLayerDoesNotSatisfyRequired pins the rule the whole judgment
// stands on: the carrier struct always has a value, so counting it would make
// a required item and one that took the zero value the same thing. The
// declared default here is deliberately not empty.
func TestDefaultLayerDoesNotSatisfyRequired(t *testing.T) {
	m := requiredSetup(t)
	r, d := newReader(t, m)
	if got := d.values["remote.addr"].value; got != "unset-but-not-empty" {
		t.Fatalf("the base layer holds %#v, and this case needs a non-zero default", got)
	}
	var opts requiredOptions
	if err := r.Decode("remote", &opts); !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("a required item holding a non-zero default returned %v, want ErrMissingRequired", err)
	}
}

func TestRequiredSatisfiedByOneLayerAlone(t *testing.T) {
	for name, give := range map[string]func(*testing.T, *manifest, *data){
		"the primary source": func(t *testing.T, m *manifest, d *data) {
			if err := d.applyPrimary(m, map[string]any{"remote": map[string]any{"addr": "a:1"}}); err != nil {
				t.Fatalf("applying the primary source failed: %v", err)
			}
		},
		"the environment": func(t *testing.T, m *manifest, d *data) {
			if _, err := d.applyEnv(m, []string{"MYAPP_REMOTE__ADDR=a:1"}); err != nil {
				t.Fatalf("applying the environment failed: %v", err)
			}
		},
		"the command line": func(t *testing.T, m *manifest, d *data) {
			p, err := parseFlags(m, []string{"--remote-addr=a:1"})
			if err != nil {
				t.Fatalf("parsing the command line failed: %v", err)
			}
			if err := d.applyFlags(m, p); err != nil {
				t.Fatalf("applying the command line failed: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := requiredSetup(t)
			r, d := newReader(t, m)
			give(t, m, d)

			var opts requiredOptions
			if err := r.Decode("remote", &opts); err != nil {
				t.Fatalf("a required item given by %s returned %v, want no error", name, err)
			}
			if opts.Addr != "a:1" {
				t.Fatalf("the decoded value is %q, want what %s gave", opts.Addr, name)
			}
		})
	}
}

// TestRequiredNotCheckedGlobally pins that the judgment happens at Decode
// rather than in one sweep over the manifest. The manifest holds the items of
// modules this run will not enable, and failing on theirs would break the
// stance that lets a default implementation stand down.
func TestRequiredNotCheckedGlobally(t *testing.T) {
	otherDefaults := requiredOptions{}
	m := collect(t,
		identifying("host", HostIdentity{Prefix: "MYAPP"}),
		declaring("remote", Schema{
			Namespace: "remote",
			Mounts:    []Mount{{Value: &requiredOptions{}}},
			Items:     map[string]Item{"addr": {Required: true}},
		}),
		declaring("local", Schema{Namespace: "local", Mounts: []Mount{{Value: &otherDefaults}}}),
	)
	r, _ := newReader(t, m)

	var opts requiredOptions
	if err := r.Decode("local", &opts); err != nil {
		t.Fatalf("decoding one module's mount failed on another module's required item: %v", err)
	}
}

type addrOnly struct {
	Addr string
}

type portOnly struct {
	Port int
}

// TestDecodeIgnoresOtherModulesKeysInSameNamespace pins that the walk is over
// the target rather than over the manifest, which is what lets two modules
// share a namespace as long as their paths stay disjoint.
func TestDecodeIgnoresOtherModulesKeysInSameNamespace(t *testing.T) {
	m := collect(t,
		declaring("first", Schema{Namespace: "shared", Mounts: []Mount{{Value: &addrOnly{Addr: "a:1"}}}}),
		declaring("second", Schema{Namespace: "shared", Mounts: []Mount{{Value: &portOnly{Port: 9}}}}),
	)
	r, _ := newReader(t, m)

	var mine addrOnly
	if err := r.Decode("shared", &mine); err != nil {
		t.Fatalf("decoding a shared namespace failed: %v", err)
	}
	if mine.Addr != "a:1" {
		t.Fatalf("the decoded value is %q, want this module's own default", mine.Addr)
	}

	var theirs portOnly
	if err := r.Decode("shared", &theirs); err != nil {
		t.Fatalf("decoding the other module's mount failed: %v", err)
	}
	if theirs.Port != 9 {
		t.Fatalf("the other module decoded %d, want its own default", theirs.Port)
	}
}

// TestDecodeWritesEveryLayer pins that Decode reads the topmost layer that
// gave a value, mount path and all.
func TestDecodeWritesEveryLayer(t *testing.T) {
	type nested struct {
		PoolSize int
	}
	type carrier struct {
		Addr  string
		Redis nested
	}
	defaults := carrier{Addr: "localhost:6379", Redis: nested{PoolSize: 10}}
	m := collect(t,
		identifying("host", HostIdentity{Prefix: "MYAPP"}),
		declaring("cache", Schema{Namespace: "cache", Mounts: []Mount{{Value: &defaults}}}),
	)
	r, d := newReader(t, m)
	if _, err := d.applyEnv(m, []string{"MYAPP_CACHE__REDIS__POOL_SIZE=42"}); err != nil {
		t.Fatalf("applying the environment failed: %v", err)
	}

	var opts carrier
	if err := r.Decode("cache", &opts); err != nil {
		t.Fatalf("decoding failed: %v", err)
	}
	if opts.Redis.PoolSize != 42 {
		t.Fatalf("the nested value decoded to %d, want the environment's value", opts.Redis.PoolSize)
	}
	if opts.Addr != "localhost:6379" {
		t.Fatalf("the sibling value decoded to %q, want the declared default", opts.Addr)
	}
}

// TestDecodeNeedsAPointerToStruct pins that a target Decode cannot write into
// panics instead of returning. What the caller passes is fixed in the source,
// not in anyone's configuration, so there is no run in which the same call
// site works; a host handed an error here could only re-raise it.
func TestDecodeNeedsAPointerToStruct(t *testing.T) {
	m := collect(t, declaring("cache", Schema{Namespace: "cache", Mounts: []Mount{{Value: &decodeOptions{}}}}))
	r, _ := newReader(t, m)

	for name, c := range map[string]struct {
		target any
		names  string
	}{
		"a value":               {target: decodeOptions{}, names: "config.decodeOptions"},
		"a nil pointer":         {target: (*decodeOptions)(nil), names: "*config.decodeOptions"},
		"a pointer to a scalar": {target: new(int), names: "*int"},
	} {
		t.Run(name, func(t *testing.T) {
			// An illegal target is a programming error at the call site: it
			// is unrelated to data, it shows up on first execution, and the
			// host has nothing to handle. It therefore panics rather than
			// returning a sentinel a host would be invited to inspect.
			defer func() {
				raised := recover()
				if raised == nil {
					t.Fatalf("decoding into %s returned, and there is nowhere to write", name)
				}
				text := fmt.Sprint(raised)
				if !strings.Contains(text, c.names) || !strings.Contains(text, `"cache"`) {
					t.Fatalf("the panic reads %q, which does not name both the target %s and "+
						"the path it was given for", text, c.names)
				}
			}()
			_ = r.Decode("cache", c.target)
		})
	}
}

// cyclicTarget is a carrier that reaches itself. Collection refuses a declared
// one, so a cycle can only arrive through a target the caller assembled for
// itself and handed to Decode.
type cyclicTarget struct {
	Addr string
	Next *cyclicTarget
}

// TestDecodeRefusesATargetThatCyclesBack pins that the walk over the target
// stops on a back edge, and that it stops by panicking: the carrier is
// assembled at the call site, so a cycle in it is that site's programming
// error rather than anything the host could act on.
func TestDecodeRefusesATargetThatCyclesBack(t *testing.T) {
	m := collect(t, declaring("cache", Schema{Namespace: "cache", Mounts: []Mount{{Value: &decodeOptions{}}}}))
	r, _ := newReader(t, m)

	var target cyclicTarget
	target.Next = &target
	defer func() {
		raised := recover()
		if raised == nil {
			t.Fatalf("decoding into a target that reaches itself returned, and the walk does " +
				"not terminate")
		}
		text := fmt.Sprint(raised)
		if !strings.Contains(text, "Next") {
			t.Fatalf("the panic reads %q, which does not name the field that closes the cycle", text)
		}
	}()
	_ = r.Decode("cache", &target)
}

// TestDecodeReadsAnInlinedEmbeddedField pins that the expansion and the decode
// walk derive one and the same path for an embedded field. They are separate
// implementations of one rule, and a module that declared cache.addr while
// reading cache.embedded-options.addr would fail at neither end: the key would
// simply never arrive.
func TestDecodeReadsAnInlinedEmbeddedField(t *testing.T) {
	type inlineTarget struct {
		EmbeddedOptions
		Name string
	}
	defaults := inlineTarget{}
	m := collect(t, declaring("cache", Schema{Namespace: "cache", Mounts: []Mount{{Value: &defaults}}}))
	r, d := newReader(t, m)
	if err := d.applyPrimary(m, map[string]any{"cache": map[string]any{"addr": "given:1"}}); err != nil {
		t.Fatalf("applying the primary source failed: %v", err)
	}

	var opts inlineTarget
	if err := r.Decode("cache", &opts); err != nil {
		t.Fatalf("decoding failed: %v", err)
	}
	if opts.Addr != "given:1" {
		t.Fatalf("the inlined field decoded to %q, want the value the primary source gave", opts.Addr)
	}
}
