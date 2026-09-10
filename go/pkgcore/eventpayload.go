package pkgcore

import "encoding/json"

// This file holds the structural decoders for Event.Payload. A payload
// reaches a subscriber in one of two shapes, depending on the delivery path:
// a same-process publish on the in-memory bus hands the handler the
// publisher's own concrete value, while a broker-backed bus delivers what its
// transport decoded -- an ordinary map[string]any carrying the same JSON
// encoding. A subscriber in another module cannot name the publisher's type
// at all (importing it would invert the module dependency direction), so it
// reads the payload structurally: decode it through JSON and look up the
// fields by their spellings, never by a type assertion.
//
// Every function here is tolerant by design: an unusable payload answers
// false (or a decode error), never a panic and never a partial value. A
// subscriber's contract with its publisher is log-and-continue -- a payload
// it cannot read is dropped, not answered back -- so there is no error to
// propagate on the probe path.

// EventPayloadFields decodes an event payload of any of those shapes into its
// top-level JSON object fields. It reports false when payload is nil, cannot
// be marshalled to JSON, or does not decode to a JSON object (a string, an
// array or a null).
func EventPayloadFields(payload any) (map[string]any, bool) {
	if payload == nil {
		return nil, false
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

// EventPayloadString returns the first non-empty string value found among keys
// in an event payload, probed in the order given so a caller can list the
// spellings a field may carry (a JSON tag, a struct's own field name). A key
// present with a value that is not a non-empty string is skipped, so probing
// continues to the next spelling. It reports false for every unusable shape --
// see EventPayloadFields -- and when no key carries a usable value.
func EventPayloadString(payload any, keys ...string) (string, bool) {
	fields, ok := EventPayloadFields(payload)
	if !ok {
		return "", false
	}
	for _, key := range keys {
		if value, present := fields[key]; present {
			if s, isString := value.(string); isString && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// EventPayloadBool is EventPayloadString's bool counterpart: it returns the
// first bool value found among keys, probed in the order given. false is a
// meaningful value rather than a miss -- a payload that explicitly carries
// false answers (false, true) -- so only an absent or non-bool value moves
// the probe on.
func EventPayloadBool(payload any, keys ...string) (bool, bool) {
	fields, ok := EventPayloadFields(payload)
	if !ok {
		return false, false
	}
	for _, key := range keys {
		if value, present := fields[key]; present {
			if b, isBool := value.(bool); isBool {
				return b, true
			}
		}
	}
	return false, false
}

// DecodeEventPayload decodes an event payload of any of those shapes into v
// (which must be a non-nil pointer) by round-tripping it through JSON, and
// returns the marshal or unmarshal error when the payload cannot be read that
// way. It is the typed counterpart of the probe helpers: a subscriber that
// CAN name a shape of its own -- or that must surface the failure rather than
// drop the event -- decodes directly instead of probing field spellings.
func DecodeEventPayload(payload any, v any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, v)
}
