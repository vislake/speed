module github.com/vislake/speed/go/ratelimit

go 1.26.0

// apperr.HasCode (the error-code probe this module's test asserts on) is not
// in a released pkgcore version yet, so this module resolves pkgcore from
// the sibling checkout. A replace directive in a dependency is ignored by
// consumers, so this affects this module's own standalone builds only.
replace github.com/vislake/speed/go/pkgcore => ../pkgcore

require github.com/vislake/speed/go/pkgcore v0.0.1

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/nicksnyder/go-i18n/v2 v2.6.1 // indirect
	golang.org/x/text v0.41.0 // indirect
)
