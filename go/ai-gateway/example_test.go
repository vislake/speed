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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/vislake/speed/go/dbkit"

	// Blank-imported for its init side effect: registers
	// dbkit.DialectSQLite so dbkit.Open below has a driver to build from.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/storage"

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
	if regErr := aigateway.RegisterCredentialAPIKeySerializer(cipher); regErr != nil {
		fmt.Println("register serializer:", regErr)
		return
	}

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

	// The host assembles the registry the pipeline resolves providers
	// through: a composition selecting the module's real
	// chat.openai-compatible component, driven through the assembly's four
	// stages. The declaration component's Init is where Module.Register runs
	// (the Init stage is the declaration window), which is what attaches the
	// registry to the Gateway as its provider resolution source; the
	// assembly-time instance of the provider is one construction of the same
	// constructor every request builds its own from through the credential
	// override.
	reg := componenttest.NewRegistry()
	if err = reg.Register(pkgcore.Component{
		Name: "example.declare",
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return new(int), nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			return module.Register(reg)
		},
	}); err != nil {
		fmt.Println("register declaration component:", err)
		return
	}
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"example.declare": nil,
			aigateway.ProviderOpenAICompatible: map[string]any{
				"base_url": srv.URL,
				"api_key":  "sk-example-key",
			},
		},
		"strict": true,
	}))
	if err = reg.Prepare(ctx); err != nil {
		fmt.Println("prepare:", err)
		return
	}
	if err = reg.Construct(ctx); err != nil {
		fmt.Println("construct:", err)
		return
	}
	if err = reg.Verify(ctx); err != nil {
		fmt.Println("verify:", err)
		return
	}
	if err = reg.Init(ctx); err != nil {
		fmt.Println("register module:", err)
		return
	}
	defer func() { _ = reg.Close(ctx) }()

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
	// directly for self-containment. Module.Register is what normally
	// declares the purpose, and the registration is idempotent, so
	// declaring it here keeps the example runnable on its own rather than
	// only after some other test has bootstrapped this module.
	pkgcore.RegisterSystemPurpose(aigateway.SystemPurposeCredentialWrite)
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

