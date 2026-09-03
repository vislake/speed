package version

import "testing"

// TestValidate mirrors the release pipeline's own acceptance boundary: the
// versions here are the same cases release.yml's version-input validation
// and lockstep-release.py's VERSION_PATTERN accept or refuse. A mismatch
// between this table and either of those is a broken release form, not a
// test detail.
func TestValidate(t *testing.T) {
	t.Parallel()

	valid := []string{
		"v0.1.0",
		"v1.2.3",
		"v10.20.30",
		"v1.2.3-alpha",
		"v1.2.3-rc.1",
		"v1.2.3-alpha.1.beta",
		"v1.2.3-20260903.0",
		"v1.2.3-a-b.c-d",
		"v1.2.3-0",
	}
	for _, v := range valid {
		v := v
		t.Run("accept "+v, func(t *testing.T) {
			t.Parallel()
			if err := Validate(v); err != nil {
				t.Errorf("Validate(%q) = %v, want nil", v, err)
			}
			if !IsValid(v) {
				t.Errorf("IsValid(%q) = false, want true", v)
			}
		})
	}

	invalid := []string{
		"",
		"1.2.3",             // leading v required
		"V1.2.3",            // uppercase v not accepted
		"v1.2",              // patch segment required
		"v1.2.3.4",          // a fourth numeric segment is not part of the form
		"v1.2.3-",           // empty prerelease suffix
		"v1.2.3-alpha.",     // dangling dot
		"v1.2.3-alpha..1",   // empty prerelease segment
		"v1.2.3_alpha",      // underscore is not accepted
		"v1.2.3-alpha beta", // no spaces
		"v",
		"version",
		"v1.2.3.4-beta",
		"1.2.3-beta",
		"v 1.2.3",
	}
	for _, v := range invalid {
		v := v
		t.Run("reject "+v, func(t *testing.T) {
			t.Parallel()
			if err := Validate(v); err == nil {
				t.Errorf("Validate(%q) = nil, want error", v)
			}
			if IsValid(v) {
				t.Errorf("IsValid(%q) = true, want false", v)
			}
		})
	}
}
