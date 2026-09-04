package rbac

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// throughJSON round-trips a payload the way pkgcore's distributed
// EventBus does: the publisher's struct is marshalled, and the subscriber
// receives whatever encoding/json decodes it back into (a
// map[string]any). Writing this out rather than hand-building the map is
// what makes these tests catch a field RENAME -- a hand-written map with
// the old key would keep passing while the real bus stopped decoding.
func throughJSON(t *testing.T, payload any) any {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshalling the payload: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshalling the payload: %v", err)
	}
	return decoded
}

func TestRoleBindingChangedFromWire_ConcreteStruct_PassesThrough(t *testing.T) {
	want := RoleBindingChangedEvent{
		TenantID:    "tenant-a",
		UserID:      "user-1",
		RoleID:      "role-1",
		RoleKey:     "owner",
		NodeID:      "node-7",
		ActorUserID: "admin-9",
		ChangedAt:   time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC),
	}
	got, ok := roleBindingChangedFromWire(want)
	if !ok {
		t.Fatal("the in-memory bus's own payload type was not recognized")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %+v, want %+v", got, want)
	}
}

func TestRoleBindingChangedFromWire_JSONMap_DecodesEveryField(t *testing.T) {
	// The distributed deployment mode's path. If this stops working, a
	// revoke on one replica no longer invalidates any other replica's
	// cache and the TTL becomes the only thing converging them -- a
	// security regression that no standalone-mode test could see.
	want := RoleBindingChangedEvent{
		TenantID:    "tenant-a",
		UserID:      "user-1",
		RoleID:      "role-1",
		RoleKey:     "admin",
		NodeID:      "node-7",
		ActorUserID: "admin-9",
		ChangedAt:   time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC),
	}

	got, ok := roleBindingChangedFromWire(throughJSON(t, want))
	if !ok {
		t.Fatal("a payload that crossed a replica boundary was not recognized")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %+v, want %+v", got, want)
	}
}

func TestRoleBindingChangedFromWire_TenantWideGrant_DecodesAsEmptyNode(t *testing.T) {
	// The empty-string node sentinel must survive the wire: decoded as
	// anything else, a tenant-wide grant would arrive looking node-scoped.
	want := RoleBindingChangedEvent{TenantID: "tenant-a", UserID: "user-1", RoleID: "role-1", RoleKey: "member"}
	got, ok := roleBindingChangedFromWire(throughJSON(t, want))
	if !ok {
		t.Fatal("the payload was not recognized")
	}
	if got.NodeID != "" {
		t.Fatalf("NodeID = %q, want empty (tenant-wide)", got.NodeID)
	}
}

func TestRoleBindingChangedFromWire_MissingAddressFields_IsRejected(t *testing.T) {
	// TenantID and UserID together address the cache entry, which is the
	// only thing the subscriber does with this event. Without either, the
	// event is not actionable, and accepting it would invalidate the entry
	// of the zero-value subject instead.
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{name: "no tenant", payload: map[string]any{"UserID": "user-1"}},
		{name: "empty tenant", payload: map[string]any{"TenantID": "", "UserID": "user-1"}},
		{name: "no user", payload: map[string]any{"TenantID": "tenant-a"}},
		{name: "empty user", payload: map[string]any{"TenantID": "tenant-a", "UserID": ""}},
		{name: "tenant is not a string", payload: map[string]any{"TenantID": 7, "UserID": "user-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := roleBindingChangedFromWire(tc.payload); ok {
				t.Fatalf("payload %v was accepted, want rejected", tc.payload)
			}
		})
	}
}

func TestRoleBindingChangedFromWire_UnparseableTimestamp_KeepsTheEvent(t *testing.T) {
	// Invalidation does not depend on the timestamp, so dropping the event
	// over one would trade a cosmetic problem for a stale-cache one.
	got, ok := roleBindingChangedFromWire(map[string]any{
		"TenantID":  "tenant-a",
		"UserID":    "user-1",
		"ChangedAt": "not a timestamp",
	})
	if !ok {
		t.Fatal("an event with an unparseable timestamp was dropped")
	}
	if !got.ChangedAt.IsZero() {
		t.Fatalf("ChangedAt = %v, want the zero time", got.ChangedAt)
	}
}

