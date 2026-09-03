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
//	db      run the project's migrations (planned)
//	config  inspect the project's dynamic configuration (planned)
//
// This build wires new and upgrade; db and config land in the later rounds
// of the saasctl milestone (docs/internal/02-repo-and-release.md and
// docs/internal/15-roadmap.md track which).
package main
