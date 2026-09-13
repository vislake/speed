package config

import (
	"fmt"
	"strings"

	"github.com/vislake/speed/pkg/core"
)

// The names this module keeps for itself. A declaration that took one of them
// would silently hijack the config locator or the help output.
const (
	locatorFlag      = "config"
	helpFlag         = "help"
	locatorEnvSuffix = "_CONFIG"
)

// manifest is the complete set of input items of a run, collected from every
// registered module. Everything downstream reads it: the environment variable
// names are derived from it, the command line is assembled from it, and the
// unknown-key and type checks are made against it.
type manifest struct {
	// items are the input items in collection order: by module name, and
	// within a module in declaration order.
	items []*manifestItem
	// byPath, byFlag, byShort and byEnv index the items by the names each
	// layer addresses them with. An item missing a name is absent from the
	// corresponding index.
	byPath  map[string]*manifestItem
	byFlag  map[string]*manifestItem
	byShort map[string]*manifestItem
	byEnv   map[string]*manifestItem
	// interior maps every path an item's ancestors occupy to the item that
	// put it there, which is what makes a path that is a leaf for one module
	// and an interior node for another detectable.
	interior map[string]*manifestItem
	// host is the declared host identity, zero when no module declared one.
	host HostIdentity
	// hasHost records whether a host identity was declared at all, which
	// tells an absent declaration from one that declares empty strings.
	hasHost bool
}

// newManifest collects the declarations of every registered module and merges
// them into one manifest, failing on the first conflict.
//
// The domain is every registered module, including the ones this run will not
// enable: which modules run is not known until they state it, and two
// implementations of the same capability must divide the paths between them
// even though only one of them ever runs.
func newManifest(reg *core.Registry) (*manifest, error) {
	m := &manifest{
		byPath:   make(map[string]*manifestItem),
		byFlag:   make(map[string]*manifestItem),
		byShort:  make(map[string]*manifestItem),
		byEnv:    make(map[string]*manifestItem),
		interior: make(map[string]*manifestItem),
	}
	if err := m.collectHost(reg); err != nil {
		return nil, err
	}
	for _, declared := range core.Resources[Schema](reg) {
		items, err := expandSchema(declared.Module, declared.Value, m.host.Prefix)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if err := m.add(item); err != nil {
				return nil, err
			}
		}
	}
	return m, nil
}

// collectHost takes the host identity, of which there may be at most one: two
// prefixes or two default locators give no ground to choose between them.
func (m *manifest) collectHost(reg *core.Registry) error {
	declared := core.Resources[HostIdentity](reg)
	if len(declared) > 1 {
		return fmt.Errorf("%w: modules %q and %q both declare a HostIdentity, and a run has one "+
			"environment variable prefix and one default config locator. Leave the declaration "+
			"to the host module",
			ErrConfigConflict, declared[0].Module, declared[1].Module)
	}
	if len(declared) == 1 {
		m.host, m.hasHost = declared[0].Value, true
	}
	return nil
}

// add puts one input item into the manifest, refusing it when it collides with
// an item already there. The item already there is always the one from the
// earlier module in name order, so the two parties of a conflict are named in
// the same order however the modules were registered.
func (m *manifest) add(item *manifestItem) error {
	if err := m.checkPath(item); err != nil {
		return err
	}
	if err := m.checkFlagNames(item); err != nil {
		return err
	}
	if err := m.checkEnvName(item); err != nil {
		return err
	}
	m.items = append(m.items, item)
	m.byPath[item.path] = item
	for _, ancestor := range ancestors(item.path) {
		if _, taken := m.interior[ancestor]; !taken {
			m.interior[ancestor] = item
		}
	}
	if item.item.FlagName != "" {
		m.byFlag[item.item.FlagName] = item
	}
	if item.item.FlagShort != "" {
		m.byShort[item.item.FlagShort] = item
	}
	if item.envName != "" {
		m.byEnv[item.envName] = item
	}
	return nil
}

