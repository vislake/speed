package notification

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// TestInboxMessage_GetTenantID_ReturnsEmbeddedTenantModelValue pins the
// tenant-context mechanism the whole isolation story stands on: the value
// comes from the embedded dbkit.TenantModel field, and InboxMessage needs no
// GetTenantID of its own. A refactor that replaced the embed with a
// hand-declared TenantID field -- and forgot the method -- fails here first,
// before any repository test.
func TestInboxMessage_GetTenantID_ReturnsEmbeddedTenantModelValue(t *testing.T) {
	m := InboxMessage{TenantModel: dbkit.TenantModel{TenantID: "tenant-acme"}}
	if got, want := m.GetTenantID(), pkgcore.TenantID("tenant-acme"); got != want {
		t.Fatalf("GetTenantID() = %q, want %q", got, want)
	}
}

// TestInboxMessage_TableName_IsInAppMessages pins the row's home. The table
// name is referenced verbatim nowhere else in Go code -- migrations own the
// DDL and repository tests assert through sqlite_master -- so this is the
// single place a rename has to touch.
func TestInboxMessage_TableName_IsInAppMessages(t *testing.T) {
	if got, want := (InboxMessage{}).TableName(), tableInAppMessages; got != want {
		t.Fatalf("TableName() = %q, want %q", got, want)
	}
}

// compile-time check that the zero value already satisfies dbkit.TenantScoped:
// dbkit.Repository[InboxMessage] can only exist if this holds, but the
// assertion here makes the contract visible and breaks the build at the model
// rather than deep inside a repository instantiation.
var _ dbkit.TenantScoped = InboxMessage{}
