package main

// document.go assembles the reference document: one ordered item list that
// merges the dynamic layer (the frozen schema snapshot, from
// config.Service.Describe) and the bootstrap layer (the curated env-var
// rows), plus the rendering of the two committed outputs and the root
// .env.example. Both outputs derive from the same item list so the Markdown
// table and the machine-readable JSON agree row for row, mirroring
// tools/gen_error_code_index.py's own twin-output discipline.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/vislake/speed/go/config"
)

// referenceItem is one row of the merged reference.
type referenceItem struct {
	// Layer is "dynamic" (configs-table configuration) or "bootstrap"
	// (process environment).
	Layer string `json:"layer"`

	// Key is the configuration key (dynamic) or environment variable
	// (bootstrap).
	Key string `json:"key"`

	// Module is the owning module: the key's dot-prefix for a dynamic item,
	// or the host (the reference app) for a bootstrap one.
	Module string `json:"module"`

	// Group mirrors the declaration's Group when the declaration carries
	// one.
	Group string `json:"group,omitempty"`

	// Type is the value type: "string", "int", "bool" or "duration" for a
	// dynamic item; the parsed kind for a bootstrap one ("string", "int",
	// "bool" or "hexkey").
	Type string `json:"type"`

	// Source names where the value comes from. Dynamic items live in the
	// configs table, writable at the system and tenant scope tiers.
	// Bootstrap items are read from the process environment (with the
	// fallback this table's Default column states); the pkgcore/config
	// loader generalizes that single source into flags > SPEED_* env >
	// optional YAML/JSON file > struct defaults for hosts that drive it.
	Source string `json:"source"`

	// Scope lists the scope tiers that can hold a value. Dynamic items
	// resolve tenant over system over schema default. Bootstrap items have
	// no scope tiers -- a process resolves them once.
	Scope []string `json:"scope"`

	// Sensitive marks secrets: encrypted at rest and redacted on the bus
	// for a dynamic item; secret material for a bootstrap one.
	Sensitive bool `json:"sensitive"`

	// Public marks values the unauthenticated public endpoint serves
	// (dynamic items only).
	Public bool `json:"public,omitempty"`

	// HasDefault reports whether a default exists (dynamic items).
	HasDefault bool `json:"has_default,omitempty"`

	// Default is the default value: the canonical form for a dynamic item
	// (the redacted marker when Sensitive -- a secret's plaintext never
	// crosses this boundary, exactly as it never crosses the event bus), or
	// the unset fallback for a bootstrap one.
	Default string `json:"default"`

	// Min and Max are a dynamic int/duration item's declared bounds.
	Min *string `json:"min,omitempty"`
	Max *string `json:"max,omitempty"`

	// IsFeatureFlag marks entries folded in from the FeatureFlag registrar.
	IsFeatureFlag bool `json:"is_feature_flag,omitempty"`

	// FlagDependsOn lists a feature flag's dependencies.
	FlagDependsOn []string `json:"flag_depends_on,omitempty"`

	// Consumers names who reads the value.
	Consumers string `json:"consumers"`

	// Description is the item's meaning.
	Description string `json:"description"`
}

// document is the assembled reference.
type document struct {
	// Items holds every row of both layers in rendering order.
	Items []referenceItem
	// Sources records the extraction source per layer, for the generated
	// header.
	Sources []string
}

