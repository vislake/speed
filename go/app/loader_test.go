package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	pkgconfig "github.com/vislake/speed/go/pkgcore/config"
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
			return &testMarker{name: name}, nil
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
	reg := loaderTestRegistry(t, probeComponent("probe"))
	override := pkgcore.ComponentConfig{}.With("components", pkgcore.ComponentConfig{}.
		With("probe", pkgcore.ComponentConfig{}.With("token", "from-code")))
	reg.Put(CompositionOverrides{Config: override})

	spec := LoadSpec{
		Host: &host,
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
	reg := loaderTestRegistry(t)

	if err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{},
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
	reg := loaderTestRegistry(t)

	if err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{},
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
	reg := loaderTestRegistry(t, probeComponent("mailer"))

	args := []string{
		"--composition.components.mailer.smtp.host=mail.example.com",
		"--composition.components.mailer.smtp.tls.required=true",
		"--composition.components.mailer=false",
	}
	if err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    args,
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
// chain -- a text value from the environment's spelling of the declared
// path, key material from its own variable or the declared defaults table --
// and is published by key path and by derivation purpose.
func TestLoad_ResolvesBootstrapMaterial(t *testing.T) {
	t.Setenv("TEST_TOKEN", "host-token-value")

	var host testHostConfig
	wantKey := testKey(0x77)
	t.Setenv("TEST_PROBE__BLIND_INDEX_KEY", hex.EncodeToString(wantKey))
	reg := loaderTestRegistry(t, probeComponent("probe",
		pkgcore.BootstrapKey{Key: "token", Format: "string"},
		pkgcore.BootstrapKey{Key: "probe.blind_index_key", Format: "hexkey"},
	))

	if err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{},
	}); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	material, err := pkgcore.BootstrapMaterialOf(reg)
	if err != nil {
		t.Fatalf("read the published material source: %v", err)
	}
	if value, ok := material.Value("token"); !ok || value != "host-token-value" {
		t.Errorf("material token = %v (present %v), want the environment's resolved value", value, ok)
	}
	if key, ok := material.Material("probe.blind_index_key"); !ok || !bytes.Equal(key, wantKey) {
		t.Errorf("material probe.blind_index_key = %x (present %v), want the injected value", key, ok)
	}
	if key, ok := material.Material(testCipherKeyPath); !ok || !bytes.Equal(key, testDevDefaults()[testCipherKeyPath]) {
		t.Errorf("material %s = %x (present %v), want the declared defaults table's value", testCipherKeyPath, key, ok)
	}
	purpose, err := pkgcore.BootstrapKeyPurpose("token")
	if err != nil {
		t.Fatalf("compose the purpose: %v", err)
	}
	if value, ok := material.ValueForPurpose(purpose); !ok || value != "host-token-value" {
		t.Errorf("material for purpose %q = %v (present %v), want the by-purpose reading", purpose, value, ok)
	}
}

// schemaKeySchema is the loader tests' schema fixture: one derive-tagged
// key-material field, the shape a component declares its own key at.
type schemaKeySchema struct {
	BlindIndexKey []byte `json:"blind_index_key" config:"derive,sensitive"`
}

// ConfigDocs implements pkgcore.Documented for the fixture, the pairing a
// sensitive field is required to carry.
func (*schemaKeySchema) ConfigDocs() map[string]pkgcore.FieldDoc {
	return map[string]pkgcore.FieldDoc{
		"blind_index_key": {Description: "the fixture's key material", Default: "documented non-secret development default"},
	}
}

// schemaKeyComponent is a registered component that declares its key
// material as a ConfigSchema derive field under the given namespace.
func schemaKeyComponent(name, namespace string) pkgcore.Component {
	return pkgcore.Component{
		Name:            name,
		ConfigNamespace: namespace,
		ConfigSchema:    (*schemaKeySchema)(nil),
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &testMarker{name: name}, nil
		},
	}
}