// waitForImageGenerateJob polls queue.Get until id's Job reaches a terminal
// Status or deadline passes, mirroring go/jobs/example_test.go's own
// waitForTerminal -- this package's async image pipeline has no second
// job-status mechanism of its own (see image_gateway.go's doc comment), so
// polling go/jobs' existing Queue.Get is the documented way a caller
// retrieves a completed image job's result.
func waitForImageGenerateJob(ctx context.Context, queue jobs.Queue, id jobs.JobID, deadline time.Time) (*jobs.Job, error) {
	for {
		job, err := queue.Get(ctx, id)
		if err != nil || job.Status.Terminal() || time.Now().After(deadline) {
			return job, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Example_generateImage walks the async image-generation pipeline end to
// end: wire a jobs.StandaloneQueue and a real go/storage
// ObjectService (both required by WithImageGeneration), call
// Gateway.GenerateImage, drain the module's declared job handlers onto the
// queue, and poll the enqueued job to completion -- against a fake
// OpenAI-compatible images endpoint, no live vendor API key needed or used.
//
// It also demonstrates the object-reference boundary image_gateway.go's
// own doc comment describes: GenerateImage itself never sees an image
// byte, and the completed job's ImageJobResult carries only a go/storage
// object id, read back here through the same ObjectService the job handler
// wrote it with.
func Example_generateImage() {
	ctx := context.Background()

	// A tiny, real PNG stands in for the vendor's generated image -- the
	// same "encode once, base64 it back" shape
	// openai_compatible_image_test.go's own fixtures use.
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 60), G: uint8(y * 60), B: 90, A: 255})
		}
	}
	var generatedPNG bytes.Buffer
	if err := png.Encode(&generatedPNG, img); err != nil {
		fmt.Println("encode generated png:", err)
		return
	}

	// A fake OpenAI-compatible images endpoint stands in for a real vendor
	// endpoint, answering the images-generations wire shape
	// openai_compatible_image.go parses.
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{
				{"b64_json": base64.StdEncoding.EncodeToString(generatedPNG.Bytes())},
			},
			"usage": map[string]any{
				"image_count": 1,
				"steps":       4,
				"size":        "512x512",
			},
		})
	}))
	defer imgSrv.Close()

	cipher, err := dbkit.NewCipher([]byte("01234567890123456789012345678901"))
	if err != nil {
		fmt.Println("new cipher:", err)
		return
	}
	if regErr := aigateway.RegisterCredentialAPIKeySerializer(cipher); regErr != nil {
		fmt.Println("register serializer:", regErr)
		return
	}

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:aigateway_image_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// One jobs.StandaloneQueue carries both the image-generation task
	// GenerateImage enqueues and storage's own thumbnail-derive task --
	// WithImageGeneration and storage.WithQueue deliberately share it, the
	// same shape examples/reference-app/internal/app/server.go wires.
	queue := jobs.NewStandaloneQueue(db, jobs.WithPollInterval(5*time.Millisecond))

	// go/storage's ObjectService is safe to call right after NewModule,
	// before the assembly has attached its seams (storage/module.go's own doc
	// comment) -- which is what lets it be handed to WithImageGeneration
	// below before the single assembly call at the end of this function.
	storageModule := storage.NewModule(db, storage.WithQueue(queue))

	module := aigateway.NewModule(db,
		aigateway.WithModelRoute("image:default", aigateway.ProviderOpenAICompatibleImage, "vendor-image-model"),
		aigateway.WithImageGeneration(queue, storageModule.ObjectService()),
	)

	migrations := dbkit.NewMigrationRegistry()
	if err = migrations.Register(storageModule); err != nil {
		fmt.Println("register storage migrations:", err)
		return
	}
	if err = migrations.Register(module); err != nil {
		fmt.Println("register ai-gateway migrations:", err)
		return
	}
	if err = migrations.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		fmt.Println("apply migrations:", err)
		return
	}

	// The host assembles the registry the job handlers read: a local object
	// store over a directory of its own (storage's service writes the
	// generated image through it), the in-process values
	// componenttest.NewRegistry puts, and the image provider component every
	// route resolves through. The three stages below are the same ones
	// componenttest.DeclareInto runs -- a declaration component whose Init
	// drives both modules' Register inside the assembly's one Init window,
	// under a strict composition -- written out here because the composition
	// must also select the provider component, which is what makes the
	// route's provider name resolvable.
	dir, err := os.MkdirTemp("", "aigateway-image-example-")
	if err != nil {
		fmt.Println("temp dir:", err)
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	reg := componenttest.NewRegistry()
	reg.Put(pkgcore.NewLocalObjectStore(dir))
	if err = reg.Register(pkgcore.Component{
		Name: "example.declare",
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return new(int), nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			if declareErr := storageModule.Register(reg); declareErr != nil {
				return declareErr
			}
			return module.Register(reg)
		},
	}); err != nil {
		fmt.Println("register declaration component:", err)
		return
	}
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"example.declare": nil,
			aigateway.ProviderOpenAICompatibleImage: map[string]any{
				"base_url": imgSrv.URL,
				"api_key":  "sk-example-image-key",
			},
		},
		"strict": true,
	}))
	if err = reg.Prepare(ctx); err != nil {
		fmt.Println("prepare:", err)
		return
	}
	if err = reg.Construct(ctx); err != nil {
		fmt.Println("construct:", err)
		return
	}
	if err = reg.Verify(ctx); err != nil {
		fmt.Println("verify:", err)
		return
	}
	if err = reg.Init(ctx); err != nil {
		fmt.Println("declare:", err)
		return
	}

	// Wire the queue to every job handler the assembly's Register calls
	// declared (the image-generate handler this module claims, plus
	// storage's own thumbnail-derive and expiry-sweep handlers) and start
	// it -- the identical call examples/reference-app/internal/app/server.go's
	// own wiring makes.
	if err = jobs.Wire(ctx, queue, reg.Jobs); err != nil {
		fmt.Println("wire queue:", err)
		return
	}
	if err = queue.Start(ctx); err != nil {
		fmt.Println("start queue:", err)
		return
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_ = queue.Close(shutdownCtx)
	}()

	// Store the platform-wide credential under the image provider's own
	// name -- a separate row from a chat credential, even for the same
	// vendor, since routing and credential resolution are both keyed by
	// Provider (see route.go). The module's component descriptor carries
	// the purpose and the assembly registers it, but this example drives
	// the modules' declaration turns without their descriptors;
	// registering it here keeps the example runnable on its own, and the
	// registration is idempotent.
	pkgcore.RegisterSystemPurpose(aigateway.SystemPurposeCredentialWrite)
	sysCtx, err := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   "example-bootstrap",
		Purpose: aigateway.SystemPurposeCredentialWrite,
		Ticket:  "EXAMPLE-2",
	})
	if err != nil {
		fmt.Println("with system context:", err)
		return
	}
	if err = module.Credentials().SetPlatformCredential(sysCtx, aigateway.ProviderOpenAICompatibleImage, "sk-example-image-key", imgSrv.URL); err != nil {
		fmt.Println("set platform credential:", err)
		return
	}

	// GenerateImage is async-only: it validates, checks entitlements (none
	// wired here, so always allowed) and resolves the route/credential once
	// to fail fast, then returns a jobs.JobID immediately -- it never calls
	// the provider or touches storage itself.
	tenantCtx := pkgcore.WithTenant(ctx, "tenant-acme")
	jobID, err := module.Gateway().GenerateImage(tenantCtx, aigateway.ImageRequest{
		Model:     "image:default",
		Operation: aigateway.ImageOperationTextToImage,
		Prompt:    "a bright, natural smile",
	})
	if err != nil {
		fmt.Println("generate image:", err)
		return
	}

	// A caller retrieves a completed image job's result purely through
	// go/jobs' own, already-shipped mechanism -- there is no second
	// job-status system here (image_gateway.go's own doc comment).
	job, err := waitForImageGenerateJob(tenantCtx, queue, jobID, time.Now().Add(5*time.Second))
	if err != nil {
		fmt.Println("get job:", err)
		return
	}
	fmt.Println("status:", job.Status)

	var result aigateway.ImageJobResult
	if err = json.Unmarshal(job.Result.Data, &result); err != nil {
		fmt.Println("decode result:", err)
		return
	}
	fmt.Println("image count:", result.Usage.ImageCount)
	fmt.Println("steps:", result.Usage.Steps)
	fmt.Println("resolution tier:", result.Usage.ResolutionTier)

	// ImageJobResult.OutputObjectID names a brand new go/storage object --
	// never a byte was passed back through the job result itself.
	output, err := storageModule.ObjectService().Get(tenantCtx, result.OutputObjectID)
	if err != nil {
		fmt.Println("get output object:", err)
		return
	}
	fmt.Println("output state:", output.State)
	fmt.Println("output mime:", *output.MIME)

	// Output:
	// status: succeeded
	// image count: 1
	// steps: 4
	// resolution tier: 512x512
	// output state: completed
	// output mime: image/png
}
