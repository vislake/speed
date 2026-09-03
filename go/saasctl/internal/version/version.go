// Package version validates release-version strings for the saasctl CLI.
//
// The accepted form is the repository's single release-version form -- the
// leading "v" plus three dot-separated numbers, with an optional prerelease
// suffix. The pattern is kept in step with its two authoritative copies:
// VERSION_PATTERN in tools/release/lockstep-release.py and the
// version-input validation in .github/workflows/release.yml, both of which
// cite the same expression ("release-version form required"). Changing the
// accepted form means changing all three in one commit.
package version

import (
	"fmt"
	"regexp"
)

// patternSource is the release-version form: a mandatory leading "v", three
// dot-separated numeric segments, and an optional prerelease suffix of one
// or more dot-separated alphanumeric-or-dash segments.
const patternSource = `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`

var versionRE = regexp.MustCompile(patternSource)

// Validate reports whether v is a release-version string. It is the Go
// twin of lockstep-release.py's VERSION_PATTERN match: a version that
// passes here is a version the release pipeline accepts.
func Validate(v string) error {
	if !versionRE.MatchString(v) {
		return fmt.Errorf("invalid release version %q: expected the form v<major>.<minor>.<patch> with an optional -prerelease suffix", v)
	}
	return nil
}

// IsValid reports whether v is a release-version string.
func IsValid(v string) bool {
	return versionRE.MatchString(v)
}
