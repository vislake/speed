package pkgcore

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/vislake/speed/go/pkgcore/i18n"
)

// component_assembly.go implements the Prepare stage's
// parse-resolve-validate-plan pipeline: the second beat of Prepare, which
// runs before anything is constructed and fails the whole assembly on the
// first problem it finds. It turns the composition configuration in the
// by-type context into the plan the ComponentRegistry then constructs, in
// exactly four steps -- select (expand the configuration's selections, pull
// uniquely-available providers), resolve (replace each selected component's
// configuration block with the optional ComponentConfigResolver's
// five-source-merged result, when the registry carries one), validate
// (dependency completeness, configuration keys -- the per-block decode, the
// required-value check, the sensitive/documentation pairing and the
// one-key-path-per-source rule -- capabilities, assets, graph), plan
// (topological order, written to the registry) -- and it never calls a New.

// selection is one resolved member of the assembly, before the plan exists.
type selection struct {
	name      string
	component Component
	cfg       ComponentConfig
	auto      bool
}

// assemblyDraft is the working state of the pipeline.
type assemblyDraft struct {
	// order is the selection order: the composition configuration's order
	// for explicitly selected components, then auto-pull discovery order.
	order []string
	// byName resolves a selected name to its selection.
	byName map[string]*selection
	// off records the names the composition configuration explicitly
	// deselected; auto-pull never pulls them back.
	off map[string]struct{}
	// edges maps each selected component to the selected components whose
	// products satisfy its requirements.
	edges map[string][]string
}

// planAssembly runs the parse-resolve-validate-plan pipeline over the
// composition configuration in the by-type context and writes the plan.
func (r *ComponentRegistry) planAssembly(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("pkgcore: (stage prepare): %w", err)
	}

	cfg, err := Get[ComponentConfig](r)
	if err != nil {
		if errors.Is(err, ErrMissingRequirement) {
			return fmt.Errorf("%w (stage prepare): the composition configuration has not been put; put a pkgcore.ComponentConfig before calling Prepare", ErrMissingRequirement)
		}
		return fmt.Errorf("pkgcore: composition configuration (stage prepare): %w", err)
	}
	comp, err := parseComposition(cfg)
	if err != nil {
		return err
	}

	draft, err := r.expandSelection(comp.components)
	if err != nil {
		return err
	}
	// resolveErr, not err: govet's shadow check flags reusing err after the
	// composition read above, which is still meaningful to a reader.
	if resolveErr := r.resolveRequirements(draft, comp.strict); resolveErr != nil {
		return resolveErr
	}
	if deliveryErr := validateDeclaredDeliveries(draft); deliveryErr != nil {
		return deliveryErr
	}
	if resolveErr := r.resolveComponentConfigs(draft); resolveErr != nil {
		return resolveErr
	}
	if validateErr := r.validateSelection(draft, comp.mode); validateErr != nil {
		return validateErr
	}

	ordered, err := topologicalOrder(draft)
	if err != nil {
		return err
	}

	planned := make([]plannedComponent, 0, len(ordered))
	byName := make(map[string]int, len(ordered))
	names := make([]string, 0, len(ordered))
	autoNames := make([]string, 0)
	for _, sel := range ordered {
		byName[sel.name] = len(planned)
		planned = append(planned, plannedComponent{component: sel.component, cfg: sel.cfg, auto: sel.auto})
		names = append(names, sel.name)
		if sel.auto {
			autoNames = append(autoNames, sel.name)
		}
	}

	r.mu.Lock()
	r.plan = planned
	r.planByName = byName
	r.mu.Unlock()

	slog.Default().Info("pkgcore: component assembly prepared",
		"components", strings.Join(names, ", "),
		"auto_selected", joinNames(autoNames),
	)
	return nil
}

