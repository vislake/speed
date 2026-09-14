package db

import (
	"io/fs"

	"gorm.io/gorm"
)

// Migrations is the resource a module with tables declares to hand its
// migration files over. This module applies them; it writes none of its own,
// because a table belongs to the module that declares it.
//
// The declaration is a struct rather than an fs.FS, and that is load-bearing
// rather than decoration. A resource query matches by assignability, and
// embed.FS satisfies fs.FS; internationalisation bundles, template sets and
// most other embedded assets are embed.FS too. With fs.FS as the resource type
// the query would collect every one of them as a migration set and this module
// would try to apply whatever SQL-looking files they happen to contain. The
// wrapper makes the declaration an act rather than a side effect of the type an
// asset happens to have.
type Migrations struct {
	// FS holds one subdirectory per dialect, named by that dialect's value,
	// and that dialect's migration files inside it.
	//
	// Ordering within a module is by file name, so a 0001_ prefix is how a
	// declaring module makes lexical order agree with intended order.
	//
	// A declaration without a subdirectory for the running dialect is zero
	// migrations for it, not an error: that is how a module states it does
	// not support that engine.
	FS fs.FS
}

// Plugin is the resource a module declares to have a GORM plugin installed on
// every handle this module hands out. It is the way a cross-cutting capability
// that needs a concept from higher up the dependency graph — tenant filtering,
// audit capture — reaches the statements this module's handles issue.
//
// This module installs it as given and interprets nothing about it. It cannot
// check that a plugin was wired correctly either: a plugin that registers
// cleanly but does not act on some class of models looks exactly like one that
// works, and validating that belongs to the module that declared it.
//
// The wrapper struct is here for the reason Migrations has one, at lower
// strength: gorm.Plugin requires Initialize(*gorm.DB) error, which an unrelated
// declaration is unlikely to satisfy by accident. It mainly keeps the two
// resource declarations the same shape.
type Plugin struct {
	// Plugin is the plugin itself, installed with (*gorm.DB).Use.
	Plugin gorm.Plugin
}
