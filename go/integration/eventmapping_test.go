package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func fixedTransform(data json.RawMessage) EventTransform {
	return func(context.Context, pkgcore.Event) (json.RawMessage, error) {
		return data, nil
	}
}

func TestEventMapping_Validate_MissingFields(t *testing.T) {
	valid := EventMapping{
		InternalType:  "org.member.joined",
		PublicType:    "org.member.joined",
		PublicVersion: "v1",
		Transform:     fixedTransform(json.RawMessage(`{}`)),
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("validate() on a fully populated mapping = %v, want nil", err)
	}

	tests := []struct {
		name   string
		mutate func(m EventMapping) EventMapping
	}{
		{"empty InternalType", func(m EventMapping) EventMapping { m.InternalType = ""; return m }},
		{"empty PublicType", func(m EventMapping) EventMapping { m.PublicType = ""; return m }},
		{"empty PublicVersion", func(m EventMapping) EventMapping { m.PublicVersion = ""; return m }},
		{"nil Transform", func(m EventMapping) EventMapping { m.Transform = nil; return m }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.mutate(valid)
			err := m.validate()
			if !apperrIs(err, ErrInvalidEventMapping) {
				t.Errorf("validate() = %v, want ErrInvalidEventMapping", err)
			}
		})
	}
}

func TestBuildEventMappingIndex_DuplicateInternalType_Refused(t *testing.T) {
	mappings := []EventMapping{
		{InternalType: "org.member.joined", PublicType: "org.member.joined", PublicVersion: "v1", Transform: fixedTransform(nil)},
		{InternalType: "org.member.joined", PublicType: "org.member.joined_v2", PublicVersion: "v1", Transform: fixedTransform(nil)},
	}
	_, err := buildEventMappingIndex(mappings)
	if !apperrIs(err, ErrDuplicateEventMapping) {
		t.Errorf("buildEventMappingIndex() error = %v, want ErrDuplicateEventMapping", err)
	}
}

func TestBuildEventMappingIndex_DistinctInternalTypesSamePublicType_Allowed(t *testing.T) {
	// Several internal events may legitimately fan into the same public
	// type/version (a merge, per this file's own doc comment).
	mappings := []EventMapping{
		{InternalType: "org.member.joined", PublicType: "org.member.changed", PublicVersion: "v1", Transform: fixedTransform(nil)},
		{InternalType: "org.member.removed", PublicType: "org.member.changed", PublicVersion: "v1", Transform: fixedTransform(nil)},
	}
	idx, err := buildEventMappingIndex(mappings)
	if err != nil {
		t.Fatalf("buildEventMappingIndex() error = %v, want nil", err)
	}
	if len(idx.byInternal) != 2 {
		t.Errorf("len(byInternal) = %d, want 2", len(idx.byInternal))
	}
	if !idx.publicTypes["org.member.changed"] {
		t.Error("publicTypes does not contain the shared public type")
	}
}

func TestBuildEventMappingIndex_InvalidMapping_Refused(t *testing.T) {
	_, err := buildEventMappingIndex([]EventMapping{{InternalType: "x"}})
	if !apperrIs(err, ErrInvalidEventMapping) {
		t.Errorf("buildEventMappingIndex() error = %v, want ErrInvalidEventMapping", err)
	}
}

func TestBuildEnvelope_WrapsTransformResultWithTypeAndVersion(t *testing.T) {
	mapping := EventMapping{
		InternalType:  "org.member.joined",
		PublicType:    "org.member.joined",
		PublicVersion: "v1",
		Transform:     fixedTransform(json.RawMessage(`{"user_id":"u-1"}`)),
	}
	body, err := buildEnvelope(context.Background(), mapping, pkgcore.Event{Type: "org.member.joined"})
	if err != nil {
		t.Fatalf("buildEnvelope: %v", err)
	}

	var envelope publicEventEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if envelope.Event.Type != "org.member.joined" || envelope.Event.Version != "v1" {
		t.Errorf("envelope.Event = %+v, want {org.member.joined v1}", envelope.Event)
	}
	if string(envelope.Data) != `{"user_id":"u-1"}` {
		t.Errorf("envelope.Data = %s, want the transform's own output verbatim", envelope.Data)
	}
}

func TestBuildEnvelope_TransformError_Propagated(t *testing.T) {
	mapping := EventMapping{
		InternalType:  "org.member.joined",
		PublicType:    "org.member.joined",
		PublicVersion: "v1",
		Transform: func(context.Context, pkgcore.Event) (json.RawMessage, error) {
			return nil, errTransformBoom
		},
	}
	_, err := buildEnvelope(context.Background(), mapping, pkgcore.Event{})
	if err == nil {
		t.Fatal("buildEnvelope() error = nil, want the transform's own error wrapped")
	}
}

var errTransformBoom = &transformError{"boom"}

type transformError struct{ msg string }

func (e *transformError) Error() string { return e.msg }