// expandSelection reads the components block into a draft: every named
// component must be registered, nil or a mapping selects it (a mapping is
// its configuration), false deselects it, and anything else is refused.
func (r *ComponentRegistry) expandSelection(components ComponentConfig) (*assemblyDraft, error) {
	draft := &assemblyDraft{
		byName: make(map[string]*selection),
		off:    make(map[string]struct{}),
		edges:  make(map[string][]string),
	}
	registeredNames := r.registeredOrder()

	for _, entry := range components.entries {
		name := entry.key
		c, exists := r.registered(name)
		if !exists {
			if suggestion := closestName(name, registeredNames); suggestion != "" {
				return nil, fmt.Errorf("%w (stage prepare): component %q is not registered; did you mean %q?", ErrUnknownComponent, name, suggestion)
			}
			return nil, fmt.Errorf("%w (stage prepare): component %q is not registered; registered components: %s", ErrUnknownComponent, name, joinNames(registeredNames))
		}

		sel := &selection{name: name, component: c}
		switch value := entry.value.(type) {
		case nil:
		case bool:
			if value {
				return nil, fmt.Errorf("pkgcore: component %q (stage prepare): a selection value must be nil or a mapping to select the component, or false to deselect it; got true", name)
			}
			draft.off[name] = struct{}{}
			continue
		default:
			cfg, isMapping := asMap(entry.value)
			if !isMapping {
				return nil, fmt.Errorf("pkgcore: component %q (stage prepare): a selection value must be nil or a mapping to select the component, or false to deselect it; got %s", name, typeName(reflect.TypeOf(entry.value)))
			}
			sel.cfg = cfg
		}
		draft.order = append(draft.order, name)
		draft.byName[name] = sel
	}
	return draft, nil
}