// buildDocument assembles the reference from the schema snapshot and the
// bootstrap table, in layer order: bootstrap first (what a process resolves
// before anything else), then the dynamic items sorted by key (the ordering
// Describe itself guarantees, stable across runs). It returns the ordered
// bootstrap rows too, so the .env.example rendering shares the exact table
// the reference table renders.
func buildDocument(moduleDir string, descriptors []config.ConfigItemDescriptor) (*document, []bootstrapVar, error) {
	boot, problems, err := bootstrapRows(moduleDir)
	if err != nil {
		return nil, nil, err
	}
	if len(problems) > 0 {
		return nil, nil, fmt.Errorf("bootstrap inventory/table mismatch:\n  %s", strings.Join(problems, "\n  "))
	}

	doc := &document{}
	for _, row := range boot {
		fallback := row.fallback
		consumers := "the reference app's bootstrap (internal/app ConfigFromEnv and its siblings)"
		desc := row.summary
		doc.Items = append(doc.Items, referenceItem{
			Layer:       "bootstrap",
			Key:         row.env,
			Module:      "reference-app",
			Type:        row.kind,
			Source:      "process environment (os.Getenv)",
			Scope:       []string{"process"},
			Sensitive:   row.secret,
			Default:     fallback,
			Consumers:   consumers,
			Description: desc,
		})
	}

	for _, d := range descriptors {
		module := d.Key
		if i := strings.IndexByte(d.Key, '.'); i >= 0 {
			module = d.Key[:i]
		}
		defaultText := "_(none)_"
		if d.HasDefault {
			defaultText = d.Default
		}
		scope := []string{"system", "tenant"}
		source := "configs table (system scope row, tenant scope row; reads resolve tenant -> system -> default)"
		doc.Items = append(doc.Items, referenceItem{
			Layer:         "dynamic",
			Key:           d.Key,
			Module:        module,
			Group:         d.Group,
			Type:          d.Type,
			Source:        source,
			Scope:         scope,
			Sensitive:     d.Sensitive,
			Public:        d.Public,
			HasDefault:    d.HasDefault,
			Default:       defaultText,
			Min:           d.Min,
			Max:           d.Max,
			IsFeatureFlag: d.IsFeatureFlag,
			FlagDependsOn: d.FlagDependsOn,
			Consumers:     module + " (the owning module reads the value at runtime)",
			Description:   d.Description,
		})
	}
	return doc, boot, nil
}

// marshalJSON renders the machine-readable twin: the same item list the
// Markdown table renders, one object per item, deterministic (the list is
// already ordered; json.MarshalIndent preserves slice order and sorts map
// keys).
func (d *document) marshalJSON() string {
	payload := map[string]any{
		"generated_by": "examples/reference-app/cmd/configrefgen (go run ./cmd/configrefgen)",
		"items":        d.Items,
	}
	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(out) + "\n"
}

