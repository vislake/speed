package aigateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// tinyPNG is a minimal, valid 1x1 PNG -- real bytes, not a placeholder --
// so http.DetectContentType genuinely reports "image/png" for it, the same
// way a real vendor's response bytes would be probed.
var tinyPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
	0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89,
	0x00, 0x00, 0x00, 0x0A, 'I', 'D', 'A', 'T',
	0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
	0x0D, 0x0A, 0x2D, 0xB4,
	0x00, 0x00, 0x00, 0x00, 'I', 'E', 'N', 'D', 0xAE, 0x42, 0x60, 0x82,
}

// --- TextToImage (JSON) -----------------------------------------------------

func TestOpenAICompatibleImageProvider_TextToImage_Success(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG)}},
			"usage": map[string]any{
				"image_count": 1,
				"steps":       25,
				"size":        "512x512",
			},
		})
	}))
	defer srv.Close()

	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	result, err := p.TextToImage(context.Background(), TextToImageRequest{
		Model:  "dall-e-3",
		Prompt: "a bright smile",
		Params: map[string]any{"quality": "hd"},
	})
	if err != nil {
		t.Fatalf("TextToImage: %v", err)
	}
	if result.Image.MIME != "image/png" {
		t.Fatalf("Image.MIME = %q, want image/png", result.Image.MIME)
	}
	if string(result.Image.Content) != string(tinyPNG) {
		t.Fatalf("Image.Content does not match the fake server's own bytes")
	}
	if result.Usage != (ImageUsage{ImageCount: 1, Steps: 25, ResolutionTier: "512x512"}) {
		t.Fatalf("Usage = %+v", result.Usage)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer sk-test")
	}
	if gotPath != imagesGenerationsPath {
		t.Fatalf("request path = %q, want %q", gotPath, imagesGenerationsPath)
	}
	if got, _ := gotBody["model"].(string); got != "dall-e-3" {
		t.Fatalf("wire model = %q, want %q", got, "dall-e-3")
	}
	if got, _ := gotBody["prompt"].(string); got != "a bright smile" {
		t.Fatalf("wire prompt = %q, want %q", got, "a bright smile")
	}
	if got, _ := gotBody["quality"].(string); got != "hd" {
		t.Fatalf("Params passthrough quality = %v, want %q", gotBody["quality"], "hd")
	}
}

func TestOpenAICompatibleImageProvider_TextToImage_ParamsCannotOverrideModelOrPrompt(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG)}},
		})
	}))
	defer srv.Close()

	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	_, err := p.TextToImage(context.Background(), TextToImageRequest{
		Model:  "dall-e-3",
		Prompt: "a bright smile",
		Params: map[string]any{"model": "smuggled-model", "prompt": "smuggled prompt"},
	})
	if err != nil {
		t.Fatalf("TextToImage: %v", err)
	}
	if got, _ := gotBody["model"].(string); got != "dall-e-3" {
		t.Fatalf("wire model = %q, want the real model %q, Params must not override it", got, "dall-e-3")
	}
	if got, _ := gotBody["prompt"].(string); got != "a bright smile" {
		t.Fatalf("wire prompt = %q, want the real prompt, Params must not override it", got)
	}
}

func TestOpenAICompatibleImageProvider_TextToImage_NonOKStatus_ProviderRequestFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	_, err := p.TextToImage(context.Background(), TextToImageRequest{Model: "dall-e-3", Prompt: "x"})
	if got, ok := apperrCode(err); !ok || got != ErrProviderRequestFailed.Code {
		t.Fatalf("TextToImage err = %v, want ErrProviderRequestFailed", err)
	}
}