// resolveRequirements walks every selected component's requirements,
// resolves each token against the selected components' Provides
// declarations, and -- unless strict is set -- auto-pulls the uniquely
// available registered component that satisfies a required token nothing
// selected provides. It fills draft.edges with the consumer-to-provider
// edges the topological order is derived from. Auto-pulled components join
// the queue and have their own requirements resolved in turn.
//
// A catalog requirement resolves against the selection's ProvidesMember
// declarations instead: every selected member delivering the token becomes
// an edge (so the members construct before the consumer), MinMembers bounds
// the member count, and neither the ambiguity switch nor auto-pull applies
// -- multiplicity is the catalog's semantics, and a member joins only by
// explicit selection.
func (r *ComponentRegistry) resolveRequirements(draft *assemblyDraft, strict bool) error {
	queue := append([]string(nil), draft.order...)

	for i := 0; i < len(queue); i++ {
		sel := draft.byName[queue[i]]
		for _, req := range sel.component.Requires {
			token := reflect.TypeOf(req.Token)

			if req.Catalog {
				members := catalogMembers(draft, token)
				if len(members) < req.MinMembers {
					return fmt.Errorf("%w (stage prepare): component %q consumes the %s catalog and %d selected member(s) deliver it, below the required minimum of %d; select more members in the composition configuration", ErrMissingRequirement, sel.name, tokenDisplay(token), len(members), req.MinMembers)
				}
				for _, member := range members {
					draft.edges[sel.name] = appendUnique(draft.edges[sel.name], member)
				}
				continue
			}

			providers := make([]string, 0, 2)
			for _, name := range draft.order {
				if providesToken(draft.byName[name].component, token) {
					providers = append(providers, name)
				}
			}

			switch {
			case len(providers) > 1:
				return fmt.Errorf("%w (stage prepare): %s is provided by multiple selected components: %s; deselect all but one", ErrAmbiguousProvider, tokenDisplay(token), joinNames(providers))
			case len(providers) == 1:
				draft.edges[sel.name] = appendUnique(draft.edges[sel.name], providers[0])
			case req.Optional:
				// An optional dependency with no provider is satisfied by
				// nothing, and the consumer runs with a zero value.
			case strict:
				return fmt.Errorf("%w (stage prepare): component %q requires %s, and no selected component provides it; strict mode disables auto-pull, so select a provider in the composition configuration", ErrMissingRequirement, sel.name, tokenDisplay(token))
			default:
				if err := r.autoPull(draft, sel, token, &queue); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// catalogMembers returns the names of the selected components whose
// ProvidesMember declarations deliver token, in selection order: the
// consumers of a catalog requirement depend on every one of them, and
// nothing else ever joins this set -- a catalog never auto-pulls.
func catalogMembers(draft *assemblyDraft, token reflect.Type) []string {
	var members []string
	for _, name := range draft.order {
		if providesMemberToken(draft.byName[name].component, token) {
			members = append(members, name)
		}
	}
	return members
}

// autoPull selects the uniquely-available registered component that
// satisfies token on behalf of consumer: exactly one candidate is selected
// and announced, none fails with ErrMissingRequirement, several fail with
// ErrAmbiguousProvider listing the candidates.
func (r *ComponentRegistry) autoPull(draft *assemblyDraft, consumer *selection, token reflect.Type, queue *[]string) error {
	var candidates []string
	for _, name := range r.registeredOrder() {
		if _, selected := draft.byName[name]; selected {
			continue
		}
		if _, deselected := draft.off[name]; deselected {
			continue
		}
		c, _ := r.registered(name)
		if providesToken(c, token) {
			candidates = append(candidates, name)
		}
	}

	switch len(candidates) {
	case 0:
		return fmt.Errorf("%w (stage prepare): component %q requires %s, and no registered component provides it; add a providing component to the binary or deselect %q", ErrMissingRequirement, consumer.name, tokenDisplay(token), consumer.name)
	case 1:
		name := candidates[0]
		c, _ := r.registered(name)
		draft.byName[name] = &selection{name: name, component: c, auto: true}
		draft.order = append(draft.order, name)
		*queue = append(*queue, name)
		draft.edges[consumer.name] = appendUnique(draft.edges[consumer.name], name)
		slog.Default().Info("pkgcore: component auto-selected to satisfy a requirement",
			"component", name,
			"required_by", consumer.name,
			"token", tokenDisplay(token),
		)
		return nil
	default:
		return fmt.Errorf("%w (stage prepare): component %q requires %s, and multiple registered components could provide it: %s; select exactly one in the composition configuration", ErrAmbiguousProvider, consumer.name, tokenDisplay(token), joinNames(candidates))
	}
}

// providesToken reports whether a component's Provides declarations match
// token.
func providesToken(c Component, token reflect.Type) bool {
	for _, declared := range c.Provides {
		if productMatchesToken(reflect.TypeOf(declared), token) {
			return true
		}
	}
	return false
}

// providesMemberToken reports whether a component's ProvidesMember
// declarations match token.
func providesMemberToken(c Component, token reflect.Type) bool {
	for _, declared := range c.ProvidesMember {
		if productMatchesToken(reflect.TypeOf(declared), token) {
			return true
		}
	}
	return false
}

// memberAddressesType reports whether a component's ProvidesMember
// declarations address target -- the reading-side match Members uses: a
// declaration promises a member whose value satisfies the declared token,
// so target addresses the member when a value of type target would satisfy
// one of those tokens (target may be the contract type itself, the type
// behind a declared pointer).
func memberAddressesType(c Component, target reflect.Type) bool {
	for _, declared := range c.ProvidesMember {
		if productMatchesToken(target, reflect.TypeOf(declared)) {
			return true
		}
	}
	return false
}

// appendUnique appends name to names when it is not already present.
func appendUnique(names []string, name string) []string {
	if slices.Contains(names, name) {
		return names
	}
	return append(names, name)
}

// validateDeclaredDeliveries refuses a selection in which two selected
// components' declarations contradict one another. Two shapes are refused:
// one single-value token delivered twice -- which every by-type reading of
// it (Get, GetOptional, the nil-for-absent sugars) would find ambiguous --
// and one token declared as a bound delivery by one component while
// another declares it as a catalog membership, two semantics a single
// token cannot carry at once. The requirement walk above anchors the
// ambiguity rule on the token a consumer names; this pass completes it
// over the selection itself, so a duplicate delivery nothing requires
// cannot slip through to the by-type context and surface only as a
// read-time failure or a sugar answering absent. Only declared deliveries
// are visible here -- a value put outside a component's declaration is
// invisible to the plan and stays a read-time condition. Multiple
// ProvidesMember declarations of one token are legal, not a collision:
// they are the catalog's members.
func validateDeclaredDeliveries(draft *assemblyDraft) error {
	for i, nameA := range draft.order {
		a := draft.byName[nameA].component
		for _, nameB := range draft.order[i+1:] {
			b := draft.byName[nameB].component
			if token, ok := overlappingDelivery(a, b); ok {
				return fmt.Errorf("%w (stage prepare): %s is provided by multiple selected components: %s; deselect all but one", ErrAmbiguousProvider, tokenDisplay(token), joinNames([]string{a.Name, b.Name}))
			}
			if token, ok := kindConflictingDelivery(a, b); ok {
				return fmt.Errorf("%w (stage prepare): %s is declared as a bound delivery by component %q and as a catalog member by component %q; one token is either bound (one selected provider, read by type) or catalog (members, read by name), never both, so align the declarations or deselect one", ErrInvalidComponent, tokenDisplay(token), a.Name, b.Name)
			}
		}
	}
	return nil
}

// overlappingDelivery reports whether one component's bound deliveries
// would be found by a by-type reading for a token another component also
// declares: the same declaration twice, or a pair whose declared types
// address one another (an interface token beside a concrete type that
// implements it). It returns the token to name in the refusal.
func overlappingDelivery(a, b Component) (reflect.Type, bool) {
	for _, declaredA := range a.Provides {
		ta := reflect.TypeOf(declaredA)
		for _, declaredB := range b.Provides {
			tb := reflect.TypeOf(declaredB)
			switch {
			case productMatchesToken(ta, tb):
				return tb, true
			case productMatchesToken(tb, ta):
				return ta, true
			}
		}
	}
	return nil, false
}

// kindConflictingDelivery reports whether one component declares a token as
// a bound delivery while the other declares it as a catalog membership --
// in either direction -- returning the token to name in the refusal.
func kindConflictingDelivery(a, b Component) (reflect.Type, bool) {
	if token, ok := boundAgainstMembers(a.Provides, b.ProvidesMember); ok {
		return token, true
	}
	return boundAgainstMembers(b.Provides, a.ProvidesMember)
}

// boundAgainstMembers reports whether a bound declaration and a member
// declaration address one another: one token under the two delivery kinds,
// matched the way every declaration match works (the declared type, or the
// type behind the pointer, assignable to the other).
func boundAgainstMembers(bound, members []any) (reflect.Type, bool) {
	for _, declaredBound := range bound {
		tb := reflect.TypeOf(declaredBound)
		for _, declaredMember := range members {
			tm := reflect.TypeOf(declaredMember)
			switch {
			case productMatchesToken(tb, tm):
				return tm, true
			case productMatchesToken(tm, tb):
				return tb, true
			}
		}
	}
	return nil, false
}

// validateSelection runs the per-component validations in selection order:
// the configuration block against the component's ConfigSchema, the
// documentation pairing a sensitive schema field requires, the declared
// capabilities against the deployment mode, and the embedded assets -- then
// the set-level checks over the selected whole: the one-key-path-per-source
// rule, the locale resources and the migration sets.
func (r *ComponentRegistry) validateSelection(draft *assemblyDraft, mode DeploymentMode) error {
	selected := make([]*selection, 0, len(draft.order))
	for _, name := range draft.order {
		sel := draft.byName[name]
		fields, err := analyzeComponentSchema(sel.component)
		if err != nil {
			return err
		}
		if err := validateConfigBlock(sel.component, sel.cfg, fields); err != nil {
			return err
		}
		if err := validateSensitiveDocs(sel.component, fields); err != nil {
			return err
		}
		if err := validateComponentCapabilities(sel.component, mode); err != nil {
			return err
		}
		if err := validateComponentAssets(sel.component); err != nil {
			return err
		}
		selected = append(selected, sel)
	}
	if err := validateConfigKeyPaths(draft); err != nil {
		return err
	}
	if err := validateLocaleAssets(selected); err != nil {
		return err
	}
	return validateMigrationLedgerKeys(selected)
}

// resolveComponentConfigs gives each selected component's configuration
// block its final form. When the registry carries a ComponentConfigResolver
// (the assembling engine's five-source parser, put before Prepare), every
// selected component that declares a schema has its file block replaced by
// the resolver's merged result -- what the component's New then receives as
// its cfg, decode text unchanged. Without one the blocks stand exactly as
// the composition configuration carried them, the file-only behaviour a
// bare registry keeps.
func (r *ComponentRegistry) resolveComponentConfigs(draft *assemblyDraft) error {
	resolver, present, err := GetOptional[ComponentConfigResolver](r)
	if err != nil {
		return fmt.Errorf("pkgcore: the component configuration resolver (stage prepare): %w", err)
	}
	if !present {
		return nil
	}

	for _, name := range draft.order {
		sel := draft.byName[name]
		if sel.component.ConfigSchema == nil {
			// A component that declares no schema has nothing to resolve;
			// its block is validated empty instead.
			continue
		}
		merged, err := resolver.ResolveComponentConfig(sel.name, sel.component.ConfigSchema, sel.cfg)
		if err != nil {
			return fmt.Errorf("pkgcore: component %q (stage prepare): resolve the configuration: %w", sel.name, err)
		}
		sel.cfg = merged
	}
	return nil
}

// analyzeComponentSchema flattens a component's ConfigSchema, naming the
// component in any parse error.
func analyzeComponentSchema(c Component) ([]schemaField, error) {
	fields, err := analyzeConfigSchema(c.ConfigSchema)
	if err != nil {
		return nil, fmt.Errorf("pkgcore: component %q (stage prepare): %w", c.Name, err)
	}
	return fields, nil
}

// validateConfigBlock strictly decodes a component's configuration block --
// the resolver's merged result when one ran -- against its ConfigSchema, and
// checks every field the schema declares required actually arrived. A
// component that declares no schema accepts no keys, so any key in its
// block is unknown.
func validateConfigBlock(c Component, cfg ComponentConfig, fields []schemaField) error {
	if c.ConfigSchema == nil {
		if cfg.Len() == 0 {
			return nil
		}
		return fmt.Errorf("pkgcore: component %q (stage prepare): %w; the component declares no configuration",
			c.Name, unknownKeysError(cfg.Keys(), nil))
	}

	target := reflect.New(reflect.TypeOf(c.ConfigSchema).Elem())
	if err := cfg.Decode(target.Interface()); err != nil {
		return fmt.Errorf("pkgcore: component %q (stage prepare): %w", c.Name, err)
	}

	var missing []string
	for _, f := range fields {
		if f.required && !configCarriesKey(cfg, f.key) {
			missing = append(missing, f.key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w (stage prepare): component %q: the ConfigSchema declares key %s required, and no configuration source supplied a value for it; supply the value in the component's configuration block, or drop the required declaration",
			ErrMissingConfigValue, c.Name, quoteAll(missing))
	}
	return nil
}

// validateSensitiveDocs enforces the documentation half of the sensitive tag
// option: a field marked sensitive must carry a ConfigDocs entry with a
// non-empty description, because a secret no operator is told how to supply
// or rotate is one they cannot deploy. A schema declaring no Documented at
// all cannot mark a field sensitive.
func validateSensitiveDocs(c Component, fields []schemaField) error {
	docs := configDocs(c.ConfigSchema)
	var undocumented []string
	for _, f := range fields {
		if !f.sensitive {
			continue
		}
		doc, ok := configDocFor(docs, f.key)
		if ok && strings.TrimSpace(doc.Description) != "" {
			continue
		}
		undocumented = append(undocumented, f.key)
	}
	if len(undocumented) == 0 {
		return nil
	}
	return fmt.Errorf("%w (stage prepare): component %q marks configuration key %s sensitive, and a sensitive field must document itself: implement ConfigDocs() on the ConfigSchema target with an entry carrying a non-empty Description under the field's local key path",
		ErrInvalidComponent, c.Name, quoteAll(undocumented))
}

// validateConfigKeyPaths runs the one-key-path-per-source rule over the
// selected whole: every key path a source can resolve in the selection --
// each component's source-addressed schema fields (the expose option, which
// derive implies) under the namespace prefix its ConfigNamespace declares,
// plus each component's BootstrapKeys declarations -- must be unique. A key
// path two sources produce would let one of them silently resolve the
// other's value, so the assembly refuses it, naming the key path and both
// sources. A field no source opens (no expose option) resolves from its own
// component's configuration block alone, so it holds no shareable address
// and claims no key path here: two components' block-only fields of one
// name read their own blocks and never each other's value.
func validateConfigKeyPaths(draft *assemblyDraft) error {
	owner := make(map[string]string)
	for _, name := range draft.order {
		sel := draft.byName[name]
		prefix, err := configKeyPrefix(sel.name, sel.component.ConfigNamespace)
		if err != nil {
			return fmt.Errorf("pkgcore: component %q (stage prepare): %w", sel.name, err)
		}
		fields, err := analyzeConfigSchema(sel.component.ConfigSchema)
		if err != nil {
			return fmt.Errorf("pkgcore: component %q (stage prepare): %w", sel.name, err)
		}
		for _, f := range fields {
			if !f.expose {
				continue
			}
			who := fmt.Sprintf("component %q (ConfigSchema field %q)", sel.name, f.name)
			if err := claimConfigKeyPath(owner, prefix+f.key, who); err != nil {
				return err
			}
		}
		for _, key := range sel.component.BootstrapKeys {
			who := fmt.Sprintf("component %q (BootstrapKeys declaration)", sel.name)
			if err := claimConfigKeyPath(owner, key.Key, who); err != nil {
				return err
			}
		}
	}
	return nil
}

// claimConfigKeyPath records one source's claim on a key path, refusing a
// claim on a path another source already produced. Paths compare
// case-insensitively, the way config keys resolve.
func claimConfigKeyPath(owner map[string]string, path, source string) error {
	folded := strings.ToLower(path)
	if previous, taken := owner[folded]; taken {
		return fmt.Errorf("%w (stage prepare): configuration key path %q is produced by both %s and %s; give one of them a distinct key path or namespace",
			ErrConfigKeyConflict, path, previous, source)
	}
	owner[folded] = source
	return nil
}

// configCarriesKey reports whether a component's configuration block carries
// a value at the local dotted key path, matching each segment
// case-insensitively the way Decode matches keys.
func configCarriesKey(cfg ComponentConfig, local string) bool {
	segment, rest, nested := strings.Cut(local, ".")
	raw, ok := configLookupFold(cfg, segment)
	if !ok {
		return false
	}
	if !nested {
		return true
	}
	child, isMapping := asMap(raw)
	if !isMapping {
		return false
	}
	return configCarriesKey(child, rest)
}

// configLookupFold reads a raw value out of a ComponentConfig by a key
// matched case-insensitively.
func configLookupFold(cfg ComponentConfig, key string) (any, bool) {
	if raw, ok := cfg.lookup(key); ok {
		return raw, true
	}
	for _, candidate := range cfg.Keys() {
		if strings.EqualFold(candidate, key) {
			return cfg.lookup(candidate)
		}
	}
	return nil, false
}

// validateComponentCapabilities compares a component's declared capabilities
// against the deployment mode's requirement: a missing non-durability
// capability fails the assembly naming the component, the missing bits and
// the mode, while a missing SurvivesRestart alone is a startup warning --
// losing state across a restart is a legitimate choice, never a startup
// failure, and no built-in mode requires the bit today.
func validateComponentCapabilities(c Component, mode DeploymentMode) error {
	required := mode.RequiredCapabilities()
	missing := required &^ c.Capabilities
	if missing == 0 {
		return nil
	}
	if missing&^SurvivesRestart == 0 {
		slog.Default().Warn("pkgcore: component does not survive a process restart",
			"component", c.Name,
			"deployment_mode", string(mode),
		)
		return nil
	}
	return fmt.Errorf("%w (stage prepare): component %q lacks %s, required by deployment mode %q; select an implementation that declares it", ErrCapabilityUnsatisfied, c.Name, missing, mode)
}

// supportedDialects are the migration subdirectory names a component's
// Migrations embed.FS may carry, mirroring dbkit's two dialects -- spelled
// here as literals because dbkit sits above pkgcore in the dependency graph
// and cannot be imported from it.
var supportedDialects = []string{"postgres", "sqlite"}

// validateComponentAssets validates a component's embedded assets -- the
// migration set's ownership and shape, the OpenAPI fragment's parseability
// -- and, over the whole set for the locales, key-set parity and the
// catalog's language coverage. All of it runs before anything is
// constructed.
func validateComponentAssets(c Component) error {
	var zeroFS embed.FS
	if c.Migrations != zeroFS {
		if c.Module == "" {
			return fmt.Errorf("%w (stage prepare): component %q: the component carries a migration set but declares no module; a migration set's ledger key is the module it belongs to, because a component name can be prefixed or overridden by a host while the module name cannot", ErrInvalidAsset, c.Name)
		}
		if err := validateMigrations(c.Migrations); err != nil {
			return fmt.Errorf("%w (stage prepare): component %q: %w", ErrInvalidAsset, c.Name, err)
		}
	}
	if err := validateOpenAPIFragment(c.OpenAPISpec); err != nil {
		return fmt.Errorf("%w (stage prepare): component %q: %w", ErrInvalidAsset, c.Name, err)
	}
	return nil
}

// validateMigrations checks the shape of a migration set: every root entry a
// dialect directory named after a supported dialect, every file in one a
// numbered .sql file whose lexical order is the apply order. A component
// with no migrations (its zero embed.FS) does not reach this check.
func validateMigrations(fsys fs.FS) error {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return fmt.Errorf("migration set: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return fmt.Errorf("migration set: %q is not a dialect directory; migrations live under one subdirectory per dialect (%s)", entry.Name(), strings.Join(supportedDialects, ", "))
		}
		if !slices.Contains(supportedDialects, entry.Name()) {
			return fmt.Errorf("migration set: dialect directory %q is not supported (supported dialects: %s)", entry.Name(), strings.Join(supportedDialects, ", "))
		}
		files, err := fs.ReadDir(fsys, entry.Name())
		if err != nil {
			return fmt.Errorf("migration set: read %q: %w", entry.Name(), err)
		}
		for _, file := range files {
			if file.IsDir() {
				return fmt.Errorf("migration set: %q holds a nested directory; migrations are flat .sql files", entry.Name()+"/"+file.Name())
			}
			if err := checkMigrationFileName(file.Name()); err != nil {
				return fmt.Errorf("migration set: %s: %w", entry.Name()+"/"+file.Name(), err)
			}
		}
	}
	return nil
}

// checkMigrationFileName validates one migration file name against the
// "<number>_<name>.sql" convention a lexical sort orders by.
func checkMigrationFileName(name string) error {
	stem, ok := strings.CutSuffix(name, ".sql")
	if !ok {
		return errors.New("want a numbered .sql file, such as 0001_create_users.sql")
	}
	number, rest, ok := strings.Cut(stem, "_")
	if !ok || number == "" || rest == "" {
		return errors.New("want a numbered .sql file, such as 0001_create_users.sql")
	}
	for _, r := range number {
		if r < '0' || r > '9' {
			return fmt.Errorf("the leading segment %q is not a number", number)
		}
	}
	return nil
}

// validateOpenAPIFragment checks that an OpenAPI fragment is usable before
// the host merges it: non-empty fragments must be valid UTF-8 text and carry
// a top-level openapi: declaration (or parse as JSON, whose validity alone
// proves the shape). Fragments are YAML documents, and this package sits at
// the zero-third-party-dependency floor, so full grammar validation is not
// repeated here: the merge tooling parses every fragment at build time, and
// the check here catches the class that would break it at startup -- empty,
// binary or non-OpenAPI content.
func validateOpenAPIFragment(spec []byte) error {
	if len(spec) == 0 {
		return nil
	}
	if !utf8.Valid(spec) {
		return errors.New("OpenAPI fragment: the spec is not valid UTF-8 text")
	}
	if json.Valid(spec) {
		return nil
	}
	for _, line := range strings.Split(string(spec), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "openapi:") {
			return nil
		}
	}
	return errors.New("OpenAPI fragment: the spec carries no top-level openapi: declaration")
}

// validateLocaleAssets runs the selected components that ship locale
// resources through the message catalog's own contract implementation --
// pkgcore/i18n's Builder -- so key-set parity, id prefixes and the shared
// language set are enforced exactly as the eventual catalog merge will
// enforce them, before anything is constructed. Nothing is merged: the
// builder is built and discarded.
//
// The id prefix is the component's MODULE name when it implements one: the
// message ids belong to the module's contract, so every implementation of a
// module shares one prefix, and a host that renamed or copied a descriptor
// (an override component carries the module's own locale resources) still
// validates and merges under the module's name rather than the host's.
func validateLocaleAssets(selected []*selection) error {
	var zeroFS embed.FS
	builder := i18n.NewBuilder()
	for _, sel := range selected {
		if sel.component.Locales == zeroFS {
			continue
		}
		if err := builder.AddModule(localePrefix(sel.component), sel.component.Locales); err != nil {
			return fmt.Errorf("%w (stage prepare): component %q: locale resources: %w", ErrInvalidAsset, sel.name, err)
		}
	}
	return nil
}

// localePrefix returns the id prefix a component's locale resources merge
// under: the module name it implements, or its own component name for an
// assembly-time step that implements no module.
func localePrefix(c Component) string {
	if c.Module != "" {
		return c.Module
	}
	return c.Name
}

// validateMigrationLedgerKeys enforces the migration ledger's one-set-per-key
// contract over the selected whole: a set is recorded under the module its
// component implements (Asset.Module), so two selected components
// implementing the same module must not both carry a set -- the second
// would collide with the first under one ledger key and never apply. Every
// carrier reaching this check has already passed validateComponentAssets, so
// its Module is non-empty; a directory-style module's members may share the
// module as long as at most one of them carries the set. Components outside
// the selection are not checked: only the selected set's assets are applied.
func validateMigrationLedgerKeys(selected []*selection) error {
	var zeroFS embed.FS
	owner := make(map[string]string)
	for _, sel := range selected {
		if sel.component.Migrations == zeroFS {
			continue
		}
		module := sel.component.Module
		if first, ok := owner[module]; ok {
			return fmt.Errorf("%w (stage prepare): components %q and %q both carry a migration set and both implement module %q, whose single ledger key admits one set; select one carrier per module or merge the sets into the module's own component", ErrInvalidAsset, first, sel.component.Name, module)
		}
		owner[module] = sel.component.Name
	}
	return nil
}

// topologicalOrder returns the selected components ordered so that every
// component appears after the components providing its requirements. The
// seed order -- the composition configuration's order first, then the
// auto-pulled components by name -- breaks ties between independent
// components, so the plan is deterministic for one composition. A dependency
// cycle fails the assembly, naming the cycle path.
func topologicalOrder(draft *assemblyDraft) ([]*selection, error) {
	seeds := make([]string, 0, len(draft.order))
	var autoNames []string
	for _, name := range draft.order {
		if draft.byName[name].auto {
			autoNames = append(autoNames, name)
			continue
		}
		seeds = append(seeds, name)
	}
	slices.Sort(autoNames)
	seeds = append(seeds, autoNames...)

	const (
		unvisited = iota
		visiting
		visited
	)
	state := make(map[string]int, len(seeds))
	ordered := make([]*selection, 0, len(seeds))
	path := make([]string, 0, len(seeds))

	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case visited:
			return nil
		case visiting:
			return fmt.Errorf("%w (stage prepare): %s; break the cycle by removing one Requires edge", ErrDependencyCycle, formatCycle(path, name))
		}
		state[name] = visiting
		path = append(path, name)
		for _, dependency := range draft.edges[name] {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		state[name] = visited
		ordered = append(ordered, draft.byName[name])
		return nil
	}

	for _, name := range seeds {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}
