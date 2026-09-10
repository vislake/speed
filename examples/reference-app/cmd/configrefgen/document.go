package main

// document.go assembles the reference document: one ordered item list that
// merges the dynamic layer (the frozen schema snapshot, from
// config.Service.Describe), the bootstrap layer (the curated env-var rows) and
// the platform modules' own bootstrap-key declarations (reg.Bootstrap, joined
// to their variables through platformEnvNames), plus the rendering of the
// committed outputs and the documentation site's copy of the reference. Every
// output derives from the same assembled facts so the Markdown table, the
// machine-readable JSON, the site page and the root .env.example agree, mirroring
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

// declaredBootstrapKey is one bootstrap key a platform module declared, as the
// reference renders it: the declaration's own facts, plus the environment
// variable the reference app reads that key from today (the transition's
// bridge, platformEnvNames).
type declaredBootstrapKey struct {
	// Key is the dotted key path the module declared.
	Key string `json:"key"`
	// Env is the variable carrying the key in the reference app today.
	Env string `json:"env"`
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
	// Items holds every row of both layers in rendering order.
	Items []referenceItem
	// Sources records the extraction source per layer, for the generated
	// header.
	Sources []string
	// DeclaredModules lists the platform module declarations, in module
	// order.
	DeclaredModules []declaredModule
	// SilentModules names the composed modules that declared no bootstrap key
	// at all: an honest state, never a gap to fill.
	SilentModules []string
}

