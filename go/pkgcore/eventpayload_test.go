package pkgcore

import (
	"maps"
	"testing"
)

// taggedPayload is the same-process shape: a publisher's own struct, whose
// JSON tags are what a subscriber can read without importing the type.
type taggedPayload struct {
	UserID string `json:"user_id"`
	Count  int    `json:"count"`
	Active bool   `json:"active"`
}

func TestEventPayloadFields(t *testing.T) {
	nilTyped := (*taggedPayload)(nil)

	tests := []struct {
		name    string
		payload any
		want    map[string]any
		wantOK  bool
	}{
		{
			name:    "a struct payload delivers its JSON tags",
			payload: taggedPayload{UserID: "u-1", Count: 3, Active: true},
			want:    map[string]any{"user_id": "u-1", "count": float64(3), "active": true},
			wantOK:  true,
		},
		{
			name:    "an untagged struct delivers its Go field names",
			payload: struct{ UserID string }{UserID: "u-1"},
			want:    map[string]any{"UserID": "u-1"},
			wantOK:  true,
		},
		{
			name:    "the wire shape a broker-backed bus delivers passes through",
			payload: map[string]any{"user_id": "u-1", "count": 3},
			// The round trip is what the helper guarantees, so a number
			// comes back as the float64 JSON decodes it to -- exactly what
			// a broker-backed bus already hands a subscriber.
			want:   map[string]any{"user_id": "u-1", "count": float64(3)},
			wantOK: true,
		},
		{
			name:    "an empty object is a usable payload with no fields",
			payload: map[string]any{},
			want:    map[string]any{},
			wantOK:  true,
		},
		{
			name:    "nil payload",
			payload: nil,
			wantOK:  false,
		},
		{
			name:    "a typed nil marshals to JSON null, not an object",
			payload: nilTyped,
			wantOK:  false,
		},
		{
			name:    "a string payload is not an object",
			payload: "u-1",
			wantOK:  false,
		},
		{
			name:    "an array payload is not an object",
			payload: []any{"u-1"},
			wantOK:  false,
		},
		{
			name:    "a payload that cannot be marshalled",
			payload: make(chan int),
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := EventPayloadFields(tt.payload)
			if ok != tt.wantOK {
				t.Fatalf("EventPayloadFields(%v) ok = %t, want %t", tt.payload, ok, tt.wantOK)
			}
			if !maps.Equal(got, tt.want) {
				t.Errorf("EventPayloadFields(%v) = %v, want %v", tt.payload, got, tt.want)
			}
		})
	}
}

func TestEventPayloadString(t *testing.T) {
	tests := []struct {
		name    string
		payload any
		keys    []string
		want    string
		wantOK  bool
	}{
		{
			name:    "the tagged spelling of a same-process struct",
			payload: taggedPayload{UserID: "u-1"},
			keys:    []string{"user_id"},
			want:    "u-1",
			wantOK:  true,
		},
		{
			name:    "the wire shape",
			payload: map[string]any{"user_id": "u-1"},
			keys:    []string{"user_id"},
			want:    "u-1",
			wantOK:  true,
		},
		{
			name:    "spellings are probed in the order given",
			payload: map[string]any{"user_id": "snake", "userId": "camel"},
			keys:    []string{"user_id", "userId"},
			want:    "snake",
			wantOK:  true,
		},
		{
			name:    "an empty string at an earlier spelling falls through",
			payload: map[string]any{"user_id": "", "userId": "u-2"},
			keys:    []string{"user_id", "userId"},
			want:    "u-2",
			wantOK:  true,
		},
		{
			name:    "a non-string value at an earlier spelling falls through",
			payload: map[string]any{"user_id": 42, "userId": "u-2"},
			keys:    []string{"user_id", "userId"},
			want:    "u-2",
			wantOK:  true,
		},
		{
			name:    "no key carries the field",
			payload: map[string]any{"other": "x"},
			keys:    []string{"user_id", "userId"},
			wantOK:  false,
		},
		{
			name:    "only a non-string spelling is present",
			payload: map[string]any{"user_id": 42},
			keys:    []string{"user_id"},
			wantOK:  false,
		},
		{
			name:    "a nested value is not a top-level field",
			payload: map[string]any{"user": map[string]any{"user_id": "u-1"}},
			keys:    []string{"user_id"},
			wantOK:  false,
		},
		{
			name:    "nil payload",
			payload: nil,
			keys:    []string{"user_id"},
			wantOK:  false,
		},
		{
			name:    "no keys to probe",
			payload: map[string]any{"user_id": "u-1"},
			keys:    nil,
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := EventPayloadString(tt.payload, tt.keys...)
			if ok != tt.wantOK {
				t.Fatalf("EventPayloadString(%v, %v) ok = %t, want %t", tt.payload, tt.keys, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("EventPayloadString(%v, %v) = %q, want %q", tt.payload, tt.keys, got, tt.want)
			}
		})
	}
}