// TestLoad_ResolvesSchemaDeclaredKeyMaterial pins the second declaration
// face: a component's ConfigSchema derive field resolves into the published
// material at the field's final key path -- the namespace prefix the
// component declares, then the field's local key path -- read from the
// variable that path derives, so a component's Prepare callback reads the
// same material the same way whether the component declares the key on its
// BootstrapKeys seat or as its own configuration field.
func TestLoad_ResolvesSchemaDeclaredKeyMaterial(t *testing.T) {
	t.Run("a module namespace keeps the platform key path", func(t *testing.T) {
		wantKey := testKey(0x55)
		envName := pkgconfig.EnvName(testEnvPrefix, "schemakey.blind_index_key")
		t.Setenv(envName, hex.EncodeToString(wantKey))

		var host testHostConfig
		reg := loaderTestRegistry(t, schemaKeyComponent("schemakey", "schemakey"))

		if err := Load(context.Background(), reg, LoadSpec{
			Host:    &host,
			Options: testConfigOptions(),
			Args:    []string{},
		}); err != nil {
			t.Fatalf("Load() error = %v", err)
		}

		material, err := pkgcore.BootstrapMaterialOf(reg)
		if err != nil {
			t.Fatalf("read the published material source: %v", err)
		}
		if key, ok := material.Material("schemakey.blind_index_key"); !ok || !bytes.Equal(key, wantKey) {
			t.Errorf("material schemakey.blind_index_key = %x (present %v), want the value set as %s", key, ok, envName)
		}
	})

	t.Run("the default namespace prefixes the component name", func(t *testing.T) {
		wantKey := testKey(0x66)
		envName := pkgconfig.EnvName(testEnvPrefix, "components.schemakey.blind_index_key")
		t.Setenv(envName, hex.EncodeToString(wantKey))

		var host testHostConfig
		reg := loaderTestRegistry(t, schemaKeyComponent("schemakey", ""))

		if err := Load(context.Background(), reg, LoadSpec{
			Host:    &host,
			Options: testConfigOptions(),
			Args:    []string{},
		}); err != nil {
			t.Fatalf("Load() error = %v", err)
		}

		material, err := pkgcore.BootstrapMaterialOf(reg)
		if err != nil {
			t.Fatalf("read the published material source: %v", err)
		}
		if key, ok := material.Material("components.schemakey.blind_index_key"); !ok || !bytes.Equal(key, wantKey) {
			t.Errorf("material components.schemakey.blind_index_key = %x (present %v), want the value set as %s", key, ok, envName)
		}
	})

	t.Run("one declaration carried by two components is one entry", func(t *testing.T) {
		wantKey := testKey(0x77)
		envName := pkgconfig.EnvName(testEnvPrefix, "schemakey.blind_index_key")
		t.Setenv(envName, hex.EncodeToString(wantKey))

		var host testHostConfig
		reg := loaderTestRegistry(t,
			schemaKeyComponent("schemakey", "schemakey"),
			schemaKeyComponent("host.schemakey", "schemakey"),
		)

		if err := Load(context.Background(), reg, LoadSpec{
			Host:    &host,
			Options: testConfigOptions(),
			Args:    []string{},
		}); err != nil {
			t.Fatalf("Load() error = %v", err)
		}

		material, err := pkgcore.BootstrapMaterialOf(reg)
		if err != nil {
			t.Fatalf("read the published material source: %v", err)
		}
		if key, ok := material.Material("schemakey.blind_index_key"); !ok || !bytes.Equal(key, wantKey) {
			t.Errorf("material schemakey.blind_index_key = %x (present %v), want the value set as %s", key, ok, envName)
		}
	})
}

// badTagSchema is a schema whose tag vocabulary cannot be described: the
// loader's gather reports it as the component's own declaration problem
// rather than resolving half of it.
type badTagSchema struct {
	Key []byte `json:"key" config:"nonsense"`
}

// TestLoad_RefusesASchemaWhoseKeyMaterialCannotBeRead pins the refusal of a
// component whose ConfigSchema cannot be projected: the loader names the
// component and the reason, at the stage, before anything is constructed --
// the same treatment a malformed BootstrapKeys declaration gets.
func TestLoad_RefusesASchemaWhoseKeyMaterialCannotBeRead(t *testing.T) {
	var host testHostConfig
	c := schemaKeyComponent("brokenkey", "brokenkey")
	c.ConfigSchema = (*badTagSchema)(nil)
	reg := loaderTestRegistry(t, c)

	err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{},
	})
	if err == nil {
		t.Fatal("Load() with an undescribable schema error = nil, want a refusal")
	}
	for _, want := range []string{"stage prepare", `component "brokenkey"`, "cannot be read"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal misses %q: %v", want, err)
		}
	}
}

