package aigateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
)

// imagesGenerationsPath is the OpenAI-compatible text-to-image endpoint
// path, appended to a provider's configured base URL -- the real OpenAI
// "/v1/images/generations" wire shape.
const imagesGenerationsPath = "/images/generations"

// imagesEditsPath is the OpenAI-compatible image-edit endpoint path, used
// for both ImageToImage and Inpaint: the real OpenAI "/v1/images/edits"
// endpoint accepts an input image plus a prompt, with an optional mask --
// present for Inpaint, absent for ImageToImage -- exactly the shape this
// provider builds.
const imagesEditsPath = "/images/edits"

// OpenAICompatibleImageProvider implements ImageProvider against the
// images-generation/images-edits REST schema shared by OpenAI itself and
// OpenAI-compatible image hosts: JSON for text-to-image, multipart/
// form-data for image-to-image and inpainting (an image file, an optional
// mask file, and the prompt/model as form fields) -- both posted to
// baseURL with a Bearer Authorization header.
//
// Like OpenAICompatibleProvider it is implemented directly against the
// wire schema with stdlib net/http, encoding/json and mime/multipart only
// -- no vendor SDK -- keeping this module's default image path, like its
// chat path, at zero third-party dependency.
//
// The zero value is not ready to use; construct one with
// NewOpenAICompatibleImageProvider.
type OpenAICompatibleImageProvider struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// OpenAICompatibleImageOption configures an OpenAICompatibleImageProvider
// at construction time.
type OpenAICompatibleImageOption func(*OpenAICompatibleImageProvider)

// WithImageHTTPClient overrides the *http.Client the provider issues
// requests with (default: a client with no timeout of its own, relying on
// ctx and defaultHTTPTimeout). Tests use this to point the provider at an
// httptest.Server, mirroring WithHTTPClient's identical role for the chat
// provider.
func WithImageHTTPClient(client *http.Client) OpenAICompatibleImageOption {
	return func(p *OpenAICompatibleImageProvider) {
		if client != nil {
			p.httpClient = client
		}
	}
}

// NewOpenAICompatibleImageProvider returns an ImageProvider that calls
// baseURL with apiKey as a Bearer credential. baseURL must not include a
// trailing slash requirement of its own -- imagesGenerationsPath and
// imagesEditsPath are appended directly.
func NewOpenAICompatibleImageProvider(baseURL, apiKey string, opts ...OpenAICompatibleImageOption) *OpenAICompatibleImageProvider {
	p := &OpenAICompatibleImageProvider{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: &http.Client{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// compile-time check that *OpenAICompatibleImageProvider satisfies
// ImageProvider.
var _ ImageProvider = (*OpenAICompatibleImageProvider)(nil)

// openaiImageDataWire is one generated image inside a
// images-generations/images-edits response's "data" array. b64_json is the
// only encoding this provider requests or accepts -- never a "url" entry,
// which would require this provider to perform a second, unauthenticated
// fetch of a vendor-hosted URL.
type openaiImageDataWire struct {
	B64JSON string `json:"b64_json"`
}

// openaiImageUsageWire is the billing-dimension shape this provider reads
// from a response's "usage" object -- image count, diffusion steps and the
// vendor's own resolution/size label, per the design doc's rule that image
// metering is not token-based. A response carrying no "usage" object at
// all (some vendors omit it) leaves Usage nil; imageResultFromWire falls
// back to the real delivered image count in that case.
type openaiImageUsageWire struct {
	ImageCount int    `json:"image_count"`
	Steps      int    `json:"steps"`
	Size       string `json:"size"`
}

// openaiImageResponseWire is the response body shape shared by both
// endpoints.
type openaiImageResponseWire struct {
	Data  []openaiImageDataWire `json:"data"`
	Usage *openaiImageUsageWire `json:"usage"`
}

// imageResultFromWire decodes wire into an ImageResult: the first (and, for
// this provider's own requests, only) image's bytes, base64-decoded, with
// its MIME type detected from the decoded bytes themselves via
// http.DetectContentType rather than trusted from any vendor-supplied
// field -- the same "probe, never trust a header" discipline go/storage's
// own revalidation pipeline applies to uploaded bytes.
func imageResultFromWire(wire openaiImageResponseWire) (ImageResult, error) {
	if len(wire.Data) == 0 {
		return ImageResult{}, ErrProviderResponseInvalid.WithParam("reason", "no image data in response")
	}
	raw, err := base64.StdEncoding.DecodeString(wire.Data[0].B64JSON)
	if err != nil {
		return ImageResult{}, ErrProviderResponseInvalid.WithCause(err)
	}
	mime := http.DetectContentType(raw)

	usage := ImageUsage{ImageCount: len(wire.Data)}
	if wire.Usage != nil {
		if wire.Usage.ImageCount > 0 {
			usage.ImageCount = wire.Usage.ImageCount
		}
		usage.Steps = wire.Usage.Steps
		usage.ResolutionTier = wire.Usage.Size
	}
	return ImageResult{Image: ImageBytes{Content: raw, MIME: mime}, Usage: usage}, nil
}

// buildImageGenerationBody builds the JSON wire body for a TextToImage
// call, merging req.Params in first so that model/prompt always win over a
// same-named Params entry -- the identical rule buildRequestBody enforces
// for chat.
func buildImageGenerationBody(req TextToImageRequest) ([]byte, error) {
	body := make(map[string]any, len(req.Params)+2)
	for k, v := range req.Params {
		body[k] = v
	}
	body["model"] = req.Model
	body["prompt"] = req.Prompt

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("aigateway: encode image generation request: %w", err)
	}
	return encoded, nil
}

// TextToImage implements ImageProvider.
func (p *OpenAICompatibleImageProvider) TextToImage(ctx context.Context, req TextToImageRequest) (ImageResult, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()

	body, err := buildImageGenerationBody(req)
	if err != nil {
		return ImageResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+imagesGenerationsPath, bytes.NewReader(body))
	if err != nil {
		return ImageResult{}, fmt.Errorf("aigateway: build image generation request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return ImageResult{}, ErrProviderRequestFailed.WithCause(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ImageResult{}, errorFromResponse(resp)
	}

	var wire openaiImageResponseWire
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return ImageResult{}, ErrProviderResponseInvalid.WithCause(err)
	}
	return imageResultFromWire(wire)
}

// extensionForMIME maps an image MIME type to the filename extension this
// provider uses for the multipart form file part -- cosmetic only, since
// this provider's own requests set the part's Content-Type explicitly, but
// still a real, media-type-appropriate name rather than a fixed
// placeholder.
func extensionForMIME(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	default:
		return ".bin"
	}
}

// formValue renders one Params entry as a multipart form field value:
// verbatim for a string, JSON-encoded for anything else. Vendor-specific
// passthrough parameters for a multipart request have no single wire
// convention the way a JSON body's native types do, so this is the
// provider's own choice, documented here rather than left implicit.
func formValue(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("aigateway: encode image edit param: %w", err)
	}
	return string(b), nil
}

