package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// loaderTestRegistry returns a registry carrying one probe component that
// declares the bootstrap keys a test's host target binds, plus the given
// extra components.
func loaderTestRegistry(t *testing.T, extra ...pkgcore.Component) *pkgcore.ComponentRegistry {
	t.Helper()
	reg := pkgcore.NewComponentRegistry()
	for _, c := range extra {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register %q: %v", c.Name, err)
		}
	}
	return reg
}

// probeComponent is a minimal registered component: a product, and the
// bootstrap keys the test hands it.
func probeComponent(name string, keys ...pkgcore.BootstrapKey) pkgcore.Component {
	return pkgcore.Component{
		Name:          name,
		BootstrapKeys: keys,
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &transitionMarker{name: name}, nil
		},
	}
}

// compositionOf reads the composition configuration Load published.
func compositionOf(t *testing.T, reg *pkgcore.ComponentRegistry) pkgcore.ComponentConfig {
	t.Helper()
	composition, err := pkgcore.Get[pkgcore.ComponentConfig](reg)
	if err != nil {
		t.Fatalf("read the published composition configuration: %v", err)
	}
	return composition
}

// componentBlockOf reads the components block out of a composition.
func componentBlockOf(t *testing.T, composition pkgcore.ComponentConfig) pkgcore.ComponentConfig {
	t.Helper()
	raw, ok := composition.Get("components")
	if !ok {
		t.Fatal("the composition carries no components block")
	}
	return asComponentConfig(raw)
}

// TestLoad_TheFiveSourcesLayInOrder pins the layering: the builtin default
// stands when nothing supplies a value, the project file beats it, the
// environment beats the file, the command line beats the environment, and
// the host's code override beats everything.
func TestLoad_TheFiveSourcesLayInOrder(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "composition.yaml")
	contents := "composition:\n  deployment: distributed\n  components:\n    probe:\n      token: from-file\n"
	if err := os.WriteFile(filePath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write the composition file: %v", err)
	}
	t.Setenv("TEST_COMPOSITION__DEPLOYMENT", "standalone")
	t.Setenv("TEST_COMPOSITION__COMPONENTS__PROBE__TOKEN", "from-env")

	var host testHostConfig
	host.PlatformConfig = testPlatformConfig()
	reg := loaderTestRegistry(t, probeComponent("probe"))
	override := pkgcore.ComponentConfig{}.With("components", pkgcore.ComponentConfig{}.
		With("probe", pkgcore.ComponentConfig{}.With("token", "from-code")))
	reg.Put(CompositionOverrides{Config: override})

	spec := LoadSpec{
		Host:     &host,
		Platform: &host.PlatformConfig,
		Options: append(testConfigOptions(),
			ConfigFile(filePath),
			ConfigArgs([]string{"--composition.components.probe.token=from-flag"}),
		),
		Args: []string{"--composition.components.probe.token=from-flag"},
	}
	if err := Load(context.Background(), reg, spec); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	composition := compositionOf(t, reg)
	deployment, _ := composition.Get("deployment")
	if deployment != "standalone" {
		t.Errorf("deployment = %v, want the environment's value over the file's", deployment)
	}
	block := componentBlockOf(t, composition)
	probe := asComponentConfig(mustGet(t, block, "probe"))
	token, _ := probe.Get("token")
	if token != "from-code" {
		t.Errorf("probe.token = %v, want the code override to win the whole chain", token)
	}
}

// TestLoad_BuiltinDefaultsStandWhenNothingSuppliesValues pins the lowest
// layer: the standalone deployment and the default-participating
// observability component.
func TestLoad_BuiltinDefaultsStandWhenNothingSuppliesValues(t *testing.T) {
	var host testHostConfig
	host.PlatformConfig = testPlatformConfig()
	reg := loaderTestRegistry(t)

	if err := Load(context.Background(), reg, LoadSpec{
		Host:     &host,
		Platform: &host.PlatformConfig,
		Options:  testConfigOptions(),
		Args:     []string{},
	}); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	composition := compositionOf(t, reg)
	deployment, ok := composition.Get("deployment")
	if !ok || deployment != string(pkgcore.DeploymentModeStandalone) {
		t.Errorf("deployment = %v (present %v), want the builtin standalone default", deployment, ok)
	}
	block := componentBlockOf(t, composition)
	obs, ok := block.Get("observability")
	if !ok || obs != nil {
		t.Errorf("components.observability = %v (present %v), want the builtin selection with no configuration", obs, ok)
	}
}

