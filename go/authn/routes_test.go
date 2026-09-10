package authn

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// Tests for routes.go: the partition must claim exactly authn's own mount
// point and everything below it -- never a path that merely shares a
// prefix segment with it -- and must preserve the order and the handlers of
// every route it hands back.

// routePaths is the projection the assertions below compare: a partition's
// exact contents, in order.
func routePaths(routes []pkgcore.MountedRoute) []string {
	paths := make([]string, 0, len(routes))
	for _, route := range routes {
		paths = append(paths, route.Path)
	}
	return paths
}

func equalPaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestExemptSubtree_PartitionsAuthnsOwnMountPoint(t *testing.T) {
	authnHandler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	subHandler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	routes := []pkgcore.MountedRoute{
		{Path: "/api/v1/notes", Handler: subHandler},
		{Path: apiPath, Handler: authnHandler},
		{Path: apiPath + "/social", Handler: subHandler},
		{Path: "/api/v1/admin", Handler: subHandler},
		// Shares the leading characters of apiPath but not a path segment:
		// it is an unrelated route, not part of authn's subtree.
		{Path: apiPath + "x", Handler: subHandler},
	}

	subtree, rest := ExemptSubtree(routes)

	wantSubtree := []string{apiPath, apiPath + "/social"}
	if got := routePaths(subtree); !equalPaths(got, wantSubtree) {
		t.Fatalf("subtree paths = %v, want %v", got, wantSubtree)
	}
	wantRest := []string{"/api/v1/notes", "/api/v1/admin", apiPath + "x"}
	if got := routePaths(rest); !equalPaths(got, wantRest) {
		t.Fatalf("rest paths = %v, want %v", got, wantRest)
	}
	// The handlers travel with their routes: the subtree route is the very
	// handler the registry mounted, not a copy that lost it. Handlers are
	// func values, so identity is compared through their code pointers.
	if reflect.ValueOf(subtree[0].Handler).Pointer() != reflect.ValueOf(authnHandler).Pointer() {
		t.Fatal("the subtree route's handler did not survive the partition")
	}
	// The input is left untouched.
	if got := routePaths(routes); len(got) != 5 || routes[1].Path != apiPath {
		t.Fatalf("the input routes were modified: %v", got)
	}
}

func TestExemptSubtree_NoAuthnRoute_EmptySubtreeAndWholeInputAsRest(t *testing.T) {
	routes := []pkgcore.MountedRoute{
		{Path: "/api/v1/notes", Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})},
		{Path: "/api/v1/admin", Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})},
	}

	subtree, rest := ExemptSubtree(routes)

	if len(subtree) != 0 {
		t.Fatalf("subtree = %v, want empty for a composition with no authn route", routePaths(subtree))
	}
	if got := routePaths(rest); !equalPaths(got, []string{"/api/v1/notes", "/api/v1/admin"}) {
		t.Fatalf("rest paths = %v, want the whole input", got)
	}
}

func TestExemptSubtree_EmptyInput_YieldsEmptyPartitions(t *testing.T) {
	subtree, rest := ExemptSubtree(nil)
	if len(subtree) != 0 || len(rest) != 0 {
		t.Fatalf("ExemptSubtree(nil) = (%v, %v), want two empty partitions", routePaths(subtree), routePaths(rest))
	}
}
