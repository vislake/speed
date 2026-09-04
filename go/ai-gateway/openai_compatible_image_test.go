package aigateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
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
