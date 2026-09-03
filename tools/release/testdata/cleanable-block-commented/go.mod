// The same commented transitional replace as cleanable-commented, but
// inside a `replace (` block. Both parser branches must agree: strip the
// trailing comment and plan the drop. Before the comment-stripping fix
// the block branch refused this file while the single-line branch
// silently ignored it.
module github.com/vislake/speed/go/config

go 1.25.0

replace (
	github.com/vislake/speed/go/pkgcore => ../pkgcore // transition, remove at v1.0.0
)

require github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