// writeImagePart writes one image file part (field "image" or "mask") of a
// multipart image-edit request.
func writeImagePart(w *multipart.Writer, field string, img ImageBytes) error {
	part, err := w.CreateFormFile(field, field+extensionForMIME(img.MIME))
	if err != nil {
		return fmt.Errorf("aigateway: create %s form part: %w", field, err)
	}
	if _, err := part.Write(img.Content); err != nil {
		return fmt.Errorf("aigateway: write %s form part: %w", field, err)
	}
	return nil
}

// buildImageEditMultipart builds the multipart/form-data body for an
// image-edit call (ImageToImage or Inpaint): "model" and "prompt" fields,
// an "image" file part, an optional "mask" file part (Inpaint only, when
// mask is non-nil), and every Params entry as an additional form field.
// Returns the encoded body and its Content-Type header value (which
// carries the boundary multipart.Writer generated).
func buildImageEditMultipart(model, prompt string, input ImageBytes, mask *ImageBytes, params map[string]any) (*bytes.Buffer, string, error) {
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)

	if err := w.WriteField("model", model); err != nil {
		return nil, "", fmt.Errorf("aigateway: write model form field: %w", err)
	}
	if err := w.WriteField("prompt", prompt); err != nil {
		return nil, "", fmt.Errorf("aigateway: write prompt form field: %w", err)
	}
	if err := writeImagePart(w, "image", input); err != nil {
		return nil, "", err
	}
	if mask != nil {
		if err := writeImagePart(w, "mask", *mask); err != nil {
			return nil, "", err
		}
	}
	for k, v := range params {
		val, err := formValue(v)
		if err != nil {
			return nil, "", err
		}
		if err := w.WriteField(k, val); err != nil {
			return nil, "", fmt.Errorf("aigateway: write %s form field: %w", k, err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("aigateway: close multipart writer: %w", err)
	}
	return buf, w.FormDataContentType(), nil
}

// editRequest runs one multipart image-edit call shared by ImageToImage
// and Inpaint -- mask is nil for the former, non-nil for the latter.
func (p *OpenAICompatibleImageProvider) editRequest(ctx context.Context, model, prompt string, input ImageBytes, mask *ImageBytes, params map[string]any) (ImageResult, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()

	body, contentType, err := buildImageEditMultipart(model, prompt, input, mask, params)
	if err != nil {
		return ImageResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+imagesEditsPath, body)
	if err != nil {
		return ImageResult{}, fmt.Errorf("aigateway: build image edit request: %w", err)
	}
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return ImageResult{}, ErrProviderRequestFailed.WithCause(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ImageResult{}, errorFromResponse(resp)
	}

	var wire openaiImageResponseWire
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return ImageResult{}, ErrProviderResponseInvalid.WithCause(err)
	}
	return imageResultFromWire(wire)
}

// ImageToImage implements ImageProvider.
func (p *OpenAICompatibleImageProvider) ImageToImage(ctx context.Context, req ImageToImageRequest) (ImageResult, error) {
	return p.editRequest(ctx, req.Model, req.Prompt, req.Input, nil, req.Params)
}

// Inpaint implements ImageProvider.
func (p *OpenAICompatibleImageProvider) Inpaint(ctx context.Context, req InpaintRequest) (ImageResult, error) {
	mask := req.Mask
	return p.editRequest(ctx, req.Model, req.Prompt, req.Input, &mask, req.Params)
}
