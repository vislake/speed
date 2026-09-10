package main

// document.go assembles the reference document: the dynamic layer (the frozen
// schema snapshot, from config.Service.Describe) and the bootstrap layer (the
// platform modules' own bootstrap-key declarations, reg.Bootstrap), plus the
// rendering of the committed outputs and the documentation site's copy of the
// reference. Every output derives from the same assembled facts so the
// Markdown table, the machine-readable JSON and the site page agree, mirroring
// tools/gen_error_code_index.py's own twin-output discipline.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/pkgcore"
)

// sitePagePath is the documentation site's copy of the reference, in the
// user-guide area beside the error-code index's own generated page.
const sitePagePath = "docs/site/content.en/docs/user-guide/configuration.md"

// referenceItem is one row of the reference: a runtime configuration item or
// feature flag from the frozen schema.
type referenceItem struct {
	// Key is the configuration key.
	Key string `json:"key"`

	// Module is the owning module: the key's dot-prefix.
	Module string `json:"module"`

	// Group mirrors the declaration's Group when the declaration carries
	// one.
	Group string `json:"group,omitempty"`

	// Type is the value type: "string", "int", "bool" or "duration".
	Type string `json:"type"`

	// Source names where the value comes from: the configs table, writable at
	// the system and tenant scope tiers.
	Source string `json:"source"`

	// Scope lists the scope tiers that can hold a value. A read resolves
	// tenant over system over schema default.
	Scope []string `json:"scope"`

	// Sensitive marks secrets: encrypted at rest and redacted on the bus.
	Sensitive bool `json:"sensitive"`

	// Public marks values the unauthenticated public endpoint serves.
	Public bool `json:"public,omitempty"`

	// HasDefault reports whether a default exists.
	HasDefault bool `json:"has_default,omitempty"`

	// Default is the default value: the canonical form for a dynamic item
	// (the redacted marker when Sensitive -- a secret's plaintext never
	// crosses this boundary, exactly as it never crosses the event bus).
	Default string `json:"default"`

	// Min and Max are an int/duration item's declared bounds.
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

// declaredBootstrapKey is one bootstrap key a platform module declared, as the
// reference renders it: the declaration's own facts.
type declaredBootstrapKey struct {
	// Key is the dotted key path the module declared.
	Key string `json:"key"`
	// Module is the declaring module (the key's own prefix, by convention).
	Module string `json:"module"`
	// Format is the declared value shape.
	Format string `json:"format"`
	// Default is the declared fallback statement.
	Default string `json:"default"`
	// Sensitive marks declared key material.
	Sensitive bool `json:"sensitive"`
	// Example is the declared suggested value, empty when none is recommended.
	Example string `json:"example,omitempty"`
	// Description is the declared contract text.
	Description string `json:"description"`
}

// declaredModule groups one module's declared bootstrap keys.
type declaredModule struct {
	// Name is the module that declared them.
	Name string `json:"module"`
	// Keys are its declarations, ordered by key.
	Keys []declaredBootstrapKey `json:"keys"`
}

// document is the assembled reference.
type document struct {
	// Items holds the dynamic layer's rows in rendering order.
	Items []referenceItem
	// DeclaredModules lists the platform module declarations, in module
	// order.
	DeclaredModules []declaredModule
	// SilentModules names the composed modules that declared no bootstrap key
	// at all: an honest state, never a gap to fill.
	SilentModules []string
}

// buildDocument assembles the reference from the schema snapshot and the module
// declarations: the bootstrap keys first (what a process resolves before
// anything else), then the dynamic items sorted by key (the ordering Describe
// itself guarantees, stable across runs).
func buildDocument(descriptors []config.ConfigItemDescriptor, declared []pkgcore.BootstrapKey, composedModules []string) (*document, error) {
	if overlaps := overlappingKeys(descriptors, declared); len(overlaps) > 0 {
		return nil, fmt.Errorf("keys declared on both configuration layers (each key belongs to exactly one):\n  %s", strings.Join(overlaps, "\n  "))
	}

	doc := &document{}
	doc.DeclaredModules, doc.SilentModules = declaredKeys(declared, composedModules)

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
	return doc, nil
}

// declaredKeys folds the registry's declarations into per-module groups and
// reports which composed modules declared nothing. Both come out of the census
// itself: a module is silent because the declarations do not mention it, never
// because a list says so.
func declaredKeys(declared []pkgcore.BootstrapKey, composedModules []string) ([]declaredModule, []string) {
	byModule := make(map[string][]declaredBootstrapKey)
	for _, key := range declared {
		byModule[key.Group] = append(byModule[key.Group], declaredBootstrapKey{
			Key:         key.Key,
			Module:      key.Group,
			Format:      key.Format,
			Default:     key.Default,
			Sensitive:   key.Sensitive,
			Example:     key.Example,
			Description: key.Description,
		})
	}

	modules := make([]declaredModule, 0, len(byModule))
	for name, keys := range byModule {
		sort.Slice(keys, func(i, j int) bool { return keys[i].Key < keys[j].Key })
		modules = append(modules, declaredModule{Name: name, Keys: keys})
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Name < modules[j].Name })

	declaring := make(map[string]struct{}, len(byModule))
	for name := range byModule {
		declaring[name] = struct{}{}
	}
	var silent []string
	for _, name := range composedModules {
		if _, ok := declaring[name]; !ok {
			silent = append(silent, name)
		}
	}
	sort.Strings(silent)
	return modules, silent
}