func TestEventPayloadBool(t *testing.T) {
	tests := []struct {
		name    string
		payload any
		keys    []string
		want    bool
		wantOK  bool
	}{
		{
			name:    "a true value",
			payload: map[string]any{"succeeded": true},
			keys:    []string{"succeeded"},
			want:    true,
			wantOK:  true,
		},
		{
			name:    "an explicit false is a value, not a miss",
			payload: map[string]any{"succeeded": false},
			keys:    []string{"succeeded"},
			want:    false,
			wantOK:  true,
		},
		{
			name:    "the tagged spelling of a same-process struct",
			payload: taggedPayload{Active: true},
			keys:    []string{"active"},
			want:    true,
			wantOK:  true,
		},
		{
			name:    "spellings are probed in the order given",
			payload: map[string]any{"succeeded": "not-a-bool", "Succeeded": true},
			keys:    []string{"succeeded", "Succeeded"},
			want:    true,
			wantOK:  true,
		},
		{
			name:    "no key carries the field",
			payload: map[string]any{"other": true},
			keys:    []string{"succeeded"},
			wantOK:  false,
		},
		{
			name:    "only a non-bool spelling is present",
			payload: map[string]any{"succeeded": "true"},
			keys:    []string{"succeeded"},
			wantOK:  false,
		},
		{
			name:    "nil payload",
			payload: nil,
			keys:    []string{"succeeded"},
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := EventPayloadBool(tt.payload, tt.keys...)
			if ok != tt.wantOK {
				t.Fatalf("EventPayloadBool(%v, %v) ok = %t, want %t", tt.payload, tt.keys, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("EventPayloadBool(%v, %v) = %t, want %t", tt.payload, tt.keys, got, tt.want)
			}
		})
	}
}

func TestDecodeEventPayload(t *testing.T) {
	t.Run("a map payload decodes into a caller's own shape", func(t *testing.T) {
		var got taggedPayload
		if err := DecodeEventPayload(map[string]any{"user_id": "u-1", "count": 3}, &got); err != nil {
			t.Fatalf("DecodeEventPayload() error = %v", err)
		}
		if want := (taggedPayload{UserID: "u-1", Count: 3}); got != want {
			t.Errorf("DecodeEventPayload() = %+v, want %+v", got, want)
		}
	})

	t.Run("a struct payload round-trips", func(t *testing.T) {
		var got taggedPayload
		if err := DecodeEventPayload(taggedPayload{UserID: "u-1", Active: true}, &got); err != nil {
			t.Fatalf("DecodeEventPayload() error = %v", err)
		}
		if want := (taggedPayload{UserID: "u-1", Active: true}); got != want {
			t.Errorf("DecodeEventPayload() = %+v, want %+v", got, want)
		}
	})

	t.Run("a payload that cannot be marshalled reports the failure", func(t *testing.T) {
		var got taggedPayload
		if err := DecodeEventPayload(make(chan int), &got); err == nil {
			t.Fatal("DecodeEventPayload() error = nil, want a marshal error")
		}
	})

	t.Run("a payload of the wrong shape reports the failure", func(t *testing.T) {
		var got taggedPayload
		if err := DecodeEventPayload("not-an-object", &got); err == nil {
			t.Fatal("DecodeEventPayload() error = nil, want an unmarshal error")
		}
	})

	t.Run("a non-pointer target reports the failure", func(t *testing.T) {
		if err := DecodeEventPayload(map[string]any{"user_id": "u-1"}, taggedPayload{}); err == nil {
			t.Fatal("DecodeEventPayload() error = nil, want an unmarshal error")
		}
	})
}
