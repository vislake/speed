package config

import (
	"slices"
	"strings"
)

// applyEnv overlays the environment on the config data and returns the names
// of the variables under the host's prefix that no input item claims.
//
// Those names are a diagnostic rather than a failure. The primary source and
// the command line are written by the host word for word, so a strange key
// there can only be a mistake; the process environment is not the host's
// alone, and an orchestrator injecting <PREFIX>_SERVICE_HOST would otherwise
// kill the process with a deployment action that has nothing to do with the
// configuration. The caller writes the names out, which keeps the means of
// noticing without turning an environment nobody controls into a startup
// condition.
func (d *data) applyEnv(m *manifest, environ []string) ([]string, error) {
	values := environMap(environ)
	for _, item := range m.items {
		if item.envName == "" {
			continue
		}
		text, set := values[item.envName]
		if !set {
			continue
		}
		value, err := coerceText(item.path, text, item)
		if err != nil {
			return nil, err
		}
		d.set(item, value, layerEnv)
	}
	return unclaimedEnvNames(m, values), nil
}

// environMap turns the NAME=VALUE form into a lookup. A later entry wins, as
// it does for the process itself.
func environMap(environ []string) map[string]string {
	values := make(map[string]string, len(environ))
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			continue
		}
		values[name] = value
	}
	return values
}

// unclaimedEnvNames lists the variables under the prefix that no input item
// reads, in name order. The scan domain is the prefix, so a pinned name such
// as NO_COLOR is outside it; the config locator is inside it but claimed by
// this module, and is not reported.
func unclaimedEnvNames(m *manifest, values map[string]string) []string {
	if m.host.Prefix == "" {
		return nil
	}
	head := strings.ToUpper(m.host.Prefix) + "_"
	locator := locatorEnvName(m.host.Prefix)
	var unclaimed []string
	for name := range values {
		if !strings.HasPrefix(name, head) || name == locator {
			continue
		}
		if _, claimed := m.byEnv[name]; claimed {
			continue
		}
		unclaimed = append(unclaimed, name)
	}
	slices.Sort(unclaimed)
	return unclaimed
}
