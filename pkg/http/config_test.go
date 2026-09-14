package http

import (
	"context"
	"errors"
	nethttp "net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vislake/speed/pkg/config"
	jsonformat "github.com/vislake/speed/pkg/config/format/json"
	filesource "github.com/vislake/speed/pkg/config/source/file"
	"github.com/vislake/speed/pkg/core"
)

// readThrough runs the real config module over a JSON document and hands back
// what this module's own section decodes into.
//
// The module's own declaration is what is exercised: a stub reader would prove
// the struct tags match the stub, while what has to hold is that config accepts
// this declaration, treats the endpoint set as a container leaf and coerces the
// durations by its own rule.
//
// declared is the schema to register, so one test can put a deliberately
// defective declaration through the same path.
func readThrough(t *testing.T, document string, declared config.Schema) (moduleConfig, error, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("writing the config document: %v", err)
	}

	// config reads the command line unconditionally, and the arguments go
	// test passes a test binary are not this program's.
	saved := os.Args
	os.Args = []string{"http-test"}
	t.Cleanup(func() { os.Args = saved })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reg := core.New()
	reg.Register(config.Module())
	reg.Register(jsonformat.Module())
	reg.Register(filesource.Module())
	reg.Register(core.Module{
		Name:      "host",
		Resources: []any{config.HostIdentity{Prefix: "HTTPTEST", DefaultLocator: "file:" + path}},
	})

	var got moduleConfig
	var decodeErr error
	reg.Register(core.Module{
		Name:      "reader",
		Resources: []any{declared},
		Prepare: func(_ context.Context, r *core.Registry) (core.Enablement, error) {
			reader, err := core.Resolve[config.Reader](r)
			if err != nil {
				return core.Enablement{}, err
			}
			// The decode failure is carried out rather than returned:
			// the test is about what this module's section decodes to,
			// and aborting the startup would hide it behind whatever
			// core wraps a Prepare failure in.
			decodeErr = reader.Decode(configPath, &got)
			return core.Enablement{State: core.StateEnabled}, nil
		},
		// Startup is complete by the Serve stage, so this is where the run
		// has served its purpose.
		Serve: func(context.Context, *core.Registry, any) error {
			cancel()
			return nil
		},
	})

	return got, decodeErr, reg.Run(ctx)
}

// TestEndpointsIsPrimaryOnlyContainerLeaf pins the origin declaration. The
// endpoint set is a map, which config classifies as a container leaf: the
// primary source replaces it as a whole, its keys never reach the manifest, and
// declaring an environment or command-line origin on it is ErrInvalidSchema.
func TestEndpointsIsPrimaryOnlyContainerLeaf(t *testing.T) {
	item, declared := schema().Items[endpointsItem]
	if !declared {
		t.Fatalf("the schema declares no %q item, so its origins are the default set", endpointsItem)
	}
	if item.Origins != config.OriginPrimary {
		t.Errorf("the endpoint set declares the origins %v; only the primary source can give a "+
			"container leaf", item.Origins)
	}

	field, ok := reflect.TypeFor[moduleConfig]().FieldByName("Endpoints")
	if !ok {
		t.Fatal("moduleConfig has no Endpoints field")
	}
	if field.Type.Kind() != reflect.Map {
		t.Errorf("the endpoint set is a %s; a map is what makes it a container leaf config "+
			"replaces as a whole", field.Type.Kind())
	}
}

// TestEnvOriginOnTheEndpointSetIsRefused is the other half of the same fact,
// and the half a declaration could get wrong: config refuses the declaration
// outright rather than quietly deriving an environment variable nobody can use.
func TestEnvOriginOnTheEndpointSetIsRefused(t *testing.T) {
	defective := schema()
	defective.Items = map[string]config.Item{
		endpointsItem: {Origins: config.OriginPrimary | config.OriginEnv, Group: configGroup, Description: "x"},
	}

	_, _, runErr := readThrough(t, `{"http":{"endpoints":{}}}`, defective)
	if !errors.Is(runErr, config.ErrInvalidSchema) {
		t.Fatalf("an environment origin on a container leaf was not refused: %v", runErr)
	}
}