// escapeCell escapes the one character that breaks a Markdown table cell
// and collapses newlines.
func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// renderMarkdown renders the full reference document.
func (d *document) renderMarkdown() string {
	var b strings.Builder

	// Counts for the generated footer.
	var nDynamic, nFlags, nSensitive, nBootstrap int
	for _, it := range d.Items {
		switch it.Layer {
		case "dynamic":
			nDynamic++
			if it.IsFeatureFlag {
				nFlags++
			}
			if it.Sensitive {
				nSensitive++
			}
		case "bootstrap":
			nBootstrap++
		}
	}

	b.WriteString("# Configuration reference\n\n")
	b.WriteString("This reference lists every configuration value a speed-based application built from this repository's modules resolves: the **bootstrap** layer, the environment variables a process reads before anything else starts, and the **dynamic** layer, the configuration items and feature flags an operator edits at runtime. It is generated from the live configuration schema, never hand-written -- root CLAUDE.md's documentation discipline that \"the configuration reference is generated from the config schema\" applies to exactly this document; a stale reference is a CI failure.\n\n")
	b.WriteString("How it is built: the dynamic layer is enumerated through `config.Service.Describe` from the frozen schema of a composed host that registers every platform module whose declarations fold into the schema (authn, metering, compliance, sharing, pki and org -- see the generator's own header comment in `examples/reference-app/cmd/configrefgen/`); the bootstrap layer is the reference app's own environment surface, its inventory walked out of the app source and its per-variable facts curated in the same generator under a coverage gate. The two committed outputs (`docs/config-reference.md` and `docs/config-reference.json`) and the root `.env.example` are byte-identical across runs; regeneration is `go run ./cmd/configrefgen` from `examples/reference-app/`.\n\n")

	b.WriteString("## Bootstrap configuration\n\n")
	b.WriteString("The bootstrap layer is the reference app's startup environment: every variable below is read with `os.Getenv` (there is no dotenv loader in this codebase -- the committed `.env.example` at the repository root is the documented carrier for a shell or process manager to apply), and every variable is optional, falling back to the documented development default that keeps `go run ./cmd/server` booting a working standalone server with zero external dependencies. The generalized loader mechanism behind this surface is `go/pkgcore/config`: a host that drives it resolves each key from command-line flags, then the `SPEED_*` environment, then an optional YAML/JSON config file (`WithConfigFile`; an absent file is skipped silently), then the defaults set on its target struct -- flags > env > file > defaults. This app's own bootstrap does not drive the loader (its values carry app-specific resolution rules, key derivation among them); the pkgcore package documents the mechanism, and `config.example.yaml` at the repository root demonstrates its file format with load-verification.\n\n")
	b.WriteString("| Variable | Type | Unset fallback | Secret | What it configures |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, it := range d.Items {
		if it.Layer != "bootstrap" {
			continue
		}
		secret := ""
		if it.Sensitive {
			secret = "yes"
		}
		b.WriteString("| `" + it.Key + "` | " + it.Type + " | " + escapeCell(it.Default) + " | " + secret + " | " + escapeCell(it.Description) + " |\n")
	}
	b.WriteString("\n")
	b.WriteString("Secret materials (marked above) must come from a secret store in a real deployment, never from a committed file: the six key variables have documented, recognizable, NON-SECRET development defaults checked into the app source that a real deployment must override, and `APP_ROOT_KEY` is the recommended single secret from which all six are derived. The demo-affordance variables (`APP_DEMO_*`, `APP_AI_GATEWAY_IMAGE_*`, `APP_DISABLE_*`) gate demo behavior and are test rigs by design; `APP_DISABLE_DEMO_USER_HEADER` is the kill switch DEPLOY.md recommends for any deployment a real user might reach.\n\n")

	b.WriteString("## Dynamic configuration\n\n")
	b.WriteString("The dynamic layer holds the configuration an operator edits at runtime, served by the `go/config` module: every module declares the items and feature flags it owns during `Register` (`reg.Config.Add` / `reg.Features.Add`), and `config.Module.Attach` freezes them into one schema the moment Bootstrap returns. Values live in the shared `configs` table (platform data, keyed by `(key, scope, tenant_id)`) under two scope tiers: a **system** row is platform-wide (writing one requires an audited system context), a **tenant** row overrides it for one tenant, and a read resolves tenant row -> system row -> the schema default. Scope is a property of the row, not of the key: every key below may hold a row at either tier. A key with no default and no row at any reachable scope has no value to serve (`config.item_unset`). Writes publish `config.item.changed` (with `[redacted]` markers in both value slots for a Sensitive item) and produce an audit record.\n\n")
	b.WriteString("Sensitive items are encrypted at rest: the `configs` table stores `base64(ciphertext)` sealed by the host's `dbkit.Cipher` key (`APP_CONFIG_KEY` above) -- never plaintext -- and are never served on the public endpoint, whose rows are exactly the items marked Public below (`/api/config/public`, `config.PathPublic`). A Sensitive item's default is redacted in this very table (the `[redacted]` marker), because this document is a committed artifact a secret's plaintext has no more business crossing than the event bus.\n\n")
	b.WriteString("| Key | Kind | Type | Default | Bounds | Sensitive | Public | Group | Description |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, it := range d.Items {
		if it.Layer != "dynamic" {
			continue
		}
		kind := "item"
		if it.IsFeatureFlag {
			kind = "flag"
		}
		bounds := "--"
		if it.Min != nil || it.Max != nil {
			minv, maxv := "--", "--"
			if it.Min != nil {
				minv = *it.Min
			}
			if it.Max != nil {
				maxv = *it.Max
			}
			bounds = minv + " .. " + maxv
		}
		desc := it.Description
		if len(it.FlagDependsOn) > 0 {
			deps := make([]string, len(it.FlagDependsOn))
			for i, dep := range it.FlagDependsOn {
				deps[i] = "`" + dep + "`"
			}
			suffix := "(depends on " + strings.Join(deps, ", ") + ")"
			if desc == "" {
				desc = suffix
			} else {
				desc = desc + " " + suffix
			}
		}
		b.WriteString("| `" + it.Key + "` | " + kind + " | " + it.Type + " | ")
		b.WriteString(escapeCell(it.Default) + " | " + escapeCell(bounds) + " | ")
		b.WriteString(strconv.FormatBool(it.Sensitive) + " | " + strconv.FormatBool(it.Public) + " | " + escapeCell(it.Group) + " | " + escapeCell(desc) + " |\n")
	}
	b.WriteString("\n")
	b.WriteString("The owning module of each key is its dot-prefix (`authn.social.*` belongs to authn), the module whose runtime code reads the value; `Group` is the admin-console grouping the declaration carried. Feature flags are bool items whose \"enabled\" meaning is decided by `config.Service.IsEnabled`'s dependency walk over the flag graph. The JSON twin of this document carries each row as structured fields (`layer`, `key`, `module`, `scope`, `sensitive`, `default`, ...). Concrete example rows of the `configs` table itself -- a platform row and a tenant override for authn and sharing items, the Sensitive handling included -- live in `docs/config-row-examples.json`.\n\n")

	b.WriteString("<!-- Generated by examples/reference-app/cmd/configrefgen (go run ./cmd/configrefgen from examples/reference-app/). Do not hand-edit. Reference stats: " +
		strconv.Itoa(nBootstrap) + " bootstrap variable(s), " + strconv.Itoa(nDynamic) +
		" dynamic item(s)/flag(s) (" + strconv.Itoa(nFlags) + " flag(s), " + strconv.Itoa(nSensitive) +
		" sensitive) across " + strconv.Itoa(moduleCount(d)) + " declaring module(s). Drift gate: docs-check.yml runs the generator with --check. -->\n")
	return b.String()
}

