package db

import (
	"time"

	"github.com/vislake/speed/pkg/config"
)

// Config is the set of input items an implementation of the database
// capability accepts. Every implementation accepts the same ones, so the
// carrier is declared once here rather than copied into each subpackage: the
// items describe a connection and a key, neither of which is a property of the
// engine behind them.
//
// What is not shared is where they hang. Each implementation mounts this
// carrier under its own namespace, db.postgres or db.sqlite, and that
// separation is load-bearing rather than tidy: the configuration manifest is
// collected from every registered module, this run's disabled ones included,
// so two implementations declaring input items on one path conflict as soon as
// a host imports both, and that conflict is raised before exclusivity is
// resolved. Importing several implementations and configuring one is the
// intended shape.
type Config struct {
	// DSN is the connection locator, in the form this implementation's own
	// driver reads. It is marked sensitive: it carries credentials, and both
	// the logs and the help output would otherwise echo it.
	//
	// It is not marked required. An absent locator is how an assembly says it
	// runs without this engine, which Prepare turns into a disabled module
	// with a reason rather than a startup failure.
	DSN string `config:"dsn"`
	// MaxOpenConns is the largest number of connections the pool opens.
	MaxOpenConns int `config:"max-open-conns"`
	// MaxIdleConns is how many idle connections the pool keeps.
	MaxIdleConns int `config:"max-idle-conns"`
	// ConnMaxLifetime is how long one connection is reused before it is
	// replaced.
	//
	// Its type is time.Duration rather than a count of seconds, which is what
	// makes config refuse a bare number here: 1800 would be half an hour to a
	// reader and 1.8 microseconds to the machine, and the two cannot be told
	// apart once the unit has been dropped.
	ConnMaxLifetime time.Duration `config:"conn-max-lifetime"`
	// EncryptionKey is the root key the field encryption and blind index
	// subkeys are derived from. Marked sensitive.
	//
	// An absent key is not a startup failure: which models carry encrypted
	// fields is not known until GORM parses them, which happens on first use,
	// long after startup. Encryption and decryption then fail on the
	// statement that reached one.
	EncryptionKey string `config:"encryption-key"`
	// EncryptionRetiredKeys are root keys that are no longer written with but
	// still decrypt what they wrote. Marked sensitive.
	EncryptionRetiredKeys []string `config:"encryption-retired-keys"`
}

// lockingConfig is what an implementation that supplies a cross-process
// migration mutex accepts: the common items plus the bound on waiting for that
// mutex. The embedded carrier is inlined by config exactly as encoding/json
// would inline it, so the items keep the paths they have without a mutex.
//
// The two carriers exist so that an implementation with no mutex does not
// declare migration-lock-timeout. The mounted prototype is what config reads
// the accepted keys out of, so mounting this one everywhere would give every
// engine a dial that changes nothing, and a host setting it would be told
// nothing.
type lockingConfig struct {
	Config
	// MigrationLockTimeout bounds the wait for the migration mutex. Past it
	// the startup fails with ErrMigrationLockTimeout rather than waiting on a
	// replica that may never finish.
	MigrationLockTimeout time.Duration `config:"migration-lock-timeout"`
}

// The input item paths, relative to an implementation's namespace. They are
// held as constants because they appear twice over: once in the declaration
// and once in the text that names one to an operator, and a reason naming a
// key that does not exist is worse than no reason at all.
const (
	dsnKey                   = "dsn"
	maxOpenConnsKey          = "max-open-conns"
	maxIdleConnsKey          = "max-idle-conns"
	connMaxLifetimeKey       = "conn-max-lifetime"
	encryptionKeyKey         = "encryption-key"
	encryptionRetiredKeysKey = "encryption-retired-keys"
)

