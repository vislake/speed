package config

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// Tests for buildSchema, the Attach-time fold of the booted registry's
// ConfigItem and FeatureFlag declarations into one frozen schema snapshot.
// buildSchema's failure modes are declaration-level -- duplicates,
// collisions, unresolved flag dependencies, dependency cycles, mis-typed
// defaults and bounds -- so every test declares its own minimal item/flag
// set and asserts on the returned error's code and params.

func TestBuildSchema_FoldsItemsAndFlags(t *testing.T) {
	deps := []string{"flag.b"}
	sch, err := buildSchema(
		[]pkgcore.ConfigItem{
			{Key: "brand.site_name", Type: "string", Default: "Smile Studio", Public: true, Description: "The tenant's display name", Group: "brand"},
			{Key: "billing.retry_limit", Type: "int", Default: int(3), Min: int(1), Max: int64(10), Description: "Invoice retries"},
		},
		[]pkgcore.FeatureFlag{
			{Key: "flag.b", Default: false, Description: "Base flag"},
			{Key: "flag.a", Default: true, Description: "Depends on b", DependsOn: deps},
		},
	)
	if err != nil {
		t.Fatalf("buildSchema returned an error: %v", err)
	}

	item, ok := sch.lookup("brand.site_name")
	if !ok {
		t.Fatal("brand.site_name was not folded into the schema")
	}
	if item.typ != "string" || item.sensitive || !item.public {
		t.Fatalf("brand.site_name folded as typ=%q sensitive=%v public=%v", item.typ, item.sensitive, item.public)
	}
	if !item.hasDefault || item.defaultCanonical != "Smile Studio" {
		t.Fatalf("brand.site_name default folded as hasDefault=%v canonical=%q", item.hasDefault, item.defaultCanonical)
	}
	if item.isFlag || len(item.flagDeps) != 0 {
		t.Fatalf("brand.site_name folded as isFlag=%v flagDeps=%v", item.isFlag, item.flagDeps)
	}

	retry, ok := sch.lookup("billing.retry_limit")
	if !ok {
		t.Fatal("billing.retry_limit was not folded into the schema")
	}
	if retry.minCanonical == nil || *retry.minCanonical != "1" || retry.maxCanonical == nil || *retry.maxCanonical != "10" {
		t.Fatalf("billing.retry_limit bounds folded as min=%v max=%v", retry.minCanonical, retry.maxCanonical)
	}

	flagA, ok := sch.lookup("flag.a")
	if !ok {
		t.Fatal("flag.a was not folded into the schema")
	}
	if !flagA.isFlag || flagA.typ != itemTypeBool || !flagA.hasDefault || flagA.defaultCanonical != "true" {
		t.Fatalf("flag.a folded as isFlag=%v typ=%q hasDefault=%v default=%q", flagA.isFlag, flagA.typ, flagA.hasDefault, flagA.defaultCanonical)
	}
	// The flag's DependsOn list must be copied, not aliased: the schema is
	// frozen at Attach, so a later mutation of the registration-time slice
	// must not reach it.
	deps[0] = "mutated"
	if len(flagA.flagDeps) != 1 || flagA.flagDeps[0] != "flag.b" {
		t.Fatalf("flag.a dependencies were not copied at fold time: %v", flagA.flagDeps)
	}

	flagB, ok := sch.lookup("flag.b")
	if !ok {
		t.Fatal("flag.b was not folded into the schema")
	}
	if !flagB.isFlag || flagB.defaultCanonical != "false" {
		t.Fatalf("flag.b folded as isFlag=%v default=%q", flagB.isFlag, flagB.defaultCanonical)
	}
}

func TestBuildSchema_ItemWithoutDefaultHasNone(t *testing.T) {
	sch, err := buildSchema(
		[]pkgcore.ConfigItem{{Key: "brand.help_url", Type: "string"}},
		nil,
	)
	if err != nil {
		t.Fatalf("buildSchema returned an error: %v", err)
	}
	item, ok := sch.lookup("brand.help_url")
	if !ok {
		t.Fatal("brand.help_url was not folded into the schema")
	}
	if item.hasDefault {
		t.Fatal("an item declared without a Default must fold as hasDefault=false")
	}
}

func TestBuildSchema_RejectsDuplicateItemKeys(t *testing.T) {
	_, err := buildSchema(
		[]pkgcore.ConfigItem{
			{Key: "brand.site_name", Type: "string", Default: "A"},
			{Key: "brand.site_name", Type: "string", Default: "B"},
		},
		nil,
	)
	assertCode(t, err, ErrSchemaConflict)
	assertParam(t, err, "key", "brand.site_name")
}

