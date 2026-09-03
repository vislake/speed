package org

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestBuildPath(t *testing.T) {
	tests := []struct {
		name       string
		parentPath string
		id         string
		want       string
	}{
		{name: "root has no parent path", parentPath: "", id: "aa", want: "/aa/"},
		{name: "child appends to the parent path", parentPath: "/aa/", id: "bb", want: "/aa/bb/"},
		{name: "grandchild keeps the whole chain", parentPath: "/aa/bb/", id: "cc", want: "/aa/bb/cc/"},
		{
			name:       "a parent path missing its trailing separator is repaired",
			parentPath: "/aa",
			id:         "bb",
			want:       "/aa/bb/",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildPath(tc.parentPath, tc.id); got != tc.want {
				t.Errorf("buildPath(%q, %q) = %q, want %q", tc.parentPath, tc.id, got, tc.want)
			}
		})
	}
}

func TestPathSegments(t *testing.T) {
	tests := []struct {
		name string
		path string
		want []string
	}{
		{name: "root", path: "/aa/", want: []string{"aa"}},
		{name: "three levels", path: "/aa/bb/cc/", want: []string{"aa", "bb", "cc"}},
		{name: "empty path has no segments", path: "", want: nil},
		{name: "separator only has no segments", path: "/", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pathSegments(tc.path)
			if len(got) != len(tc.want) {
				t.Fatalf("pathSegments(%q) = %v, want %v", tc.path, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("pathSegments(%q) = %v, want %v", tc.path, got, tc.want)
				}
			}
		})
	}
}

func TestDepthOf(t *testing.T) {
	tests := []struct {
		name string
		path string
		want int
	}{
		{name: "root sits at rootDepth", path: "/aa/", want: rootDepth},
		{name: "child", path: "/aa/bb/", want: 1},
		{name: "grandchild", path: "/aa/bb/cc/", want: 2},
		{name: "a path with no segments has no depth", path: "/", want: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := depthOf(tc.path); got != tc.want {
				t.Errorf("depthOf(%q) = %d, want %d", tc.path, got, tc.want)
			}
		})
	}
}

func TestSubtreePrefix(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "already terminated", path: "/aa/bb/", want: "/aa/bb/"},
		{name: "missing terminator is added", path: "/aa/bb", want: "/aa/bb/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := subtreePrefix(tc.path); got != tc.want {
				t.Errorf("subtreePrefix(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestSubtreePrefix_SiblingSharingAnIDPrefix_DoesNotMatch is the reason the
// grammar carries a TRAILING separator. Without it, the prefix of node "aa"
// would be "/r/aa", which is a string prefix of sibling "aab"'s path
// "/r/aab/" -- so a subtree scan would silently swallow an unrelated branch.
// This is not hypothetical for variable-length ids, and pinning it here
// keeps a future "simplification" of the grammar from reintroducing it.
func TestSubtreePrefix_SiblingSharingAnIDPrefix_DoesNotMatch(t *testing.T) {
	own := subtreePrefix("/r/aa/")
	sibling := "/r/aab/"

	if !strings.HasPrefix("/r/aa/deeper/", own) {
		t.Errorf("subtree prefix %q must match its own descendant", own)
	}
	if strings.HasPrefix(sibling, own) {
		t.Errorf("subtree prefix %q must NOT match sibling path %q", own, sibling)
	}
}

func TestIsDescendantOf(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		ancestorPath string
		want         bool
	}{
		{name: "child of root", path: "/aa/bb/", ancestorPath: "/aa/", want: true},
		{name: "grandchild of root", path: "/aa/bb/cc/", ancestorPath: "/aa/", want: true},
		{name: "a node is not its own descendant", path: "/aa/", ancestorPath: "/aa/", want: false},
		{name: "sibling", path: "/aa/bb/", ancestorPath: "/aa/cc/", want: false},
		{name: "ancestor is not a descendant", path: "/aa/", ancestorPath: "/aa/bb/", want: false},
		{name: "sibling sharing an id prefix", path: "/r/aab/", ancestorPath: "/r/aa/", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDescendantOf(tc.path, tc.ancestorPath); got != tc.want {
				t.Errorf("isDescendantOf(%q, %q) = %v, want %v", tc.path, tc.ancestorPath, got, tc.want)
			}
		})
	}
}

func TestRebasePath(t *testing.T) {
	tests := []struct {
		name            string
		path            string
		oldAncestorPath string
		newAncestorPath string
		want            string
		wantOK          bool
	}{
		{
			name:            "the moved node itself",
			path:            "/r/a/",
			oldAncestorPath: "/r/a/",
			newAncestorPath: "/r/b/a/",
			want:            "/r/b/a/",
			wantOK:          true,
		},
		{
			name:            "a descendant is carried along",
			path:            "/r/a/x/y/",
			oldAncestorPath: "/r/a/",
			newAncestorPath: "/r/b/a/",
			want:            "/r/b/a/x/y/",
			wantOK:          true,
		},
		{
			name:            "a node outside the subtree is refused rather than rewritten",
			path:            "/r/c/",
			oldAncestorPath: "/r/a/",
			newAncestorPath: "/r/b/a/",
			wantOK:          false,
		},
		{
			name:            "a sibling sharing an id prefix is refused",
			path:            "/r/ab/",
			oldAncestorPath: "/r/a/",
			newAncestorPath: "/r/b/a/",
			wantOK:          false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := rebasePath(tc.path, tc.oldAncestorPath, tc.newAncestorPath)
			if ok != tc.wantOK {
				t.Fatalf("rebasePath(%q, %q, %q) ok = %v, want %v",
					tc.path, tc.oldAncestorPath, tc.newAncestorPath, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("rebasePath(%q, %q, %q) = %q, want %q",
					tc.path, tc.oldAncestorPath, tc.newAncestorPath, got, tc.want)
			}
		})
	}
}

// TestValidateNodeID is the enforced half of path.go's dialect-identity
// proof. Every rejected case below would, if stored, break the equivalence
// between SQLite's ASCII-case-insensitive LIKE and PostgreSQL's
// case-sensitive one, or turn a bound prefix into a wildcard pattern.
func TestValidateNodeID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "a real uuid", id: "6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f"},
		{name: "hex digits only", id: "0123456789abcdef"},
		{name: "empty", id: "", wantErr: true},
		{name: "the LIKE any-sequence wildcard", id: "aa%bb", wantErr: true},
		{name: "the LIKE single-character wildcard", id: "aa_bb", wantErr: true},
		{name: "an uppercase hex digit", id: "6F1C2D3E", wantErr: true},
		{name: "an uppercase letter outside hex", id: "Store", wantErr: true},
		{name: "a lowercase letter outside hex", id: "store", wantErr: true},
		{name: "the path separator smuggled into an id", id: "aa/bb", wantErr: true},
		{name: "a backslash, LIKE's default escape on some engines", id: `aa\bb`, wantErr: true},
		{name: "a space", id: "aa bb", wantErr: true},
		{name: "a non-ASCII rune", id: "aaébb", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNodeID(tc.id)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateNodeID(%q) = nil, want ErrInvalidNodeID", tc.id)
				}
				assertCode(t, err, ErrInvalidNodeID.Code)
				return
			}
			if err != nil {
				t.Fatalf("validateNodeID(%q) = %v, want nil", tc.id, err)
			}
		})
	}
}

// TestValidateNodeID_UUIDNewString_IsAlwaysInTheAlphabet closes the loop on
// property 1 of the dialect-identity proof: the generator TreeService
// actually uses must only ever emit ids the validator accepts. A future
// switch to an id scheme with uppercase characters (a Crockford-base32 ULID,
// say) fails here, in this module, instead of silently diverging the two
// dialects' prefix scans in production.
func TestValidateNodeID_UUIDNewString_IsAlwaysInTheAlphabet(t *testing.T) {
	const samples = 200
	for i := 0; i < samples; i++ {
		id := uuid.NewString()
		if err := validateNodeID(id); err != nil {
			t.Fatalf("uuid.NewString() produced %q, which validateNodeID rejects: %v", id, err)
		}
		if strings.ToLower(id) != id {
			t.Fatalf("uuid.NewString() produced %q, which is not all-lowercase", id)
		}
	}
}

func TestValidatePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "root", path: "/aa/"},
		{name: "three real uuids", path: "/6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f/aa/bb/"},
		{name: "missing leading separator", path: "aa/", wantErr: true},
		{name: "missing trailing separator", path: "/aa", wantErr: true},
		{name: "no segments", path: "/", wantErr: true},
		{name: "empty", path: "", wantErr: true},
		{name: "a segment outside the alphabet", path: "/aa/Store/", wantErr: true},
		{name: "a segment carrying a LIKE wildcard", path: "/aa/b%b/", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePath(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validatePath(%q) = nil, want ErrInvalidNodeID", tc.path)
				}
				assertCode(t, err, ErrInvalidNodeID.Code)
				return
			}
			if err != nil {
				t.Fatalf("validatePath(%q) = %v, want nil", tc.path, err)
			}
		})
	}
}

func TestValidateName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		want     string
		wantCode string
	}{
		{name: "plain name", input: "North Region", want: "North Region"},
		{name: "surrounding whitespace is trimmed once, here", input: "  Store 7  ", want: "Store 7"},
		{name: "empty", input: "", wantCode: ErrNodeNameRequired.Code},
		{name: "whitespace only", input: "   \t\n ", wantCode: ErrNodeNameRequired.Code},
		{name: "exactly the limit", input: strings.Repeat("a", maxNameLen), want: strings.Repeat("a", maxNameLen)},
		{name: "one rune past the limit", input: strings.Repeat("a", maxNameLen+1), wantCode: ErrNodeNameTooLong.Code},
		{
			// Multi-byte runes must count as runes, not bytes: SQLite would
			// accept either, so only this test catches a byte-based bound.
			name:  "multi-byte runes count as runes",
			input: strings.Repeat("é", maxNameLen),
			want:  strings.Repeat("é", maxNameLen),
		},
		{
			name:     "multi-byte runes one past the limit",
			input:    strings.Repeat("é", maxNameLen+1),
			wantCode: ErrNodeNameTooLong.Code,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateName(tc.input)
			if tc.wantCode != "" {
				if err == nil {
					t.Fatalf("validateName(%q) = nil error, want %s", tc.input, tc.wantCode)
				}
				assertCode(t, err, tc.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("validateName(%q) = %v, want nil", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("validateName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
