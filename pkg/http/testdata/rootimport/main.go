// Command rootimport is the fixture the root package's test starts: a program
// that imports this module's root package and nothing else from pkg/http.
// pkg/core comes along for the registry handle the report is read through.
//
// It reports its own process registry as one JSON object on standard output —
// every module name in it, and the names of the modules delivering Router.
// Registration happens in an implementation subpackage, so a program that
// imported nothing from pkg/http but the root package has no entry point in
// its registry; the test reads that and not the absence of a message.
//
// The import list is the fixture. Adding anything else from this module —
// pkg/http/stdmux above all — changes what the program observes, which is what
// makes it evidence.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"

	"github.com/vislake/speed/pkg/core"
	speedhttp "github.com/vislake/speed/pkg/http"
)

// observation is what this program saw in its own process registry.
type observation struct {
	// Modules is every module name the registry holds.
	Modules []string `json:"modules"`
	// Routers is the names of the modules delivering Router. An entry point
	// subpackage registers one of these.
	Routers []string `json:"routers"`
}

func main() {
	seen := observation{Modules: []string{}, Routers: []string{}}
	for _, module := range core.ProcessRegistry.Modules() {
		seen.Modules = append(seen.Modules, module.Name)
		for _, provision := range module.Provides {
			if reflect.TypeOf(provision.Token) == reflect.TypeFor[*speedhttp.Router]() {
				seen.Routers = append(seen.Routers, module.Name)
			}
		}
	}
	sort.Strings(seen.Modules)
	sort.Strings(seen.Routers)

	if err := json.NewEncoder(os.Stdout).Encode(seen); err != nil {
		fmt.Fprintln(os.Stderr, "reporting the process registry:", err)
		os.Exit(1)
	}
}
