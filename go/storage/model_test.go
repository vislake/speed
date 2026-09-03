package storage

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/storage/migrations"
)

// compile-time checks that both models satisfy dbkit.TenantScoped, the
// constraint every tenant-data row must meet (docs/internal/04-data-and-
// tenancy.md): the embedded TenantModel supplies both the tenant_id column
// and the GetTenantID method the isolation machinery reads.
var (
	_ dbkit.TenantScoped = Object{}
	_ dbkit.TenantScoped = ObjectDerivative{}
)

// TestModel_TableNames pins the table names the models claim to the names
// the migrations actually create. The names live in tableObjects /
// tableObjectDerivatives, which TableName returns and which the migrations'
// CREATE TABLE statements must match; the literals here are the ground
// truth both sides are pinned to.
func TestModel_TableNames(t *testing.T) {
	if got := (Object{}).TableName(); got != "objects" {
		t.Errorf("Object.TableName() = %q, want %q", got, "objects")
	}
	if got := (ObjectDerivative{}).TableName(); got != "object_derivatives" {
		t.Errorf("ObjectDerivative.TableName() = %q, want %q", got, "object_derivatives")
	}
	if tableObjects != "objects" {
		t.Errorf("tableObjects = %q, want %q", tableObjects, "objects")
	}
	if tableObjectDerivatives != "object_derivatives" {
		t.Errorf("tableObjectDerivatives = %q, want %q", tableObjectDerivatives, "object_derivatives")
	}
}

// TestModel_GetTenantID_EchoesTheTenantID proves the promoted TenantModel
// behaves as dbkit's isolation layers expect: GetTenantID returns exactly
// the value in the tenant_id column, whatever it is.
func TestModel_GetTenantID_EchoesTheTenantID(t *testing.T) {
	o := Object{TenantModel: dbkit.TenantModel{TenantID: "tenant-a"}}
	if got := o.GetTenantID(); got != pkgcore.TenantID("tenant-a") {
		t.Errorf("Object.GetTenantID() = %q, want %q", got, "tenant-a")
	}
	o.TenantID = "tenant-b"
	if got := o.GetTenantID(); got != pkgcore.TenantID("tenant-b") {
		t.Errorf("after reassignment, Object.GetTenantID() = %q, want %q", got, "tenant-b")
	}

	d := ObjectDerivative{TenantModel: dbkit.TenantModel{TenantID: "tenant-c"}}
	if got := d.GetTenantID(); got != pkgcore.TenantID("tenant-c") {
		t.Errorf("ObjectDerivative.GetTenantID() = %q, want %q", got, "tenant-c")
	}
}

// TestModel_StateConstants_AreDistinctAndStable pins the object lifecycle
// vocabulary: the three states the state column may hold, spelled exactly
// as the lifecycle round's transitions and the sweep round's queries will
// compare against. A rename ripples through every switch statement that
// reads the column, so each value is frozen here rather than left implicit.
func TestModel_StateConstants_AreDistinctAndStable(t *testing.T) {
	states := map[string]string{
		"ObjectStateUploading": ObjectStateUploading,
		"ObjectStateCompleted": ObjectStateCompleted,
		"ObjectStateDeleting":  ObjectStateDeleting,
	}
	want := map[string]string{
		"ObjectStateUploading": "uploading",
		"ObjectStateCompleted": "completed",
		"ObjectStateDeleting":  "deleting",
	}
	seen := make(map[string]bool, len(states))
	for name, got := range states {
		if want[name] != got {
			t.Errorf("%s = %q, want %q", name, got, want[name])
		}
		if got == "" {
			t.Errorf("%s is empty", name)
		}
		if seen[got] {
			t.Errorf("state value %q is shared by more than one constant", got)
		}
		seen[got] = true
	}
}

// TestModel_DerivativeKindThumbnail pins the one derivative kind this round
// ships. The kind column is open-ended, so this test is what a new kind
// consciously extends, never what an accidental rename silently breaks.
func TestModel_DerivativeKindThumbnail(t *testing.T) {
	if DerivativeKindThumbnail != "thumbnail" {
		t.Errorf("DerivativeKindThumbnail = %q, want %q", DerivativeKindThumbnail, "thumbnail")
	}
}

// TestModel_IDColumnWidths_MatchTheMigrations pins the schema contract the
// model layer makes with the versioned SQL: every id and object_id column
// in every dialect's migration declares VARCHAR(<objectIDLen>). objectIDLen
// is the width of an application-generated UUID string, and the migrations
// are what a deployment actually runs -- a model tag alone could drift from
// them silently, so the SQL files themselves are the thing asserted against.
func TestModel_IDColumnWidths_MatchTheMigrations(t *testing.T) {
	column := regexp.MustCompile(`^\s*(?:id|object_id)\s+VARCHAR\(([0-9]+)\)`)
	files := []string{
		"sqlite/0001_create_objects.sql",
		"sqlite/0002_create_object_derivatives.sql",
		"postgres/0001_create_objects.sql",
		"postgres/0002_create_object_derivatives.sql",
	}

	checked := 0
	for _, file := range files {
		raw, err := migrations.FS.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			match := column.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			width, err := strconv.Atoi(match[1])
			if err != nil {
				t.Fatalf("parse width in %s line %q: %v", file, line, err)
			}
			checked++
			if width != objectIDLen {
				t.Errorf("%s declares an id width of %d, want objectIDLen (%d)", file, width, objectIDLen)
			}
		}
	}
	// Every file carries an id column, and 0002 additionally carries
	// object_id: anything fewer means a column stopped being declared.
	if checked < 6 {
		t.Errorf("found %d id/object_id columns across the migration files, want at least 6 (id in both 0001s, id and object_id in both 0002s)", checked)
	}
}

// TestObjectIDLen_IsAStandardUUIDLength pins objectIDLen to the length
// uuid.NewString produces -- 36 characters. The constant exists so the
// width appears once, in model.go, instead of as magic numbers in column
// tags; this test is the reminder of what that number means.
func TestObjectIDLen_IsAStandardUUIDLength(t *testing.T) {
	if objectIDLen != 36 {
		t.Errorf("objectIDLen = %d, want 36 (the length of uuid.NewString)", objectIDLen)
	}
}
