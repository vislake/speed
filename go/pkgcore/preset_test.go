package pkgcore

import "testing"

// TestPresetStandalone_NamesARegisteredImplementationForEverySeam pins the
// invariant NewKernel's zero-value default relies on: every key
// PresetStandalone sets must resolve through the matching package-level
// SeamRegistry with an empty Config, or a bare NewKernel().Bootstrap() would
// fail even though it is documented to start in seconds with nothing else
// running.
func TestPresetStandalone_NamesARegisteredImplementationForEverySeam(t *testing.T) {
	if _, _, err := EventBusRegistry.Build(PresetStandalone[presetKeyEventBus], Config{}); err != nil {
		t.Errorf("EventBusRegistry.Build(%q) error = %v, want nil", PresetStandalone[presetKeyEventBus], err)
	}
	if _, _, err := KVStoreRegistry.Build(PresetStandalone[presetKeyKVStore], Config{}); err != nil {
		t.Errorf("KVStoreRegistry.Build(%q) error = %v, want nil", PresetStandalone[presetKeyKVStore], err)
	}
	if _, _, err := MailerRegistry.Build(PresetStandalone[presetKeyMailer], Config{}); err != nil {
		t.Errorf("MailerRegistry.Build(%q) error = %v, want nil", PresetStandalone[presetKeyMailer], err)
	}
	if _, _, err := ObjectStoreRegistry.Build(PresetStandalone[presetKeyObjectStore], Config{}); err != nil {
		t.Errorf("ObjectStoreRegistry.Build(%q) error = %v, want nil", PresetStandalone[presetKeyObjectStore], err)
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
	if _, caps, err := EventBusRegistry.Build(PresetStandalone[presetKeyEventBus], Config{}); err != nil {
		t.Fatalf("EventBusRegistry.Build() error = %v, want nil", err)
	} else if caps.Has(MultiReplicaSafe) {
		t.Errorf("eventbus %q declares MultiReplicaSafe, want PresetStandalone to stay single-process", PresetStandalone[presetKeyEventBus])
	}
	if _, caps, err := KVStoreRegistry.Build(PresetStandalone[presetKeyKVStore], Config{}); err != nil {
		t.Fatalf("KVStoreRegistry.Build() error = %v, want nil", err)
	} else if caps.Has(MultiReplicaSafe) {
		t.Errorf("kv %q declares MultiReplicaSafe, want PresetStandalone to stay single-process", PresetStandalone[presetKeyKVStore])
	}
	if _, caps, err := MailerRegistry.Build(PresetStandalone[presetKeyMailer], Config{}); err != nil {
		t.Fatalf("MailerRegistry.Build() error = %v, want nil", err)
	} else if caps.Has(MultiReplicaSafe) {
		t.Errorf("mailer %q declares MultiReplicaSafe, want PresetStandalone to stay single-process", PresetStandalone[presetKeyMailer])
	}
	if _, caps, err := ObjectStoreRegistry.Build(PresetStandalone[presetKeyObjectStore], Config{}); err != nil {
		t.Fatalf("ObjectStoreRegistry.Build() error = %v, want nil", err)
	} else if caps.Has(MultiReplicaSafe) {
		t.Errorf("objectstore %q declares MultiReplicaSafe, want PresetStandalone to stay single-process", PresetStandalone[presetKeyObjectStore])
	}
}

// TestPresetDistributed_NamesAMultiReplicaSafeImplementationForEverySeam
// pins the complementary invariant: every seam PresetDistributed names must
// resolve to an implementation declaring MultiReplicaSafe, or
// WithPreset(PresetDistributed) alone could never satisfy
// DeploymentModeDistributed's requirement. The Redis-backed seams build with
// an empty Config (they fall back to a bare-minimum "localhost:6379"
// default, per redisClientFromConfig's own doc comment); the SMTP and S3
// seams need cfg fields with no safe default and are checked against a
// minimally-populated Config here instead, purely to reach their declared
// Capabilities -- constructing a live mailer or store is not this test's
// concern.
func TestPresetDistributed_NamesAMultiReplicaSafeImplementationForEverySeam(t *testing.T) {
	if _, caps, err := EventBusRegistry.Build(PresetDistributed[presetKeyEventBus], Config{}); err != nil {
		t.Errorf("EventBusRegistry.Build(%q) error = %v, want nil", PresetDistributed[presetKeyEventBus], err)
	} else if !caps.Has(MultiReplicaSafe) {
		t.Errorf("eventbus %q capabilities = %v, want MultiReplicaSafe", PresetDistributed[presetKeyEventBus], caps)
	}
	if _, caps, err := KVStoreRegistry.Build(PresetDistributed[presetKeyKVStore], Config{}); err != nil {
		t.Errorf("KVStoreRegistry.Build(%q) error = %v, want nil", PresetDistributed[presetKeyKVStore], err)
	} else if !caps.Has(MultiReplicaSafe) {
		t.Errorf("kv %q capabilities = %v, want MultiReplicaSafe", PresetDistributed[presetKeyKVStore], caps)
	}
	if _, caps, err := MailerRegistry.Build(PresetDistributed[presetKeyMailer], Config{"host": "smtp.example.com"}); err != nil {
		t.Errorf("MailerRegistry.Build(%q) error = %v, want nil", PresetDistributed[presetKeyMailer], err)
	} else if !caps.Has(MultiReplicaSafe) {
		t.Errorf("mailer %q capabilities = %v, want MultiReplicaSafe", PresetDistributed[presetKeyMailer], caps)
	}
	s3Cfg := Config{"endpoint": "s3.example.com", "bucket": "objects", "access_key": "ak", "secret_key": "sk"}
	if _, caps, err := ObjectStoreRegistry.Build(PresetDistributed[presetKeyObjectStore], s3Cfg); err != nil {
		t.Errorf("ObjectStoreRegistry.Build(%q) error = %v, want nil", PresetDistributed[presetKeyObjectStore], err)
	} else if !caps.Has(MultiReplicaSafe) {
		t.Errorf("objectstore %q capabilities = %v, want MultiReplicaSafe", PresetDistributed[presetKeyObjectStore], caps)
	}
}

// TestPresetDistributed_SMTPAndS3SeamsRequireConfig pins the documented gap
// PresetDistributed's own doc comment names: the mailer and object-store
// seams have no safe default credentials, so resolving them with an empty
// Config fails with ErrMissingSeamConfig instead of silently building an
// unusable mailer or store.
func TestPresetDistributed_SMTPAndS3SeamsRequireConfig(t *testing.T) {
	if _, _, err := MailerRegistry.Build(PresetDistributed[presetKeyMailer], Config{}); err == nil {
		t.Error("MailerRegistry.Build() with an empty Config succeeded, want ErrMissingSeamConfig")
	}
	if _, _, err := ObjectStoreRegistry.Build(PresetDistributed[presetKeyObjectStore], Config{}); err == nil {
		t.Error("ObjectStoreRegistry.Build() with an empty Config succeeded, want ErrMissingSeamConfig")
	}
}
