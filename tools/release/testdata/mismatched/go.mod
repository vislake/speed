// A mismatched-sibling publishable module: the replace is the sibling
// form, but the target directory does not match the replaced module's own
// name (a copy-paste of another module's replace line). The cleanup must
// refuse it: deleting the line would not restore the tidied state the
// first release needs.
module github.com/vislake/speed/go/dbkit

go 1.25.0

replace github.com/vislake/speed/go/pkgcore => ../tenancy

require github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
