module example.com/smile-studio

go 1.25.0

// A consumer that pinned a speed module to an older release through a
// module-to-module replace: the pin wins over the require line at build
// time, so an upgrade that rewrote the requires and reported clean would be
// a lie about the version the project actually builds.
require (
	github.com/vislake/speed/go/authn v0.1.0
	github.com/vislake/speed/go/config v0.1.0
)

replace github.com/vislake/speed/go/authn => github.com/vislake/speed/go/authn v0.1.0
