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

// TestRenderMarkdown_SensitiveItemDefault_IsRedacted proves a Sensitive
// item's Default is never rendered in the clear: it must carry the same
// redactedMarker events.go's redactIf already uses to keep a Sensitive
// value off the event bus, never the plaintext default -- a generated
// configuration reference is a committed, shared document exactly like
// that bus.
func TestRenderMarkdown_SensitiveItemDefault_IsRedacted(t *testing.T) {
	rendered := RenderMarkdown([]ConfigItemDescriptor{
		{
			Key: "billing.stripe_api_key", Type: "string", Sensitive: true,
			Description: "The tenant's Stripe secret key.", Group: "billing",
			HasDefault: true, Default: "sk_live_super_secret_value",
		},
	})

	if strings.Contains(rendered, "sk_live_super_secret_value") {
		t.Errorf("RenderMarkdown() leaked a Sensitive item's plaintext Default into the generated doc:\n%s", rendered)
	}
	want := "| `billing.stripe_api_key` | item | string | `" + redactedMarker + "` | -- | true | false | billing | The tenant's Stripe secret key. |"
	if !strings.Contains(rendered, want) {
		t.Errorf("RenderMarkdown() missing redacted row %q\nfull output:\n%s", want, rendered)
	}
}

// TestRenderMarkdown_NonSensitiveItemDefault_RendersRealValue guards against
// over-redaction: a non-Sensitive item's Default must still render its real
// value, unchanged.
func TestRenderMarkdown_NonSensitiveItemDefault_RendersRealValue(t *testing.T) {
	rendered := RenderMarkdown([]ConfigItemDescriptor{
		{
			Key: "brand.site_name", Type: "string",
			Description: "The tenant's display name", Group: "brand",
			HasDefault: true, Default: "Smile Studio",
		},
	})

	if !strings.Contains(rendered, "`Smile Studio`") {
		t.Errorf("RenderMarkdown() over-redacted a non-Sensitive item's Default:\n%s", rendered)
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