// buildDocument assembles the reference from the schema snapshot, the module
// declarations and the bootstrap table, in layer order: bootstrap first (what a
// process resolves before anything else), then the dynamic items sorted by key
// (the ordering Describe itself guarantees, stable across runs). It returns the
// ordered bootstrap rows too, so the .env.example rendering shares the exact
// table the reference table renders.
func buildDocument(moduleDir string, descriptors []config.ConfigItemDescriptor, declared []pkgcore.BootstrapKey, composedModules []string) (*document, []bootstrapVar, error) {
	boot, problems, err := bootstrapRows(moduleDir)
	if err != nil {
		return nil, nil, err
	}
	if len(problems) > 0 {
		return nil, nil, fmt.Errorf("bootstrap inventory/table mismatch:\n  %s", strings.Join(problems, "\n  "))
	}
	if problems := reconcileDeclarations(declared); len(problems) > 0 {
		return nil, nil, fmt.Errorf("module declarations disagree with the bootstrap table:\n  %s", strings.Join(problems, "\n  "))
	}
	if overlaps := overlappingKeys(descriptors, declared); len(overlaps) > 0 {
		return nil, nil, fmt.Errorf("keys declared on both configuration layers (each key belongs to exactly one):\n  %s", strings.Join(overlaps, "\n  "))
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

// declaredKeys folds the registry's declarations into per-module groups and
// reports which composed modules declared nothing. Both come out of the census
// itself: a module is silent because the declarations do not mention it, never
// because a list says so.
func declaredKeys(declared []pkgcore.BootstrapKey, composedModules []string) ([]declaredModule, []string) {
	byModule := make(map[string][]declaredBootstrapKey)
	for _, key := range declared {
		env := platformEnvNames[key.Key]
		byModule[key.Group] = append(byModule[key.Group], declaredBootstrapKey{
			Key:         key.Key,
			Env:         env,
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

// marshalJSON renders the machine-readable twin: the same item list the
// Markdown table renders, plus the module declarations, one object per item,
// deterministic (the lists are already ordered; json.MarshalIndent preserves
// slice order and sorts map keys).
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
	bootstrap int
	declared  int
}

func (d *document) counts() counts {
	var c counts
	for _, it := range d.Items {
		switch it.Layer {
		case "dynamic":
			c.dynamic++
			if it.IsFeatureFlag {
				c.flags++
			}
			if it.Sensitive {
				c.sensitive++
			}
		case "bootstrap":
			c.bootstrap++
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
	b.WriteString("How it is built: the dynamic layer is enumerated through `config.Service.Describe` from the frozen schema of a composed host that registers every platform module whose declarations fold into the schema (authn, metering, compliance, sharing, pki and org), the platform bootstrap keys come from those modules' `reg.Bootstrap` declarations on the same composed host, and the reference app's own environment surface (the variables the host reads today) is inventoried from the app source under a coverage gate -- see the generator's own header comment in `examples/reference-app/cmd/configrefgen/`. Every committed output -- this document, its JSON twin `docs/config-reference.json`, the root `.env.example`, the derived `config.example.json` and the documentation site's copy of this page -- is byte-identical across runs; regeneration is `go run ./cmd/configrefgen` from `examples/reference-app/`.\n\n")

	b.WriteString("## Bootstrap configuration\n\n")
	b.WriteString(bootstrapIntro)
	b.WriteString("\n\n")
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
		if it.Layer != "dynamic" {
			continue
		}
		b.WriteString(renderDynamicRow(it))
	}
	b.WriteString("\n")
	b.WriteString(dynamicFooter)
	b.WriteString("\n")

	b.WriteString("<!-- Generated by examples/reference-app/cmd/configrefgen (go run ./cmd/configrefgen from examples/reference-app/). Do not hand-edit. Reference stats: " +
		strconv.Itoa(c.bootstrap) + " bootstrap variable(s), " + strconv.Itoa(c.declared) +
		" module-declared bootstrap key(s), " + strconv.Itoa(c.dynamic) +
		" dynamic item(s)/flag(s) (" + strconv.Itoa(c.flags) + " flag(s), " + strconv.Itoa(c.sensitive) +
		" sensitive) across " + strconv.Itoa(moduleCount(d)) + " declaring module(s). Drift gate: docs-check.yml runs the generator with --check. -->\n")
	return b.String()
}

// bootstrapIntro is the bootstrap section's lead text: what the layer is, how
// the loader resolves it, and where each half of the documentation comes from.
const bootstrapIntro = "The bootstrap layer is the process-start input a speed-based application resolves before anything else starts. Two halves are documented here: the **platform keys**, declared by the modules that consume them on the registry's bootstrap seat (`reg.Bootstrap.Add`), and the **reference app's own variables**, the environment surface the app reads today (with `os.Getenv`; the app does not drive the loader yet). Every variable below is optional in development, falling back to the documented default that keeps `go run ./cmd/server` booting a working standalone server with zero external dependencies.\n\n" +
	"The loader mechanism behind the platform keys is `go/pkgcore/config`. A host that drives it resolves each key from four sources, highest priority first: command-line flags (`--database.dsn=…`), environment variables, an optional YAML **or JSON** config file (`WithConfigFile`; an absent file is skipped silently, a malformed one is a hard error), then the defaults already set on the host's target struct. Keys are derived from the target struct's fields: `Database.DSN` is the key `database.dsn`, the flag `--database.dsn` and the variable `SPEED_DATABASE__DSN` -- the loader's default prefix `SPEED_`, the key uppercased, and a double underscore for each level of nesting. A single underscore is not a nesting marker (`SPEED_DATABASE_DSN` resolves to nothing), and variables that match no key are ignored rather than rejected. `WithEnvPrefix` replaces the prefix for a host whose variables already carry another one (`APP_`, say), and a field may pin its exact variable name with `config:\"env=PORT\"` for names no derivation reaches; a pinned field reads that name and no other. Two fields resolving to one variable is refused when the target is described, because a single variable cannot feed two fields. `config.Verify(target, declared)` then checks a declared key list against the target struct, which is how a host proves its target binds the keys its modules declared."

// bootstrapSecretsNote is the secret-handling note under the variable table.
const bootstrapSecretsNote = "Secret materials (marked above) must come from a secret store in a real deployment, never from a committed file: the six derived key variables belong to platform modules (each declaration below states what its key protects and why it is a separate secret) and have documented, recognizable, NON-SECRET development defaults that a real deployment must override, while `APP_ROOT_KEY` is the host's recommended single secret from which all six are derived. The demo-affordance variables (`APP_DEMO_*`, `APP_AI_GATEWAY_IMAGE_*`, `APP_DISABLE_*`) gate demo behavior and are test rigs by design; `APP_DISABLE_DEMO_USER_HEADER` is the kill switch DEPLOY.md recommends for any deployment a real user might reach."

// renderDeclaredModules renders the per-module bootstrap-key declarations, and
// the modules that declared none.
func (d *document) renderDeclaredModules() string {
	var b strings.Builder
	b.WriteString("### Bootstrap keys declared by platform modules\n\n")
	b.WriteString("Each platform module declares the process-start keys it consumes on the registry's bootstrap seat, so the key's contract -- what it protects, why it is a separate secret, what an operator should expect when it is unset -- travels with the module instead of living in a host's own notes. The declarations below are rendered from `reg.Bootstrap`; the \"environment variable\" column names the spelling the reference app reads the same key from today, which is the transition's bridge until the app's loader-shaped struct pins it.\n\n")
	for _, module := range d.DeclaredModules {
		b.WriteString("**" + module.Name + "**\n\n")
		b.WriteString("| Key | Format | Sensitive | Unset fallback | Environment variable in this app | What the key protects |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, key := range module.Keys {
			sensitive := strconv.FormatBool(key.Sensitive)
			b.WriteString("| `" + key.Key + "` | " + key.Format + " | " + sensitive + " | " + escapeCell(key.Default) + " | `" + key.Env + "` | " + escapeCell(key.Description) + " |\n")
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
const dynamicFooter = "The owning module of each key is its dot-prefix (`authn.social.*` belongs to authn), the module whose runtime code reads the value; `Group` is the admin-console grouping the declaration carried. Feature flags are bool items whose \"enabled\" meaning is decided by `config.Service.IsEnabled`'s dependency walk over the flag graph. The JSON twin of this document carries each row as structured fields (`layer`, `key`, `module`, `scope`, `sensitive`, `default`, ...) plus the module declarations. Concrete example rows of the `configs` table itself -- a platform row and a tenant override for authn and sharing items, the Sensitive handling included -- live in `docs/config-row-examples.json`."

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
	b.WriteString("description: \"Every bootstrap key and runtime configuration item a speed-based application resolves: the keys platform modules declare, the variables this repository's reference app reads, and the dynamic items an operator edits per tenant.\"\n")
	b.WriteString("weight: 98\n")
	b.WriteString("---\n")
	b.WriteString("# Configuration reference\n\n")
	b.WriteString("A speed-based application resolves configuration in two layers that never share a key. This page is generated from the modules' own declarations and the reference app's source -- never hand-written -- and a stale copy fails CI.\n\n")
	b.WriteString("## Bootstrap configuration\n\n")
	b.WriteString(bootstrapIntro)
	b.WriteString("\n\n")
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
	b.WriteString(d.renderDeclaredModules())
	b.WriteString("\n")
	b.WriteString("## Dynamic configuration\n\n")
	b.WriteString(dynamicIntro)
	b.WriteString("\n\n")
	b.WriteString("| Key | Kind | Type | Default | Bounds | Sensitive | Public | Group | Description |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, it := range d.Items {
		if it.Layer != "dynamic" {
			continue
		}
		b.WriteString(renderDynamicRow(it))
	}
	b.WriteString("\n")
	b.WriteString(dynamicFooter)
	b.WriteString("\n\n")
	b.WriteString("<!-- Generated by examples/reference-app/cmd/configrefgen (go run ./cmd/configrefgen from examples/reference-app/). Do not hand-edit. Reference stats: " +
		strconv.Itoa(c.bootstrap) + " bootstrap variable(s), " + strconv.Itoa(c.declared) +
		" module-declared bootstrap key(s), " + strconv.Itoa(c.dynamic) +
		" dynamic item(s)/flag(s) (" + strconv.Itoa(c.flags) + " flag(s), " + strconv.Itoa(c.sensitive) +
		" sensitive) across " + strconv.Itoa(moduleCount(d)) + " declaring module(s). Drift gate: docs-check.yml runs the generator with --check. -->\n")
	return b.String()
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
	b.WriteString("# this file lacks fails the generator's drift gate. The six key-material rows\n")
	b.WriteString("# are also declared by the platform modules that consume them (see the\n")
	b.WriteString("# reference's per-module section), and the generator fails when the\n")
	b.WriteString("# declaration and the row disagree. Every variable is OPTIONAL: each falls\n")
	b.WriteString("# back to the documented development default in the comment below, so\n")
	b.WriteString("# `go run ./cmd/server` with none of this set boots a working standalone\n")
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