// TestLoad_ReadsTheQ8EnvSpelling pins the environment spelling: a component
// name's dots are underscored, and each nesting level is the double
// underscore. The mailer.smtp name is the built-in pkgcore ships, so the
// registry needs no probe for it.
func TestLoad_ReadsTheQ8EnvSpelling(t *testing.T) {
	t.Setenv("TEST_COMPOSITION__COMPONENTS__MAILER_SMTP__HOST", "mail.example.com")
	t.Setenv("TEST_COMPOSITION__COMPONENTS__MAILER_SMTP__PORT", "587")

	var host testHostConfig
	host.PlatformConfig = testPlatformConfig()
	reg := loaderTestRegistry(t)

	if err := Load(context.Background(), reg, LoadSpec{
		Host:     &host,
		Platform: &host.PlatformConfig,
		Options:  testConfigOptions(),
		Args:     []string{},
	}); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	block := componentBlockOf(t, compositionOf(t, reg))
	mailer := asComponentConfig(mustGet(t, block, "mailer.smtp"))
	if host, _ := mailer.Get("host"); host != "mail.example.com" {
		t.Errorf("mailer.smtp.host = %v, want the APP_COMPOSITION__COMPONENTS__MAILER_SMTP__HOST value", host)
	}
	if port, _ := mailer.Get("port"); port != "587" {
		t.Errorf("mailer.smtp.port = %v, want the underscored spelling to carry both keys", port)
	}
}

// TestLoad_ReadsTheQ8FlagSpelling pins the command-line spelling: the
// component name keeps its literal dots, matched by longest registered
// prefix, and a bare selection value reads as its boolean. The mailer.smtp
// name is the built-in pkgcore ships; only the bare "mailer" name needs a
// probe.
func TestLoad_ReadsTheQ8FlagSpelling(t *testing.T) {
	var host testHostConfig
	host.PlatformConfig = testPlatformConfig()
	reg := loaderTestRegistry(t, probeComponent("mailer"))

	args := []string{
		"--composition.components.mailer.smtp.host=mail.example.com",
		"--composition.components.mailer.smtp.tls.required=true",
		"--composition.components.mailer=false",
	}
	if err := Load(context.Background(), reg, LoadSpec{
		Host:     &host,
		Platform: &host.PlatformConfig,
		Options:  testConfigOptions(),
		Args:     args,
	}); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	block := componentBlockOf(t, compositionOf(t, reg))
	mailer := asComponentConfig(mustGet(t, block, "mailer.smtp"))
	if host, _ := mailer.Get("host"); host != "mail.example.com" {
		t.Errorf("mailer.smtp.host = %v, want the longest-prefix name match to keep the dots literal", host)
	}
	tls := asComponentConfig(mustGet(t, mailer, "tls"))
	if required, _ := tls.Get("required"); required != "true" {
		t.Errorf("mailer.smtp.tls.required = %v, want the nested key path", required)
	}
	if selection, _ := block.Get("mailer"); selection != false {
		t.Errorf("components.mailer = %v (%T), want false to deselect it", selection, selection)
	}
}

// TestLoad_ResolvesBootstrapMaterial pins the material source: every
// registered component's declared key resolves through the loader's own
// chain -- the environment's spelling for the host's own key, the platform
// target's prefilled material for a declared platform path -- and is
// published by key path and by derivation purpose.
func TestLoad_ResolvesBootstrapMaterial(t *testing.T) {
	t.Setenv("TEST_TOKEN", "host-token-value")

	var host testHostConfig
	host.PlatformConfig = testPlatformConfig()
	wantKey := host.Authn.Blind_Index_Key
	reg := loaderTestRegistry(t, probeComponent("probe",
		pkgcore.BootstrapKey{Key: "token", Format: "string"},
		pkgcore.BootstrapKey{Key: "authn.blind_index_key", Format: "hexkey"},
	))

	if err := Load(context.Background(), reg, LoadSpec{
		Host:     &host,
		Platform: &host.PlatformConfig,
		Options:  testConfigOptions(),
		Args:     []string{},
	}); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	material, err := pkgcore.BootstrapMaterialOf(reg)
	if err != nil {
		t.Fatalf("read the published material source: %v", err)
	}
	if value, ok := material.Value("token"); !ok || value != "host-token-value" {
		t.Errorf("material token = %v (present %v), want the host target's resolved value", value, ok)
	}
	key, ok := material.Material("authn.blind_index_key")
	if !ok || len(key) != len(wantKey) {
		t.Fatalf("material authn.blind_index_key = %d bytes (present %v), want the platform target's prefilled material", len(key), ok)
	}
	for i := range key {
		if key[i] != wantKey[i] {
			t.Fatalf("material authn.blind_index_key byte %d = %d, want %d", i, key[i], wantKey[i])
		}
	}
	purpose, err := pkgcore.BootstrapKeyPurpose("token")
	if err != nil {
		t.Fatalf("compose the purpose: %v", err)
	}
	if value, ok := material.ValueForPurpose(purpose); !ok || value != "host-token-value" {
		t.Errorf("material for purpose %q = %v (present %v), want the by-purpose reading", purpose, value, ok)
	}
}