// schemaBootstrapKeysFor drives the declaration gather for one component
// directly, the white-box reading the Load-level tests cannot reach.
func schemaBootstrapKeysFor(t *testing.T, reg *pkgcore.ComponentRegistry, c pkgcore.Component) ([]pkgcore.BootstrapKey, error) {
	t.Helper()
	return schemaBootstrapKeys(reg, c)
}

// TestSchemaBootstrapKeys_RefusesTheUndescribable pins the gather's two
// failure returns: a schema the projection cannot read, and a component the
// registry does not carry (no namespace to resolve the fields under).
func TestSchemaBootstrapKeys_RefusesTheUndescribable(t *testing.T) {
	reg := loaderTestRegistry(t, schemaKeyComponent("schemakey", "schemakey"))

	if _, err := schemaBootstrapKeysFor(t, reg, pkgcore.Component{Name: "schemakey", ConfigSchema: (*badTagSchema)(nil)}); err == nil {
		t.Error("an undescribable schema = nil error, want the projection's refusal")
	}
	if _, err := schemaBootstrapKeysFor(t, reg, pkgcore.Component{Name: "ghost", ConfigSchema: (*schemaKeySchema)(nil)}); err == nil {
		t.Error("an unregistered component = nil error, want the namespace refusal")
	}
}

// TestLoad_LeavesAnUnresolvedDeclaredKeyAbsent pins the semantic the
// declaration removed the binding check for: a declared key no source
// supplies and no table entry stands for is not a failure -- the assembly
// publishes no value for it, and the consumer that needs it reports the
// missing material itself.
func TestLoad_LeavesAnUnresolvedDeclaredKeyAbsent(t *testing.T) {
	var host testHostConfig
	reg := loaderTestRegistry(t, probeComponent("probe",
		pkgcore.BootstrapKey{Key: "probe.unbound", Format: "string"},
	))

	if err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{},
	}); err != nil {
		t.Fatalf("Load() error = %v, want a declaration with no supplied value to resolve to nothing", err)
	}

	material, err := pkgcore.BootstrapMaterialOf(reg)
	if err != nil {
		t.Fatalf("read the published material source: %v", err)
	}
	if value, ok := material.Value("probe.unbound"); ok {
		t.Errorf("material probe.unbound = %v, want the key absent: no source supplied it", value)
	}
}

// TestLoad_RefusesAnInvalidDeclaredKeyPath pins the declaration-shape
// failure: a key path with an empty segment is refused, naming the declaring
// component, and the legacy purpose error stays in the chain.
func TestLoad_RefusesAnInvalidDeclaredKeyPath(t *testing.T) {
	var host testHostConfig
	reg := loaderTestRegistry(t, probeComponent("probe",
		pkgcore.BootstrapKey{Key: "probe..double", Format: "string"},
	))

	err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{},
	})
	if err == nil || !strings.Contains(err.Error(), "probe..double") {
		t.Fatalf("Load() with an invalid declared key path error = %v, want one naming the path", err)
	}
	if !errors.Is(err, pkgcore.ErrInvalidBootstrapKeyPath) {
		t.Fatalf("refusal = %v, want it to wrap pkgcore.ErrInvalidBootstrapKeyPath", err)
	}
	for _, fragment := range []string{"stage prepare", `component "probe"`} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("refusal misses %q: %v", fragment, err)
		}
	}
}

// TestLoad_RefusesAnUnknownDeclaredFormat pins the format closed set's
// four-element failure: the stage, the declaring component, the offending
// format and the accepted set.
func TestLoad_RefusesAnUnknownDeclaredFormat(t *testing.T) {
	var host testHostConfig
	reg := loaderTestRegistry(t, probeComponent("probe",
		pkgcore.BootstrapKey{Key: "probe.timeout", Format: "duration"},
	))

	err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{},
	})
	if err == nil {
		t.Fatal("Load() with an unknown declared format error = nil, want a refusal")
	}
	for _, fragment := range []string{"stage prepare", `component "probe"`, `"duration"`, `"hexkey"`} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("format refusal misses %q: %v", fragment, err)
		}
	}
}

