package dbkit_test

// Runnable documentation for dbkit's public API, mirroring
// go/pkgcore/example_test.go's convention: every example here is compiled
// and executed by `go test`, so a change to dbkit's public API that breaks
// the documented usage fails the build instead of only rotting in prose.
//
// Example demonstrates exactly the pattern AGENTS.md's "Typical
// integration" section walks through: a business module defines a
// tenant-scoped model, embeds dbkit.Repository[T] in its own repository
// type instead of holding a *gorm.DB, opens a connection through
// dbkit.Open, and drives a Create followed by a FindByID through it.

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// exampleSubscription is a tenant-scoped model, matching the shape every
// business module's own models are expected to follow: exported "ID" and
// "TenantID" string fields by those exact names (dbkit.Repository[T] reads
// both through reflection; see repository.go's own doc comment), with
// tenant_id the leftmost column of the composite primary key per the
// backend coding standard's data-model rules.
type exampleSubscription struct {
	ID       string `gorm:"primaryKey;size:26"`
	TenantID string `gorm:"primaryKey;size:26;not null"`
	PlanID   string `gorm:"size:64;not null"`
	Status   string `gorm:"size:32;not null"`
}

// GetTenantID satisfies dbkit.TenantScoped.
func (s exampleSubscription) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(s.TenantID) }

// TableName pins exampleSubscription's table name explicitly, independent
// of GORM's pluralization rules, matching the raw CREATE TABLE Example
// applies below.
func (exampleSubscription) TableName() string { return "example_subscriptions" }

// exampleSubscriptionRepository is the data-access type a real business
// module would define, embedding dbkit.Repository[T] instead of holding a
// *gorm.DB directly (backend coding standard, section 3.2).
type exampleSubscriptionRepository struct {
	*dbkit.Repository[exampleSubscription]
}

func newExampleSubscriptionRepository(db *gorm.DB) *exampleSubscriptionRepository {
	return &exampleSubscriptionRepository{Repository: dbkit.NewRepository[exampleSubscription](db)}
}

// Activate creates an active subscription under ctx's tenant, mirroring
// the convenience method AGENTS.md's own illustration adds on top of the
// embedded Repository[T].
func (r *exampleSubscriptionRepository) Activate(ctx context.Context, id, planID string) (*exampleSubscription, error) {
	sub := &exampleSubscription{ID: id, PlanID: planID, Status: "active"}
	if err := r.Create(ctx, sub); err != nil {
		return nil, err
	}
	return sub, nil
}

func Example() {
	ctx := context.Background()

	// A real caller opens PostgreSQL in production (dbkit.DialectPostgres);
	// SQLite keeps this example self-contained and dependency-free under
	// `go test`, with no external service required.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:dbkit_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// A real module applies its own versioned migrations through
	// dbkit.MigrationRegistry (see migrations.go); a plain Exec stands in
	// for that here to keep this example self-contained.
	if err = db.Exec(`CREATE TABLE example_subscriptions (
		id        VARCHAR(26) NOT NULL,
		tenant_id VARCHAR(26) NOT NULL,
		plan_id   VARCHAR(64) NOT NULL,
		status    VARCHAR(32) NOT NULL,
		PRIMARY KEY (tenant_id, id)
	)`).Error; err != nil {
		fmt.Println("migrate:", err)
		return
	}

	repo := newExampleSubscriptionRepository(db)

	// A real request's context already carries the tenant, injected by
	// tenancy.Middleware from the access token claims; building it
	// explicitly here is only for illustration.
	ctx = pkgcore.WithTenant(ctx, "tenant-acme")

	sub, err := repo.Activate(ctx, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "plan_pro")
	if err != nil {
		fmt.Println("activate:", err)
		return
	}

	got, err := repo.FindByID(ctx, sub.ID) // promoted from the embedded *dbkit.Repository[exampleSubscription]
	if err != nil {
		fmt.Println("find:", err)
		return
	}
	fmt.Println(got.PlanID, got.Status)

	// Output:
	// plan_pro active
}

// exampleBlindAccount is the encrypted-field-plus-blind-index model shape:
// Email holds the sensitive value, encrypted at rest through the serializer
// named by its tag, and EmailIndex is the plain indexed column declared
// right next to it, storing nothing but 64 hex characters of HMAC — never
// anything derived by GORM, always written explicitly by the application
// from (*dbkit.BlindIndexer).Index (see the BlindIndexer doc comment).
type exampleBlindAccount struct {
	ID         uint   `gorm:"primaryKey"`
	Email      string `gorm:"column:email;serializer:example_blind_email_enc"`
	EmailIndex string `gorm:"column:email_index;size:64;not null"`
}

