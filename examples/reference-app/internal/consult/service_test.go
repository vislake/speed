package consult

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/notes"
)

// testCredentialCipherKey is a fixed, recognizable 32-byte AES-GCM key for
// registerCredentialSerializerOnce below -- a test fixture, never a secret,
// exactly like go/ai-gateway's own model_test.go testCipherKey.
var testCredentialCipherKey = []byte{
	0xc0, 0xc1, 0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc7,
	0xc8, 0xc9, 0xca, 0xcb, 0xcc, 0xcd, 0xce, 0xcf,
	0xd0, 0xd1, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7,
	0xd8, 0xd9, 0xda, 0xdb, 0xdc, 0xdd, 0xde, 0xdf,
}

var registerCredentialSerializerOnce sync.Once

// registerCredentialSerializer registers ai-gateway's
// CredentialAPIKeySerializerName exactly once for this test binary. GORM
// resolves a model's named serializer while it parses the schema (the same
// ordering rule aigateway.CredentialAPIKeySerializerName's own doc comment
// states), so this must run before newTestService's dbtest.NewSQLite /
// migrations.Apply ever touches the ai_gateway_credentials table.
func registerCredentialSerializer() {
	registerCredentialSerializerOnce.Do(func() {
		cipher, err := dbkit.NewCipher(testCredentialCipherKey)
		if err != nil {
			panic("consult test: NewCipher on the fixed 32-byte fixture key: " + err.Error())
		}
		dbkit.RegisterEncryptedSerializer(aigateway.CredentialAPIKeySerializerName, cipher)
	})
}

// fakeProviderName is the ChatProviderRegistry name fakeChatProvider below
// self-registers under for these tests, isolated from the real, process-
// global aigateway.ChatProviderRegistry through the per-test registry
// newTestService builds.
const fakeProviderName = "chat.fake-consult-provider"

// testSystemPurpose is the SystemPurpose newTestService's platform-
// credential write grants itself -- the same fixture-purpose convention
// go/ai-gateway's own tests and go/dbkit's hard_delete_test.go both follow.
const testSystemPurpose pkgcore.SystemPurpose = "consult.test.credential_write"

// fakeChatProvider is a minimal aigateway.ChatProvider test double that
// records the last request it received and answers with a fixed reply.
// go/ai-gateway's own fakeChatProvider (gateway_test.go) is unexported, so
// this package declares its own copy of the identical shape rather than
// importing it.
type fakeChatProvider struct {
	lastReq aigateway.ChatRequest
	reply   string
}

// Chat implements aigateway.ChatProvider.
func (f *fakeChatProvider) Chat(_ context.Context, req aigateway.ChatRequest) (aigateway.ChatResponse, error) {
	f.lastReq = req
	return aigateway.ChatResponse{
		Message: aigateway.ChatMessage{Role: aigateway.RoleAssistant, Content: f.reply},
		Usage:   aigateway.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
	}, nil
}

// ChatStream implements aigateway.ChatProvider. Unused by this package's
// tests (Service.Suggest only ever calls Chat), but required to satisfy the
// interface.
func (f *fakeChatProvider) ChatStream(_ context.Context, req aigateway.ChatRequest) (<-chan aigateway.ChatChunk, error) {
	f.lastReq = req
	ch := make(chan aigateway.ChatChunk)
	close(ch)
	return ch, nil
}

// compile-time check that *fakeChatProvider satisfies aigateway.ChatProvider.
var _ aigateway.ChatProvider = (*fakeChatProvider)(nil)

