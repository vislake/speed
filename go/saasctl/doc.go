// Package main builds the saasctl command-line tool: the consumer-side
// companion of the speed modules. Where modules are libraries a business
// project pulls in via go get, saasctl is the small CLI that creates, reads
// and upgrades the projects consuming them.
//
// Subcommands:
//
//	new     scaffold a new consumer project
//	upgrade rewrite a project's speed module requires to one lockstep
//	        version
//	db      maintain a generated project's SQLite database
//	config  show how a generated project's bootstrap configuration
//	        resolves
//
// This build wires all four commands; db migrate and config print landed
// with the saasctl milestone's B3 round (docs/internal/15-roadmap.md
// tracks which).
package main
