package main

// document.go assembles the reference document: the dynamic layer (the frozen
// schema snapshot, from config.Service.Describe) and the bootstrap layer (the
// platform modules' own bootstrap-key declarations, their component
// descriptors' BootstrapKeys), plus the
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

	// loader is go/pkgcore/config, the bootstrap loader this reference's Env
	// column names variables after: the cell must print the name the loader
	// itself would read, so it calls the loader's own derivation rather than
	// re-deriving the rule (the import alias keeps the unqualified name
	// config for go/config, the module this package mostly speaks to).
	loader "github.com/vislake/speed/go/pkgcore/config"
)

// sitePagePath is the documentation site's copy of the reference, in the
// user-guide area beside the error-code index's own generated page.
const sitePagePath = "docs/site/content.en/docs/user-guide/configuration.md"

// The reference's two sections carry deliberately distinct record types. A
// declaredBootstrapKey is process-start input: an environment variable (or
// flag, or config-file entry) a host resolves once, fixed for the process's
// lifetime, with no tenant dimension and no runtime edit surface. A
// referenceItem is a runtime entry in the configs table: a per-tenant value an
// operator edits while the process runs. The two differ in source, lifetime,
// scope, value semantics, editor and failure consequence, so their field sets
// stay separate and are never merged into one record type -- the module-tagged
// row each section renders makes the sections comparable, not interchangeable.

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
// reference renders it: the declaration's own facts plus the environment
// variable name the loader's default rule derives from the key path, as one
// flat entry carrying its declaring module -- the shape the dynamic section's
// rows carry too.
type declaredBootstrapKey struct {
	// Key is the dotted key path the module declared.
	Key string `json:"key"`
	// Module is the declaring module, the declaration's Group -- conventionally
	// the key path's first segment. The Markdown groups its tables by it; the
	// JSON twin carries it on every row and stays one flat list.
	Module string `json:"module"`
	// Env is the environment variable name the loader derives from Key under
	// its default prefix (loader.EnvName(loader.EnvPrefix, Key)) -- the
	// derivation an unpinned host field reads. A host may pin a different
	// name for its own field; that choice is the host's, not this reference's.
	Env string `json:"env"`
	// Type is the declared value shape: "string", "int", "bool" or "hexkey".
	Type string `json:"type"`
	// UnsetFallback states in operator terms what an unset key resolves to.
	// It is deliberately not the dynamic layer's "default": the two layers'
	// fallbacks have different sources and different consequences.
	UnsetFallback string `json:"unset_fallback"`
	// Sensitive marks declared key material.
	Sensitive bool `json:"sensitive"`
	// Description is the declared contract text.
	Description string `json:"description"`
}

// document is the assembled reference.
type document struct {
	// Items holds the dynamic layer's rows in rendering order.
	Items []referenceItem
	// Declared holds the bootstrap layer's entries as one flat list, ordered
	// by declaring module then key path. The Markdown groups it into one table
	// per module for the human reader; the JSON twin renders the same list
	// flat -- the grouping is a rendering choice, not a second shape of the
	// facts.
	Declared []declaredBootstrapKey
}

