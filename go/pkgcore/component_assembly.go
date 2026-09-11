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

// component_assembly.go implements the Prepare stage's parse-resolve-validate-plan
// pipeline: the second beat of Prepare, which runs before anything is
// constructed and fails the whole assembly on the first problem it finds. It
// turns the composition configuration in the by-type context into the plan
// the ComponentRegistry then constructs, in exactly three steps --
// select (expand the configuration's selections, pull uniquely-available
// providers), validate (dependency completeness, configuration keys,
// capabilities, assets, graph), plan (topological order, written to the
// registry) -- and it never calls a New.

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
func (r *ComponentRegistry) resolveRequirements(draft *assemblyDraft, strict bool) error {
	queue := append([]string(nil), draft.order...)

	for i := 0; i < len(queue); i++ {
		sel := draft.byName[queue[i]]
		for _, req := range sel.component.Requires {
			token := reflect.TypeOf(req.Token)

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

// appendUnique appends name to names when it is not already present.
func appendUnique(names []string, name string) []string {
	if slices.Contains(names, name) {
		return names
	}
	return append(names, name)
}

// validateSelection runs the per-component validations in selection order:
// the configuration block against the component's ConfigSchema, the
// declared capabilities against the deployment mode, and the embedded
// assets -- then the set-level checks over the selected whole, the locale
// resources and the migration sets.
func (r *ComponentRegistry) validateSelection(draft *assemblyDraft, mode DeploymentMode) error {
	selected := make([]*selection, 0, len(draft.order))
	for _, name := range draft.order {
		sel := draft.byName[name]
		if err := validateConfigBlock(sel.component, sel.cfg); err != nil {
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
	if err := validateLocaleAssets(selected); err != nil {
		return err
	}
	return validateMigrationLedgerKeys(selected)
}

// validateConfigBlock strictly decodes a component's configuration block
// against its ConfigSchema. A component that declares no schema accepts no
// keys, so any key in its block is unknown.
func validateConfigBlock(c Component, cfg ComponentConfig) error {
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
	return nil
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
