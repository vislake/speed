package config

import (
	"strconv"
	"strings"
)

// RenderMarkdown renders items -- typically a *Service.Describe() call's
// return value -- into one Markdown table: the short script
// docs/internal/13-documentation-standards.md's must-have doc list asks
// for alongside the full configuration item listing itself, "generated
// from the config schema, never hand-written." It is an ordinary exported
// function rather than a separate command-line tool: unlike
// tools/gen_error_code_index.py's error-code index (statically greppable
// from Go source alone), a live configuration schema only exists after a
// real host has run Kernel.Bootstrap and Module.Attach -- there is no
// static source form to scan, since every module's ConfigItem/FeatureFlag
// declarations only become one merged schema at Attach time. A host wanting
// a generated docs/config-reference.md writes:
//
//	svc, err := configModule.Attach(reg)
//	// ...
//	os.WriteFile("docs/config-reference.md", []byte(config.RenderMarkdown(svc.Describe())), 0o644)
//
// RenderMarkdown renders items in the order given -- it performs no
// sorting of its own, since Describe() is already the ordering authority
// (sorted by Key) and a caller building a custom view (grouped by Group,
// say) may deliberately hand over a different order.
func RenderMarkdown(items []ConfigItemDescriptor) string {
	var b strings.Builder
	b.WriteString("# Configuration reference\n\n")
	b.WriteString("Generated from the live configuration schema via `config.Service.Describe` and `config.RenderMarkdown` -- do not hand-edit.\n\n")
	b.WriteString("| Key | Kind | Type | Default | Bounds | Sensitive | Public | Group | Description |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, it := range items {
		b.WriteString("| `")
		b.WriteString(it.Key)
		b.WriteString("` | ")
		if it.IsFeatureFlag {
			b.WriteString("flag")
		} else {
			b.WriteString("item")
		}
		b.WriteString(" | ")
		b.WriteString(it.Type)
		b.WriteString(" | ")
		b.WriteString(renderDefault(it))
		b.WriteString(" | ")
		b.WriteString(renderBounds(it))
		b.WriteString(" | ")
		b.WriteString(strconv.FormatBool(it.Sensitive))
		b.WriteString(" | ")
		b.WriteString(strconv.FormatBool(it.Public))
		b.WriteString(" | ")
		b.WriteString(escapeMarkdownCell(it.Group))
		b.WriteString(" | ")
		b.WriteString(renderDescription(it))
		b.WriteString(" |\n")
	}
	return b.String()
}

// renderDefault renders one row's Default cell: the canonical value in a
// code span when HasDefault, or an explicit "none" marker otherwise -- a
// key with no default has no value to serve at all until a row exists at
// some scope (ErrItemUnset), which is worth a reader seeing at a glance
// rather than an empty, ambiguous cell.
func renderDefault(it ConfigItemDescriptor) string {
	if !it.HasDefault {
		return "_(none)_"
	}
	return "`" + escapeMarkdownCell(it.Default) + "`"
}

// renderBounds renders one row's Bounds cell from Min/Max, both of which
// are nil for the common case (most items declare neither).
func renderBounds(it ConfigItemDescriptor) string {
	if it.Min == nil && it.Max == nil {
		return "--"
	}
	min := "--"
	if it.Min != nil {
		min = *it.Min
	}
	max := "--"
	if it.Max != nil {
		max = *it.Max
	}
	return min + " .. " + max
}

// renderDescription appends a feature flag's FlagDependsOn, when present,
// to its Description -- a flag's own runtime "enabled" meaning is
// incomplete without knowing what it depends on, and a config reference is
// exactly the place a reader looks that fact up.
func renderDescription(it ConfigItemDescriptor) string {
	desc := escapeMarkdownCell(it.Description)
	if len(it.FlagDependsOn) == 0 {
		return desc
	}
	deps := make([]string, len(it.FlagDependsOn))
	for i, d := range it.FlagDependsOn {
		deps[i] = "`" + d + "`"
	}
	suffix := "(depends on " + strings.Join(deps, ", ") + ")"
	if desc == "" {
		return suffix
	}
	return desc + " " + suffix
}

// escapeMarkdownCell escapes the one character (a literal pipe) that would
// otherwise break a Markdown table row's column boundaries, and collapses
// embedded newlines to spaces -- a multi-line Description would otherwise
// silently truncate the row at the renderer's first line break.
func escapeMarkdownCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
