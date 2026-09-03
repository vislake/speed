// An untidy publishable module: its module is under the publishable
// prefix, but its replace targets a consumer-shaped relative path
// (../../go/...). The first release cannot simply delete such a line
// (nothing about it is transitional), so the cleanup engine must refuse
// it instead of planning an edit.
module github.com/vislake/speed/go/observability

go 1.25.0

replace github.com/vislake/speed/go/pkgcore => ../../go/pkgcore

require github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
