package config

import (
	"strings"
	"testing"
)

func TestRenderMarkdown_RendersOneRowPerItemInGivenOrder(t *testing.T) {
	min := "1"
	max := "10"
	items := []ConfigItemDescriptor{
		{
			Key: "brand.site_name", Type: "string", Public: true,
			Description: "The tenant's display name", Group: "brand",
			HasDefault: true, Default: "Smile Studio",
		},
		{
			Key: "billing.retry_limit", Type: "int",
			Description: "How many | retries", Group: "billing",
			HasDefault: true, Default: "3", Min: &min, Max: &max,
		},
		{
			Key: "support.reply_email", Type: "string", Sensitive: true,
			Description: "no default here",
		},
		{
			Key: "ai.premium_upsell", Type: itemTypeBool, IsFeatureFlag: true,
			HasDefault: true, Default: "true", FlagDependsOn: []string{"ai.smile_preview"},
		},
	}

	rendered := RenderMarkdown(items)

	for _, want := range []string{
		"# Configuration reference",
		"| `brand.site_name` | item | string | `Smile Studio` | -- | false | true | brand | The tenant's display name |",
		"| `billing.retry_limit` | item | int | `3` | 1 .. 10 | false | false | billing | How many \\| retries |",
		"| `support.reply_email` | item | string | _(none)_ | -- | true | false |  | no default here |",
		"| `ai.premium_upsell` | flag | bool | `true` | -- | false | false |  | (depends on `ai.smile_preview`) |",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("RenderMarkdown() missing row %q\nfull output:\n%s", want, rendered)
		}
	}

	// One row per input item, in the order given.
	siteIdx := strings.Index(rendered, "brand.site_name")
	retryIdx := strings.Index(rendered, "billing.retry_limit")
	replyIdx := strings.Index(rendered, "support.reply_email")
	upsellIdx := strings.Index(rendered, "ai.premium_upsell")
	if siteIdx >= retryIdx || retryIdx >= replyIdx || replyIdx >= upsellIdx {
		t.Errorf("RenderMarkdown() did not preserve input order: indices = %d, %d, %d, %d", siteIdx, retryIdx, replyIdx, upsellIdx)
	}
}

func TestRenderMarkdown_EmptyInput_StillRendersTheHeader(t *testing.T) {
	rendered := RenderMarkdown(nil)
	if !strings.Contains(rendered, "| Key | Kind | Type") {
		t.Errorf("RenderMarkdown(nil) = %q, want the table header present", rendered)
	}
}

func TestRenderMarkdown_NewlineInDescriptionIsCollapsed(t *testing.T) {
	rendered := RenderMarkdown([]ConfigItemDescriptor{
		{Key: "x", Type: "string", Description: "line one\nline two"},
	})
	if strings.Contains(rendered, "line one\nline two") {
		t.Error("RenderMarkdown() left a raw newline inside a table cell")
	}
	if !strings.Contains(rendered, "line one line two") {
		t.Errorf("RenderMarkdown() = %q, want the newline collapsed to a space", rendered)
	}
}

// TestService_Describe_RenderMarkdown_EndToEnd proves the two exported
// pieces actually compose: a real attached Service's Describe() output
// renders through RenderMarkdown into a table containing every declared
// key.
func TestService_Describe_RenderMarkdown_EndToEnd(t *testing.T) {
	svc := attachDefaultServiceForTest(t)
	rendered := RenderMarkdown(svc.Describe())
	for _, key := range []string{
		"brand.site_name", "brand.welcome_interval", "support.reply_email",
		"billing.retry_limit", "brand.help_url", "ai.smile_preview", "ai.premium_upsell",
	} {
		if !strings.Contains(rendered, "`"+key+"`") {
			t.Errorf("RenderMarkdown(svc.Describe()) missing key %q\nfull output:\n%s", key, rendered)
		}
	}
}