// TestEndpointsDecodeFromThePrimarySource pins the whole path: the declaration
// this module registers is accepted, and a document written against it arrives
// as the endpoints it describes.
func TestEndpointsDecodeFromThePrimarySource(t *testing.T) {
	got, decodeErr, runErr := readThrough(t, `{
		"http": {
			"endpoints": {
				"public": {"address": ":8080", "read-timeout": "45s"},
				"admin":  {"address": "127.0.0.1:9090"}
			}
		}
	}`, schema())
	if runErr != nil {
		t.Fatalf("the run failed: %v", runErr)
	}
	if decodeErr != nil {
		t.Fatalf("decoding this module's section: %v", decodeErr)
	}

	if len(got.Endpoints) != 2 {
		t.Fatalf("the document declares two endpoints and %d arrived: %+v", len(got.Endpoints), got.Endpoints)
	}
	if got.Endpoints["public"].Address != ":8080" {
		t.Errorf("the public endpoint's address arrived as %q", got.Endpoints["public"].Address)
	}
	if read := got.Endpoints["public"].ReadTimeout; read == nil || *read != 45*time.Second {
		t.Errorf("the public endpoint's read-timeout arrived as %v", read)
	}
	if got.Endpoints["admin"].ReadTimeout != nil {
		t.Error("the admin endpoint gives no read-timeout, and one arrived anyway")
	}
}

// TestDurationItemRejectsBareNumber pins config's rule for a duration, which
// this module's field types opt into: a bare 300 reads as five minutes to a
// person and means three hundred nanoseconds to the machine, so only the string
// form is accepted.
func TestDurationItemRejectsBareNumber(t *testing.T) {
	_, decodeErr, runErr := readThrough(t,
		`{"http":{"endpoints":{"public":{"address":":8080","read-timeout":300}}}}`, schema())
	// config coerces the primary source against the declared types while it
	// builds the reader, so the refusal lands on the run rather than on the
	// decode. Either way it is a refusal, and asserting on the pair keeps the
	// test about the rule rather than about where config applies it.
	refusal := errors.Join(runErr, decodeErr)
	if refusal == nil {
		t.Fatal("a bare number was accepted as a duration")
	}
	mustContain(t, refusal.Error(), "duration", "what the value was refused as")
	mustContain(t, refusal.Error(), "read-timeout", "the item it was given for")
}

// TestEndpointDefaultsAppliedAfterDecode pins the three states of an optional
// item. The defaults cannot live in the prototype: a container leaf is replaced
// as a whole, so a document that declares any endpoint at all wipes them out.
func TestEndpointDefaultsAppliedAfterDecode(t *testing.T) {
	off := time.Duration(0)
	explicit := 5 * time.Second
	cfg := moduleConfig{Endpoints: map[string]endpointConfig{
		"absent":   {Address: ":1"},
		"switched": {Address: ":2", IdleTimeout: &off},
		"given":    {Address: ":3", IdleTimeout: &explicit},
	}}

	settings, err := cfg.resolve()
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	byName := make(map[string]endpointSettings, len(settings))
	for _, s := range settings {
		byName[s.name] = s
	}

	if byName["absent"].idleTimeout != defaultIdleTimeout {
		t.Errorf("an absent idle-timeout resolved to %v, and the default is %v",
			byName["absent"].idleTimeout, defaultIdleTimeout)
	}
	if byName["switched"].idleTimeout != 0 {
		t.Errorf("an explicit 0 resolved to %v; 0 is how the bound is switched off",
			byName["switched"].idleTimeout)
	}
	if byName["given"].idleTimeout != explicit {
		t.Errorf("an explicit %v resolved to %v", explicit, byName["given"].idleTimeout)
	}
}

// TestEndpointsResolveInNameOrder pins the order everything downstream inherits:
// the order endpoints are bound in, the order they are stopped in, and the order
// a diagnostic lists them in. Left to map iteration, each run would differ.
func TestEndpointsResolveInNameOrder(t *testing.T) {
	cfg := moduleConfig{Endpoints: map[string]endpointConfig{
		"public": {Address: ":1"}, "admin": {Address: ":2"}, "metrics": {Address: ":3"},
	}}
	settings, err := cfg.resolve()
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	got := make([]string, 0, len(settings))
	for _, s := range settings {
		got = append(got, s.name)
	}
	if want := []string{"admin", "metrics", "public"}; !equalStrings(got, want) {
		t.Errorf("the endpoints resolved in the order %v, and name order is %v", got, want)
	}
}