// moduleCount counts the distinct owning modules of dynamic items.
func moduleCount(d *document) int {
	seen := map[string]bool{}
	for _, it := range d.Items {
		if it.Layer == "dynamic" {
			seen[it.Module] = true
		}
	}
	return len(seen)
}

// renderEnvExample renders the root .env.example from the bootstrap rows:
// one commented, ordered block per variable, in the same group order as the
// reference table, with the variable's fallback and summary rendered as its
// comment and its example (or blank) value on the assignment line.
func renderEnvExample(rows []bootstrapVar) string {
	var b strings.Builder
	b.WriteString("# Environment variables the reference app reads at startup (ConfigFromEnv in\n")
	b.WriteString("# internal/app/server.go and the app-level reads in internal/app/*.go) -- every\n")
	b.WriteString("# value is read with os.Getenv, so this file is a reference for what to set,\n")
	b.WriteString("# not something the app loads itself (there is no dotenv loader in this\n")
	b.WriteString("# codebase; apply it with your shell or process manager, e.g.\n")
	b.WriteString("# `env $(cat .env | xargs) go run ./cmd/server`).\n")
	b.WriteString("#\n")
	b.WriteString("# Generated from the same table that feeds docs/config-reference.md's bootstrap\n")
	b.WriteString("# section by examples/reference-app/cmd/configrefgen; a key the app reads but\n")
	b.WriteString("# this file lacks fails the generator's drift gate. Every variable is OPTIONAL:\n")
	b.WriteString("# each falls back to the documented development default in the comment below,\n")
	b.WriteString("# so `go run ./cmd/server` with none of this set boots a working standalone\n")
	b.WriteString("# server. Copy this file to .env, fill in what you need, and never commit the\n")
	b.WriteString("# real .env (see the repository's .gitignore). Secret materials (marked below)\n")
	b.WriteString("# must come from a secret store in a real deployment.\n\n")
	groupTitle := map[string]string{
		"core":          "Core bootstrap",
		"keys":          "Key materials (each a hex-encoded 32-byte key; recommended: set APP_ROOT_KEY alone)",
		"seams":         "Infrastructure seams (leave unset to keep the zero-setup standalone defaults)",
		"observability": "Observability",
		"network":       "Client-address trust",
		"serving":       "Frontend serving",
		"demo":          "Demo and test rigs",
	}
	current := ""
	for _, row := range rows {
		if row.group != current {
			current = row.group
			fmt.Fprintf(&b, "\n# --- %s ---\n", groupTitle[current])
		}
		// The assignment line renders the row's curated example value -- a
		// harmless blank for every key best left unset until needed. A
		// blank assignment reads exactly like an unset variable (os.Getenv
		// cannot distinguish them), so nothing here can poison a boot the
		// way a literal "0" in APP_SMTP_PORT would (a non-empty value
		// trips the SMTP pair's completeness rule), and a secret row left
		// blank keeps its development fallback instead of tripping the
		// hex-key parse with a placeholder -- exactly the reference app's
		// own .env.example convention, whose one populated secret row
		// (APP_ROOT_KEY) carries the replace-me placeholder this table's
		// curated example supplies.
		summary := strings.TrimSuffix(strings.TrimSpace(row.summary), ".")
		secret := ""
		if row.secret {
			secret = " (SECRET)"
		}
		fmt.Fprintf(&b, "# %s.%s Unset fallback: %s.\n%s=%s\n",
			summary, secret, row.fallback, row.env, row.example)
	}
	return b.String()
}