// TestLoad_RefusesAnUnboundDeclaredKey pins the binding verification's
// four-element failure: the stage, the declaring component, the unbound key
// and the remedy, all named.
func TestLoad_RefusesAnUnboundDeclaredKey(t *testing.T) {
	var host testHostConfig
	host.PlatformConfig = testPlatformConfig()
	reg := loaderTestRegistry(t, probeComponent("probe",
		pkgcore.BootstrapKey{Key: "probe.unbound", Format: "string"},
	))

	err := Load(context.Background(), reg, LoadSpec{
		Host:     &host,
		Platform: &host.PlatformConfig,
		Options:  testConfigOptions(),
		Args:     []string{},
	})
	if err == nil {
		t.Fatal("Load() with an unbound declared key error = nil, want a binding refusal")
	}
	for _, fragment := range []string{"stage prepare", `component "probe"`, "probe.unbound", "add a field for that key path"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("binding refusal misses %q: %v", fragment, err)
		}
	}
}

// TestLoad_RefusesAnInvalidDeclaredKeyPath pins the declaration-shape
// failure: a key path with an empty segment is refused, naming the declaring
// component.
func TestLoad_RefusesAnInvalidDeclaredKeyPath(t *testing.T) {
	var host testHostConfig
	host.PlatformConfig = testPlatformConfig()
	reg := loaderTestRegistry(t, probeComponent("probe",
		pkgcore.BootstrapKey{Key: "probe..double", Format: "string"},
	))

	err := Load(context.Background(), reg, LoadSpec{
		Host:     &host,
		Platform: &host.PlatformConfig,
		Options:  testConfigOptions(),
		Args:     []string{},
	})
	if err == nil || !strings.Contains(err.Error(), "probe..double") {
		t.Fatalf("Load() with an invalid declared key path error = %v, want one naming the path", err)
	}
	if !errors.Is(err, pkgcore.ErrInvalidBootstrapKeyPath) {
		t.Fatalf("refusal = %v, want it to wrap pkgcore.ErrInvalidBootstrapKeyPath", err)
	}
}

// TestLoad_RequiresAHostTarget pins the entry validation.
func TestLoad_RequiresAHostTarget(t *testing.T) {
	reg := loaderTestRegistry(t)
	err := Load(context.Background(), reg, LoadSpec{Options: testConfigOptions()})
	if err == nil || !strings.Contains(err.Error(), "LoadSpec.Host") {
		t.Fatalf("Load() without a host target error = %v, want one naming LoadSpec.Host", err)
	}
}

// TestLoad_RefusesABadCompositionValue pins the composition layer's refusal:
// a value a text source supplied that does not fit its shape fails the load
// rather than reaching the plan.
func TestLoad_RefusesABadCompositionValue(t *testing.T) {
	t.Setenv("TEST_COMPOSITION__STRICT", "not-a-bool")
	var host testHostConfig
	host.PlatformConfig = testPlatformConfig()
	reg := loaderTestRegistry(t)

	err := Load(context.Background(), reg, LoadSpec{
		Host:     &host,
		Platform: &host.PlatformConfig,
		Options:  testConfigOptions(),
		Args:     []string{},
	})
	if err == nil || !strings.Contains(err.Error(), "strict") {
		t.Fatalf("Load() with a malformed strict value error = %v, want one naming the key", err)
	}
}

// TestObservabilityComponent_RunsItsWholeLifecycle pins the engine's
// observability component end to end: selected with its configuration block,
// its Prepare initializes the providers and publishes the runtime, New hands
// the runtime back, and Close shuts the providers down.
func TestObservabilityComponent_RunsItsWholeLifecycle(t *testing.T) {
	var host testHostConfig
	spec := driverLoadSpec(t, &host)
	spec.Overrides = &CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components",
		pkgcore.ComponentConfig{}.With("observability", pkgcore.ComponentConfig{}.
			With("service_name", "app-observability-test")))}

	reg := pkgcore.NewComponentRegistry()
	if err := Assemble(context.Background(), reg, spec); err != nil {
		t.Fatalf("Assemble() with the observability component selected error = %v", err)
	}
	if _, err := pkgcore.Get[*observabilityRuntime](reg); err != nil {
		t.Fatalf("Get[*observabilityRuntime] error = %v, want the runtime its Prepare published", err)
	}
	if err := Shutdown(context.Background(), reg); err != nil {
		t.Fatalf("Shutdown() error = %v, want the providers flushed cleanly", err)
	}
}

// mustGet reads a raw value out of a config, failing the test when the key
// is absent.
func mustGet(t *testing.T, c pkgcore.ComponentConfig, key string) any {
	t.Helper()
	value, ok := c.Get(key)
	if !ok {
		t.Fatalf("config key %q is absent (keys: %v)", key, c.Keys())
	}
	return value
}
