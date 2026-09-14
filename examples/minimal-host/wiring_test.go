package main

import (
	"testing"

	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/log"
)

// This case reads the descriptors instead of running the host, which is why it
// is not in e2e_test.go: the property it states leaves no trace in a run.
//
// A module that takes a logger out of the registry without declaring the
// requirement is handed one all the same, as long as some other module in the
// run declared it — one declaration anywhere puts logging ahead of every
// construction and behind every shutdown, so the module with the omission looks
// exactly like the modules without it. The omission only surfaces when the
// module that carries the declaration is removed or stands down, and then it
// surfaces as a startup failure in a module nobody changed. The declaration is
// therefore worth asserting where it is written rather than where it is felt.
func TestEveryModuleThatLogsDeclaresTheLoggingDependency(t *testing.T) {
	// Every module of this host that holds a logger. The list is written out
	// because nothing in a descriptor says a module logs: what a module
	// resolves is inside its New, and the declaration below is the only
	// outward statement of it.
	for _, module := range []core.Module{appModule(), localGreeterModule(), remoteGreeterModule()} {
		if !requiresCapability(module, (*log.Logger)(nil)) {
			t.Errorf("module %q takes its logger from the registry and declares no requirement "+
				"on the %T capability, so nothing orders logging around it",
				module.Name, (*log.Logger)(nil))
		}
	}
}

// requiresCapability reports whether a descriptor declares a requirement on a
// capability. A token is a typed nil pointer, so comparing two of them compares
// the capability each designates.
func requiresCapability(module core.Module, capability core.Token) bool {
	for _, requirement := range module.Requires {
		if requirement.Token == capability {
			return true
		}
	}
	return false
}
