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

// TestRenderMarkdown_SensitiveAndNonSensitiveItems_DoNotCrossContaminate is
// an adversarial check for P2-3: render several items together -- a
// Sensitive one first, a Sensitive one with an empty-string Default, and a
// non-Sensitive one whose real Default happens to look distinctive -- and
// confirm each row's redaction decision is independent of its neighbours'.
// A bug that redacted (or failed to redact) based on prior-row state, or
// that leaked a secret across a shared string-builder append, would show up
// here even though it would not show up testing one item at a time.
func TestRenderMarkdown_SensitiveAndNonSensitiveItems_DoNotCrossContaminate(t *testing.T) {
	rendered := RenderMarkdown([]ConfigItemDescriptor{
		{
			Key: "billing.stripe_api_key", Type: "string", Sensitive: true,
			Group: "billing", HasDefault: true, Default: "sk_live_first_secret",
		},
		{
			// A Sensitive item whose Default is the empty string: HasDefault
			// is true (a real, deliberate empty default), so this must still
			// render the redacted marker, not "_(none)_" and not a bare
			// empty code span that would let a reader wrongly infer "empty,
			// therefore safe, therefore not worth checking Sensitive for".
			Key: "billing.empty_secret", Type: "string", Sensitive: true,
			Group: "billing", HasDefault: true, Default: "",
		},
		{
			Key: "brand.site_name", Type: "string",
			Group: "brand", HasDefault: true, Default: "Acme Dental",
		},
		{
			Key: "billing.webhook_secret", Type: "string", Sensitive: true,
			Group: "billing", HasDefault: true, Default: "sk_live_second_secret",
		},
	})

	for _, leaked := range []string{"sk_live_first_secret", "sk_live_second_secret"} {
		if strings.Contains(rendered, leaked) {
			t.Errorf("RenderMarkdown() leaked %q into the generated doc:\n%s", leaked, rendered)
		}
	}
	if !strings.Contains(rendered, "`Acme Dental`") {
		t.Errorf("RenderMarkdown() over-redacted the non-Sensitive neighbour's Default:\n%s", rendered)
	}
	wantEmptySecretRow := "| `billing.empty_secret` | item | string | `" + redactedMarker + "` | -- | true | false | billing |  |"
	if !strings.Contains(rendered, wantEmptySecretRow) {
		t.Errorf("RenderMarkdown() must redact a Sensitive item's empty-string Default too, not render it as blank or none:\nwant row %q\nfull output:\n%s", wantEmptySecretRow, rendered)
	}
	// Every Sensitive row's Default cell must carry the marker exactly once,
	// each on its own row.
	if got := strings.Count(rendered, redactedMarker); got != 3 {
		t.Errorf("RenderMarkdown() rendered %d occurrences of %q, want exactly 3 (one per Sensitive item)\nfull output:\n%s", got, redactedMarker, rendered)
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
