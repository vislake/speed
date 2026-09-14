module github.com/vislake/speed/examples/minimal-host

go 1.26.0

// No pkg module has a released version yet, so this host resolves every one of
// them from the sibling checkouts. A consumer of a published example would drop
// the replace directives and depend on the released versions.
replace github.com/vislake/speed/pkg/config => ../../pkg/config

replace github.com/vislake/speed/pkg/core => ../../pkg/core

replace github.com/vislake/speed/pkg/log => ../../pkg/log

require (
	github.com/vislake/speed/pkg/config v0.0.0
	github.com/vislake/speed/pkg/core v0.0.0
	github.com/vislake/speed/pkg/log v0.0.0
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect
