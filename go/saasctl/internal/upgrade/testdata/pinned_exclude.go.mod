module example.com/smile-studio

go 1.25.0

// A consumer that excluded a speed module at the very version an upgrade
// targets: the exclude directive tells the go command that version of the
// module is unusable -- the module graph can never resolve to it, so a
// require line rewritten to that version would be honored by no build. An
// upgrade that rewrote the requires and reported clean would be a lie about
// the version the project actually builds.
require (
	github.com/vislake/speed/go/authn v0.1.0
	github.com/vislake/speed/go/config v0.1.0
)

exclude github.com/vislake/speed/go/authn v0.2.0
