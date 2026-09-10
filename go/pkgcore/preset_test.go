package pkgcore

import "testing"

// TestPresetStandalone_NamesARegisteredImplementationForEverySeam pins the
// invariant NewKernel's zero-value default relies on: every key
// PresetStandalone sets must resolve through the matching package-level
// SeamRegistry with the entry's own Config, or a bare NewKernel().Bootstrap()
// would fail even though it is documented to start in seconds with nothing
// else running.
func TestPresetStandalone_NamesARegisteredImplementationForEverySeam(t *testing.T) {
	if _, _, err := EventBusRegistry.Build(PresetStandalone[presetKeyEventBus].Implementation, PresetStandalone[presetKeyEventBus].Config); err != nil {
		t.Errorf("EventBusRegistry.Build(%q) error = %v, want nil", PresetStandalone[presetKeyEventBus].Implementation, err)
	}
	if _, _, err := KVStoreRegistry.Build(PresetStandalone[presetKeyKVStore].Implementation, PresetStandalone[presetKeyKVStore].Config); err != nil {
		t.Errorf("KVStoreRegistry.Build(%q) error = %v, want nil", PresetStandalone[presetKeyKVStore].Implementation, err)
	}
	if _, _, err := MailerRegistry.Build(PresetStandalone[presetKeyMailer].Implementation, PresetStandalone[presetKeyMailer].Config); err != nil {
		t.Errorf("MailerRegistry.Build(%q) error = %v, want nil", PresetStandalone[presetKeyMailer].Implementation, err)
	}
	if _, _, err := ObjectStoreRegistry.Build(PresetStandalone[presetKeyObjectStore].Implementation, PresetStandalone[presetKeyObjectStore].Config); err != nil {
		t.Errorf("ObjectStoreRegistry.Build(%q) error = %v, want nil", PresetStandalone[presetKeyObjectStore].Implementation, err)
	}
}

// TestPresetStandalone_NoneOfItsImplementationsDeclareMultiReplicaSafe pins
// the reason a bare NewKernel(WithDeploymentMode(DeploymentModeDistributed))
// fails: PresetStandalone's whole point is a zero-external-dependency,
// single-process composition, so none of its four implementations may
// declare MultiReplicaSafe -- if one ever did, the capability-unsatisfied
// tests in registry_test.go documenting that failure would stop being true
// without this test catching the regression first.
func TestPresetStandalone_NoneOfItsImplementationsDeclareMultiReplicaSafe(t *testing.T) {
	// Four explicit checks rather than a table: a generic table cannot hold
	// four differently-typed SeamRegistry[T] values without boxing each one
	// through `any` behind its own adapter, which buys nothing a plain
	// sequence of four checks does not already give.
	if _, caps, err := EventBusRegistry.Build(PresetStandalone[presetKeyEventBus].Implementation, PresetStandalone[presetKeyEventBus].Config); err != nil {
		t.Fatalf("EventBusRegistry.Build() error = %v, want nil", err)
	} else if caps.Has(MultiReplicaSafe) {
		t.Errorf("eventbus %q declares MultiReplicaSafe, want PresetStandalone to stay single-process", PresetStandalone[presetKeyEventBus].Implementation)
	}
	if _, caps, err := KVStoreRegistry.Build(PresetStandalone[presetKeyKVStore].Implementation, PresetStandalone[presetKeyKVStore].Config); err != nil {
		t.Fatalf("KVStoreRegistry.Build() error = %v, want nil", err)
	} else if caps.Has(MultiReplicaSafe) {
		t.Errorf("kv %q declares MultiReplicaSafe, want PresetStandalone to stay single-process", PresetStandalone[presetKeyKVStore].Implementation)
	}
	if _, caps, err := MailerRegistry.Build(PresetStandalone[presetKeyMailer].Implementation, PresetStandalone[presetKeyMailer].Config); err != nil {
		t.Fatalf("MailerRegistry.Build() error = %v, want nil", err)
	} else if caps.Has(MultiReplicaSafe) {
		t.Errorf("mailer %q declares MultiReplicaSafe, want PresetStandalone to stay single-process", PresetStandalone[presetKeyMailer].Implementation)
	}
	if _, caps, err := ObjectStoreRegistry.Build(PresetStandalone[presetKeyObjectStore].Implementation, PresetStandalone[presetKeyObjectStore].Config); err != nil {
		t.Fatalf("ObjectStoreRegistry.Build() error = %v, want nil", err)
	} else if caps.Has(MultiReplicaSafe) {
		t.Errorf("objectstore %q declares MultiReplicaSafe, want PresetStandalone to stay single-process", PresetStandalone[presetKeyObjectStore].Implementation)
	}
}