// TableName pins exampleBlindAccount's table name explicitly, independent
// of GORM's pluralization rules, matching the raw CREATE TABLE Example
// applies below.
func (exampleBlindAccount) TableName() string { return "example_blind_index_accounts" }

// exampleRandomKey returns a fresh 32-byte secret — the one key shape
// dbkit.NewCipher and dbkit.NewBlindIndexer both require. A real deployment
// reads its secrets from the secret manager instead; this stands in for that
// injection point so the example stays self-contained.
func exampleRandomKey() ([]byte, error) {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	return key, err
}

// ExampleBlindIndexer demonstrates the field-level-encryption companion
// documented on dbkit.BlindIndexer: an email address stored encrypted gets a
// plain, indexed HMAC column next to it, so exact-match lookups can find the
// row without decrypting every record — with the write side (Index) and the
// query side (Equal) both funneling the caller's raw input through the same
// per-column normalization.
func ExampleBlindIndexer() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:dbkit_blind_index_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// A real module applies its own versioned migrations through
	// dbkit.MigrationRegistry (see migrations.go); a plain Exec stands in
	// for that here to keep this example self-contained.
	if err = db.Exec(`CREATE TABLE example_blind_index_accounts (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		email       BLOB NOT NULL,
		email_index VARCHAR(64) NOT NULL
	)`).Error; err != nil {
		fmt.Println("migrate:", err)
		return
	}

	// Bootstrap, where secrets are injected: the encryption key and the
	// blind-index key are two separate secrets — see dbkit.NewCipher's
	// key-separation warning for why they must never be the same bytes —
	// and both obey the same 32-byte policy NewCipher enforces with
	// dbkit.ErrInvalidKeySize.
	emailKey, err := exampleRandomKey()
	if err != nil {
		fmt.Println("keys:", err)
		return
	}
	indexKey, err := exampleRandomKey()
	if err != nil {
		fmt.Println("keys:", err)
		return
	}

	cipher, err := dbkit.NewCipher(emailKey)
	if err != nil {
		fmt.Println("cipher:", err)
		return
	}
	dbkit.RegisterEncryptedSerializer("example_blind_email_enc", cipher)

	// The BlindIndexer binds the index column's name, its HMAC key, and its
	// canonical form into one object, so the write side and the query side
	// cannot drift apart: both always normalize through the same function
	// before hashing.
	emailIndex, err := dbkit.NewBlindIndexer("email_index", indexKey, dbkit.NormalizeEmail)
	if err != nil {
		fmt.Println("indexer:", err)
		return
	}

	// Write: the serializer encrypts Email into the column, and the index
	// column holds Index of the same plaintext — normalized on the way in,
	// so a value typed with odd casing still indexes canonically.
	indexValue, err := emailIndex.Index("User@Example.COM")
	if err != nil {
		fmt.Println("index:", err)
		return
	}
	account := exampleBlindAccount{Email: "User@Example.COM", EmailIndex: indexValue}
	if err = db.Create(&account).Error; err != nil {
		fmt.Println("create:", err)
		return
	}

	// Query: Equal takes the raw input the caller actually has — possibly
	// with whitespace and different case than the write — normalizes it
	// exactly as Index did, and yields a WHERE condition for the column.
	cond, err := emailIndex.Equal(" user@example.com ")
	if err != nil {
		fmt.Println("equal:", err)
		return
	}
	var got exampleBlindAccount
	if err = db.Where(cond).First(&got).Error; err != nil {
		fmt.Println("find:", err)
		return
	}
	// Email decrypts back to exactly what was stored — the variant casing of
	// the query input never touches the encrypted plaintext.
	fmt.Println("found:", got.Email)

	// Input with no canonical form is rejected on the query side just as on
	// the write side — the mechanism never silently hashes a value it cannot
	// normalize, which is what would turn a lookup into a mysterious miss.
	if _, err = emailIndex.Equal(""); err != nil {
		fmt.Println("rejected:", err)
	}

	// Output:
	// found: User@Example.COM
	// rejected: dbkit: blind index column "email_index": dbkit: email normalization: input is empty
}

// exampleTask is a tenant-scoped model that also opts into dbkit's
// automatic write-capture plugin by implementing dbkit.Auditable.
// AuditResourceType names the kind of resource a captured write is about;
// nothing else about the model changes.
type exampleTask struct {
	ID       string `gorm:"primaryKey;size:26"`
	TenantID string `gorm:"primaryKey;size:26;not null"`
	Title    string `gorm:"size:255;not null"`
}

// GetTenantID satisfies dbkit.TenantScoped.
func (t exampleTask) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(t.TenantID) }

