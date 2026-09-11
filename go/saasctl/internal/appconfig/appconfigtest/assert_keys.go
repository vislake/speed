// Package appconfigtest is the test support the packages around appconfig
// share: one derivation of the bootstrap environment surface appconfig
// resolves, and the assertion that a list of bootstrap variables names that
// surface exactly. The packages whose tests must clear or restore the whole
// surface each carry their own list of variable names, and those lists are
// the drift risk -- a variable joining or leaving appconfig leaves every
// copy stale, silently -- so the expectation lives here, derived from
// appconfig's own behavior, rather than restated beside each list.
package appconfigtest

import (
	"fmt"
	"testing"

	"github.com/vislake/speed/go/saasctl/internal/appconfig"
)

// probeAppName is the app name the surface derivation loads under. Load
// renders the name in its error texts only, and the all-unset environment
// the derivation reads cannot fail a load.
const probeAppName = "appconfigtest"

// surfaceKeys returns the environment variables appconfig.Load resolves, in
// the order it resolves them: a generated project's bootstrap environment
// surface, read from the twin's own behavior rather than from a copy of it.
// The recording lookup answers every variable with "unset" -- the
// all-defaults environment Load accepts -- so the variables Load asks for
// are exactly the ones a generated project's bootstrap reads, and a
// variable joining or leaving that surface moves this answer with it.
func surfaceKeys(t *testing.T) []string {
	t.Helper()
	var keys []string
	if _, err := appconfig.Load(probeAppName, func(key string) (string, bool) {
		keys = append(keys, key)
		return "", false
	}); err != nil {
		t.Fatalf("load the all-defaults bootstrap environment: %v", err)
	}
	return keys
}

// discrepancies returns one message per way keys diverges from the resolved
// surface: a resolved variable the list omits, a name the list carries that
// Load never resolves, a name listed twice. It is empty when the list names
// the surface exactly. The comparison is content, not order: Load's
// resolution order is not the order the lists carry, and the lists' shared
// order is a readability convention no behavior depends on.
func discrepancies(surface, keys []string) []string {
	var problems []string
	listed := make(map[string]bool, len(keys))
	for _, key := range keys {
		if listed[key] {
			problems = append(problems, fmt.Sprintf("the list names %s twice", key))
		}
		listed[key] = true
	}
	resolved := make(map[string]bool, len(surface))
	for _, key := range surface {
		resolved[key] = true
		if !listed[key] {
			problems = append(problems, fmt.Sprintf("appconfig.Load resolves %s but the list omits it", key))
		}
	}
	for _, key := range keys {
		if !resolved[key] {
			problems = append(problems, fmt.Sprintf("the list names %s but appconfig.Load never resolves it", key))
		}
	}
	return problems
}

// AssertKeys fails the test when keys does not name exactly the bootstrap
// environment surface appconfig.Load resolves -- every resolved variable
// present, nothing extra, nothing twice. The expectation comes from Load's
// own behavior (see surfaceKeys), never from a hand-maintained copy of the
// appconfig constants, so a variable joining or leaving the surface fails
// the caller's list here until the list moves with it.
func AssertKeys(t *testing.T, keys []string) {
	t.Helper()
	for _, problem := range discrepancies(surfaceKeys(t), keys) {
		t.Error(problem)
	}
}
