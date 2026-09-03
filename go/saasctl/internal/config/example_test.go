package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// ExampleRun prints the bootstrap configuration of the project whose
// go.mod sits in the working directory -- the command's defaults, no
// arguments and no environment: the default go.mod path resolves to the
// temp directory the example moves into, the app name derives from the
// go.mod's module path, and every variable falls back to the generated
// app's own default. The two key rows render as [redacted] with their
// development-default provenance.
func ExampleRun() {
	dir, err := os.MkdirTemp("", "config-example")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)
	err = os.WriteFile(filepath.Join(dir, "go.mod"), []byte(`module example.com/smile/cli-app

go 1.25.0
`), 0o644)
	if err != nil {
		fmt.Println(err)
		return
	}

	// The example shares the test process with the print tests, which
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

	code := Run([]string{"print"}, os.Stdout, os.Stderr)
	if code != 0 {
		fmt.Println("exit code:", code)
		return
	}
	// Output:
	// deployment mode  standalone   unset or empty (default standalone)
	// port             8080         unset or empty (default 8080)
	// sqlite path      cli-app.db   unset or empty (default cli-app.db)
	// config key       [redacted]   unset or empty (development default)
	// org index key    [redacted]   unset or empty (development default)
}