// buildDocument assembles the reference from the schema snapshot and the module
// declarations: the bootstrap keys first (what a process resolves before
// anything else), then the dynamic items sorted by key (the ordering Describe
// itself guarantees, stable across runs).
func buildDocument(descriptors []config.ConfigItemDescriptor, declared []pkgcore.BootstrapKey) (*document, error) {
	if overlaps := overlappingKeys(descriptors, declared); len(overlaps) > 0 {
		return nil, fmt.Errorf("keys declared on both configuration layers (each key belongs to exactly one):\n  %s", strings.Join(overlaps, "\n  "))
	}

	doc := &document{}
	doc.Declared = declaredKeys(declared)

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

// declaredKeys folds the registry's declarations into one flat list ordered by
// declaring module then key path, and fills every entry's Env cell from the
// loader's own derivation.
func declaredKeys(declared []pkgcore.BootstrapKey) []declaredBootstrapKey {
	rows := make([]declaredBootstrapKey, 0, len(declared))
	for _, key := range declared {
		rows = append(rows, declaredBootstrapKey{
			Key:           key.Key,
			Module:        key.Group,
			Env:           loader.EnvName(loader.EnvPrefix, key.Key),
			Type:          key.Format,
			UnsetFallback: key.Default,
			Sensitive:     key.Sensitive,
			Description:   key.Description,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Module != rows[j].Module {
			return rows[i].Module < rows[j].Module
		}
		return rows[i].Key < rows[j].Key
	})
	return rows
}

// marshalJSON renders the machine-readable twin: the bootstrap declarations
// the reference's first section renders, as one flat entry list, plus the
// dynamic item list its second section renders. The top level is a struct, not
// a map, so the two sections keep the document's own order (slice order inside
// each is already deterministic) rather than a map's key order.
func (d *document) marshalJSON() string {
	payload := struct {
		BootstrapKeys []declaredBootstrapKey `json:"bootstrap_keys"`
		DynamicItems  []referenceItem        `json:"dynamic_items"`
	}{
		BootstrapKeys: d.Declared,
		DynamicItems:  d.Items,
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
	c.declared = len(d.Declared)
	return c
}

// renderMarkdown renders the full reference document.
func (d *document) renderMarkdown() string {
	var b strings.Builder

	b.WriteString("# Configuration reference\n\n")
	b.WriteString("This reference lists every configuration value a speed-based application built from this repository's modules resolves: the **bootstrap** layer, the process-start input the modules declare and a host resolves before anything else starts, and the **dynamic** layer, the configuration items and feature flags an operator edits at runtime. It is generated from the live configuration schema and the modules' own declarations, never hand-written -- root CLAUDE.md's documentation discipline that \"the configuration reference is generated from the config schema\" applies to exactly this document; a stale reference is a CI failure.\n\n")
	b.WriteString("How it is built: the dynamic layer is enumerated through `config.Service.Describe` from the frozen schema of a composed host that registers every platform module whose declarations fold into the schema (authn, metering, compliance, sharing, pki and org), and the bootstrap layer renders those same modules' declared keys from their component descriptors (`pkgcore.Component.BootstrapKeys`) -- the two sources are the modules' own declarations, never a hand-kept list. Every committed output -- this document, its JSON twin `docs/config-reference.json`, the derived `docs/config.example.json` and the documentation site's copy of this page -- is byte-identical across runs; regeneration is `go run .` from `tools/configrefgen/`.\n\n")

	b.WriteString(d.renderSections())
	b.WriteString(d.generatedComment())
	return b.String()
}

// renderSections renders the reference's shared body: the bootstrap and
// dynamic sections, from "## Bootstrap configuration" through the dynamic
// footer. Both page variants carry exactly this body -- renderMarkdown
// frames it with the reference's own header, sitePage with Hugo front
// matter and its site lead -- so it is rendered in one place; the two
// variants had drifted into near-duplicate copies of it.
func (d *document) renderSections() string {
	var b strings.Builder

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
	return b.String()
}

// generatedComment renders the trailing generated-stats comment every
// committed output variant carries.
func (d *document) generatedComment() string {
	c := d.counts()
	return "<!-- Generated by tools/configrefgen (go run . from tools/configrefgen/). Do not hand-edit. Reference stats: " +
		strconv.Itoa(c.declared) + " module-declared bootstrap key(s), " + strconv.Itoa(c.dynamic) +
		" dynamic item(s)/flag(s) (" + strconv.Itoa(c.flags) + " flag(s), " + strconv.Itoa(c.sensitive) +
		" sensitive) across " + strconv.Itoa(moduleCount(d)) + " declaring module(s). Drift gate: docs-check.yml runs the generator with --check. -->\n"
}

// bootstrapIntro is the bootstrap section's lead text: what the layer is, who
// declares its keys, and the mechanism a host drives to resolve them.
const bootstrapIntro = "The bootstrap layer is the process-start input a speed-based application resolves once, before anything else is wired; it has no tenant dimension and no runtime edit surface, so a change takes effect at the next start. Its keys are declared by the modules that consume them: a module states each key's contract on its component descriptor (`pkgcore.Component.BootstrapKeys`), which the assembly's loader resolves before anything is constructed, and never resolves the value itself -- the host does, and injects it. A module that consumes no process-start input declares nothing, which is an honest state rather than a gap.\n\n" +
	"The mechanism a host drives to resolve the values is `go/pkgcore/config`. A host that drives the loader declares a target struct whose fields are the keys: the field `Database.DSN` is the key `database.dsn`, the flag `--database.dsn`, and -- under the loader's default prefix -- the environment variable `SPEED_DATABASE__DSN`, where a double underscore marks each level of nesting; a single underscore is never a nesting marker. `WithEnvPrefix` replaces the prefix for a host whose variables already carry another one, and a field whose variable name does not derive from its key can pin that exact name (`config:\"env=PORT\"`, the customary unprefixed name for the port a platform tells the process to listen on), after which the field reads that name and no other. Every key resolves from four sources, highest priority first: command-line flags, environment variables, an optional YAML or JSON config file (`WithConfigFile`; an absent file is skipped silently, a malformed one is a hard error), then the defaults already set on the target struct. A declared key material's field -- typed `[]byte` and tagged `config:\"derive\"` -- gains a fifth source between the file and the default: the root-key derivation (see the note below the tables). A value supplied by a text source is judged as text: a field that can hold an empty value takes it, and a field with no representation for one -- a number, a bool -- refuses the load rather than quietly becoming that field's zero value. `config.Verify(target, declared)` then checks a declared key list against a target struct, every declared key mapping onto a field, which is how a host proves its target binds the keys its modules declared.\n\n" +
	"A paired example of this file source is committed under `docs/`: `docs/config.example.yaml` and its derived `docs/config.example.json`, covering the platform keys a host must feed, in both accepted formats."

// bootstrapSecretsNote is the secret-handling note under the declared-key
// tables: what an unset key means, and the platform capability that derives
// one key's material from a shared root key.
const bootstrapSecretsNote = "Secret materials (Sensitive above) must come from a secret store in a real deployment, never from a committed file. The Unset fallback column states what an unset key resolves to; for key materials that is a documented, recognizable, NON-SECRET development default a real deployment must override, and each declaration's own text says what its key protects and how it is isolated from every other key.\n\n" +
	"A deployment that would rather manage one secret than one per key can derive a declared key's material from a single 32-byte root key: the key path's purpose string is a platform convention fixed at the bootstrap seat (`pkgcore.BootstrapKeyPurpose` -- the literal `\"speed.\" + keyPath + \".v1\"`, one per declared key path), and `dbkit.DeriveKey(rootKey, purpose)` (HKDF-SHA256) turns the root key plus that purpose into the key's 32-byte material -- `speed.config.cipher_key.v1` for `config.cipher_key`, `speed.authn.blind_index_key.v1` and `speed.authn.pii_cipher_key.v1` for authn's two, and `speed.notification.contact_index_key.v1`, `speed.org.invitation_email_index_key.v1` and `speed.pki.local_key_cipher_key.v1` for the remaining three, composed for a host in one call (`dbkit.DeriveBootstrapKey`). The loader applies the precedence: a host tags each key-material field `derive`, names its root-key source (`WithRootKey` or `WithRootKeyEnv`, the latter read inside the same load), and installs `WithKeyDerivation(dbkit.DeriveBootstrapKey)` -- the field then resolves an explicit 64-hex-character value over the root key's derivation over its own struct default, and an emptied variable reads as unset. Which variable carries the root key, and which carries a single key's own override, remains the host's choice: the names belong to the host, and the platform only receives the bytes. Because a purpose embeds the declared key path, renaming a declared key path is a rotation of that key's material, and must ship as one."

// renderDeclaredModules renders the per-module bootstrap-key tables from the
// flat declaration list: each declaring module opens its own table, the
// human-readable counterpart of the JSON twin's flat entries.
func (d *document) renderDeclaredModules() string {
	var b strings.Builder
	b.WriteString("### Bootstrap keys declared by platform modules\n\n")
	b.WriteString("Each platform module declares the process-start keys it consumes on its component descriptor, so the key's contract -- what it protects, why it is a separate secret, what an operator should expect when it is unset -- travels with the module instead of living in a host's own notes. The tables below are rendered from those declarations (`pkgcore.Component.BootstrapKeys`). The Env variable column is the name the loader derives from the key path under its default prefix, which is what an unpinned host field reads; a host that pins a different name for its own field is exercising its own naming choice.\n\n")
	module := ""
	for _, key := range d.Declared {
		if key.Module != module {
			if module != "" {
				b.WriteString("\n")
			}
			module = key.Module
			b.WriteString("**" + module + "**\n\n")
			b.WriteString("| Key | Env variable | Type | Sensitive | Unset fallback | What the key protects |\n")
			b.WriteString("|---|---|---|---|---|---|\n")
		}
		sensitive := strconv.FormatBool(key.Sensitive)
		b.WriteString("| `" + key.Key + "` | `" + key.Env + "` | " + key.Type + " | " + sensitive + " | " + escapeCell(key.UnsetFallback) + " | " + escapeCell(key.Description) + " |\n")
	}
	if module != "" {
		b.WriteString("\n")
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
const dynamicIntro = "The dynamic layer holds the configuration an operator edits at runtime, served by the `go/config` module: every module declares the items and feature flags it owns from its `Register` callback, which runs in the component assembly's Init stage (`reg.ConfigSeat().Add` / `reg.FeaturesSeat().Add`), and `config.Module.Attach` freezes them into one schema once every module's declaration turn has ended. Values live in the shared `configs` table (platform data, keyed by `(key, scope, tenant_id)`) under two scope tiers: a **system** row is platform-wide (writing one requires an audited system context), a **tenant** row overrides it for one tenant, and a read resolves tenant row -> system row -> the schema default. Scope is a property of the row, not of the key: every key below may hold a row at either tier. A key with no default and no row at any reachable scope has no value to serve (`config.item_unset`). Writes publish `config.item.changed` (with `[redacted]` markers in both value slots for a Sensitive item) and produce an audit record.\n\n" +
	"Sensitive items are encrypted at rest: the `configs` table stores `base64(ciphertext)` sealed by the host's `dbkit.Cipher` key, never plaintext, and are never served on the public endpoint, whose rows are exactly the items marked Public below (`/api/v1/config/public`, `config.PathPublic`). A Sensitive item's default is redacted in this very table (the `[redacted]` marker), because this document is a committed artifact a secret's plaintext has no more business crossing than the event bus."

// dynamicFooter is the dynamic section's closing text.
const dynamicFooter = "The owning module of each key is its dot-prefix (`authn.social.*` belongs to authn), the module whose runtime code reads the value; `Group` is the admin-console grouping the declaration carried. Feature flags are bool items whose \"enabled\" meaning is decided by `config.Service.IsEnabled`'s dependency walk over the flag graph. The JSON twin of this document carries both layers as flat row lists: a bootstrap entry holds `key`, `module`, `env`, `type`, `unset_fallback`, `sensitive` and `description`; a dynamic item's structured fields follow (`key`, `module`, `scope`, `sensitive`, `default`, ...)."

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

	b.WriteString("---\n")
	b.WriteString("title: \"Configuration reference\"\n")
	b.WriteString("description: \"Every bootstrap key and runtime configuration item a speed-based application resolves: the process-start keys platform modules declare, and the dynamic items an operator edits per tenant.\"\n")
	b.WriteString("weight: 98\n")
	b.WriteString("---\n")
	b.WriteString("# Configuration reference\n\n")
	b.WriteString("A speed-based application resolves configuration in two layers that never share a key. This page is generated from the modules' own declarations -- never hand-written -- and a stale copy fails CI.\n\n")
	b.WriteString(d.renderSections())
	b.WriteString("\n")
	b.WriteString(d.generatedComment())
	return b.String()
}
