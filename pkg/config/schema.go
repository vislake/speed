// Package config loads the inputs an assembly needs before anything else
// exists: the values that are required before a database is reachable.
//
// A module declares the input items it accepts as a Schema resource. config
// walks the registry, merges every declaration into one manifest, derives the
// environment variable names and assembles the command line from it, reads the
// primary source the config locator points at, applies the environment and
// command-line layers over it, and hands each module its own values back
// through Reader.
//
// The root package carries the declarations, the manifest and the layers, and
// defines the Source and Format extension points. Concrete transports and
// formats live in subpackages, so their third-party dependencies stay out of
// the hosts that do not import them.
package config

// Schema is the resource type a module declares to state which input items it
// accepts. The module puts it in its descriptor's Resources, and config
// collects every one of them before reading any source: without the full
// manifest there is no way to tell an unknown key from a misspelled one, and
// no way to know which arguments the command line consists of.
type Schema struct {
	// Namespace is where this module's input items are mounted in the config
	// data. An empty namespace puts them at the top level.
	//
	// The module name takes no part in the paths, so renaming a module does
	// not move its configuration section. Two modules may share a namespace
	// as long as their input item paths stay disjoint.
	Namespace string
	// Mounts are the carrier structs, relative to the namespace. A module
	// may declare several, one per struct it decodes.
	Mounts []Mount
	// Items carries the origins and the metadata of individual input items,
	// keyed by the path relative to the namespace. An item absent from here
	// takes the default origins, which is why most items never appear.
	Items map[string]Item
}

// Mount is one carrier struct together with the path it hangs from. The struct
// is a prototype: it is read for the field structure and for the defaults, and
// never receives the values of a run. Two registries using the same prototype
// decode independently of each other.
type Mount struct {
	// Path is the prefix relative to the namespace. An empty path mounts the
	// struct at the namespace root.
	Path string
	// Value is the carrier struct, or a pointer to one. The current value of
	// each field is that field's default.
	Value any
}

// Origin is the set of sources an input item takes its value from.
type Origin uint8

const (
	// OriginPrimary takes the value from the primary config source, the file
	// or remote config centre the locator points at.
	OriginPrimary Origin = 1 << iota
	// OriginEnv takes the value from an environment variable.
	OriginEnv
	// OriginFlag takes the value from a command-line argument.
	OriginFlag
)

// resolved returns the origin set an item really carries. The zero value
// stands for the ordinary combination rather than for no origin at all: nearly
// every item takes that combination, which is what keeps those items out of
// Items entirely.
func (o Origin) resolved() Origin {
	if o == 0 {
		return OriginPrimary | OriginEnv
	}
	return o
}

// has reports whether the set contains an origin.
func (o Origin) has(other Origin) bool { return o&other != 0 }

// Item is the origins and the metadata of one input item. The primary source,
// the environment and the command line are not separate mechanisms but
// different origins of the same item, which is what this type expresses.
type Item struct {
	// Origins are the sources this item accepts. The zero value stands for
	// OriginPrimary | OriginEnv.
	Origins Origin
	// FlagName is the command-line long name. It is mandatory when Origins
	// contains OriginFlag and is never derived: the command line is a human
	// interface, and a name derived from a path is rarely the wanted one.
	FlagName string
	// FlagShort is the command-line short name, optional.
	FlagShort string
	// EnvName pins the environment variable name. An empty name is derived
	// from the prefix and the path; pinning is how conventional names such
	// as NO_COLOR are expressed.
	EnvName string
	// Placeholder is the value placeholder in the help output.
	Placeholder string
	// Group is the section the help output lists this item under.
	Group string
	// Description explains the item in the help output. It is mandatory for
	// a sensitive item, whose default is not echoed: without it the reader
	// has nothing to go on.
	Description string
	// Required makes Decode fail when none of the primary source, the
	// environment and the command line gave the item a value. The default
	// layer does not count as a value, or a required item and one that took
	// the zero value could not be told apart.
	Required bool
	// Sensitive keeps the value out of logs and keeps the default out of the
	// help output.
	Sensitive bool
}

// HostIdentity is the resource a host declares to state the two inputs that
// precede all configuration and can therefore come from nowhere else. At most
// one may be registered.
//
// It travels as a resource rather than as a package-level setting so that the
// identity follows the registry: two registries in one process can carry
// different prefixes without interfering.
type HostIdentity struct {
	// Prefix is the environment variable prefix. With no prefix, derived
	// variable names do not apply while pinned ones still do.
	Prefix string
	// DefaultLocator is the config locator to use when neither the
	// environment nor the command line gives one. An empty locator with
	// nothing else given means there is no primary config source, which is a
	// legal configuration.
	DefaultLocator string
}