// TestPresetDistributed_NamesAMultiReplicaSafeImplementationForEverySeam
// pins the complementary invariant: every seam PresetDistributed names must
// resolve to an implementation declaring MultiReplicaSafe, or
// WithPreset(PresetDistributed) alone could never satisfy
// DeploymentModeDistributed's requirement. The Redis-backed seams -- built
// by the eventbus/redis and kv/redis subpackages this test binary's own
// example_test.go blank-imports -- build from the entries' own (empty)
// Config: they fall back to a bare-minimum "localhost:6379" default, per
// each subpackage's own clientFromConfig doc comment. The entries carry no
// Config either way, so the SMTP and S3 seams -- which need cfg fields with
// no safe default -- are checked against a minimally-populated Config here,
// purely to reach their declared Capabilities; constructing a live mailer or
// store is not this test's concern.
func TestPresetDistributed_NamesAMultiReplicaSafeImplementationForEverySeam(t *testing.T) {
	if _, caps, err := EventBusRegistry.Build(PresetDistributed[presetKeyEventBus].Implementation, PresetDistributed[presetKeyEventBus].Config); err != nil {
		t.Errorf("EventBusRegistry.Build(%q) error = %v, want nil", PresetDistributed[presetKeyEventBus].Implementation, err)
	} else if !caps.Has(MultiReplicaSafe) {
		t.Errorf("eventbus %q capabilities = %v, want MultiReplicaSafe", PresetDistributed[presetKeyEventBus].Implementation, caps)
	}
	if _, caps, err := KVStoreRegistry.Build(PresetDistributed[presetKeyKVStore].Implementation, PresetDistributed[presetKeyKVStore].Config); err != nil {
		t.Errorf("KVStoreRegistry.Build(%q) error = %v, want nil", PresetDistributed[presetKeyKVStore].Implementation, err)
	} else if !caps.Has(MultiReplicaSafe) {
		t.Errorf("kv %q capabilities = %v, want MultiReplicaSafe", PresetDistributed[presetKeyKVStore].Implementation, caps)
	}
	if _, caps, err := MailerRegistry.Build(PresetDistributed[presetKeyMailer].Implementation, Config{"host": "smtp.example.com"}); err != nil {
		t.Errorf("MailerRegistry.Build(%q) error = %v, want nil", PresetDistributed[presetKeyMailer].Implementation, err)
	} else if !caps.Has(MultiReplicaSafe) {
		t.Errorf("mailer %q capabilities = %v, want MultiReplicaSafe", PresetDistributed[presetKeyMailer].Implementation, caps)
	}
	s3Cfg := Config{"endpoint": "s3.example.com", "bucket": "objects", "access_key": "ak", "secret_key": "sk"}
	if _, caps, err := ObjectStoreRegistry.Build(PresetDistributed[presetKeyObjectStore].Implementation, s3Cfg); err != nil {
		t.Errorf("ObjectStoreRegistry.Build(%q) error = %v, want nil", PresetDistributed[presetKeyObjectStore].Implementation, err)
	} else if !caps.Has(MultiReplicaSafe) {
		t.Errorf("objectstore %q capabilities = %v, want MultiReplicaSafe", PresetDistributed[presetKeyObjectStore].Implementation, caps)
	}
}

// TestPresetDistributed_SMTPAndS3EntryConfigsRequireConfig pins the
// documented gap PresetDistributed's own doc comment names: the mailer and
// object-store seams have no safe default credentials, and the entries the
// built-in preset carries supply no Config, so resolving either through the
// preset exactly as it ships fails with ErrMissingSeamConfig instead of
// silently building an unusable mailer or store.
func TestPresetDistributed_SMTPAndS3EntryConfigsRequireConfig(t *testing.T) {
	if _, _, err := MailerRegistry.Build(PresetDistributed[presetKeyMailer].Implementation, PresetDistributed[presetKeyMailer].Config); err == nil {
		t.Error("MailerRegistry.Build() with the preset entry's own Config succeeded, want ErrMissingSeamConfig")
	}
	if _, _, err := ObjectStoreRegistry.Build(PresetDistributed[presetKeyObjectStore].Implementation, PresetDistributed[presetKeyObjectStore].Config); err == nil {
		t.Error("ObjectStoreRegistry.Build() with the preset entry's own Config succeeded, want ErrMissingSeamConfig")
	}
}

// TestPreset_With_OverridesACopyAndLeavesTheReceiverUntouched pins the
// override method's two guarantees: the entry it sets is the one the
// returned preset carries, and the receiver -- in particular a package-level
// preset, the shape every caller actually starts from -- is never written
// through. Without the copy, PresetStandalone.With would rewrite the
// built-in default every later NewKernel() in the process depends on.
func TestPreset_With_OverridesACopyAndLeavesTheReceiverUntouched(t *testing.T) {
	base := Preset{
		presetKeyEventBus: {Implementation: "eventbus.memory"},
		presetKeyMailer:   {Implementation: "mailer.console"},
	}
	overridden := base.With(presetKeyMailer, SeamPreset{
		Implementation: "mailer.smtp",
		Config:         Config{"host": "smtp.example.com"},
	})

	if got := overridden[presetKeyMailer]; got.Implementation != "mailer.smtp" || got.Config["host"] != "smtp.example.com" {
		t.Errorf("overridden mailer entry = %+v, want the override visible through the returned preset", got)
	}
	if got := base[presetKeyMailer]; got.Implementation != "mailer.console" || got.Config != nil {
		t.Errorf("receiver's mailer entry = %+v, want the receiver untouched by With", got)
	}
	if got := overridden[presetKeyEventBus]; got.Implementation != "eventbus.memory" {
		t.Errorf("overridden eventbus entry = %+v, want the base preset's other entries carried over", got)
	}

	// A seam the receiver did not set is added, not rejected: the method is
	// the config-file layer's shape, where a host names entries the built-in
	// preset may or may not carry.
	added := base.With(presetKeyObjectStore, SeamPreset{Implementation: "objectstore.s3"})
	if got := added[presetKeyObjectStore].Implementation; got != "objectstore.s3" {
		t.Errorf("added objectstore entry = %q, want the new entry present on the returned preset", got)
	}
	if _, present := base[presetKeyObjectStore]; present {
		t.Error("base carries an objectstore entry after With, want the receiver untouched")
	}

	// A nil receiver is a usable base -- the zero Preset a host that names
	// every seam itself starts from.
	var zero Preset
	if got := zero.With(presetKeyEventBus, SeamPreset{Implementation: "eventbus.memory"})[presetKeyEventBus].Implementation; got != "eventbus.memory" {
		t.Errorf("With on a nil Preset = %q, want the entry set on the returned copy", got)
	}
}
