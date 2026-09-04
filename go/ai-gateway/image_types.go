package aigateway

import "context"

// ImageOperation names which of the three image tasks
// docs/internal/08-ai-gateway.md's multi-modal-expansion section describes
// an ImageRequest performs: text-to-image, image-to-image, or image
// editing/inpainting (an input image plus a mask plus a prompt).
type ImageOperation string

const (
	// ImageOperationTextToImage generates a new image from Prompt alone.
	// ImageRequest.InputObjectID and MaskObjectID must both be empty.
	ImageOperationTextToImage ImageOperation = "text_to_image"

	// ImageOperationImageToImage transforms InputObjectID's image according
	// to Prompt. ImageRequest.InputObjectID is required; MaskObjectID must
	// be empty.
	ImageOperationImageToImage ImageOperation = "image_to_image"

	// ImageOperationInpaint edits InputObjectID's image inside the region
	// MaskObjectID marks, according to Prompt. Both ImageRequest.
	// InputObjectID and MaskObjectID are required.
	ImageOperationInpaint ImageOperation = "inpaint"
)

// ImageRequest is Gateway.GenerateImage's input -- the caller-facing,
// async-only image-generation request.
//
// Unlike ChatRequest, whose Messages carry conversation content directly,
// ImageRequest never carries raw image bytes: per the design doc's rule
// that input and output images travel through storage uniformly, with the
// interface passing object references rather than byte streams,
// InputObjectID and MaskObjectID name go/storage objects the caller already
// uploaded and
// completed, and the new image this request produces lands as a brand new
// go/storage object of its own -- see image_gateway.go's own doc comment
// for exactly where the object-reference boundary sits relative to
// ImageProvider's own byte-based methods, and why.
type ImageRequest struct {
	// Model is the logical model key at the Gateway boundary, exactly like
	// ChatRequest.Model -- resolved through the same WithModelRoute
	// mechanism (a route's Provider names an ImageProviderRegistry entry,
	// never a ChatProviderRegistry one) and the same CredentialService
	// table, keyed by that Provider name.
	Model string

	// Operation selects which of the three image tasks to run; see each
	// ImageOperation constant's own doc comment for which of InputObjectID/
	// MaskObjectID it requires.
	Operation ImageOperation

	// Prompt is the natural-language instruction every operation requires,
	// non-empty.
	Prompt string

	// InputObjectID is the go/storage object id of the source image, for
	// ImageOperationImageToImage and ImageOperationInpaint. It must be
	// empty for ImageOperationTextToImage.
	InputObjectID string

	// MaskObjectID is the go/storage object id of the edit mask, for
	// ImageOperationInpaint only. It must be empty for every other
	// operation.
	MaskObjectID string

	// Params carries vendor-specific arguments (size, quality, style, and
	// so on) passed through to the provider's wire request verbatim,
	// mirroring ChatRequest.Params exactly -- the design doc's identical
	// "avoid an over-rigid abstraction" rule applies to images too.
	Params map[string]any
}

// validate checks the fields Gateway.GenerateImage requires before any
// entitlement check, routing or job is attempted.
func (r ImageRequest) validate() error {
	if r.Model == "" {
		return ErrEmptyModel
	}
	if r.Prompt == "" {
		return ErrEmptyPrompt.WithParam("model", r.Model)
	}
	switch r.Operation {
	case ImageOperationTextToImage:
		if r.InputObjectID != "" || r.MaskObjectID != "" {
			return ErrImageInputNotAllowed.WithParam("operation", string(r.Operation))
		}
	case ImageOperationImageToImage:
		if r.InputObjectID == "" {
			return ErrImageInputRequired.WithParam("operation", string(r.Operation))
		}
		if r.MaskObjectID != "" {
			return ErrImageInputNotAllowed.WithParam("operation", string(r.Operation)).WithParam("field", "mask_object_id")
		}
	case ImageOperationInpaint:
		if r.InputObjectID == "" {
			return ErrImageInputRequired.WithParam("operation", string(r.Operation))
		}
		if r.MaskObjectID == "" {
			return ErrImageMaskRequired.WithParam("operation", string(r.Operation))
		}
	default:
		return ErrInvalidImageOperation.WithParam("operation", string(r.Operation))
	}
	return nil
}

// ImageUsage is the vendor billing dimensions for one image-generation
// call, per the design doc's explicit rule that image metering reports
// image count, diffusion steps and resolution tier, rather than tokens.
type ImageUsage struct {
	// ImageCount is how many images the vendor actually generated and
	// returned for this call.
	ImageCount int
	// Steps is the number of diffusion steps the vendor reported, 0 when
	// the vendor did not report one (not every vendor exposes step count).
	Steps int
	// ResolutionTier is the vendor's own resolution/size label for the
	// generated image (for example "1024x1024" or "hd"), empty when the
	// vendor did not report one.
	ResolutionTier string
}

// ImageBytes is the raw-bytes shape ImageProvider's three methods trade in
// -- the vendor-transport layer, playing the same role for ImageProvider
// that ChatMessage plays for ChatProvider. See image_gateway.go's own doc
// comment for why ImageProvider itself is byte-based while the surrounding
// Gateway/job-handler layer is where the design doc's object-reference rule
// is actually enforced.
type ImageBytes struct {
	// Content is the raw, undecoded image bytes.
	Content []byte
	// MIME is the media type of Content (for example "image/png"). Never
	// empty on a value ImageProvider returns; may be empty on a value a
	// caller builds for a vendor able to infer type from content alone
	// (none of this package's own callers do this -- the job handler
	// always sets it from the source object's own finalized MIME).
	MIME string
}

// ImageResult is what one ImageProvider method returns: the generated
// image's raw bytes plus the real vendor usage that call incurred.
type ImageResult struct {
	Image ImageBytes
	Usage ImageUsage
}

// TextToImageRequest is ImageProvider.TextToImage's input. Model is already
// the concrete vendor model id by the time a provider sees it -- the
// Gateway boundary's logical-to-vendor translation has already happened,
// exactly as ChatRequest.Model's own doc comment describes for chat.
type TextToImageRequest struct {
	Model  string
	Prompt string
	Params map[string]any
}

// ImageToImageRequest is ImageProvider.ImageToImage's input.
type ImageToImageRequest struct {
	Model  string
	Prompt string
	Input  ImageBytes
	Params map[string]any
}

// InpaintRequest is ImageProvider.Inpaint's input.
type InpaintRequest struct {
	Model  string
	Prompt string
	Input  ImageBytes
	Mask   ImageBytes
	Params map[string]any
}

// ImageProvider is the vendor-agnostic image abstraction every image
// integration implements, the multi-modal counterpart of ChatProvider.
// Business code never calls an ImageProvider directly -- it calls
// Gateway.GenerateImage, and the job handler Module.Register registers
// resolves and calls the provider on its behalf; see image_gateway.go for
// the full pipeline and its object-reference/raw-bytes boundary.
//
// Each method is a distinct operation rather than one method branching on
// ImageOperation, mirroring the design doc's own three-operation framing
// and ChatProvider's Chat/ChatStream split by call shape.
type ImageProvider interface {
	// TextToImage generates a new image from req.Prompt alone.
	TextToImage(ctx context.Context, req TextToImageRequest) (ImageResult, error)

	// ImageToImage transforms req.Input according to req.Prompt.
	ImageToImage(ctx context.Context, req ImageToImageRequest) (ImageResult, error)

	// Inpaint edits req.Input inside the region req.Mask marks, according
	// to req.Prompt.
	Inpaint(ctx context.Context, req InpaintRequest) (ImageResult, error)
}