// TestMissingAddressIsStartupFailure pins the one item with no default. A
// message that only said "missing address" would leave the host counting
// entries in its own file.
func TestMissingAddressIsStartupFailure(t *testing.T) {
	cfg := moduleConfig{Endpoints: map[string]endpointConfig{"admin": {}}}
	_, err := cfg.resolve()
	if err == nil {
		t.Fatal("an endpoint with no address was accepted")
	}
	mustContain(t, err.Error(), `"admin"`, "the endpoint that has no address")
	mustContain(t, err.Error(), "http.endpoints.admin.address", "the key to write")
}

// TestReadHeaderTimeoutAlwaysSet pins the bound on a server this module builds.
// A server without it is held open indefinitely by a client sending its headers
// one byte at a time, and the endpoint goes on reporting that it accepts
// requests while the connections pile up.
func TestReadHeaderTimeoutAlwaysSet(t *testing.T) {
	settings, err := moduleConfig{Endpoints: map[string]endpointConfig{
		"public": {Address: "127.0.0.1:0"},
	}}.resolve()
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	logger, _ := newRecordingLogger()
	l, _ := startTestEndpoint(t, settings[0], nethttp.NotFoundHandler(), logger)

	if l.server.ReadHeaderTimeout == 0 {
		t.Error("the server was built without a header timeout")
	}
	if l.server.ReadHeaderTimeout != defaultReadHeaderTimeout {
		t.Errorf("the header timeout is %v, and the default is %v",
			l.server.ReadHeaderTimeout, defaultReadHeaderTimeout)
	}
}

// TestReadHeaderTimeoutCannotBeSwitchedOff pins one of the two bounds that have
// no "off" state. The criterion is who carries the consequence of switching the
// item off: this one is carried by whoever reaches the endpoint, not by the host
// that wrote the 0, so the 0 is a configuration mistake and the message says why.
func TestReadHeaderTimeoutCannotBeSwitchedOff(t *testing.T) {
	off := time.Duration(0)
	_, err := moduleConfig{Endpoints: map[string]endpointConfig{
		"public": {Address: ":8080", ReadHeaderTimeout: &off},
	}}.resolve()
	if err == nil {
		t.Fatal("read-header-timeout was switched off with 0")
	}
	mustContain(t, err.Error(), "read-header-timeout", "the item that cannot be switched off")
	mustContain(t, err.Error(), "one byte at a time", "why it cannot")
}

// TestDrainTimeoutRefusesExplicitZero pins the second of the two bounds that
// cannot be switched off, and pins it through the real config module: what makes
// the refusal reachable at all is that config delivers an explicit "0s" as a
// pointer to 0 rather than as an absent key, which is the difference between
// "switch it off" and "take the default".
//
// Switching this one off would have Close wait forever on a request that cannot
// finish: the context Stop and Close receive has its cancellation stripped and
// the registry imposes no shutdown timeout of its own, so a host blocked in Run
// has nothing left to interrupt it with. The drain timeout is the item
// adr-http-shutdown names as the one that meets "a callback has to be bounded".
func TestDrainTimeoutRefusesExplicitZero(t *testing.T) {
	got, decodeErr, runErr := readThrough(t,
		`{"http":{"endpoints":{"public":{"address":":8080","drain-timeout":"0s"}}}}`, schema())
	if runErr != nil {
		t.Fatalf("the run failed: %v", runErr)
	}
	if decodeErr != nil {
		t.Fatalf("decoding this module's section: %v", decodeErr)
	}
	given := got.Endpoints["public"].DrainTimeout
	if given == nil {
		t.Fatal("config delivered an explicit \"0s\" as an absent key, so the endpoint would " +
			"silently take the default instead of being refused")
	}
	if *given != 0 {
		t.Fatalf("an explicit \"0s\" arrived as %v", *given)
	}

	// resolve is what Prepare runs the decoded section through, so its
	// refusal is the startup failure.
	_, err := got.resolve()
	if err == nil {
		t.Fatal("drain-timeout was switched off with 0")
	}
	mustContain(t, err.Error(), "drain-timeout", "the item that cannot be switched off")
	mustContain(t, err.Error(), `"public"`, "the endpoint it was given on")
	mustContain(t, err.Error(), "for ever", "why it cannot be switched off")
}

// TestDrainTimeoutAbsentTakesTheDefault and its positive-value twin below are
// what keep the refusal aimed at the explicit 0 alone. An implementation that
// made the item mandatory, or refused it outright, would fail these two while
// still passing the refusal test above.
func TestDrainTimeoutAbsentTakesTheDefault(t *testing.T) {
	settings, err := moduleConfig{Endpoints: map[string]endpointConfig{
		"public": {Address: ":8080"},
	}}.resolve()
	if err != nil {
		t.Fatalf("an endpoint that gives no drain-timeout was refused: %v", err)
	}
	if settings[0].drainTimeout != defaultDrainTimeout {
		t.Errorf("an absent drain-timeout resolved to %v, and the default is %v",
			settings[0].drainTimeout, defaultDrainTimeout)
	}
}

