package db

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/config"
	jsonformat "github.com/vislake/speed/pkg/config/format/json"
	filesource "github.com/vislake/speed/pkg/config/source/file"
	"github.com/vislake/speed/pkg/core"
)

// The two namespaces the shipped implementations mount under. The cases below
// need two of them to observe that the paths stay apart; each implementation
// pins its own value in its own subpackage, which is where a change to it
// would have to be made.
const (
	postgresNamespace = "db.postgres"
	sqliteNamespace   = "db.sqlite"
)

// specWithMutex is the shape of an implementation whose engine appears in
// multi-replica deployments: it supplies a migration mutex and therefore
// declares the timeout that bounds waiting for it.
func specWithMutex() Spec {
	return Spec{
		ModuleName:                  postgresNamespace,
		ConfigNamespace:             postgresNamespace,
		Dialect:                     Postgres,
		Dialector:                   func(string) gorm.Dialector { return nil },
		NewMigrationLock:            func(time.Duration) MigrationLock { return nil },
		DefaultMigrationLockTimeout: 5 * time.Minute,
	}
}

// specWithoutMutex is the shape of an implementation that needs no
// cross-process mutex.
func specWithoutMutex() Spec {
	return Spec{
		ModuleName:      sqliteNamespace,
		ConfigNamespace: sqliteNamespace,
		Dialect:         SQLite,
		Dialector:       func(string) gorm.Dialector { return nil },
	}
}

// writeConfig writes the primary config source one case runs on and returns
// the path to it.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	locator := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(locator, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the primary config source: %v", err)
	}
	return locator
}

// readerOver builds the reader a real assembly would hand these modules: the
// actual configuration loader, over a JSON primary source holding body.
//
// It is the loader itself rather than a stand-in because most of what these
// cases observe belongs to it — the defaults come from the mounted prototype
// through the base layer, the refusal of a bare number comes from the type of
// the field, and an undeclared key is refused by the manifest. A stand-in
// reader handing back a struct the test wrote would agree with any declaration
// at all.
//
// The loader reads os.Args unconditionally and a test binary always carries
// arguments of its own, so the arguments are taken away for the call. The
// cases that use it therefore do not run in parallel.
func readerOver(t *testing.T, body string, modules ...core.Module) (config.Reader, error) {
	t.Helper()
	locator := writeConfig(t, body)

	reg := core.New()
	reg.Register(jsonformat.Module())
	reg.Register(filesource.Module())
	reg.Register(core.Module{
		Name:      "host",
		Resources: []any{config.HostIdentity{Prefix: "SPEEDDBTEST", DefaultLocator: "file://" + locator}},
	})
	for _, m := range modules {
		reg.Register(m)
	}

	previous := os.Args
	os.Args = []string{"db.test"}
	defer func() { os.Args = previous }()

	instance, err := config.Module().New(t.Context(), reg)
	if err != nil {
		return nil, err
	}
	reader, ok := instance.(config.Reader)
	if !ok {
		t.Fatalf("the config module produced %T, want a config.Reader", instance)
	}
	return reader, nil
}

// readerFor builds the reader for one implementation and fails the test if the
// configuration would not load at all.
func readerFor(t *testing.T, spec Spec, body string) config.Reader {
	t.Helper()
	reader, err := readerOver(t, body, NewModule(spec))
	if err != nil {
		t.Fatalf("loading the configuration failed: %v", err)
	}
	return reader
}