// AuditResourceType satisfies dbkit.Auditable.
func (exampleTask) AuditResourceType() string { return "task" }

// TableName pins exampleTask's table name explicitly, independent of
// GORM's pluralization rules, matching the raw CREATE TABLE
// ExampleAuditable applies below.
func (exampleTask) TableName() string { return "example_tasks" }

// ExampleAuditable demonstrates dbkit's automatic write-capture mechanism:
// a model implementing Auditable, opened with Options.AuditBus set,
// publishes a dbkit.WriteCapturedEvent on every Create, Update or Delete
// -- with no change to the write call itself. A real host subscribes the
// go/dbkit/audit persister module to this event instead of the raw
// pkgcore.EventBus.Subscribe shown here, so the event ends up in the
// audit_events table; the direct subscription below keeps this example
// self-contained.
func ExampleAuditable() {
	ctx := context.Background()

	bus := pkgcore.NewMemoryEventBus()
	bus.Subscribe(dbkit.EventWriteCaptured, func(_ context.Context, evt pkgcore.Event) error {
		captured := evt.Payload.(dbkit.WriteCapturedEvent)
		fmt.Println(captured.ResourceType, captured.Operation, captured.ResourceID)
		return nil
	})

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect:  dbkit.DialectSQLite,
		DSN:      "file:dbkit_auditable_example?mode=memory&cache=shared",
		AuditBus: bus,
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	if err = db.Exec(`CREATE TABLE example_tasks (
		id        VARCHAR(26)  NOT NULL,
		tenant_id VARCHAR(26)  NOT NULL,
		title     VARCHAR(255) NOT NULL,
		PRIMARY KEY (tenant_id, id)
	)`).Error; err != nil {
		fmt.Println("migrate:", err)
		return
	}

	ctx = pkgcore.WithTenant(ctx, "tenant-acme")
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})

	task := &exampleTask{ID: "task-1", Title: "Review the audit round"}
	if err = dbkit.NewRepository[exampleTask](db).Create(ctx, task); err != nil {
		fmt.Println("create:", err)
		return
	}

	// Output:
	// task create task-1
}

// exampleTicket is a tenant-scoped model that also opts into dbkit's
// mark-delete (soft-delete) capability by implementing dbkit.SoftDeletable
// -- DeletedAt and DeletedBy are the two required fields (see
// soft_delete.go's SoftDeletable doc comment); nothing else about the
// model changes, and GetDeletedAt is never called by dbkit itself, exactly
// like GetTenantID.
type exampleTicket struct {
	ID        string     `gorm:"primaryKey;size:26"`
	TenantID  string     `gorm:"primaryKey;size:26;not null"`
	Subject   string     `gorm:"size:255;not null"`
	DeletedAt *time.Time `gorm:"column:deleted_at"`
	DeletedBy string     `gorm:"column:deleted_by;not null;default:''"`
}

// GetTenantID satisfies dbkit.TenantScoped.
func (t exampleTicket) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(t.TenantID) }

// GetDeletedAt satisfies dbkit.SoftDeletable.
func (t exampleTicket) GetDeletedAt() *time.Time { return t.DeletedAt }

// TableName pins exampleTicket's table name explicitly, independent of
// GORM's pluralization rules, matching the raw CREATE TABLE
// ExampleSoftDeletable applies below.
func (exampleTicket) TableName() string { return "example_tickets" }

// ExampleSoftDeletable demonstrates dbkit's mark-delete capability: a model
// implementing SoftDeletable gets a Delete that marks instead of physically
// removing the row (hidden from ordinary queries by the soft-delete
// auto-scope plugin Open installs unconditionally), and a Restore that
// undoes it. Repository[T]'s method set does not change to expose this --
// Delete keeps its existing signature and simply behaves differently for a
// SoftDeletable T, and Restore is the one new method.
func ExampleSoftDeletable() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:dbkit_soft_deletable_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	if err = db.Exec(`CREATE TABLE example_tickets (
		id         VARCHAR(26)  NOT NULL,
		tenant_id  VARCHAR(26)  NOT NULL,
		subject    VARCHAR(255) NOT NULL,
		deleted_at TIMESTAMP NULL,
		deleted_by VARCHAR(64) NOT NULL DEFAULT '',
		PRIMARY KEY (tenant_id, id)
	)`).Error; err != nil {
		fmt.Println("migrate:", err)
		return
	}

	ctx = pkgcore.WithTenant(ctx, "tenant-acme")
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})

	repo := dbkit.NewRepository[exampleTicket](db)
	ticket := &exampleTicket{ID: "ticket-1", Subject: "billing question"}
	if err = repo.Create(ctx, ticket); err != nil {
		fmt.Println("create:", err)
		return
	}

	// Delete marks the row instead of physically removing it -- ordinary
	// reads no longer see it.
	if err = repo.Delete(ctx, ticket.ID); err != nil {
		fmt.Println("delete:", err)
		return
	}
	if _, err = repo.FindByID(ctx, ticket.ID); err != nil {
		fmt.Println("find after delete:", err)
	}

	// Restore clears deleted_at/deleted_by, making the row visible again.
	if err = repo.Restore(ctx, ticket.ID); err != nil {
		fmt.Println("restore:", err)
		return
	}
	restored, err := repo.FindByID(ctx, ticket.ID)
	if err != nil {
		fmt.Println("find after restore:", err)
		return
	}
	fmt.Println("restored:", restored.Subject)

	// Output:
	// find after delete: dbkit.record_not_found
	// restored: billing question
}