// marshalJSON renders the machine-readable twin: the dynamic item list the
// Markdown table renders, plus the module declarations, deterministic (the
// lists are already ordered; json.MarshalIndent preserves slice order and
// sorts map keys).
func (d *document) marshalJSON() string {
	payload := map[string]any{
		"generated_by":            "examples/reference-app/cmd/configrefgen (go run ./cmd/configrefgen)",
		"items":                   d.Items,
		"declared_bootstrap_keys": d.DeclaredModules,
		"modules_declaring_none":  d.SilentModules,
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

// counts holds the per-layer totals the generated footers report.
type counts struct {
	dynamic   int
	flags     int
	sensitive int
	declared  int
}

func (d *document) counts() counts {
	var c counts
	for _, it := range d.Items {
		c.dynamic++
		if it.IsFeatureFlag {
			c.flags++
		}
		if it.Sensitive {
			c.sensitive++
		}
	}
	for _, m := range d.DeclaredModules {
		c.declared += len(m.Keys)
	}
	return c
}

// renderMarkdown renders the full reference document.
func (d *document) renderMarkdown() string {
	var b strings.Builder
	c := d.counts()

	b.WriteString("# Configuration reference\n\n")
	b.WriteString("This reference lists every configuration value a speed-based application built from this repository's modules resolves: the **bootstrap** layer, the process-start input the modules declare and a host resolves before anything else starts, and the **dynamic** layer, the configuration items and feature flags an operator edits at runtime. It is generated from the live configuration schema and the modules' own declarations, never hand-written -- root CLAUDE.md's documentation discipline that \"the configuration reference is generated from the config schema\" applies to exactly this document; a stale reference is a CI failure.\n\n")
	b.WriteString("How it is built: the dynamic layer is enumerated through `config.Service.Describe` from the frozen schema of a composed host that registers every platform module whose declarations fold into the schema (authn, metering, compliance, sharing, pki and org), and the bootstrap layer renders those same modules' `reg.Bootstrap` declarations -- the two sources are the modules' own registration-time declarations, never a hand-kept list. Every committed output -- this document, its JSON twin `docs/config-reference.json`, the derived `config.example.json` and the documentation site's copy of this page -- is byte-identical across runs; regeneration is `go run ./cmd/configrefgen` from `examples/reference-app/`.\n\n")

	b.WriteString("## Bootstrap configuration\n\n")
	b.WriteString(bootstrapIntro)
	b.WriteString("\n\n")
	b.WriteString(bootstrapSecretsNote)
	b.WriteString("\n\n")
	b.WriteString(d.renderDeclaredModules())
	b.WriteString("\n")

	b.WriteString("## Dynamic configuration\n\n")
	b.WriteString(dynamicIntro)
	b.WriteString("\n\n")
	b.WriteString("| Key | Kind | Type | Default | Bounds | Sensitive | Public | Group | Description |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, it := range d.Items {
		b.WriteString(renderDynamicRow(it))
	}
	b.WriteString("\n")
	b.WriteString(dynamicFooter)
	b.WriteString("\n")

	b.WriteString("<!-- Generated by examples/reference-app/cmd/configrefgen (go run ./cmd/configrefgen from examples/reference-app/). Do not hand-edit. Reference stats: " +
		strconv.Itoa(c.declared) + " module-declared bootstrap key(s), " + strconv.Itoa(c.dynamic) +
		" dynamic item(s)/flag(s) (" + strconv.Itoa(c.flags) + " flag(s), " + strconv.Itoa(c.sensitive) +
		" sensitive) across " + strconv.Itoa(moduleCount(d)) + " declaring module(s). Drift gate: docs-check.yml runs the generator with --check. -->\n")
	return b.String()
}

// bootstrapIntro is the bootstrap section's lead text: what the layer is, who
// declares its keys, and the mechanism a host drives to resolve them.
const bootstrapIntro = "The bootstrap layer is the process-start input a speed-based application resolves once, before anything else is wired; it has no tenant dimension and no runtime edit surface, so a change takes effect at the next start. Its keys are declared by the modules that consume them: a module states each key's contract on the registry's bootstrap seat (`reg.Bootstrap.Add`) while it registers, and never resolves the value itself -- the host does, and injects it. A module that consumes no process-start input declares nothing, which is an honest state rather than a gap.\n\n" +
	"The mechanism a host drives to resolve the values is `go/pkgcore/config`, and this repository's own reference app exercises it for its whole bootstrap surface, so the shape below is a real consumer's resolution path rather than a document-only sketch. A host that drives the loader declares a target struct whose fields are the keys: the field `Database.DSN` is the key `database.dsn`, the flag `--database.dsn`, and -- under the loader's default prefix -- the environment variable `SPEED_DATABASE__DSN`, where a double underscore marks each level of nesting; a single underscore is never a nesting marker. `WithEnvPrefix` replaces the prefix for a host whose variables already carry another one (`APP_`, say), and a field whose variable name does not derive from its key can pin that exact name (`config:\"env=PORT\"`, the customary unprefixed name for the port a platform tells the process to listen on), after which the field reads that name and no other. Every key resolves from four sources, highest priority first: command-line flags, environment variables, an optional YAML or JSON config file (`WithConfigFile`; an absent file is skipped silently, a malformed one is a hard error), then the defaults already set on the target struct. A value supplied by a text source is judged as text: a field that can hold an empty value takes it, and a field with no representation for one -- a number, a bool -- refuses the load rather than quietly becoming that field's zero value. `config.Verify(target, declared)` then checks a declared key list against a target struct, every declared key mapping onto a field, which is how a host proves its target binds the keys its modules declared."

// bootstrapSecretsNote is the secret-handling note under the declared-key
// tables.
const bootstrapSecretsNote = "Secret materials (Sensitive above) must come from a secret store in a real deployment, never from a committed file. The Unset fallback column states what an unset key resolves to; for key materials that is a documented, recognizable, NON-SECRET development default a real deployment must override, and each declaration's own text says what its key protects and how it is isolated from every other key."

// renderDeclaredModules renders the per-module bootstrap-key declarations, and
// the modules that declared none.
func (d *document) renderDeclaredModules() string {
	var b strings.Builder
	b.WriteString("### Bootstrap keys declared by platform modules\n\n")
	b.WriteString("Each platform module declares the process-start keys it consumes on the registry's bootstrap seat, so the key's contract -- what it protects, why it is a separate secret, what an operator should expect when it is unset -- travels with the module instead of living in a host's own notes. The tables below are rendered from `reg.Bootstrap` itself.\n\n")
	for _, module := range d.DeclaredModules {
		b.WriteString("**" + module.Name + "**\n\n")
		b.WriteString("| Key | Format | Sensitive | Unset fallback | What the key protects |\n")
		b.WriteString("|---|---|---|---|---|\n")
		for _, key := range module.Keys {
			sensitive := strconv.FormatBool(key.Sensitive)
			b.WriteString("| `" + key.Key + "` | " + key.Format + " | " + sensitive + " | " + escapeCell(key.Default) + " | " + escapeCell(key.Description) + " |\n")
		}
		b.WriteString("\n")
	}
	if len(d.SilentModules) > 0 {
		quoted := make([]string, len(d.SilentModules))
		for i, name := range d.SilentModules {
			quoted[i] = "`" + name + "`"
		}
		b.WriteString("Every other module in this reference's composition declares no bootstrap keys: " + strings.Join(quoted, ", ") +
			". That is an honest state rather than a gap -- a module that consumes no process-start input declares nothing, and nothing asks it for a placeholder.\n\n")
	}
	b.WriteString("A key belongs to exactly one configuration layer. A bootstrap key is process-start input, resolved once and fixed for the process's lifetime; a runtime item (the next section) is a per-tenant value an operator edits while the process runs. Declaring the same dotted key on both layers is refused at startup and in this generator, because one identifier cannot carry two meanings, two defaults and two edit surfaces.\n")
	return b.String()
}

// renderDynamicRow renders one dynamic item's Markdown table row.
func renderDynamicRow(it referenceItem) string {
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
	return "| `" + it.Key + "` | " + kind + " | " + it.Type + " | " +
		escapeCell(it.Default) + " | " + escapeCell(bounds) + " | " +
		strconv.FormatBool(it.Sensitive) + " | " + strconv.FormatBool(it.Public) + " | " + escapeCell(it.Group) + " | " + escapeCell(desc) + " |\n"
}

// dynamicIntro is the dynamic section's lead text.
const dynamicIntro = "The dynamic layer holds the configuration an operator edits at runtime, served by the `go/config` module: every module declares the items and feature flags it owns during `Register` (`reg.Config.Add` / `reg.Features.Add`), and `config.Module.Attach` freezes them into one schema the moment Bootstrap returns. Values live in the shared `configs` table (platform data, keyed by `(key, scope, tenant_id)`) under two scope tiers: a **system** row is platform-wide (writing one requires an audited system context), a **tenant** row overrides it for one tenant, and a read resolves tenant row -> system row -> the schema default. Scope is a property of the row, not of the key: every key below may hold a row at either tier. A key with no default and no row at any reachable scope has no value to serve (`config.item_unset`). Writes publish `config.item.changed` (with `[redacted]` markers in both value slots for a Sensitive item) and produce an audit record.\n\n" +
	"Sensitive items are encrypted at rest: the `configs` table stores `base64(ciphertext)` sealed by the host's `dbkit.Cipher` key, never plaintext, and are never served on the public endpoint, whose rows are exactly the items marked Public below (`/api/config/public`, `config.PathPublic`). A Sensitive item's default is redacted in this very table (the `[redacted]` marker), because this document is a committed artifact a secret's plaintext has no more business crossing than the event bus."

// dynamicFooter is the dynamic section's closing text.
const dynamicFooter = "The owning module of each key is its dot-prefix (`authn.social.*` belongs to authn), the module whose runtime code reads the value; `Group` is the admin-console grouping the declaration carried. Feature flags are bool items whose \"enabled\" meaning is decided by `config.Service.IsEnabled`'s dependency walk over the flag graph. The JSON twin of this document carries each row as structured fields (`key`, `module`, `scope`, `sensitive`, `default`, ...) plus the module declarations. Concrete example rows of the `configs` table itself -- a platform row and a tenant override for authn and sharing items, the Sensitive handling included -- live in `docs/config-row-examples.json`."

// moduleCount counts the distinct owning modules of dynamic items.
func moduleCount(d *document) int {
	seen := map[string]bool{}
	for _, it := range d.Items {
		seen[it.Module] = true
	}
	return len(seen)
}

// sitePage renders the documentation site's copy of the reference: the same
// assembled facts as the Markdown twin, framed for the site's own reader
// (a team building a product on these modules), with the Hugo front matter the
// site build needs. Like the error-code index page, it is generated rather than
// hand-maintained, and the drift gate covers it with the other outputs.
func (d *document) sitePage() string {
	var b strings.Builder
	c := d.counts()

	b.WriteString("---\n")
	b.WriteString("title: \"Configuration reference\"\n")
	b.WriteString("description: \"Every bootstrap key and runtime configuration item a speed-based application resolves: the process-start keys platform modules declare, and the dynamic items an operator edits per tenant.\"\n")
	b.WriteString("weight: 98\n")
	b.WriteString("---\n")
	b.WriteString("# Configuration reference\n\n")
	b.WriteString("A speed-based application resolves configuration in two layers that never share a key. This page is generated from the modules' own declarations -- never hand-written -- and a stale copy fails CI.\n\n")
	b.WriteString("## Bootstrap configuration\n\n")
	b.WriteString(bootstrapIntro)
	b.WriteString("\n\n")
	b.WriteString(bootstrapSecretsNote)
	b.WriteString("\n\n")
	b.WriteString(d.renderDeclaredModules())
	b.WriteString("\n")
	b.WriteString("## Dynamic configuration\n\n")
	b.WriteString(dynamicIntro)
	b.WriteString("\n\n")
	b.WriteString("| Key | Kind | Type | Default | Bounds | Sensitive | Public | Group | Description |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, it := range d.Items {
		b.WriteString(renderDynamicRow(it))
	}
	b.WriteString("\n")
	b.WriteString(dynamicFooter)
	b.WriteString("\n\n")
	b.WriteString("<!-- Generated by examples/reference-app/cmd/configrefgen (go run ./cmd/configrefgen from examples/reference-app/). Do not hand-edit. Reference stats: " +
		strconv.Itoa(c.declared) + " module-declared bootstrap key(s), " + strconv.Itoa(c.dynamic) +
		" dynamic item(s)/flag(s) (" + strconv.Itoa(c.flags) + " flag(s), " + strconv.Itoa(c.sensitive) +
		" sensitive) across " + strconv.Itoa(moduleCount(d)) + " declaring module(s). Drift gate: docs-check.yml runs the generator with --check. -->\n")
	return b.String()
}
