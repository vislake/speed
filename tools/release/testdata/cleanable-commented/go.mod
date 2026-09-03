// A publishable go/ module whose transitional replace carries a trailing
// line comment -- legal go.mod that `go mod edit` parses and preserves.
// The single-line parser must strip the comment and still plan the drop,
// exactly as it does for the bare form.
module github.com/vislake/speed/go/config

go 1.25.0

replace github.com/vislake/speed/go/pkgcore => ../pkgcore // transition, remove at v1.0.0

require github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