func TestDrainTimeoutAcceptsAPositiveValue(t *testing.T) {
	given := 90 * time.Second
	settings, err := moduleConfig{Endpoints: map[string]endpointConfig{
		"public": {Address: ":8080", DrainTimeout: &given},
	}}.resolve()
	if err != nil {
		t.Fatalf("a positive drain-timeout was refused: %v", err)
	}
	if settings[0].drainTimeout != given {
		t.Errorf("a drain-timeout of %v resolved to %v", given, settings[0].drainTimeout)
	}
}

// TestOtherDurationsStillAcceptExplicitZero pins that the exceptions are exactly
// two. Every other duration takes 0 as "this bound is off", and the consequence
// of switching one off is carried by the endpoint that asked for it. Refusing a
// third item fails this test item by item.
func TestOtherDurationsStillAcceptExplicitZero(t *testing.T) {
	off := time.Duration(0)
	cases := []struct {
		key   string
		give  func(*endpointConfig)
		taken func(endpointSettings) time.Duration
	}{
		{
			"read-timeout",
			func(e *endpointConfig) { e.ReadTimeout = &off },
			func(s endpointSettings) time.Duration { return s.readTimeout },
		},
		{
			"write-timeout",
			func(e *endpointConfig) { e.WriteTimeout = &off },
			func(s endpointSettings) time.Duration { return s.writeTimeout },
		},
		{
			"idle-timeout",
			func(e *endpointConfig) { e.IdleTimeout = &off },
			func(s endpointSettings) time.Duration { return s.idleTimeout },
		},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			given := endpointConfig{Address: ":8080"}
			c.give(&given)
			settings, err := moduleConfig{Endpoints: map[string]endpointConfig{
				"public": given,
			}}.resolve()
			if err != nil {
				t.Fatalf("an explicit 0 for %s was refused: %v", c.key, err)
			}
			if taken := c.taken(settings[0]); taken != 0 {
				t.Errorf("an explicit 0 for %s resolved to %v; 0 is how the bound is "+
					"switched off", c.key, taken)
			}
		})
	}
}

// TestNegativeDurationIsRejected pins the difference between switching a bound
// off and mistyping it. A negative deadline is already in the past, so net/http
// would fail every request on it.
func TestNegativeDurationIsRejected(t *testing.T) {
	negative := -time.Second
	_, err := moduleConfig{Endpoints: map[string]endpointConfig{
		"public": {Address: ":8080", WriteTimeout: &negative},
	}}.resolve()
	if err == nil {
		t.Fatal("a negative write-timeout was accepted")
	}
	mustContain(t, err.Error(), "write-timeout", "the item that was mistyped")
	mustContain(t, err.Error(), `"public"`, "the endpoint it was given on")
}

// TestHalfConfiguredTLSIsRejected pins the pair. One half alone is a mistake
// either way round: serving plain HTTP on an endpoint the host meant to be
// encrypted is the failure that has to be loud.
func TestHalfConfiguredTLSIsRejected(t *testing.T) {
	for _, given := range []endpointConfig{
		{Address: ":8443", TLSCertFile: "/etc/cert.pem"},
		{Address: ":8443", TLSKeyFile: "/etc/key.pem"},
	} {
		_, err := moduleConfig{Endpoints: map[string]endpointConfig{"public": given}}.resolve()
		if err == nil {
			t.Fatalf("half a TLS configuration was accepted: %+v", given)
		}
		mustContain(t, err.Error(), "tls-cert-file", "the pair that has to be given together")
	}
}

// TestNoEndpointIsNotAFailure pins that an empty endpoint set resolves cleanly.
// It is the input the disabled stance is made of, and a failure here would turn
// "this host runs no entry point" into a broken startup.
func TestNoEndpointIsNotAFailure(t *testing.T) {
	settings, err := moduleConfig{}.resolve()
	if err != nil {
		t.Fatalf("an empty endpoint set failed to resolve: %v", err)
	}
	if len(settings) != 0 {
		t.Errorf("an empty endpoint set resolved to %d endpoints", len(settings))
	}
}
