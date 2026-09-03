package version_test

import (
	"fmt"

	"github.com/vislake/speed/go/saasctl/internal/version"
)

// ExampleValidate walks the two ways to check a release-version string:
// the boolean IsValid for a quick gate and Validate when the reason a
// version is refused matters. The accepted grammar mirrors the one the
// lockstep release pipeline applies to its workflow_dispatch version
// input (release.yml / tools/release/lockstep-release.py): a leading v,
// three dot-separated numeric parts, and an optional prerelease suffix.
func ExampleValidate() {
	fmt.Println(version.IsValid("v0.1.0"))
	fmt.Println(version.IsValid("1.2.3"))
	fmt.Println(version.Validate("v1.2.3-rc.1"))
	fmt.Println(version.Validate("v1.2"))
	// Output:
	// true
	// false
	// <nil>
	// invalid release version "v1.2": expected the form v<major>.<minor>.<patch> with an optional -prerelease suffix
}