// TestPoolDefaults pins the values an assembly that configured nothing but a
// locator runs on.
//
// They are not decoration. Left at the zero value, database/sql reads
// MaxOpenConns 0 as no limit at all and ConnMaxLifetime 0 as a connection that
// is never replaced, so a declaration that forgot to carry the defaults gives
// a pool that grows without bound and holds connections past anything the
// server or a proxy between them is willing to keep — and it does that without
// a word anywhere.
func TestPoolDefaults(t *testing.T) {
	cfg, err := specWithMutex().read(readerFor(t, specWithMutex(), `{"db":{"postgres":{"dsn":"x"}}}`))
	if err != nil {
		t.Fatalf("reading a section that gives only a locator: %v", err)
	}
	if cfg.MaxOpenConns != 25 {
		t.Errorf("max-open-conns defaults to %d, want 25", cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns != 5 {
		t.Errorf("max-idle-conns defaults to %d, want 5", cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("conn-max-lifetime defaults to %v, want 30m", cfg.ConnMaxLifetime)
	}
}

// TestConfiguredPoolParametersOverrideTheDefaults is the other half of the
// case above: a declaration that carried the defaults but was never mounted
// would pass that one from the prototype alone and lose every configured
// value here.
func TestConfiguredPoolParametersOverrideTheDefaults(t *testing.T) {
	spec := specWithMutex()
	body := `{"db":{"postgres":{"dsn":"x","max-open-conns":7,"max-idle-conns":2,` +
		`"conn-max-lifetime":"90s"}}}`
	cfg, err := spec.read(readerFor(t, spec, body))
	if err != nil {
		t.Fatalf("reading a fully configured section: %v", err)
	}
	if cfg.MaxOpenConns != 7 || cfg.MaxIdleConns != 2 || cfg.ConnMaxLifetime != 90*time.Second {
		t.Errorf("the section read back as %d/%d/%v, want 7/2/1m30s",
			cfg.MaxOpenConns, cfg.MaxIdleConns, cfg.ConnMaxLifetime)
	}
}

// TestDurationItemsRejectBareNumber pins that a duration is written with a
// unit. A bare 1800 is half an hour to whoever wrote it and 1.8 microseconds
// to the machine, and a pool that replaces every connection after 1.8
// microseconds does not fail — it reconnects on every statement and looks like
// a slow database.
//
// The refusal comes from the type of the field, so this is an observation
// about the declaration: written as a count of seconds, the value below is
// accepted and means something else. It lands while the configuration is
// being loaded rather than when this module decodes, which is the earliest the
// host could be told.
func TestDurationItemsRejectBareNumber(t *testing.T) {
	for _, c := range []struct {
		item string
		body string
	}{
		{connMaxLifetimeKey, `{"db":{"postgres":{"dsn":"x","conn-max-lifetime":1800}}}`},
		{MigrationLockTimeoutKey, `{"db":{"postgres":{"dsn":"x","migration-lock-timeout":300}}}`},
	} {
		t.Run(c.item, func(t *testing.T) {
			_, err := readerOver(t, c.body, NewModule(specWithMutex()))
			if !errors.Is(err, config.ErrTypeMismatch) {
				t.Fatalf("a bare number for %s gave %v, want config.ErrTypeMismatch", c.item, err)
			}
			if want := postgresNamespace + "." + c.item; !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name %s: %v", want, err)
			}
		})
	}
}

// TestSensitiveItemsAreMarked pins the three items that carry a secret.
//
// Marking keeps the value out of the logs and the default out of the help
// output, and an unmarked one is a credential in a startup diagnostic that
// nobody notices until it is in a log aggregator. The description is mandatory
// beside it: with the default withheld, it is all the help output has left to
// say what the item is.
//
// The locator is checked for not being required in the same place, because the
// two would be set together by whoever thought a database is mandatory.
// Required turns the stance this module takes — disabled, with a reason — into
// a startup failure for every assembly that does not use a database.
func TestSensitiveItemsAreMarked(t *testing.T) {
	items := specWithMutex().schema().Items
	for _, key := range []string{dsnKey, encryptionKeyKey, encryptionRetiredKeysKey} {
		item, declared := items[key]
		if !declared {
			t.Errorf("%s is not declared at all", key)
			continue
		}
		if !item.Sensitive {
			t.Errorf("%s is not marked sensitive, so its value reaches the logs and the "+
				"help output", key)
		}
		if item.Description == "" {
			t.Errorf("%s is sensitive and has no description, so the help output has nothing "+
				"to say about it", key)
		}
	}
	if items[dsnKey].Required {
		t.Errorf("%s is marked required, which turns an assembly that runs without this "+
			"engine into a startup failure", dsnKey)
	}
}

// TestEveryDeclaredItemIsDescribed keeps the help output complete. An item
// with no description is a name and a default, which says nothing about what
// setting it does.
func TestEveryDeclaredItemIsDescribed(t *testing.T) {
	for _, spec := range []Spec{specWithMutex(), specWithoutMutex()} {
		schema := spec.schema()
		for key, item := range schema.Items {
			if item.Description == "" {
				t.Errorf("%s.%s is declared without a description", schema.Namespace, key)
			}
			if item.Group == "" {
				t.Errorf("%s.%s is declared without a help group", schema.Namespace, key)
			}
		}
	}
}

// itemPaths expands a schema into the complete paths it claims: the namespace,
// the mount path and the key of every leaf the mounted carrier carries. It is
// what the configuration manifest does with a declaration, cut down to the one
// level these carriers have.
func itemPaths(t *testing.T, schema config.Schema) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, mount := range schema.Mounts {
		v := reflect.ValueOf(mount.Value)
		for v.Kind() == reflect.Pointer {
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			t.Fatalf("a mount carries %T, which config would refuse", mount.Value)
		}
		collectPaths(t, out, v.Type(), strings.Trim(schema.Namespace+"."+mount.Path, "."))
	}
	return out
}

// collectPaths walks one carrier, inlining an embedded struct the way config
// and encoding/json both do.
func collectPaths(t *testing.T, into map[string]bool, carrier reflect.Type, prefix string) {
	t.Helper()
	for i := range carrier.NumField() {
		f := carrier.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			collectPaths(t, into, f.Type, prefix)
			continue
		}
		tag, tagged := f.Tag.Lookup("config")
		if !tagged {
			t.Fatalf("field %s of %s carries no config tag, so its key is derived and this "+
				"walk no longer matches the manifest's", f.Name, carrier)
		}
		into[prefix+"."+tag] = true
	}
}

