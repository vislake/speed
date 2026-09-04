package aigateway_test

// Runnable documentation for ai-gateway's public API, mirroring
// go/pki/example_test.go's and go/dbkit/example_test.go's convention: this
// example is compiled AND executed by `go test`, so a change to
// ai-gateway's public API that breaks the documented usage fails the build
// rather than only rotting in prose.
//
// It walks the full Gateway pipeline end to end: open a database, wire the
// credential cipher, migrate, store a platform-wide credential, declare a
// model route, then call Gateway.Chat against a fake OpenAI-compatible
// server -- no live vendor API key is needed or used.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/vislake/speed/go/dbkit"

	// Blank-imported for its init side effect: registers
	// dbkit.DialectSQLite so dbkit.Open below has a driver to build from.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/pkgcore"

	aigateway "github.com/vislake/speed/go/ai-gateway"
)

func Example() {
	ctx := context.Background()

	// A fake OpenAI-compatible server stands in for a real vendor endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message":       map[string]string{"role": "assistant", "content": "Hello from the example provider."},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11},
		})
	}))
	defer srv.Close()

	// The credential column's cipher must be registered BEFORE dbkit.Open:
	// GORM resolves a model's serializer while it parses the schema.
	cipher, err := dbkit.NewCipher([]byte("01234567890123456789012345678901"))
	if err != nil {
		fmt.Println("new cipher:", err)
		return
	}
	dbkit.RegisterEncryptedSerializer(aigateway.CredentialAPIKeySerializerName, cipher)

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:aigateway_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	module := aigateway.NewModule(db,
		aigateway.WithModelRoute("chat:default", aigateway.ProviderOpenAICompatible, "gpt-4o-mini"),
	)

	migrations := dbkit.NewMigrationRegistry()
	if err = migrations.Register(module); err != nil {
		fmt.Println("register migrations:", err)
		return
	}
	if err = migrations.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		fmt.Println("apply migrations:", err)
		return
	}

	// Store the platform-wide credential. A real deployment builds this
	// system context through tenancy.WithSystemContext, which additionally
	// publishes an audit event; this example uses the raw primitive
	// directly for self-containment.
	sysCtx, err := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   "example-bootstrap",
		Purpose: aigateway.SystemPurposeCredentialWrite,
		Ticket:  "EXAMPLE-1",
	})
	if err != nil {
		fmt.Println("with system context:", err)
		return
	}
	if err = module.Credentials().SetPlatformCredential(sysCtx, aigateway.ProviderOpenAICompatible, "sk-example-key", srv.URL); err != nil {
		fmt.Println("set platform credential:", err)
		return
	}

	// A real call carries a tenant; the credential above is the
	// platform-wide fallback every tenant without its own BYOK row
	// resolves to.
	tenantCtx := pkgcore.WithTenant(ctx, "tenant-acme")
	resp, err := module.Gateway().Chat(tenantCtx, aigateway.ChatRequest{
		Model: "chat:default",
		Messages: []aigateway.ChatMessage{
			{Role: aigateway.RoleUser, Content: "Say hello."},
		},
	})
	if err != nil {
		fmt.Println("chat:", err)
		return
	}

	fmt.Println(resp.Message.Content)
	fmt.Println(resp.Usage.TotalTokens)

	// Output:
	// Hello from the example provider.
	// 11
}