// TestOpenAICompatibleImageProvider_TextToImage_NonOKStatus_EnvelopeFieldsOnlyInLog
// is the image-side mirror of the chat raw-text-sink regression
// (TestOpenAICompatibleProvider_Chat_NonOKStatus_EnvelopeFieldsOnlyInLog):
// the image provider funnels its non-2xx answers through the same
// errorFromResponse, so the envelope's structured enumeration fields must
// be what reaches the log -- never the raw body, whichever of the two
// provider families made the call.
func TestOpenAICompatibleImageProvider_TextToImage_NonOKStatus_EnvelopeFieldsOnlyInLog(t *testing.T) {
	const echoedFragment = "quetzal-coastline-9b2d1"
	const errorBody = `{"error":{"message":"refused: your prompt contained ` + echoedFragment + `","type":"invalid_request_error","code":"content_policy_violation"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorBody))
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	ctx := obs.WithLogger(context.Background(), logger)

	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	_, err := p.TextToImage(ctx, TextToImageRequest{Model: "dall-e-3", Prompt: "a portrait of " + echoedFragment})
	if !apperr.HasCode(err, ErrProviderRequestFailed.Code) {
		t.Fatalf("TextToImage err = %v, want ErrProviderRequestFailed", err)
	}
	appErr, _ := apperr.As(err)
	if got, present := appErr.Params["body"]; present {
		t.Fatalf("error params carry the dialed endpoint's response body %q -- the body must not be handed back to the caller who steered the dial", got)
	}
	if appErr.Params["status"] != http.StatusBadRequest {
		t.Fatalf("status param = %v, want %d", appErr.Params["status"], http.StatusBadRequest)
	}
	logged := logBuf.String()
	if strings.Contains(logged, echoedFragment) {
		t.Fatalf("server-side log carries %q -- the prompt fragment the provider echoed back must not reach the log; log = %q", echoedFragment, logged)
	}
	if !strings.Contains(logged, "error_type=invalid_request_error") || !strings.Contains(logged, "error_code=content_policy_violation") {
		t.Fatalf("server-side log lacks the envelope's parsed error_type/error_code; log = %q", logged)
	}
}

func TestOpenAICompatibleImageProvider_TextToImage_EmptyData_ProviderResponseInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{}})
	}))
	defer srv.Close()

	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	_, err := p.TextToImage(context.Background(), TextToImageRequest{Model: "dall-e-3", Prompt: "x"})
	if got, ok := apperrCode(err); !ok || got != ErrProviderResponseInvalid.Code {
		t.Fatalf("TextToImage err = %v, want ErrProviderResponseInvalid", err)
	}
}

// TestOpenAICompatibleImageProvider_TextToImage_MultipleDataEntries_Refused
// pins the single-image boundary of this provider's decode path: the whole
// pipeline (the Gateway job handler through ImageJobResult) carries exactly
// one output object id, so a response whose data array holds more than one
// image -- exactly what a request smuggling "n": 4 into Params produces
// from a conforming vendor -- must be refused with a coded error BEFORE any
// usage could be recorded, never silently decoded to the first image while
// the response's own image_count bills for all of them ("charge 4, return
// 1").
func TestOpenAICompatibleImageProvider_TextToImage_MultipleDataEntries_Refused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{
				{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG)},
				{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG)},
			},
			"usage": map[string]any{"image_count": 4, "steps": 20},
		})
	}))
	defer srv.Close()

	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	_, err := p.TextToImage(context.Background(), TextToImageRequest{
		Model:  "dall-e-3",
		Prompt: "a bright smile",
		Params: map[string]any{"n": 4},
	})
	if got, ok := apperrCode(err); !ok || got != ErrMultipleImageResults.Code {
		t.Fatalf("TextToImage err = %v, want the coded ErrMultipleImageResults -- n=4 must never charge 4 and silently return 1", err)
	}
}

// TestOpenAICompatibleImageProvider_TextToImage_UsageClaimsMoreImagesThanDelivered_Refused
// closes the same overcharge shape through its other door: a response
// delivering a single data entry whose usage object claims an image_count
// above one. Trusting that claim would bill the tenant for images this
// provider never carries, so it is refused with the same coded error as a
// multi-entry data array.
func TestOpenAICompatibleImageProvider_TextToImage_UsageClaimsMoreImagesThanDelivered_Refused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG)}},
			"usage": map[string]any{
				"image_count": 4,
				"steps":       20,
			},
		})
	}))
	defer srv.Close()

	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	_, err := p.TextToImage(context.Background(), TextToImageRequest{Model: "dall-e-3", Prompt: "x"})
	if got, ok := apperrCode(err); !ok || got != ErrMultipleImageResults.Code {
		t.Fatalf("TextToImage err = %v, want the coded ErrMultipleImageResults for a usage claim above the delivered image count", err)
	}
}

// TestOpenAICompatibleImageProvider_TextToImage_RequestsB64JSONEncoding pins
// the request half of this provider's single-encoding contract: every
// images/generations call must explicitly ask for response_format
// "b64_json", because some vendors default response_format to "url" -- and a
// url answer would require this provider to perform a second,
// unauthenticated fetch of a vendor-hosted URL, which it never does. A
// Params entry smuggling any other response_format must never reach the
// wire: the provider's own forced value wins over the merge, exactly the
// model/prompt-wins rule and the chat side's stream_options precedent.
func TestOpenAICompatibleImageProvider_TextToImage_RequestsB64JSONEncoding(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG)}},
		})
	}))
	defer srv.Close()

	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	_, err := p.TextToImage(context.Background(), TextToImageRequest{
		Model:  "dall-e-3",
		Prompt: "a bright smile",
		// A smuggled response_format is exactly the shape that would defeat
		// the provider's request-side guarantee if it won the merge.
		Params: map[string]any{"response_format": "url"},
	})
	if err != nil {
		t.Fatalf("TextToImage: %v", err)
	}
	// The literal "b64_json" is asserted, not the source constant, so the
	// same test body also runs against a provider whose request carries no
	// response_format at all -- and fails there, proving the explicit
	// request is what makes it pass.
	const wantEncoding = "b64_json"
	if got, _ := gotBody["response_format"].(string); got != wantEncoding {
		t.Fatalf("wire response_format = %v, want %q -- the provider must request b64_json explicitly, and a Params entry must not override it", gotBody["response_format"], wantEncoding)
	}
}

// TestOpenAICompatibleImageProvider_ImageEdit_RequestsB64JSONEncoding is the
// multipart twin of the JSON request-encoding test above: every
// images/edits call must carry exactly one response_format field naming
// b64_json, and a Params entry smuggling the name must never reach the wire
// as a second, duplicate form field (which duplicate a multipart parser
// honors is parser-defined -- the same reasoning model/prompt-wins applies).
func TestOpenAICompatibleImageProvider_ImageEdit_RequestsB64JSONEncoding(t *testing.T) {
	var gotForm *multipart.Form
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForm = decodeMultipartRequest(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG)}},
		})
	}))
	defer srv.Close()

	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	_, err := p.ImageToImage(context.Background(), ImageToImageRequest{
		Model:  "dall-e-3",
		Prompt: "simulate a smile",
		Input:  ImageBytes{Content: []byte("input-photo-bytes"), MIME: "image/jpeg"},
		Params: map[string]any{"response_format": "url"},
	})
	if err != nil {
		t.Fatalf("ImageToImage: %v", err)
	}
	// The literal "b64_json", for the same cross-version-runnable reason the
	// JSON path's twin test states.
	const wantEncoding = "b64_json"
	if got := gotForm.Value["response_format"]; len(got) != 1 || got[0] != wantEncoding {
		t.Fatalf("wire response_format fields = %v, want exactly one naming %q -- a Params response_format entry must never reach the wire", got, wantEncoding)
	}
}

// TestOpenAICompatibleImageProvider_TextToImage_EmptyImageData_Refused pins
// the decode half of the same contract through the real call: a vendor that
// ignores the requested response_format and answers with a "url" entry --
// or an empty b64_json value -- delivers zero decodable bytes, and this
// provider refuses that rather than reporting a zero-byte successful image
// with ImageCount 1: such an answer fails the call with the coded
// ErrProviderResponseInvalid, never a zero-byte success.
func TestOpenAICompatibleImageProvider_TextToImage_EmptyImageData_Refused(t *testing.T) {
	// A url-shaped answer: the data entry carries only "url", never
	// b64_json -- exactly what a vendor defaulting response_format to "url"
	// produces when the provider's request (if any) is ignored.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"url": "https://vendor.example/images/0001.png"}},
			"usage": map[string]any{
				"image_count": 1,
				"steps":       25,
			},
		})
	}))
	defer srv.Close()

	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	result, err := p.TextToImage(context.Background(), TextToImageRequest{Model: "dall-e-3", Prompt: "x"})
	if err == nil {
		t.Fatalf("TextToImage returned a success with %d image bytes -- a url/empty answer must be refused, never a zero-byte successful image", len(result.Image.Content))
	}
	if got, ok := apperrCode(err); !ok || got != ErrProviderResponseInvalid.Code {
		t.Fatalf("TextToImage err = %v, want the coded ErrProviderResponseInvalid", err)
	}
}

// TestImageResultFromWire_EmptyB64JSON_Refused pins the same empty-result
// refusal at the decode level, where both endpoints (generation and edit)
// converge: an entry whose b64_json is the empty string decodes to zero
// bytes with a nil error -- the exact shape of a would-be successful
// zero-byte image.
func TestImageResultFromWire_EmptyB64JSON_Refused(t *testing.T) {
	_, err := imageResultFromWire(openaiImageResponseWire{
		Data: []openaiImageDataWire{{B64JSON: ""}},
	})
	if err == nil {
		t.Fatal("imageResultFromWire accepted an empty b64_json entry as a successful image, want a refusal")
	}
	if got, ok := apperrCode(err); !ok || got != ErrProviderResponseInvalid.Code {
		t.Fatalf("imageResultFromWire err = %v, want the coded ErrProviderResponseInvalid", err)
	}
}

// --- ImageToImage / Inpaint (multipart) -------------------------------------

// decodeMultipartRequest parses r's multipart/form-data body, returning the
// form and requiring the parse to succeed.
func decodeMultipartRequest(t *testing.T, r *http.Request) *multipart.Form {
	t.Helper()
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse content-type: %v", err)
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	form, err := mr.ReadForm(1 << 20)
	if err != nil {
		t.Fatalf("read multipart form: %v", err)
	}
	return form
}

// readFilePart reads one named multipart file part's bytes.
func readFilePart(t *testing.T, form *multipart.Form, field string) []byte {
	t.Helper()
	files := form.File[field]
	if len(files) == 0 {
		t.Fatalf("no %q file part in the request", field)
	}
	f, err := files[0].Open()
	if err != nil {
		t.Fatalf("open %q file part: %v", field, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %q file part: %v", field, err)
	}
	return raw
}

func TestOpenAICompatibleImageProvider_ImageToImage_SendsMultipartWithNoMask(t *testing.T) {
	var gotPath string
	var gotForm *multipart.Form
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotForm = decodeMultipartRequest(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG)}},
			"usage": map[string]any{
				"image_count": 1,
			},
		})
	}))
	defer srv.Close()

	input := ImageBytes{Content: []byte("input-photo-bytes"), MIME: "image/jpeg"}
	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	result, err := p.ImageToImage(context.Background(), ImageToImageRequest{
		Model:  "dall-e-3",
		Prompt: "simulate a smile",
		Input:  input,
		Params: map[string]any{"size": "1024x1024"},
	})
	if err != nil {
		t.Fatalf("ImageToImage: %v", err)
	}
	if result.Image.MIME != "image/png" {
		t.Fatalf("Image.MIME = %q, want image/png", result.Image.MIME)
	}
	if gotPath != imagesEditsPath {
		t.Fatalf("request path = %q, want %q", gotPath, imagesEditsPath)
	}
	if got := gotForm.Value["model"]; len(got) == 0 || got[0] != "dall-e-3" {
		t.Fatalf("wire model = %v, want %q", got, "dall-e-3")
	}
	if got := gotForm.Value["prompt"]; len(got) == 0 || got[0] != "simulate a smile" {
		t.Fatalf("wire prompt = %v, want %q", got, "simulate a smile")
	}
	if got := gotForm.Value["size"]; len(got) == 0 || got[0] != "1024x1024" {
		t.Fatalf("Params passthrough size = %v, want %q", got, "1024x1024")
	}
	if got := readFilePart(t, gotForm, "image"); string(got) != string(input.Content) {
		t.Fatalf("wire image part = %q, want %q", got, input.Content)
	}
	if len(gotForm.File["mask"]) != 0 {
		t.Fatal("ImageToImage sent a mask part, want none")
	}
}

func TestOpenAICompatibleImageProvider_Inpaint_SendsMultipartWithMask(t *testing.T) {
	var gotForm *multipart.Form
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForm = decodeMultipartRequest(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG)}},
		})
	}))
	defer srv.Close()

	input := ImageBytes{Content: []byte("input-photo-bytes"), MIME: "image/png"}
	mask := ImageBytes{Content: []byte("mask-bytes"), MIME: "image/png"}
	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	_, err := p.Inpaint(context.Background(), InpaintRequest{
		Model:  "dall-e-3",
		Prompt: "fix the teeth",
		Input:  input,
		Mask:   mask,
	})
	if err != nil {
		t.Fatalf("Inpaint: %v", err)
	}
	if got := readFilePart(t, gotForm, "image"); string(got) != string(input.Content) {
		t.Fatalf("wire image part = %q, want %q", got, input.Content)
	}
	if got := readFilePart(t, gotForm, "mask"); string(got) != string(mask.Content) {
		t.Fatalf("wire mask part = %q, want %q", got, mask.Content)
	}
}

// TestOpenAICompatibleImageProvider_ImageEdit_ParamsCannotOverrideModelOrPrompt
// pins the multipart half of the model/prompt-wins invariant the JSON
// path's buildImageGenerationBody already enforces: a Params entry smuggled
// under the name "model" or "prompt" must not reach the wire as a second,
// duplicate form field -- which multipart parser wins a duplicate is
// parser-defined, so a duplicate could genuinely override the routed model
// on some vendor -- and buildImageEditMultipart drops the two names from
// the Params passthrough loop instead.
func TestOpenAICompatibleImageProvider_ImageEdit_ParamsCannotOverrideModelOrPrompt(t *testing.T) {
	var gotForm *multipart.Form
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForm = decodeMultipartRequest(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG)}},
		})
	}))
	defer srv.Close()

	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	_, err := p.ImageToImage(context.Background(), ImageToImageRequest{
		Model:  "dall-e-3",
		Prompt: "simulate a smile",
		Input:  ImageBytes{Content: []byte("input-photo-bytes"), MIME: "image/jpeg"},
		Params: map[string]any{"model": "smuggled-model", "prompt": "smuggled prompt"},
	})
	if err != nil {
		t.Fatalf("ImageToImage: %v", err)
	}
	if got := gotForm.Value["model"]; len(got) != 1 || got[0] != "dall-e-3" {
		t.Fatalf("wire model fields = %v, want exactly one, the routed vendor model %q -- a Params model entry must never reach the wire", got, "dall-e-3")
	}
	if got := gotForm.Value["prompt"]; len(got) != 1 || got[0] != "simulate a smile" {
		t.Fatalf("wire prompt fields = %v, want exactly one, the routed prompt -- a Params prompt entry must never reach the wire", got)
	}
}

// TestOpenAICompatibleImageProvider_ImageEdit_FilePartsCarryRealContentType
// pins that a multipart image-edit request's file parts declare the image's
// actual media type rather than mime/multipart.CreateFormFile's hardcoded
// application/octet-stream: an OpenAI-compatible image-edit host may key
// part acceptance off the Content-Type header, and this gateway always
// knows the real MIME (the job handler sets it from the source object's own
// finalized MIME).
func TestOpenAICompatibleImageProvider_ImageEdit_FilePartsCarryRealContentType(t *testing.T) {
	input := ImageBytes{Content: []byte("input-photo-bytes"), MIME: "image/jpeg"}
	mask := ImageBytes{Content: []byte("mask-bytes"), MIME: "image/png"}
	var gotForm *multipart.Form
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForm = decodeMultipartRequest(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG)}},
		})
	}))
	defer srv.Close()

	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	if _, err := p.Inpaint(context.Background(), InpaintRequest{
		Model:  "dall-e-3",
		Prompt: "fix the teeth",
		Input:  input,
		Mask:   mask,
	}); err != nil {
		t.Fatalf("Inpaint: %v", err)
	}
	imagePart := gotForm.File["image"][0]
	if got := imagePart.Header.Get("Content-Type"); got != input.MIME {
		t.Fatalf("image part Content-Type = %q, want the real media type %q", got, input.MIME)
	}
	maskPart := gotForm.File["mask"][0]
	if got := maskPart.Header.Get("Content-Type"); got != mask.MIME {
		t.Fatalf("mask part Content-Type = %q, want the real media type %q", got, mask.MIME)
	}
}

func TestOpenAICompatibleImageProvider_ImageEdit_NonOKStatus_ProviderRequestFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	p := NewOpenAICompatibleImageProvider(srv.URL, "sk-test")
	_, err := p.ImageToImage(context.Background(), ImageToImageRequest{
		Model: "dall-e-3", Prompt: "x", Input: ImageBytes{Content: []byte("x"), MIME: "image/png"},
	})
	if got, ok := apperrCode(err); !ok || got != ErrProviderRequestFailed.Code {
		t.Fatalf("ImageToImage err = %v, want ErrProviderRequestFailed", err)
	}
}