// exampleHardDeletePurpose is the SystemPurpose ExampleRepository_HardDelete grants
// itself, on the fixture-purpose convention hard_delete_test.go's
// hardDeleteTestPurpose follows (RegisterSystemPurpose is idempotent and
// mutex-guarded, so the registration is a no-op from the second grant on).
// In production the grant comes from a whitelisted module entering
// tenancy.WithSystemContext's audited wrapper; this example registers and
// grants directly only because it sits below tenancy in the dependency
// graph, exactly as dbkit's own hard_delete.go does.
const exampleHardDeletePurpose pkgcore.SystemPurpose = "dbkit.example.hard_delete"

// ExampleRepository_HardDelete demonstrates Repository[T].HardDelete, the restricted,
// irreversible half of the delete semantics: an ordinary tenant-scoped
// context is refused outright (the gate runs before the database is
// touched), while a system context granted over that same tenant context
// passes the gate and the row is physically erased -- a SoftDeletable T
// included, so a soft-deleted row is removable too. The erasure is proven
// with db.WithContext(...).Unscoped(), which bypasses the query-only
// soft-delete auto-scope plugin: finding nothing there means the row is
// physically gone, not merely hidden.
func ExampleRepository_HardDelete() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:dbkit_hard_delete_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	if err = db.Exec(`CREATE TABLE example_tickets (
		id         VARCHAR(26)  NOT NULL,
		tenant_id  VARCHAR(26)  NOT NULL,
		subject    VARCHAR(255) NOT NULL,
		deleted_at TIMESTAMP NULL,
		deleted_by VARCHAR(64) NOT NULL DEFAULT '',
		PRIMARY KEY (tenant_id, id)
	)`).Error; err != nil {
		fmt.Println("migrate:", err)
		return
	}

	ctx = pkgcore.WithTenant(ctx, "tenant-acme")
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})

	repo := dbkit.NewRepository[exampleTicket](db)
	ticket := &exampleTicket{ID: "ticket-1", Subject: "billing question"}
	if err = repo.Create(ctx, ticket); err != nil {
		fmt.Println("create:", err)
		return
	}

	// An ordinary tenant-scoped context is refused: the gate runs before
	// the database is touched. The refusal is matched by code through
	// apperr.As, never by errors.Is against the package-level var.
	if err = repo.HardDelete(ctx, ticket.ID); err != nil {
		if appErr, ok := apperr.As(err); ok {
			fmt.Println("hard delete on tenant ctx:", appErr.Code)
		} else {
			fmt.Println("hard delete on tenant ctx: unexpected error:", err)
			return
		}
	}

	// A granted system context layered over the tenant context passes the
	// gate -- but the tenant stays binding: the DELETE below is scoped to
	// tenant-acme and to it only.
	pkgcore.RegisterSystemPurpose(exampleHardDeletePurpose)
	sysCtx, err := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   "retention-cleanup-job",
		Purpose: exampleHardDeletePurpose,
		Ticket:  "dbkit-hard-delete-example",
	})
	if err != nil {
		fmt.Println("system context:", err)
		return
	}
	if err = repo.HardDelete(sysCtx, ticket.ID); err != nil {
		fmt.Println("hard delete:", err)
		return
	}

	// Ground truth: the physical row is gone. .Unscoped() skips the
	// soft-delete auto-scope (which would hide a merely soft-deleted row),
	// and the context carries tenant-acme so the tenant-scoping plugin's
	// own filter applies -- gorm's ErrRecordNotFound prints as "record not
	// found".
	var gone exampleTicket
	if err = db.WithContext(ctx).Unscoped().Where("id = ?", ticket.ID).First(&gone).Error; err != nil {
		fmt.Println("erased:", err)
	}

	// Output:
	// hard delete on tenant ctx: dbkit.hard_delete_requires_system_context
	// erased: record not found
}
