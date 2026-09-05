package config

import (
	"testing"
)

// TestService_Describe_ReturnsEveryItemAndFlagSortedByKey pins the shape
// the whole method exists for: a caller (a Markdown reference generator,
// an admin console) sees every declared item AND every declared flag,
// sorted by Key, with no schema.go internals leaking through -- exercising
// serviceTestSchemaItems' full field spread (a public string with a
// default, a Sensitive item, an int with Min/Max bounds, a key with no
// default at all) plus serviceTestSchemaFlags' dependency chain.
func TestService_Describe_ReturnsEveryItemAndFlagSortedByKey(t *testing.T) {
	svc := attachDefaultServiceForTest(t)

	got := svc.Describe()

	wantCount := len(serviceTestSchemaItems) + len(serviceTestSchemaFlags)
	if len(got) != wantCount {
		t.Fatalf("Describe() returned %d entries, want %d (%d items + %d flags)",
			len(got), wantCount, len(serviceTestSchemaItems), len(serviceTestSchemaFlags))
	}

	for i := 1; i < len(got); i++ {
		if got[i-1].Key >= got[i].Key {
			t.Fatalf("Describe() not sorted by Key: %q then %q", got[i-1].Key, got[i].Key)
		}
	}

	byKey := make(map[string]ConfigItemDescriptor, len(got))
	for _, d := range got {
		byKey[d.Key] = d
	}

	site, ok := byKey["brand.site_name"]
	if !ok {
		t.Fatal("Describe() missing brand.site_name")
	}
	if site.Type != "string" || !site.Public || site.Sensitive {
		t.Errorf("brand.site_name = %+v, want Type=string Public=true Sensitive=false", site)
	}
	if !site.HasDefault || site.Default != "Smile Studio" {
		t.Errorf("brand.site_name Default = (%v, %q), want (true, %q)", site.HasDefault, site.Default, "Smile Studio")
	}
	if site.Description != "The tenant's display name" || site.Group != "brand" {
		t.Errorf("brand.site_name Description/Group = (%q, %q), want the declared values", site.Description, site.Group)
	}
	if site.IsFeatureFlag {
		t.Error("brand.site_name.IsFeatureFlag = true, want false (a plain ConfigItem)")
	}

	sensitive, ok := byKey["support.reply_email"]
	if !ok {
		t.Fatal("Describe() missing support.reply_email")
	}
	if !sensitive.Sensitive {
		t.Error("support.reply_email.Sensitive = false, want true")
	}
	if sensitive.HasDefault {
		t.Error("support.reply_email.HasDefault = true, want false (no Default was declared)")
	}

	bounded, ok := byKey["billing.retry_limit"]
	if !ok {
		t.Fatal("Describe() missing billing.retry_limit")
	}
	if bounded.Min == nil || bounded.Max == nil {
		t.Fatalf("billing.retry_limit Min/Max = (%v, %v), want both non-nil", bounded.Min, bounded.Max)
	}
	if *bounded.Min != "1" || *bounded.Max != "10" {
		t.Errorf("billing.retry_limit Min/Max = (%q, %q), want (\"1\", \"10\")", *bounded.Min, *bounded.Max)
	}

	unset, ok := byKey["brand.help_url"]
	if !ok {
		t.Fatal("Describe() missing brand.help_url")
	}
	if unset.HasDefault {
		t.Error("brand.help_url.HasDefault = true, want false")
	}

	flag, ok := byKey["ai.premium_upsell"]
	if !ok {
		t.Fatal("Describe() missing ai.premium_upsell")
	}
	if !flag.IsFeatureFlag {
		t.Error("ai.premium_upsell.IsFeatureFlag = false, want true")
	}
	if flag.Type != itemTypeBool {
		t.Errorf("ai.premium_upsell.Type = %q, want %q", flag.Type, itemTypeBool)
	}
	if len(flag.FlagDependsOn) != 1 || flag.FlagDependsOn[0] != "ai.smile_preview" {
		t.Errorf("ai.premium_upsell.FlagDependsOn = %v, want [ai.smile_preview]", flag.FlagDependsOn)
	}

	plain, ok := byKey["ai.smile_preview"]
	if !ok {
		t.Fatal("Describe() missing ai.smile_preview")
	}
	if len(plain.FlagDependsOn) != 0 {
		t.Errorf("ai.smile_preview.FlagDependsOn = %v, want empty", plain.FlagDependsOn)
	}
}

// TestService_Describe_MutatingTheResultDoesNotReachTheSchema proves the
// returned slices and pointers are fresh copies: mutating them must never
// corrupt the frozen schema a later Describe() call (or the Service's own
// Get/Set paths) reads from.
func TestService_Describe_MutatingTheResultDoesNotReachTheSchema(t *testing.T) {
	svc := attachDefaultServiceForTest(t)

	first := svc.Describe()
	for i := range first {
		if first[i].Key == "billing.retry_limit" {
			*first[i].Min = "corrupted"
		}
		if first[i].Key == "ai.premium_upsell" && len(first[i].FlagDependsOn) > 0 {
			first[i].FlagDependsOn[0] = "corrupted"
		}
	}

	second := svc.Describe()
	for _, d := range second {
		if d.Key == "billing.retry_limit" {
			if d.Min == nil || *d.Min != "1" {
				t.Errorf("billing.retry_limit.Min after mutating a PRIOR Describe() result = %v, want \"1\" (schema corrupted)", d.Min)
			}
		}
		if d.Key == "ai.premium_upsell" {
			if len(d.FlagDependsOn) != 1 || d.FlagDependsOn[0] != "ai.smile_preview" {
				t.Errorf("ai.premium_upsell.FlagDependsOn after mutating a PRIOR Describe() result = %v, want [ai.smile_preview] (schema corrupted)", d.FlagDependsOn)
			}
		}
	}
}

// TestService_Describe_NoItemsOrFlags_ReturnsEmptyNotNil pins the boundary
// case: a Service attached over an empty schema returns a zero-length
// slice a caller can range over unconditionally, not nil forcing a special
// case.
func TestService_Describe_NoItemsOrFlags_ReturnsEmptyNotNil(t *testing.T) {
	svc, _ := attachServiceForTest(t, openServiceTestDB(t), nil, nil, nil)
	got := svc.Describe()
	if got == nil {
		t.Error("Describe() = nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("Describe() = %d entries, want 0", len(got))
	}
}
