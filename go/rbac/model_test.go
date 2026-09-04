package rbac

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm/schema"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac/migrations"
)

func TestModels_TableNames(t *testing.T) {
	// The table names are part of the migration contract: gorm's own
	// pluralization would name these "roles" / "role_permissions" /
	// "role_bindings", which would collide with other modules' tables in
	// the one shared database every module migrates into.
	cases := []struct {
		got  string
		want string
	}{
		{Role{}.TableName(), "rbac_roles"},
		{RolePermission{}.TableName(), "rbac_role_permissions"},
		{RoleBinding{}.TableName(), "rbac_role_bindings"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("TableName() = %q, want %q", tc.got, tc.want)
		}
	}
}

func TestModels_GetTenantID_ReadsThePromotedField(t *testing.T) {
	// GetTenantID is promoted from dbkit.TenantModel. It is what
	// Repository[T]'s defense-in-depth check compares against the
	// context's tenant, so a model whose promoted accessor does not see
	// the stored value denies even its legitimate owner.
	role := Role{}
	role.TenantID = "t1"
	if got := role.GetTenantID(); got != pkgcore.TenantID("t1") {
		t.Fatalf("Role.GetTenantID() = %q, want %q", got, "t1")
	}

	perm := RolePermission{}
	perm.TenantID = "t2"
	if got := perm.GetTenantID(); got != pkgcore.TenantID("t2") {
		t.Fatalf("RolePermission.GetTenantID() = %q, want %q", got, "t2")
	}

	binding := RoleBinding{}
	binding.TenantID = "t3"
	if got := binding.GetTenantID(); got != pkgcore.TenantID("t3") {
		t.Fatalf("RoleBinding.GetTenantID() = %q, want %q", got, "t3")
	}
}

func TestRoleBinding_IsTenantWide(t *testing.T) {
	// The empty-string sentinel is the tenant root. It must never be
	// confused with a real node id, and a node-scoped binding must never
	// report itself as tenant-wide -- that would be the exact
	// narrowing-fails-open bug the design forbids.
	if !(RoleBinding{NodeID: ""}).IsTenantWide() {
		t.Fatal("a binding with an empty node id must be tenant-wide")
	}
	if (RoleBinding{NodeID: "/g1/r2"}).IsTenantWide() {
		t.Fatal("a node-scoped binding must not report itself as tenant-wide")
	}
}

// TestModels_ColumnsMatchTheMigrations is the model/migration drift gate.
//
// There is no AutoMigrate in this codebase (root CLAUDE.md), so nothing at
// run time reconciles a struct field with the column a versioned migration
// actually created: a renamed field, a forgotten column, or a column added
// to only one of the two dialect files fails as a confusing SQL error at
// the first query instead. This test compares the gorm-resolved column set
// of each model against the columns EVERY migration file of a dialect
// contributes to that table, in filename order -- not just 0001's CREATE
// TABLE -- because 0002_add_soft_delete.sql grows rbac_role_bindings by two
// columns through ALTER TABLE ADD COLUMN rather than a fresh CREATE TABLE,
// exactly as go/org's own soft-delete round grew org_nodes and memberships.
func TestModels_ColumnsMatchTheMigrations(t *testing.T) {
	models := map[string]any{
		"rbac_roles":            &Role{},
		"rbac_role_permissions": &RolePermission{},
		"rbac_role_bindings":    &RoleBinding{},
	}

	for _, dialect := range []string{"postgres", "sqlite"} {
		t.Run(dialect, func(t *testing.T) {
			tables := make(map[string][]string)
			for _, name := range sortedMigrationNames(t, dialect) {
				sqlBytes, err := migrations.FS.ReadFile(dialect + "/" + name)
				if err != nil {
					t.Fatalf("reading %s/%s: %v", dialect, name, err)
				}
				text := stripSQLComments(string(sqlBytes))
				for table, cols := range parseCreateTableColumns(text) {
					tables[table] = append(tables[table], cols...)
				}
				for table, cols := range parseAlterTableAddColumns(text) {
					tables[table] = append(tables[table], cols...)
				}
			}
			for table := range tables {
				sort.Strings(tables[table])
			}

			for table, model := range models {
				gotSQL, ok := tables[table]
				if !ok {
					t.Fatalf("no %s migration creates or extends table %q", dialect, table)
				}
				wantModel := modelColumns(t, model)
				if !reflect.DeepEqual(gotSQL, wantModel) {
					t.Fatalf("%s.%s columns = %v, but the model resolves to %v", dialect, table, gotSQL, wantModel)
				}
			}
		})
	}
}