func TestRoleBindingChangedFromWire_ForeignPayload_IsNotThisEvent(t *testing.T) {
	for _, payload := range []any{nil, "a string", 42, RoleChangedEvent{TenantID: "tenant-a"}} {
		if _, ok := roleBindingChangedFromWire(payload); ok {
			t.Fatalf("payload %#v was accepted as a binding change", payload)
		}
	}
}

func TestRoleChangedFromWire_ConcreteStruct_PassesThrough(t *testing.T) {
	want := RoleChangedEvent{
		TenantID:    "tenant-a",
		RoleID:      "role-1",
		RoleKey:     "owner",
		Permissions: []string{"notes:read", "notes:write"},
		ActorUserID: "admin-9",
		ChangedAt:   time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC),
	}
	got, ok := roleChangedFromWire(want)
	if !ok {
		t.Fatal("the in-memory bus's own payload type was not recognized")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %+v, want %+v", got, want)
	}
}

func TestRoleChangedFromWire_JSONMap_RebuildsThePermissionSlice(t *testing.T) {
	// Permissions arrives as []any, never []string: a type assertion to
	// []string silently yields nil, which would leave an audit consumer
	// believing the role now grants nothing.
	want := RoleChangedEvent{
		TenantID:    "tenant-a",
		RoleID:      "role-1",
		RoleKey:     "owner",
		Permissions: []string{"notes:read", "notes:write"},
		ActorUserID: "admin-9",
		ChangedAt:   time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC),
	}
	got, ok := roleChangedFromWire(throughJSON(t, want))
	if !ok {
		t.Fatal("a payload that crossed a replica boundary was not recognized")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %+v, want %+v", got, want)
	}
}

func TestRoleChangedFromWire_NonStringPermissionEntries_AreSkipped(t *testing.T) {
	got, ok := roleChangedFromWire(map[string]any{
		"TenantID":    "tenant-a",
		"Permissions": []any{"notes:read", 7, nil, "notes:write"},
	})
	if !ok {
		t.Fatal("the payload was rejected")
	}
	if !reflect.DeepEqual(got.Permissions, []string{"notes:read", "notes:write"}) {
		t.Fatalf("Permissions = %v, want the two string entries only", got.Permissions)
	}
}

func TestRoleChangedFromWire_MissingTenant_IsRejected(t *testing.T) {
	// A role change invalidates a whole tenant. Without the tenant there
	// is nothing to invalidate, and accepting it would clear the entries
	// of the empty tenant id instead.
	for _, payload := range []map[string]any{
		{"RoleID": "role-1"},
		{"TenantID": ""},
		{"TenantID": 7},
	} {
		if _, ok := roleChangedFromWire(payload); ok {
			t.Fatalf("payload %v was accepted, want rejected", payload)
		}
	}
}

func TestRoleChangedFromWire_ForeignPayload_IsNotThisEvent(t *testing.T) {
	for _, payload := range []any{nil, "a string", 42, RoleBindingChangedEvent{TenantID: "tenant-a", UserID: "user-1"}} {
		if _, ok := roleChangedFromWire(payload); ok {
			t.Fatalf("payload %#v was accepted as a role change", payload)
		}
	}
}

func TestEventDecls_MatchTheDeclaredTypesAndPayloads(t *testing.T) {
	// The declaration is what a subscriber (and the future event catalog)
	// reads to know which concrete type to expect. A payload type name
	// that drifted from the struct would make that declaration a lie, so
	// the names are compared against the types themselves.
	wantPayload := map[string]string{
		EventRoleBindingAssigned: reflect.TypeOf(RoleBindingChangedEvent{}).String(),
		EventRoleBindingRevoked:  reflect.TypeOf(RoleBindingChangedEvent{}).String(),
		EventRoleBindingRestored: reflect.TypeOf(RoleBindingChangedEvent{}).String(),
		EventRoleChanged:         reflect.TypeOf(RoleChangedEvent{}).String(),
	}
	if len(eventDecls) != len(wantPayload) {
		t.Fatalf("declared %d events, want %d", len(eventDecls), len(wantPayload))
	}
	for _, decl := range eventDecls {
		want, ok := wantPayload[decl.Type]
		if !ok {
			t.Fatalf("event %q is declared but not expected", decl.Type)
		}
		if decl.PayloadType != want {
			t.Fatalf("event %q declares payload type %q, want %q", decl.Type, decl.PayloadType, want)
		}
		if decl.Description == "" {
			t.Fatalf("event %q has no description", decl.Type)
		}
	}
}