// TestTwoImplementationsDeclareDisjointConfigPaths pins the separation the
// design puts the implementations' input items under.
//
// The manifest is collected from every registered module, this run's disabled
// ones included, so two implementations declaring an item on one path conflict
// as soon as a host imports both — and the conflict is raised while the
// manifest is being collected, before exclusivity resolution would have stood
// one of them down. A host that imports two engines and configures one is the
// intended shape, and it is the separate namespaces that make it work.
//
// Both halves are observed: the paths of the two shipped shapes do not meet,
// and a third implementation that mounted under a namespace already taken
// really does fail the load. Without the second half this case would be
// asserting that two different strings are different.
func TestTwoImplementationsDeclareDisjointConfigPaths(t *testing.T) {
	withMutex, withoutMutex := specWithMutex(), specWithoutMutex()
	if got := withMutex.schema().Namespace; got != postgresNamespace {
		t.Errorf("the mutex-carrying implementation mounts under %q, want %q", got, postgresNamespace)
	}
	if got := withoutMutex.schema().Namespace; got != sqliteNamespace {
		t.Errorf("the other implementation mounts under %q, want %q", got, sqliteNamespace)
	}

	left, right := itemPaths(t, withMutex.schema()), itemPaths(t, withoutMutex.schema())
	shared := slices.Sorted(maps.Keys(left))
	shared = slices.DeleteFunc(shared, func(p string) bool { return !right[p] })
	if len(shared) != 0 {
		t.Errorf("the two implementations declare %v on the same paths; a host importing both "+
			"fails to load its configuration at all", shared)
	}
	if _, err := readerOver(t, `{"db":{"postgres":{"dsn":"x"}}}`,
		NewModule(withMutex), NewModule(withoutMutex)); err != nil {
		t.Fatalf("a host importing both implementations could not load its configuration: %v", err)
	}

	rival := specWithoutMutex()
	rival.ModuleName = "db.rival"
	rival.ConfigNamespace = postgresNamespace
	_, err := readerOver(t, `{}`, NewModule(withMutex), NewModule(rival))
	if !errors.Is(err, config.ErrConfigConflict) {
		t.Errorf("two implementations sharing a namespace loaded with %v, want "+
			"config.ErrConfigConflict: that is the failure separate namespaces avoid", err)
	}
}

// TestOnlyAnImplementationWithAMutexDeclaresTheTimeout pins that the timeout
// follows the mutex.
//
// An engine that needs no cross-process mutex has nothing to wait for, so the
// item would be a dial that changes nothing — and a dial that changes nothing
// is worse than none, because a host that sets it is told nothing and goes on
// believing it did something. Here the host is told: the key is not one this
// implementation accepts.
func TestOnlyAnImplementationWithAMutexDeclaresTheTimeout(t *testing.T) {
	if _, declared := specWithoutMutex().schema().Items[MigrationLockTimeoutKey]; declared {
		t.Errorf("an implementation with no mutex declares %s", MigrationLockTimeoutKey)
	}
	if _, declared := specWithMutex().schema().Items[MigrationLockTimeoutKey]; !declared {
		t.Errorf("an implementation with a mutex does not declare %s", MigrationLockTimeoutKey)
	}

	body := `{"db":{"sqlite":{"dsn":"x","migration-lock-timeout":"5m"}}}`
	_, err := readerOver(t, body, NewModule(specWithoutMutex()))
	if !errors.Is(err, config.ErrUnknownKey) {
		t.Fatalf("setting %s on an implementation without a mutex gave %v, want "+
			"config.ErrUnknownKey", MigrationLockTimeoutKey, err)
	}
}

