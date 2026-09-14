package db_test

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// softDeletedRow is a model carrying what soft delete rests on: a DeletedAt
// field. GORM reads that field as the instruction to turn a delete into an
// update of the column and to leave the row out of the ordinary queries, which
// is a behaviour of the handle rather than of this module.
type softDeletedRow struct {
	ID        uint `gorm:"primarykey"`
	Name      string
	DeletedAt gorm.DeletedAt `gorm:"index"`
}

// TestSoftDeleteRoundTripsThroughTheDeliveredHandle pins the delivery the design
// lists as a capability of this module: a row deleted through the handle stops
// appearing, and what the delete did is mark it rather than remove it.
//
// Nothing here is this module's own code — GORM implements soft delete — and
// that is the reason to observe it at all. The handle a dependant holds is
// assembled by this module, so a plugin installed on it or a session setting
// changed on it takes the behaviour away, and the shape of that failure is a
// query answering with the wrong rows rather than with an error.
//
// Every step is read: the row is found before the delete, which a handle that
// never wrote anything would fail; the default scope no longer finds it, which a
// handle whose delete did nothing would fail; the unscoped read and the row
// count over the pool find it with the marker set, which a handle whose delete
// removed the row would fail; and a delete past the scope removes it for good,
// which a handle whose deletes are all no-ops would fail.
func TestSoftDeleteRoundTripsThroughTheDeliveredHandle(t *testing.T) {
	h, err := startHost(t, sqliteSpec(), hostConfig(t))
	if err != nil {
		t.Fatalf("starting a host on a configured database: %v", err)
	}
	handle := h.Instance.DB()
	if err := handle.AutoMigrate(&softDeletedRow{}); err != nil {
		t.Fatalf("creating the table this case writes to: %v", err)
	}

	row := softDeletedRow{Name: "kept"}
	if err := handle.Create(&row).Error; err != nil {
		t.Fatalf("writing a row: %v", err)
	}
	var live softDeletedRow
	if err := handle.First(&live, row.ID).Error; err != nil {
		t.Fatalf("reading back the row that was just written: %v", err)
	}

	if err := handle.Delete(&row).Error; err != nil {
		t.Fatalf("deleting the row: %v", err)
	}

	var gone softDeletedRow
	if err := handle.First(&gone, row.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("reading the row back after the delete gave %v, want gorm.ErrRecordNotFound: the "+
			"default scope has to leave a deleted row out", err)
	}

	var kept softDeletedRow
	if err := handle.Unscoped().First(&kept, row.ID).Error; err != nil {
		t.Fatalf("reading the deleted row past the soft-delete scope: %v", err)
	}
	if !kept.DeletedAt.Valid {
		t.Error("the row came back with no deletion timestamp, so what the delete did was not " +
			"record the deletion")
	}

	// The storage itself, read over the pool rather than through the query
	// builder the two reads above went through: the row is still there, and
	// the marker column is set on it.
	var rows, marked int
	if err := poolOf(t, handle).QueryRowContext(t.Context(),
		`SELECT COUNT(*), COUNT(deleted_at) FROM soft_deleted_rows`).Scan(&rows, &marked); err != nil {
		t.Fatalf("reading the table over the pool: %v", err)
	}
	if rows != 1 || marked != 1 {
		t.Errorf("the table holds %d rows of which %d carry a deletion timestamp, want the deleted "+
			"row to stay in place marked (1 of 1)", rows, marked)
	}

	// The other direction: a delete past the scope removes the row, which is
	// what tells the marker apart from a delete that never reaches the
	// database at all.
	if err := handle.Unscoped().Delete(&kept).Error; err != nil {
		t.Fatalf("deleting the row past the soft-delete scope: %v", err)
	}
	if err := poolOf(t, handle).QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM soft_deleted_rows`).Scan(&rows); err != nil {
		t.Fatalf("reading the table over the pool: %v", err)
	}
	if rows != 0 {
		t.Errorf("the table still holds %d rows after a delete past the soft-delete scope, want the "+
			"row removed", rows)
	}
}
