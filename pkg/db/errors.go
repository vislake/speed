package db

import "errors"

// The sentinel errors this module judges for itself. Every one of them aborts
// the startup: a database that cannot be reached, a plugin that did not install
// or a migration that did not apply are all states a process must not run on.
//
// Callers tell the classes apart with errors.Is, so every wrap along the way
// uses %w. A %v would leave the message almost unchanged and make the sentinel
// unreachable, which is the failure this note exists to prevent.
//
// The table has four entries and there is no fifth. Encryption and decryption
// fail at run time — a missing key, a damaged ciphertext, a key that does not
// match its ciphertext all surface on the query that hit them, carried back by
// GORM — and they get no sentinel here: this table is for classifying a startup
// failure, and a run-time error belongs to the code that issued the statement.
var (
	// ErrConnectFailed reports that the driver would not open, or that the
	// connectivity check did not pass. Its text names the dialect and the
	// step that failed, and never echoes the locator: a DSN carries
	// credentials and this error reaches the startup diagnostics.
	//
	// Open reports the same sentinel when it fails at run time. The fixing
	// action is identical — check that locator and the service behind it —
	// so there is no second sentinel for it.
	ErrConnectFailed = errors.New("db: connection could not be established")
	// ErrPluginFailed reports that a declared plugin did not install.
	ErrPluginFailed = errors.New("db: declared plugin failed to install")
	// ErrMigrationFailed reports that a migration did not apply, that the
	// migration record table could not be read or written, that a migration
	// directory holds something other than a .sql file, or that the
	// migration mutex could not be given up.
	//
	// On a migration it names the declaring module and the file: that module
	// is what has to change, and this one has no way to know how. On an entry
	// that is not a migration it names that entry. On the record table there
	// is nothing comparable to name, so the text gives the operation that
	// failed. On a mutex that would not be released the text says whether the
	// migrations went in first, because the sentinel on its own would send
	// the reader to migration files that are not the problem. One sentinel
	// covers them all because the host's response is the same: the process
	// must not run.
	ErrMigrationFailed = errors.New("db: migration could not be applied")
	// ErrMigrationLockTimeout reports that the migration mutex was not
	// acquired within migration-lock-timeout. It is separate from
	// ErrMigrationFailed because the response differs: the question is why
	// another replica is taking so long, or whether the limit is too short,
	// not which migration to fix.
	ErrMigrationLockTimeout = errors.New("db: migration mutex was not acquired before the timeout")
)