func TestBuildSchema_RejectsItemFlagCollision(t *testing.T) {
	_, err := buildSchema(
		[]pkgcore.ConfigItem{{Key: "brand.site_name", Type: "string", Default: "A"}},
		[]pkgcore.FeatureFlag{{Key: "brand.site_name", Default: true}},
	)
	assertCode(t, err, ErrSchemaConflict)
	assertParam(t, err, "key", "brand.site_name")
}

func TestBuildSchema_RejectsDuplicateFlagKeys(t *testing.T) {
	_, err := buildSchema(
		nil,
		[]pkgcore.FeatureFlag{
			{Key: "flag.a", Default: true},
			{Key: "flag.a", Default: false},
		},
	)
	assertCode(t, err, ErrSchemaConflict)
	assertParam(t, err, "key", "flag.a")
}

func TestBuildSchema_RejectsUnresolvedFlagDependency(t *testing.T) {
	_, err := buildSchema(
		nil,
		[]pkgcore.FeatureFlag{{Key: "flag.a", Default: true, DependsOn: []string{"flag.missing"}}},
	)
	assertCode(t, err, ErrSchemaConflict)
	assertParam(t, err, "key", "flag.a")
	assertParam(t, err, "depends_on", "flag.missing")
}

func TestBuildSchema_RejectsFlagDependencyOnPlainItem(t *testing.T) {
	// A flag may only depend on another declared flag: a dependency on a
	// plain bool ConfigItem would have no flag semantics to walk.
	_, err := buildSchema(
		[]pkgcore.ConfigItem{{Key: "brand.dark_mode", Type: "bool", Default: false}},
		[]pkgcore.FeatureFlag{{Key: "flag.a", Default: true, DependsOn: []string{"brand.dark_mode"}}},
	)
	assertCode(t, err, ErrSchemaConflict)
	assertParam(t, err, "key", "flag.a")
	assertParam(t, err, "depends_on", "brand.dark_mode")
}

func TestBuildSchema_RejectsFlagDependencyCycles(t *testing.T) {
	tests := []struct {
		name  string
		flags []pkgcore.FeatureFlag
	}{
		{
			name: "direct self cycle",
			flags: []pkgcore.FeatureFlag{
				{Key: "flag.a", Default: true, DependsOn: []string{"flag.a"}},
			},
		},
		{
			name: "two flag cycle",
			flags: []pkgcore.FeatureFlag{
				{Key: "flag.a", Default: true, DependsOn: []string{"flag.b"}},
				{Key: "flag.b", Default: true, DependsOn: []string{"flag.a"}},
			},
		},
		{
			name: "transitive cycle",
			flags: []pkgcore.FeatureFlag{
				{Key: "flag.a", Default: true, DependsOn: []string{"flag.b"}},
				{Key: "flag.b", Default: true, DependsOn: []string{"flag.c"}},
				{Key: "flag.c", Default: true, DependsOn: []string{"flag.a"}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildSchema(nil, tc.flags)
			assertCode(t, err, ErrFeatureFlagDependencyCycle)
		})
	}
}

func TestBuildSchema_RejectsDefaultOfWrongGoKind(t *testing.T) {
	// buildSchema is internal, so it can be handed a declaration pkgcore's
	// registrar would already have refused: the fold must still fail rather
	// than freeze a schema it cannot serve.
	_, err := buildSchema(
		[]pkgcore.ConfigItem{{Key: "billing.retry_limit", Type: "int", Default: "three"}},
		nil,
	)
	assertCode(t, err, ErrSchemaConflict)
	assertParam(t, err, "key", "billing.retry_limit")
}

func TestBuildSchema_RejectsBoundOfWrongGoKind(t *testing.T) {
	_, err := buildSchema(
		[]pkgcore.ConfigItem{{Key: "billing.retry_limit", Type: "int", Default: int(3), Min: "one"}},
		nil,
	)
	assertCode(t, err, ErrSchemaConflict)
	assertParam(t, err, "key", "billing.retry_limit")
	assertParam(t, err, "bound", "Min")
}

func TestBuildSchema_RejectsDurationBoundsOnStringItem(t *testing.T) {
	// foldBounds only canonicalizes what the declaration carries; a bound on
	// a non-int/duration item cannot canonicalize and must fail the fold.
	_, err := buildSchema(
		[]pkgcore.ConfigItem{{Key: "brand.site_name", Type: "string", Default: "A", Min: int(1)}},
		nil,
	)
	assertCode(t, err, ErrSchemaConflict)
}
