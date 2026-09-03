package db

import (
	"fmt"
	"os"
	"path/filepath"
)

// ExampleRun migrates the project whose go.mod sits in the working
// directory -- the command's defaults, no arguments and no environment:
// the default go.mod path resolves to the temp directory the example
// moves into, the app name derives from the go.mod's module path, and the
// default database path is that app name plus .db. The one-line report
// shows what the run applied.
func ExampleRun() {
	dir, err := os.MkdirTemp("", "db-example")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)
	err = os.WriteFile(filepath.Join(dir, "go.mod"), []byte(`module example.com/smile/cli-app

go 1.25.0

require github.com/vislake/speed/go/config v0.0.0-00010101000000-000000000000
`), 0o644)
	if err != nil {
		fmt.Println(err)
		return
	}

	// The example shares the test process with the migrate tests, which
	// clear and set the same bootstrap variables through t.Setenv, so it
	// empties every one of them itself and restores whatever it found
	// once the example is over. Empty counts as unset.
	type envEntry struct {
		value   string
		present bool
	}
	saved := make(map[string]envEntry, len(bootstrapEnvKeys))
	for _, key := range bootstrapEnvKeys {
		value, present := os.LookupEnv(key)
		saved[key] = envEntry{value: value, present: present}
		if unsetErr := os.Unsetenv(key); unsetErr != nil {
			fmt.Println(unsetErr)
			return
		}
	}
	defer func() {
		for key, entry := range saved {
			if entry.present {
				_ = os.Setenv(key, entry.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}()

	// The default go.mod path resolves against the working directory, so
	// the example runs from inside its own temp dir -- and returns before
	// the temp dir is removed.
	wd, err := os.Getwd()
	if err != nil {
		fmt.Println(err)
		return
	}
	if err := os.Chdir(dir); err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = os.Chdir(wd) }()

	code := Run([]string{"migrate"}, os.Stdout, os.Stderr)
	if code != 0 {
		fmt.Println("exit code:", code)
		return
	}
	// Output:
	// Migrated cli-app.db: applied 1 migration files (config 1)
}
