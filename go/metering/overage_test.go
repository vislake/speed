package metering

import "testing"

func TestOverageThresholds_Resolve(t *testing.T) {
	def := 10.0
	thresholds := OverageThresholds{
		Default:    &def,
		PerFeature: map[string]float64{"ai.generation": 5},
	}

	tests := []struct {
		name        string
		feature     string
		wantValue   float64
		wantApplies bool
	}{
		{name: "per-feature override applies", feature: "ai.generation", wantValue: 5, wantApplies: true},
		{name: "falls back to default", feature: "api.calls", wantValue: 10, wantApplies: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, applies := thresholds.resolve(tc.feature)
			if applies != tc.wantApplies || got != tc.wantValue {
				t.Errorf("resolve(%q) = (%v, %v), want (%v, %v)", tc.feature, got, applies, tc.wantValue, tc.wantApplies)
			}
		})
	}
}

func TestOverageThresholds_Resolve_NoDefaultAndNoPerFeature_DoesNotApply(t *testing.T) {
	var thresholds OverageThresholds // zero value: nil Default, nil PerFeature
	if _, applies := thresholds.resolve("ai.generation"); applies {
		t.Error("resolve on the zero-value OverageThresholds reported applies=true, want false")
	}
}
