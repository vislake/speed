package org

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

func TestOrgNode_TableName(t *testing.T) {
	if got := (OrgNode{}).TableName(); got != tableOrgNodes {
		t.Errorf("TableName() = %q, want %q", got, tableOrgNodes)
	}
}

// TestOrgNode_GetTenantID_ReadsTheEmbeddedTenantModel pins that OrgNode's
// tenant accessor actually reports the field GORM populates. dbkit's
// TenantModel doc comment describes exactly how shadowing the promoted
// TenantID field silently breaks this -- leaving the column correct while
// GetTenantID returns "" and FindByID denies the row's own owner -- so this
// is a guard against that specific future edit, not a tautology.
func TestOrgNode_GetTenantID_ReadsTheEmbeddedTenantModel(t *testing.T) {
	tests := []struct {
		name string
		node OrgNode
		want pkgcore.TenantID
	}{
		{name: "populated", node: OrgNode{TenantModel: dbkit.TenantModel{TenantID: "tenant-a"}}, want: "tenant-a"},
		{name: "zero value", node: OrgNode{}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.node.GetTenantID(); got != tc.want {
				t.Errorf("GetTenantID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOrgNode_IsRoot(t *testing.T) {
	tests := []struct {
		name string
		node OrgNode
		want bool
	}{
		{name: "empty parent id is the root sentinel", node: OrgNode{ParentID: ""}, want: true},
		{name: "a node with a parent is not the root", node: OrgNode{ParentID: "aa"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.node.IsRoot(); got != tc.want {
				t.Errorf("IsRoot() = %v, want %v", got, tc.want)
			}
		})
	}
}
