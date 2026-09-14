package db

import (
	"errors"
	"fmt"
	"reflect"

	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/core"
)

// pluginInstall is one declared plugin together with the module that declared
// it, which is the pair every failure text below has to name: the module is who
// has to change the declaration, and this package cannot know how.
type pluginInstall struct {
	module string
	plugin gorm.Plugin
}

// collectPlugins takes the declared plugins out of the registry, in the order
// they are installed.
//
// A declaration from a module that resolution left out is dropped, the way that
// module's migrations are: a module that does not run has nothing in force here.
// The price is real and it is what "not enabled" means rather than a defect — a
// module whose whole work is declaring a plugin, the shape of a cross-cutting
// capability that needs nothing back from the database, takes that capability
// away from every dependant when it is disabled, and the statements those
// dependants go on to issue run unfiltered and unaudited with nothing said. An
// assembly that disabled the module did not ask for its plugin, and installing
// it anyway is the other reading, the one where a declaration of a module that
// never ran is in force.
//
// The order is the dependency order of the declaring modules, and declaration
// order inside one module. Install order is callback order in GORM: plugins
// registered to the same processor run in the order they were installed, so two
// modules that both act on a statement need their dependency to decide which
// acts first, and the two are ordered that way. Modules at the same place have
// no dependency between them; they fall back to module name so that one build
// installs them as the next build does.
func collectPlugins(reg *core.Registry) ([]pluginInstall, error) {
	rank := dependencyRank(reg)

	byModule := make(map[string][]Plugin)
	var modules []string
	for _, declared := range core.Resources[Plugin](reg) {
		if state, known := reg.Enablement(declared.Module); known && state.State == core.StateDisabled {
			continue
		}
		if absent(declared.Value.Plugin) {
			// Nothing to install, and installing it anyway would
			// panic inside GORM on the first call rather than report
			// anything: a declaration of nil is a defect in the
			// declaring module, and it is named here.
			return nil, fmt.Errorf("%w: module %q declares a plugin that carries nothing to "+
				"install, and a plugin of nil cannot be installed on a handle",
				ErrPluginFailed, declared.Module)
		}
		if _, seen := byModule[declared.Module]; !seen {
			modules = append(modules, declared.Module)
		}
		byModule[declared.Module] = append(byModule[declared.Module], declared.Value)
	}
	inDependencyOrder(rank, modules)

	var plugins []pluginInstall
	for _, module := range modules {
		for _, declared := range byModule[module] {
			plugins = append(plugins, pluginInstall{module: module, plugin: declared.Plugin})
		}
	}
	return plugins, nil
}

// installPlugins installs every declared plugin on one handle, in order.
//
// It runs inside the assembly and before the handle is handed to anyone, so a
// dependant that resolves the capability can never see a handle missing a
// plugin: there is no moment at which one exists and the other does not. The
// same call serves a handle built with Open, which is why it takes a handle
// rather than a product — a second connection carries the same declared plugins
// as the delivered one, and a caller that moved to another database would
// otherwise lose tenant filtering or auditing silently.
//
// A plugin that will not install fails the startup under ErrPluginFailed. This
// module cannot judge the plugin's content, so all it reports is which
// declaration failed and what the plugin itself said.
func installPlugins(session *gorm.DB, plugins []pluginInstall) error {
	declaredBy := make(map[string]string, len(plugins))
	for _, install := range plugins {
		name := install.plugin.Name()
		previous, taken := declaredBy[name]
		if err := session.Use(install.plugin); err != nil {
			// GORM reports a name that is already taken as
			// ErrRegistered and does not say whose it was, so which
			// module holds it is read from what this run already
			// installed. Both are named: a handle carries one plugin
			// per name, so one of the two declarations is redundant
			// and the operator picks which.
			if taken && errors.Is(err, gorm.ErrRegistered) {
				return fmt.Errorf("%w: modules %q and %q both declare a plugin named %q, and a "+
					"handle installs one plugin per name: %w",
					ErrPluginFailed, previous, install.module, name, err)
			}
			return fmt.Errorf("%w: installing the plugin %q declared by module %q failed: %w",
				ErrPluginFailed, name, install.module, err)
		}
		declaredBy[name] = install.module
	}
	return nil
}

// absent reports whether a declared plugin carries nothing to install: the nil
// interface, or a typed nil pointer inside it. The second is not the first, so
// a plain nil test would pass it through to a call that panics.
func absent(plugin gorm.Plugin) bool {
	if plugin == nil {
		return true
	}
	v := reflect.ValueOf(plugin)
	return v.Kind() == reflect.Pointer && v.IsNil()
}
