package pkgcore

import "testing"

func TestCapability_Has(t *testing.T) {
	tests := []struct {
		name string
		c    Capability
		want Capability
		has  bool
	}{
		{name: "zero has zero", c: 0, want: 0, has: true},
		{name: "any capability has zero (no requirement)", c: MultiReplicaSafe, want: 0, has: true},
		{name: "exact single bit matches itself", c: MultiReplicaSafe, want: MultiReplicaSafe, has: true},
		{name: "zero lacks a set bit", c: 0, want: MultiReplicaSafe, has: false},
		{name: "one bit lacks a different bit", c: MultiReplicaSafe, want: SurvivesRestart, has: false},
		{name: "both bits set satisfies either alone", c: MultiReplicaSafe | SurvivesRestart, want: MultiReplicaSafe, has: true},
		{name: "both bits set satisfies both together", c: MultiReplicaSafe | SurvivesRestart, want: MultiReplicaSafe | SurvivesRestart, has: true},
		{name: "one of two required bits missing fails", c: MultiReplicaSafe, want: MultiReplicaSafe | SurvivesRestart, has: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.Has(tt.want); got != tt.has {
				t.Errorf("Capability(%v).Has(%v) = %t, want %t", tt.c, tt.want, got, tt.has)
			}
		})
	}
}

func TestCapability_String(t *testing.T) {
	tests := []struct {
		name string
		c    Capability
		want string
	}{
		{name: "zero renders none", c: 0, want: "none"},
		{name: "single bit renders its name", c: MultiReplicaSafe, want: "MultiReplicaSafe"},
		{name: "other single bit renders its name", c: SurvivesRestart, want: "SurvivesRestart"},
		{name: "both bits render pipe-joined in declaration order", c: MultiReplicaSafe | SurvivesRestart, want: "MultiReplicaSafe|SurvivesRestart"},
		{
			// Declaration order must survive regardless of which bit is
			// physically set first when the value is constructed, since the
			// two are combined with a commutative OR.
			name: "declaration order does not depend on OR operand order",
			c:    SurvivesRestart | MultiReplicaSafe,
			want: "MultiReplicaSafe|SurvivesRestart",
		},
		{
			// An unnamed bit -- none exist yet, but the type has room for up
			// to eight -- must still surface in the rendering rather than
			// disappear silently, so a future bit added without a String
			// update is still visible in an error message.
			name: "an unnamed bit renders as its own hex literal",
			c:    Capability(1 << 7),
			want: "Capability(0x80)",
		},
		{
			name: "a named bit plus an unnamed bit renders both",
			c:    MultiReplicaSafe | Capability(1<<7),
			want: "MultiReplicaSafe|Capability(0x80)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.String(); got != tt.want {
				t.Errorf("Capability(%d).String() = %q, want %q", tt.c, got, tt.want)
			}
		})
	}
}