// MigrationLockTimeoutKey is the input item bounding the wait for the
// migration mutex, relative to an implementation's configuration namespace.
//
// This package synthesises the item into the schema of an implementation that
// supplies a mutex, and declares it for no other, so the name belongs here. It
// is exported because an implementation that supplies a mutex names the item
// in its own timeout message, and a second spelling of it in a subpackage
// would be free to drift from the one that is declared.
const MigrationLockTimeoutKey = "migration-lock-timeout"

// The defaults of the pool parameters. They are the values the mounted
// prototype carries, which is the only place config reads a default from.
const (
	defaultMaxOpenConns    = 25
	defaultMaxIdleConns    = 5
	defaultConnMaxLifetime = 30 * time.Minute
)

// configGroup is the section the help output lists these items under. Every
// implementation uses it, so a host comparing the engines it could run on sees
// their items side by side rather than in two sections that look unrelated.
const configGroup = "Database"

// defaults returns the carrier this implementation's items are declared with,
// filled with the values an absent key takes.
//
// It is one function for two jobs: the prototype config reads the accepted
// keys and the defaults out of, and the struct Decode writes into. Reading
// them from one place is what keeps the pool parameters from landing on 0 —
// unlimited connections and no lifetime cap — when a reader hands back nothing
// for this section.
func (s Spec) defaults() lockingConfig {
	return lockingConfig{
		Config: Config{
			MaxOpenConns:    defaultMaxOpenConns,
			MaxIdleConns:    defaultMaxIdleConns,
			ConnMaxLifetime: defaultConnMaxLifetime,
		},
		MigrationLockTimeout: s.DefaultMigrationLockTimeout,
	}
}

// schema is this implementation's input item declaration, mounted under its
// own namespace.
//
// Every item takes the primary config source and the environment, the ordinary
// combination, so none of them declares an origin. None takes a command-line
// argument either: a connection locator and a root key are the two things a
// host must not put in a process listing.
func (s Spec) schema() config.Schema {
	proto := s.defaults()
	mount := config.Mount{Value: &proto.Config}
	items := map[string]config.Item{
		dsnKey: {
			Sensitive: true,
			Group:     configGroup,
			Description: "the locator of the database to connect to, in the form this engine's " +
				"own driver reads; leaving it out runs the assembly without this engine",
		},
		maxOpenConnsKey: {
			Group:       configGroup,
			Description: "the largest number of connections the pool opens",
		},
		maxIdleConnsKey: {
			Group:       configGroup,
			Description: "how many idle connections the pool keeps open",
		},
		connMaxLifetimeKey: {
			Group: configGroup,
			Description: "how long one connection is reused before it is replaced, written with " +
				"a unit, such as 30m",
		},
		encryptionKeyKey: {
			Sensitive: true,
			Group:     configGroup,
			Description: "the root key the field encryption and blind index subkeys are derived " +
				"from; without it every encrypted field fails on use rather than at startup",
		},
		encryptionRetiredKeysKey: {
			Sensitive: true,
			Group:     configGroup,
			Description: "root keys that are no longer written with and still decrypt what they " +
				"wrote, given as a list",
		},
	}
	if s.NewMigrationLock != nil {
		mount = config.Mount{Value: &proto}
		items[MigrationLockTimeoutKey] = config.Item{
			Group: configGroup,
			Description: "how long to wait for another replica to finish applying migrations " +
				"before giving up, written with a unit, such as 5m",
		}
	}
	return config.Schema{
		Namespace: s.ConfigNamespace,
		Mounts:    []config.Mount{mount},
		Items:     items,
	}
}

// read decodes this implementation's own section.
//
// The target is the locking carrier whichever implementation this is. Decode
// walks the target and leaves a field no source gave a value for as it found
// it, so an implementation that never declared migration-lock-timeout reads
// back the zero its defaults put there, and it has no mutex to hand that value
// to in the first place.
func (s Spec) read(reader config.Reader) (lockingConfig, error) {
	cfg := s.defaults()
	if err := reader.Decode(s.ConfigNamespace, &cfg); err != nil {
		return lockingConfig{}, err
	}
	return cfg, nil
}