// sortedMigrationNames returns dialect's migration filenames in the same
// lexical order dbkit.MigrationRegistry applies them in, so the accumulated
// column set above reflects the migrations exactly as a real boot would
// apply them.
func sortedMigrationNames(t *testing.T, dialect string) []string {
	t.Helper()
	entries, err := migrations.FS.ReadDir(dialect)
	if err != nil {
		t.Fatalf("reading %s/: %v", dialect, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// TestMigrations_ForbidDialectSpecificConstructs pins the dual-dialect
// rules of the backend coding standard §5 on this module's own SQL, so a
// later migration cannot quietly introduce a construct that only one of
// the two supported databases understands.
func TestMigrations_ForbidDialectSpecificConstructs(t *testing.T) {
	banned := []string{"gen_random_uuid", "now()", "jsonb", "serial", "text[]", "uuid_generate"}
	for _, dialect := range []string{"postgres", "sqlite"} {
		entries, err := migrations.FS.ReadDir(dialect)
		if err != nil {
			t.Fatalf("reading %s/: %v", dialect, err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s/ contains no migration files", dialect)
		}
		for _, entry := range entries {
			data, err := migrations.FS.ReadFile(dialect + "/" + entry.Name())
			if err != nil {
				t.Fatalf("reading %s/%s: %v", dialect, entry.Name(), err)
			}
			lowered := strings.ToLower(stripSQLComments(string(data)))
			for _, token := range banned {
				if strings.Contains(lowered, token) {
					t.Fatalf("%s/%s uses the dialect-specific construct %q", dialect, entry.Name(), token)
				}
			}
		}
	}
}

// TestMigrations_TenantIDIsLeftmostInEveryIndex pins the composite-index
// rule of the backend coding standard §5: an index whose leading column is
// not tenant_id cannot serve a tenant-filtered query, which is the only
// kind of query this module ever issues.
//
// It scans EVERY migration file of a dialect, not just 0001's, so the
// partial index 0002_add_soft_delete.sql re-creates
// (uq_rbac_role_bindings_tenant_user_role_node, now WHERE deleted_at IS
// NULL) is checked exactly like every index 0001 declares from scratch.
func TestMigrations_TenantIDIsLeftmostInEveryIndex(t *testing.T) {
	indexRe := regexp.MustCompile(`(?is)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(\S+)\s+ON\s+\S+\s*\(([^)]*)\)`)
	for _, dialect := range []string{"postgres", "sqlite"} {
		var matches [][]string
		for _, name := range sortedMigrationNames(t, dialect) {
			data, err := migrations.FS.ReadFile(dialect + "/" + name)
			if err != nil {
				t.Fatalf("reading %s/%s: %v", dialect, name, err)
			}
			matches = append(matches, indexRe.FindAllStringSubmatch(stripSQLComments(string(data)), -1)...)
		}
		if len(matches) == 0 {
			t.Fatalf("the %s migrations declare no indexes", dialect)
		}
		for _, m := range matches {
			cols := strings.Split(m[2], ",")
			first := strings.TrimSpace(cols[0])
			if first != "tenant_id" {
				t.Fatalf("%s: index %s leads with %q, want tenant_id", dialect, m[1], first)
			}
		}
	}
}

// modelColumns returns model's gorm-resolved column names, sorted.
func modelColumns(t *testing.T, model any) []string {
	t.Helper()
	s, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parsing %T's gorm schema: %v", model, err)
	}
	cols := make([]string, 0, len(s.Fields))
	for _, f := range s.Fields {
		if f.DBName == "" {
			continue
		}
		cols = append(cols, f.DBName)
	}
	sort.Strings(cols)
	return cols
}

// createTableRe captures each CREATE TABLE statement's name and body.
var createTableRe = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(\w+)\s*\((.*?)\n\);`)

// alterTableAddColumnRe captures the table and column name of an
// "ALTER TABLE <table> ADD COLUMN <column> ..." statement -- the shape a
// column-adding migration like 0002_add_soft_delete.sql uses, which
// parseCreateTableColumns cannot see since such a file has no CREATE TABLE
// at all.
var alterTableAddColumnRe = regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(\w+)\s+ADD\s+COLUMN\s+(\w+)`)

// parseCreateTableColumns extracts, per table, the column names sqlText's
// CREATE TABLE statements declare. A line whose first token is a
// table-level constraint keyword (PRIMARY, UNIQUE, ...) is not a column.
//
// It returns an empty map, never a test failure, when sqlText has no CREATE
// TABLE at all: a later, column-adding-only migration file legitimately has
// none, and the caller -- which reads every migration file of a dialect, in
// order -- is what decides whether the accumulated result across every file
// is complete.
func parseCreateTableColumns(sqlText string) map[string][]string {
	constraintKeywords := map[string]bool{
		"primary": true, "unique": true, "foreign": true, "check": true, "constraint": true,
	}
	out := make(map[string][]string)
	for _, match := range createTableRe.FindAllStringSubmatch(sqlText, -1) {
		var cols []string
		for _, line := range strings.Split(match[2], "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) == 0 {
				continue
			}
			name := strings.TrimSuffix(fields[0], ",")
			if constraintKeywords[strings.ToLower(name)] {
				continue
			}
			cols = append(cols, name)
		}
		out[match[1]] = cols
	}
	return out
}

// parseAlterTableAddColumns extracts, per table, the column names sqlText's
// "ALTER TABLE ... ADD COLUMN ..." statements add -- 0002_add_soft_delete
// .sql's own shape, and any future column-adding migration's.
func parseAlterTableAddColumns(sqlText string) map[string][]string {
	out := make(map[string][]string)
	for _, match := range alterTableAddColumnRe.FindAllStringSubmatch(sqlText, -1) {
		table, column := match[1], match[2]
		out[table] = append(out[table], column)
	}
	return out
}

// stripSQLComments removes "--" line comments so the assertions above read
// the SQL itself rather than the prose explaining it.
func stripSQLComments(sqlText string) string {
	var b strings.Builder
	for _, line := range strings.Split(sqlText, "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
