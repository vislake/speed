module github.com/vislake/speed/examples/minimal-host

go 1.26.0

// Neither pkg module has a released version yet, so this host resolves both
// from the sibling checkouts. A consumer of a published example would drop
// these two lines and depend on the released versions.
replace github.com/vislake/speed/pkg/config => ../../pkg/config

replace github.com/vislake/speed/pkg/core => ../../pkg/core

require (
	github.com/vislake/speed/pkg/config v0.0.0
	github.com/vislake/speed/pkg/core v0.0.0
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect
