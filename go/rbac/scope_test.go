package rbac

import (
	"context"
	"reflect"
	"testing"
)

func TestPathWithinSubtree(t *testing.T) {
	cases := []struct {
		name     string
		nodePath string
		prefix   string
		want     bool
	}{
		{
			name:     "the subtree root is inside its own subtree",
			nodePath: "/g1/r2",
			prefix:   "/g1/r2",
			want:     true,
		},
		{
			name:     "a descendant is inside",
			nodePath: "/g1/r2/s7",
			prefix:   "/g1/r2",
			want:     true,
		},
		{
			name:     "a deep descendant is inside",
			nodePath: "/g1/r2/s7/chair3",
			prefix:   "/g1/r2",
			want:     true,
		},
		{
			// THE bug this predicate exists for. A plain
			// strings.HasPrefix answers true here and hands region 20's
			// data to whoever was granted region 2. If this case ever
			// starts passing as true, the module is leaking across the
			// organization tree.
			name:     "a sibling whose name merely starts with the prefix segment is OUTSIDE",
			nodePath: "/g1/r20",
			prefix:   "/g1/r2",
			want:     false,
		},
		{
			name:     "the same trap one level deeper",
			nodePath: "/g1/r20/s1",
			prefix:   "/g1/r2",
			want:     false,
		},
		{
			name:     "a parent is not inside its child's subtree",
			nodePath: "/g1",
			prefix:   "/g1/r2",
			want:     false,
		},
		{
			name:     "an unrelated branch is outside",
			nodePath: "/g2/r2",
			prefix:   "/g1/r2",
			want:     false,
		},
		{
			name:     "a trailing slash on the node path is insignificant",
			nodePath: "/g1/r2/",
			prefix:   "/g1/r2",
			want:     true,
		},
		{
			name:     "a trailing slash on the prefix is insignificant",
			nodePath: "/g1/r2/s7",
			prefix:   "/g1/r2/",
			want:     true,
		},
		{
			name:     "trailing slashes on both sides are insignificant",
			nodePath: "/g1/r2/",
			prefix:   "/g1/r2/",
			want:     true,
		},
		{
			name:     "the root prefix contains any absolute path",
			nodePath: "/g1/r2/s7",
			prefix:   "/",
			want:     true,
		},
		{
			name:     "the root prefix contains the root itself",
			nodePath: "/",
			prefix:   "/",
			want:     true,
		},
		{
			// An empty prefix is what an unresolved node degrades to. It
			// must match NOTHING: a wildcard here would turn every
			// resolution failure into a tenant-wide grant, the exact
			// fail-open this module refuses.
			name:     "an empty prefix matches nothing",
			nodePath: "/g1/r2",
			prefix:   "",
			want:     false,
		},
		{
			name:     "an empty prefix does not even match an empty path",
			nodePath: "",
			prefix:   "",
			want:     false,
		},
		{
			name:     "an empty node path is inside nothing",
			nodePath: "",
			prefix:   "/g1",
			want:     false,
		},
		{
			name:     "an empty node path is not inside the root either",
			nodePath: "",
			prefix:   "/",
			want:     false,
		},
		{
			name:     "a relative node path is not inside the absolute root",
			nodePath: "g1/r2",
			prefix:   "/",
			want:     false,
		},
		{
			name:     "relative paths match each other segment-wise",
			nodePath: "g1/r2/s7",
			prefix:   "g1/r2",
			want:     true,
		},
		{
			name:     "the sibling trap holds for relative paths too",
			nodePath: "g1/r20",
			prefix:   "g1/r2",
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PathWithinSubtree(tc.nodePath, tc.prefix); got != tc.want {
				t.Fatalf("PathWithinSubtree(%q, %q) = %v, want %v", tc.nodePath, tc.prefix, got, tc.want)
			}
		})
	}
}

func TestDataScope_Includes(t *testing.T) {
	cases := []struct {
		name     string
		scope    DataScope
		nodePath string
		want     bool
	}{
		{
			name:     "a denied scope includes nothing",
			scope:    DataScope{Denied: true},
			nodePath: "/g1",
			want:     false,
		},
		{
			// The sharpest form of the previous case: a Denied scope that
			// also carries prefixes (which the evaluator never builds, but
			// a caller could) must still include nothing. Denied is the
			// decision, not a hint.
			name:     "denied wins over any prefixes present",
			scope:    DataScope{Denied: true, SubtreePrefixes: []string{"/g1"}},
			nodePath: "/g1/r2",
			want:     false,
		},
		{
			name:     "a tenant-wide scope includes any node",
			scope:    DataScope{TenantWide: true},
			nodePath: "/g9/r9/s9",
			want:     true,
		},
		{
			name:     "a node inside one of several prefixes is included",
			scope:    DataScope{SubtreePrefixes: []string{"/g1/r2", "/g3"}},
			nodePath: "/g3/r1/s5",
			want:     true,
		},
		{
			name:     "a node outside every prefix is excluded",
			scope:    DataScope{SubtreePrefixes: []string{"/g1/r2", "/g3"}},
			nodePath: "/g2/r2",
			want:     false,
		},
		{
			name:     "the sibling trap holds through Includes",
			scope:    DataScope{SubtreePrefixes: []string{"/g1/r2"}},
			nodePath: "/g1/r20",
			want:     false,
		},
		{
			name:     "nesting prefixes needs no flattening by the caller",
			scope:    DataScope{SubtreePrefixes: []string{"/g1", "/g1/r2"}},
			nodePath: "/g1/r5",
			want:     true,
		},
		{
			name:     "the zero DataScope includes nothing",
			scope:    DataScope{},
			nodePath: "/g1",
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.scope.Includes(tc.nodePath); got != tc.want {
				t.Fatalf("Includes(%q) = %v, want %v", tc.nodePath, got, tc.want)
			}
		})
	}
}

func TestPermission_ComposesResourceColonAction(t *testing.T) {
	if got := Permission("notes", "read"); got != "notes:read" {
		t.Fatalf("Permission(notes, read) = %q, want notes:read", got)
	}
	// The module's own declarations must round-trip through the same
	// composer callers use, or Can would look for a string Register never
	// declared.
	if got := Permission("rbac", "manage"); got != PermissionManage {
		t.Fatalf("Permission(rbac, manage) = %q, want %q", got, PermissionManage)
	}
	if got := Permission("rbac", "read"); got != PermissionRead {
		t.Fatalf("Permission(rbac, read) = %q, want %q", got, PermissionRead)
	}
}

func TestDataScope_ZeroValueFieldsAreTheDocumentedShape(t *testing.T) {
	// A tenant-wide scope carries no prefixes, and the evaluator relies on
	// that: a caller that reads SubtreePrefixes without checking
	// TenantWide first would filter everything out. The scope is taken
	// from the evaluator rather than hand-built, so this pins what the
	// module actually produces instead of restating a struct literal.
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	scope, err := svc.DataScope(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("DataScope: %v", err)
	}
	if !scope.TenantWide {
		t.Fatalf("DataScope = %+v, want TenantWide", scope)
	}
	if !reflect.DeepEqual(scope.SubtreePrefixes, []string(nil)) {
		t.Fatalf("a tenant-wide DataScope carries prefixes %v, want none", scope.SubtreePrefixes)
	}
}