// TestLoad_RefusesASensitiveDeclarationWithoutADescription pins the pairing
// rule the Component world had never enforced before the loader reads its
// declarations: a secret key whose contract is unwritten is refused, naming
// the component.
func TestLoad_RefusesASensitiveDeclarationWithoutADescription(t *testing.T) {
	var host testHostConfig
	reg := loaderTestRegistry(t, probeComponent("probe",
		pkgcore.BootstrapKey{Key: "probe.secret", Format: "string", Sensitive: true},
	))

	err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{},
	})
	if err == nil {
		t.Fatal("Load() with a Sensitive declaration and no Description error = nil, want a refusal")
	}
	for _, fragment := range []string{"stage prepare", `component "probe"`, "probe.secret", "Sensitive", "Description"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("pairing refusal misses %q: %v", fragment, err)
		}
	}
}

// TestLoad_MergesIdenticalDeclarationsAcrossComponents pins the transition
// bridge's shape: a wrapper carries its module descriptor's own declarations,
// so the same declaration arrives twice, and two identical declarations are
// one declaration.
func TestLoad_MergesIdenticalDeclarationsAcrossComponents(t *testing.T) {
	var host testHostConfig
	decl := pkgcore.BootstrapKey{Key: "probe.shared", Format: "string"}
	t.Setenv("TEST_PROBE__SHARED", "shared-value")
	reg := loaderTestRegistry(t,
		probeComponent("probe", decl),
		probeComponent("probe.wrapper", decl),
	)

	if err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{},
	}); err != nil {
		t.Fatalf("Load() with one declaration carried twice error = %v, want it merged", err)
	}

	material, err := pkgcore.BootstrapMaterialOf(reg)
	if err != nil {
		t.Fatalf("read the published material source: %v", err)
	}
	if value, ok := material.Value("probe.shared"); !ok || value != "shared-value" {
		t.Errorf("material probe.shared = %v (present %v), want the merged declaration resolved", value, ok)
	}
	count := 0
	for _, path := range material.KeyPaths() {
		if path == "probe.shared" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("material key paths = %v, want the merged key once", material.KeyPaths())
	}
}

// TestLoad_RefusesConflictingDeclarations pins the other half of the merge
// rule: one key path two components declare differently has no single
// resolution, so the load names both components.
func TestLoad_RefusesConflictingDeclarations(t *testing.T) {
	var host testHostConfig
	reg := loaderTestRegistry(t,
		probeComponent("probe", pkgcore.BootstrapKey{Key: "probe.shared", Format: "string", Description: "one reading"}),
		probeComponent("probe.wrapper", pkgcore.BootstrapKey{Key: "probe.shared", Format: "string", Description: "another reading"}),
	)

	err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{},
	})
	if err == nil {
		t.Fatal("Load() with two differing declarations of one key error = nil, want a refusal")
	}
	for _, fragment := range []string{"stage prepare", `component "probe"`, `component "probe.wrapper"`, "probe.shared"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("conflict refusal misses %q: %v", fragment, err)
		}
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
	reg := loaderTestRegistry(t)

	err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{},
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

// TestLoad_RequiresARegistry pins the entry guard: the load publishes into a
// registry, so a nil one is refused before anything else runs.
func TestLoad_RequiresARegistry(t *testing.T) {
	var host testHostConfig
	err := Load(context.Background(), nil, LoadSpec{Host: &host, Options: testConfigOptions(), Args: []string{}})
	if err == nil || !strings.Contains(err.Error(), "component registry") {
		t.Fatalf("Load() with no registry error = %v, want one naming the registry", err)
	}
}

// TestLoad_RefusesACancelledContext pins the earliest refusal: an
// already-cancelled context fails the load before any source is read.
func TestLoad_RefusesACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var host testHostConfig
	err := Load(ctx, loaderTestRegistry(t), LoadSpec{Host: &host, Options: testConfigOptions(), Args: []string{}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Load() with a cancelled context error = %v, want context.Canceled", err)
	}
}

// TestLoad_RefusesAnUnparseableHostConfiguration pins the host load's own
// failure surface: a project file that cannot be parsed fails the load,
// naming the host configuration, before any composition or material
// resolution runs.
func TestLoad_RefusesAnUnparseableHostConfiguration(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(filePath, []byte("port: [unclosed\n"), 0o600); err != nil {
		t.Fatalf("write the unparseable file: %v", err)
	}
	var host testHostConfig
	err := Load(context.Background(), loaderTestRegistry(t), LoadSpec{
		Host:    &host,
		Options: append(testConfigOptions(), ConfigFile(filePath)),
		Args:    []string{},
	})
	if err == nil || !strings.Contains(err.Error(), "host configuration") {
		t.Fatalf("Load() with an unparseable file error = %v, want one naming the host configuration", err)
	}
}