// newTestService returns a Service backed by a fresh, per-test SQLite
// database carrying BOTH notes' and ai-gateway's real migrations, sharing
// one connection -- exactly as cmd/server/server.go's buildServer wires the
// two in production -- with provider registered as the sole
// ChatProviderRegistry entry LogicalModel routes to (isolated from the
// real, process-global aigateway.ChatProviderRegistry). It also returns the
// notes.Repository, so a test can seed a note directly.
func newTestService(t *testing.T, provider aigateway.ChatProvider) (*Service, *notes.Repository) {
	t.Helper()
	registerCredentialSerializer()

	db := dbtest.NewSQLite(t)

	migrations := dbkit.NewMigrationRegistry()
	if err := migrations.Register(notes.NewModule(db)); err != nil {
		t.Fatalf("register notes migrations: %v", err)
	}
	if err := migrations.Register(aigateway.NewModule(db)); err != nil {
		t.Fatalf("register ai-gateway migrations: %v", err)
	}
	if err := migrations.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	registry := pkgcore.NewSeamRegistry[aigateway.ChatProvider]()
	if err := registry.Register(pkgcore.Registration[aigateway.ChatProvider]{
		Name:         fakeProviderName,
		Capabilities: pkgcore.Stateless,
		New:          func(pkgcore.Config) (aigateway.ChatProvider, error) { return provider, nil },
	}); err != nil {
		t.Fatalf("register fake provider: %v", err)
	}

	notesRepo := notes.NewRepository(db)
	credentials := aigateway.NewCredentialService(db)

	pkgcore.RegisterSystemPurpose(testSystemPurpose)
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor: "test-actor", Purpose: testSystemPurpose,
	})
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if err := credentials.SetPlatformCredential(sysCtx, fakeProviderName, "sk-test", ""); err != nil {
		t.Fatalf("SetPlatformCredential: %v", err)
	}

	gateway := aigateway.NewGateway(credentials,
		aigateway.WithModelRoute(LogicalModel, fakeProviderName, "vendor-model-x"),
		aigateway.WithChatProviderRegistry(registry),
	)

	return NewService(notesRepo, gateway), notesRepo
}

func TestService_Suggest_ReadsNoteAndReturnsGatewayReply(t *testing.T) {
	provider := &fakeChatProvider{reply: "Schedule a follow-up in two weeks."}
	svc, notesRepo := newTestService(t, provider)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	note := &notes.Note{ID: uuid.NewString(), Text: "Patient reports mild sensitivity after whitening."}
	if err := notesRepo.Create(ctx, note); err != nil {
		t.Fatalf("create note: %v", err)
	}

	reply, err := svc.Suggest(ctx, note.ID)
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if reply != provider.reply {
		t.Fatalf("Suggest reply = %q, want the fake provider's fixed reply %q", reply, provider.reply)
	}
	if len(provider.lastReq.Messages) != 2 {
		t.Fatalf("provider received %d messages, want 2 (system + note text)", len(provider.lastReq.Messages))
	}
	if got := provider.lastReq.Messages[1].Content; got != note.Text {
		t.Fatalf("provider's user message = %q, want the note's text %q", got, note.Text)
	}
	if provider.lastReq.Model != "vendor-model-x" {
		t.Fatalf("provider saw Model = %q, want the routed vendor model, never the logical key", provider.lastReq.Model)
	}
}

func TestService_Suggest_UnknownNoteID_ReturnsError(t *testing.T) {
	svc, _ := newTestService(t, &fakeChatProvider{reply: "unused"})

	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := svc.Suggest(ctx, "does-not-exist"); err == nil {
		t.Fatal("Suggest with an unknown note id succeeded, want an error")
	}
}

func TestService_Suggest_NoteFromAnotherTenant_NotVisible(t *testing.T) {
	svc, notesRepo := newTestService(t, &fakeChatProvider{reply: "unused"})

	ownerCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	note := &notes.Note{ID: uuid.NewString(), Text: "Acme-only note."}
	if err := notesRepo.Create(ownerCtx, note); err != nil {
		t.Fatalf("create note: %v", err)
	}

	otherCtx := pkgcore.WithTenant(context.Background(), "tenant-globex")
	if _, err := svc.Suggest(otherCtx, note.ID); err == nil {
		t.Fatal("Suggest reached a note belonging to another tenant, want an error")
	}
}