// checkPath refuses two paths that intersect. Intersecting covers more than
// being equal: one path being a prefix of the other makes the same path a leaf
// for one module and an interior node for the other, and no primary source can
// express both at once.
func (m *manifest) checkPath(item *manifestItem) error {
	if other, taken := m.byPath[item.path]; taken {
		return fmt.Errorf("%w: modules %q and %q both declare the input item %q. Paths carry no "+
			"module name, so two modules under the same namespace have to divide the paths "+
			"between them",
			ErrConfigConflict, other.module, item.module, item.path)
	}
	if other, taken := m.interior[item.path]; taken {
		return pathIntersects(other, other.path, item, item.path)
	}
	for _, ancestor := range ancestors(item.path) {
		if other, taken := m.byPath[ancestor]; taken {
			return pathIntersects(other, ancestor, item, item.path)
		}
	}
	return nil
}

func pathIntersects(first *manifestItem, firstPath string, second *manifestItem, secondPath string) error {
	return fmt.Errorf("%w: module %q declares the input item %q while module %q declares %q, and "+
		"one path is a prefix of the other, so the same path is a value for one of them and a "+
		"section for the other. Give the two declarations separate namespaces or prefixes",
		ErrConfigConflict, first.module, firstPath, second.module, secondPath)
}

// checkFlagNames refuses a command-line name that is taken, or reserved. The
// command line is one flat namespace, so two items conflict there however far
// apart their paths are.
func (m *manifest) checkFlagNames(item *manifestItem) error {
	switch item.item.FlagName {
	case "":
	case locatorFlag, helpFlag:
		return fmt.Errorf("%w: module %q declares %q as the command-line name of %q, which this "+
			"module reserves for the config locator and the help output. Choose another name",
			ErrConfigConflict, item.module, item.item.FlagName, item.path)
	default:
		if other, taken := m.byFlag[item.item.FlagName]; taken {
			return fmt.Errorf("%w: modules %q and %q both declare the command-line name %q, for "+
				"%q and %q. Command-line names are global, whatever namespaces the items sit in",
				ErrConfigConflict, other.module, item.module, item.item.FlagName, other.path, item.path)
		}
	}
	if item.item.FlagShort == "" {
		return nil
	}
	if other, taken := m.byShort[item.item.FlagShort]; taken {
		return fmt.Errorf("%w: modules %q and %q both declare the command-line short name %q, for "+
			"%q and %q. Short names are global, whatever namespaces the items sit in",
			ErrConfigConflict, other.module, item.module, item.item.FlagShort, other.path, item.path)
	}
	return nil
}

// checkEnvName refuses an environment variable name that is taken, or reserved.
// A derived name and a pinned one meet in the same namespace, so either pair of
// them can collide.
func (m *manifest) checkEnvName(item *manifestItem) error {
	if item.envName == "" {
		return nil
	}
	if !item.envPinned && m.host.Prefix != "" && item.envName == locatorEnvName(m.host.Prefix) {
		return fmt.Errorf("%w: module %q declares %q, whose environment variable name derives to "+
			"%s, which this module reserves for the config locator. Rename the input item or pin "+
			"another name with EnvName",
			ErrConfigConflict, item.module, item.path, item.envName)
	}
	if other, taken := m.byEnv[item.envName]; taken {
		return fmt.Errorf("%w: modules %q and %q both read the environment variable %s, for %q and "+
			"%q. Environment variable names are global, whatever namespaces the items sit in",
			ErrConfigConflict, other.module, item.module, item.envName, other.path, item.path)
	}
	return nil
}

// locatorEnvName is the environment variable the config locator is read from.
func locatorEnvName(prefix string) string {
	return strings.ToUpper(prefix + locatorEnvSuffix)
}

// ancestors lists the proper prefixes of a path at its segment boundaries,
// shortest first.
func ancestors(path string) []string {
	var out []string
	for i, r := range path {
		if r == '.' {
			out = append(out, path[:i])
		}
	}
	return out
}
