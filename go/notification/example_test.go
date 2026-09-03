package notification_test

import (
	"context"
	"fmt"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/notification"
)

// ExampleInboxMessage walks a delivery into one tenant's in-app inbox the
// way the module's future consumers will: open and migrate the database,
// create a message under the recipient's tenant, read it back, mark it
// read, and watch the same id read as not-found from a tenant that does not
// own it.
//
// The example needs no testcontainers: dbkit.Open over an in-memory SQLite
// database is the standalone deployment mode's ordinary shape, and the
// module's own migration files apply from zero through the real registry.
func ExampleInboxMessage() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:notification_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	registry := dbkit.NewMigrationRegistry()
	if err = registry.Register(notification.NewModule(db)); err != nil {
		fmt.Println("register migrations:", err)
		return
	}
	if err = registry.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		fmt.Println("apply migrations:", err)
		return
	}

	repo := notification.NewRepository(db)
	ctx = pkgcore.WithTenant(ctx, "tenant-acme")

	key := "delivery-note-42"
	msg := &notification.InboxMessage{
		ID:              "inbox-000001",
		RecipientUserID: "user-7",
		TypeKey:         "note.shared",
		Title:           "Note 42 was shared with you",
		Body:            "Lin shared the Caries 101 note with you.",
		DedupeKey:       &key,
	}
	if err = repo.Create(ctx, msg); err != nil {
		fmt.Println("create:", err)
		return
	}

	got, err := repo.FindByID(ctx, msg.ID)
	if err != nil {
		fmt.Println("find:", err)
		return
	}
	fmt.Println("title:", got.Title)

	readAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	got.ReadAt = &readAt
	if err = repo.Update(ctx, got); err != nil {
		fmt.Println("mark read:", err)
		return
	}
	got, err = repo.FindByID(ctx, msg.ID)
	if err != nil {
		fmt.Println("re-read:", err)
		return
	}
	fmt.Println("read at:", got.ReadAt.Format("2006-01-02 15:04:05"))

	_, err = repo.FindByID(pkgcore.WithTenant(ctx, "tenant-bright"), msg.ID)
	if appErr, ok := apperr.As(err); ok {
		fmt.Println("other tenant:", appErr.Code)
	}

	// Output:
	// title: Note 42 was shared with you
	// read at: 2026-09-01 10:00:00
	// other tenant: dbkit.record_not_found
}
