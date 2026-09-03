package appconfig

import (
	"bytes"
	"fmt"
)

// ExampleLoad shows the provenance record Load returns for a partially set
// environment: the variables that carry values are parsed and flagged
// from-env, and the rest resolve to a generated project's own defaults --
// the same defaults the app's configFromEnv would apply.
func ExampleLoad() {
	env := map[string]string{
		DeploymentModeEnv: "distributed",
		PortEnv:           "9090",
	}
	lookup := func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
	cfg, err := Load("cli-app", lookup)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("deployment mode:", cfg.DeploymentMode, "from env:", cfg.DeploymentModeFromEnv)
	fmt.Println("port:", cfg.Port, "from env:", cfg.PortFromEnv)
	fmt.Println("sqlite path:", cfg.SQLitePath, "from env:", cfg.SQLitePathFromEnv)
	fmt.Println("config key is the documented development default:", bytes.Equal(cfg.ConfigKey, devConfigKey))
	fmt.Println("org index key is the documented development default:", bytes.Equal(cfg.OrgIndexKey, devOrgIndexKey))
	// Output:
	// deployment mode: distributed from env: true
	// port: 9090 from env: true
	// sqlite path: cli-app.db from env: false
	// config key is the documented development default: true
	// org index key is the documented development default: true
}