// TestLoad_RefusesADeclaredValueItsFormatCannotRead pins the resolution
// failure: a source-supplied value that does not fit its declared format
// fails the load before anything is constructed, naming the key path.
func TestLoad_RefusesADeclaredValueItsFormatCannotRead(t *testing.T) {
	t.Setenv("TEST_PROBE__COUNT", "not-a-number")
	reg := loaderTestRegistry(t, probeComponent("probe",
		pkgcore.BootstrapKey{Key: "probe.count", Format: "int"},
	))
	var host testHostConfig
	err := Load(context.Background(), reg, LoadSpec{Host: &host, Options: testConfigOptions(), Args: []string{}})
	if err == nil || !strings.Contains(err.Error(), "probe.count") {
		t.Fatalf("Load() with an unreadable declared value error = %v, want one naming the key path", err)
	}
}

// testDerivation is the derivation function the root-key tests install: a
// deterministic 32-byte function of the root key and the declared key path,
// the shape dbkit.DeriveBootstrapKey fills in a deployment.
func testDerivation(rootKey []byte, keyPath string) ([]byte, error) {
	material := make([]byte, 32)
	copy(material, keyPath)
	copy(material[16:], rootKey)
	return material, nil
}

// TestLoad_DerivesDeclaredMaterialFromTheRootKey pins the derivation leg of
// the source chain: material no source supplies is derived from the root key
// the options install, through the installed derivation function.
func TestLoad_DerivesDeclaredMaterialFromTheRootKey(t *testing.T) {
	const keyPath = "probe.blind_index_key"
	rootKey := testKey(0x50)
	reg := loaderTestRegistry(t, probeComponent("probe",
		pkgcore.BootstrapKey{Key: keyPath, Format: "hexkey"},
	))
	var host testHostConfig
	if err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: append(testConfigOptions(), ConfigRootKey(rootKey), ConfigKeyDerivation(testDerivation)),
		Args:    []string{},
	}); err != nil {
		t.Fatalf("Load() with a root key error = %v", err)
	}

	material, err := pkgcore.BootstrapMaterialOf(reg)
	if err != nil {
		t.Fatalf("read the published material source: %v", err)
	}
	want, err := testDerivation(rootKey, keyPath)
	if err != nil {
		t.Fatalf("compose the expected material: %v", err)
	}
	if got, ok := material.Material(keyPath); !ok || !bytes.Equal(got, want) {
		t.Fatalf("material %s = %x (present %v), want the root key's derivation %x", keyPath, got, ok, want)
	}
}

// TestLoad_ReadsTheRootKeyFromItsNamedVariable pins the environment half of
// the root-key source: the named variable's 64 hexadecimal characters are
// read inside the same load and derive declared material exactly as an
// explicitly passed key does.
func TestLoad_ReadsTheRootKeyFromItsNamedVariable(t *testing.T) {
	const keyPath = "probe.blind_index_key"
	rootKey := testKey(0x60)
	t.Setenv("TEST_ROOT_KEY", hex.EncodeToString(rootKey))
	reg := loaderTestRegistry(t, probeComponent("probe",
		pkgcore.BootstrapKey{Key: keyPath, Format: "hexkey"},
	))
	var host testHostConfig
	if err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: append(testConfigOptions(), ConfigRootKeyEnv("TEST_ROOT_KEY"), ConfigKeyDerivation(testDerivation)),
		Args:    []string{},
	}); err != nil {
		t.Fatalf("Load() with a root-key variable error = %v", err)
	}

	material, err := pkgcore.BootstrapMaterialOf(reg)
	if err != nil {
		t.Fatalf("read the published material source: %v", err)
	}
	want, err := testDerivation(rootKey, keyPath)
	if err != nil {
		t.Fatalf("compose the expected material: %v", err)
	}
	if got, ok := material.Material(keyPath); !ok || !bytes.Equal(got, want) {
		t.Fatalf("material %s = %x (present %v), want the named variable's derivation %x", keyPath, got, ok, want)
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