// TestTheMutexTimeoutDefaultsToTheImplementationValue pins that the default
// reaching the mutex is the one the implementation gave, not a zero that would
// have the run give up before it waited.
func TestTheMutexTimeoutDefaultsToTheImplementationValue(t *testing.T) {
	spec := specWithMutex()
	cfg, err := spec.read(readerFor(t, spec, `{"db":{"postgres":{"dsn":"x"}}}`))
	if err != nil {
		t.Fatalf("reading a section that configures no timeout: %v", err)
	}
	if cfg.MigrationLockTimeout != spec.DefaultMigrationLockTimeout {
		t.Errorf("%s defaults to %v, want the implementation's %v",
			MigrationLockTimeoutKey, cfg.MigrationLockTimeout, spec.DefaultMigrationLockTimeout)
	}

	configured, err := spec.read(readerFor(t, spec,
		`{"db":{"postgres":{"dsn":"x","migration-lock-timeout":"9s"}}}`))
	if err != nil {
		t.Fatalf("reading a configured timeout: %v", err)
	}
	if configured.MigrationLockTimeout != 9*time.Second {
		t.Errorf("a configured %s read back as %v, want 9s",
			MigrationLockTimeoutKey, configured.MigrationLockTimeout)
	}
}

// TestRetiredKeysReadBackAsAList pins that the retired keys are a list rather
// than one string. Read as a single value, a host rotating a second time would
// silently lose the ability to decrypt what the first key wrote.
func TestRetiredKeysReadBackAsAList(t *testing.T) {
	spec := specWithMutex()
	body := `{"db":{"postgres":{"dsn":"x","encryption-key":"new",` +
		`"encryption-retired-keys":["older","oldest"]}}}`
	cfg, err := spec.read(readerFor(t, spec, body))
	if err != nil {
		t.Fatalf("reading a section with retired keys: %v", err)
	}
	if !slices.Equal(cfg.EncryptionRetiredKeys, []string{"older", "oldest"}) {
		t.Errorf("the retired keys read back as %q, want both of them in order",
			cfg.EncryptionRetiredKeys)
	}
	if cfg.EncryptionKey != "new" {
		t.Errorf("the current key read back as %q, want the configured one", cfg.EncryptionKey)
	}
}

// TestAnUndeclaredKeyInThisSectionIsRefused pins that the section is closed. A
// misspelled key that is accepted silently leaves the item it was meant to set
// on its default, and a pool parameter or a retired key that quietly stayed
// behind is exactly the class of defect this whole declaration exists to
// prevent.
func TestAnUndeclaredKeyInThisSectionIsRefused(t *testing.T) {
	body := `{"db":{"postgres":{"dsn":"x","max-open-connections":7}}}`
	_, err := readerOver(t, body, NewModule(specWithMutex()))
	if !errors.Is(err, config.ErrUnknownKey) {
		t.Fatalf("a misspelled pool parameter gave %v, want config.ErrUnknownKey", err)
	}
}

// TestTheReaderIsAskedForThisImplementationOwnSection pins the path the module
// decodes from. It assembles the path itself out of what it declared, so a
// namespace change that misses one of the two sites reads an empty section and
// takes every default without a word.
func TestTheReaderIsAskedForThisImplementationOwnSection(t *testing.T) {
	spec := specWithMutex()
	asked := &pathRecordingReader{}
	if _, err := spec.read(asked); err != nil {
		t.Fatalf("reading through a recording reader: %v", err)
	}
	if asked.path != postgresNamespace {
		t.Errorf("the module decoded from %q, want its own section %q", asked.path, postgresNamespace)
	}
}

// pathRecordingReader records the path it was asked for and writes nothing.
type pathRecordingReader struct {
	path string
}

// Decode records the path. The target keeps what the caller put in it, which
// is what config's own reader does for a path no module declared.
func (r *pathRecordingReader) Decode(path string, _ any) error {
	r.path = path
	return nil
}

var _ config.Reader = (*pathRecordingReader)(nil)

// TestDefaultsSurviveAReaderThatGivesNothing pins that the defaults are in the
// struct the module decodes into, not only in the layer the loader builds. The
// two are the same values from the same function, and the seed is what keeps a
// section nobody declared from landing on a pool with no limits.
func TestDefaultsSurviveAReaderThatGivesNothing(t *testing.T) {
	cfg, err := specWithMutex().read(&pathRecordingReader{})
	if err != nil {
		t.Fatalf("reading through a reader that gives nothing: %v", err)
	}
	if cfg.MaxOpenConns != 25 || cfg.MaxIdleConns != 5 || cfg.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("a reader that gave nothing left the pool at %d/%d/%v, want the defaults",
			cfg.MaxOpenConns, cfg.MaxIdleConns, cfg.ConnMaxLifetime)
	}
}
