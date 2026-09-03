package storage

// DeriveService turns a completed image object's stored bytes into its
// thumbnail derivative, the module's first -- and so far only -- consumer of
// the ObjectDerivative rows and derivative-key grammar the delete protocol
// round already wired.
//
// A derive reads the object's sanitized original from the store (the same
// bytes OpenContent serves), downscales it with a pure-stdlib exact area
// average, encodes the result in the source's own format (JPEG at a fixed
// quality, PNG lossless), puts the bytes under the derivative key and only
// then inserts the derivative row -- row-last, so a crash between the two
// leaves nothing but re-derivable bytes, never a row that points at missing
// content. Re-running a finished derive is a no-op (the row's existence is
// checked before any work, and the repository's insert-if-absent closes the
// same race at the write itself), and a derive of something that has nothing
// to derive from -- an object that is gone, not completed, not an image, or
// of a media type this service has no encoder for -- is a skip, not an error:
// a job that converges on nothing to do must complete cleanly, or the queue
// would re-run it into a dead letter. Only genuine failures (store errors,
// undecodable content, an over-limit pixel count) are errors, and those are
// exactly what the jobs layer's retry policy exists for.
//
// Deriving is the delete protocol's race partner, and the derivative row's
// insert is where the two converge: the object row this service read may be
// marked deleting and removed while the bytes above are being produced, so
// the insert is gated -- in the same transaction as the row write itself --
// on the object's own row still existing and reading completed at the
// moment of the insert (repository.go's insertDerivativeIfAbsent). A delete
// that removes the object first wins the gate: the insert is refused, and
// this service drops the bytes it just wrote (best effort) and converges on
// nil, never leaving a derivative row no object row will ever walk, or
// bytes nothing references. A delete that races the gate instead blocks on
// the object row the gate locked until this insert commits, and its own row
// removal -- deleteObjectRows deletes the object row first and the
// derivative rows last -- then removes the row that just landed. There is
// no separate post-write re-read to race anymore: the gate is atomic with
// the insert where a re-read was not, so no interleaving can slip a
// derivative row past a completed deletion. (The gate's row lock is a
// genuine FOR UPDATE on PostgreSQL; the SQLite driver has no row locks and
// the clause vanishes, SQLite's single-writer serialization answering a
// racing writer with a busy error the queue's retry converges.)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// thumbnailJPEGQuality is the fixed JPEG quality every JPEG thumbnail is
// encoded at. A thumbnail is a derived artifact, never a preservation copy:
// a uniform quality keeps every thumbnail's weight predictable, and 75 is
// the JPEG encoder's own suggestion for a good visual trade-off.
const thumbnailJPEGQuality = 75

// DeriveService derives and stores one object's thumbnail derivative. It
// reads the object row and bytes through the same repositories the transfer
// runtime uses -- one database, one store, one tenant at a time -- and is
// constructed by the Module next to ObjectService. Like ObjectService it is
// inert until Module.Register attaches the registry: until then requireStore
// fails closed with ErrStoreUnavailable and nothing is derived.
type DeriveService struct {
	serviceHost

	// objects is the metadata repository object lookups route through, the
	// same instance Module hands ObjectService.
	objects *ObjectRepository
	// derivatives is the repository the derived row lands in.
	derivatives *DerivativeRepository
	// maxEdge is the longer-edge bound resolved at construction;
	// defaultDerivativeMaxEdge when the host configured none.
	maxEdge int
	// maxPixels is the pixel ceiling the source image is decoded against,
	// the same ceiling the transfer pipeline enforces at completion. A
	// worker must not decode an image the transfer pipeline already refused
	// for size; the config probe below re-checks the stored bytes before
	// the full decode happens.
	maxPixels int64
}

// newDeriveService returns a DeriveService deriving over objects and
// derivatives. maxEdge is the longer-edge bound of derived thumbnails; a
// non-positive value resolves to defaultDerivativeMaxEdge. maxPixels must be
// the ceiling the host's transfer pipeline enforces (a positive value: the
// probe below refuses every image when it is zero).
func newDeriveService(objects *ObjectRepository, derivatives *DerivativeRepository, maxEdge int, maxPixels int64) *DeriveService {
	if maxEdge <= 0 {
		maxEdge = defaultDerivativeMaxEdge
	}
	return &DeriveService{
		objects:     objects,
		derivatives: derivatives,
		maxEdge:     maxEdge,
		maxPixels:   maxPixels,
	}
}

// DeriveThumbnail derives, encodes and stores the thumbnail of one completed
// image object of the caller's tenant, then records the derivative row. It
// is the derive worker's body and doubles as the module's service entry
// point for a caller that wants a thumbnail synchronously.
//
// The method converges on nothing to do, returning nil, when the object does
// not exist (the delete protocol finished it between the enqueue and this
// run), is not in the completed state (the same race, one step earlier), is
// not an image, or is of an image media type this service has no encoder
// for. It is idempotent: an object that already carries a thumbnail row is
// left untouched. Real failures -- the store refusing the read or the write,
// bytes that do not decode, a source over the pixel ceiling, a completed row
// whose bytes or finalized metadata are missing -- are errors the jobs layer
// retries.
//
// Skip decisions are logged, not silent, so a tenant that expected a
// thumbnail for every uploaded image can see why a particular object never
// got one.
func (s *DeriveService) DeriveThumbnail(ctx context.Context, objectID string) error {
	// The object row first, through the shared lookup: a row that never
	// existed, or one the delete protocol removed since the task was
	// enqueued, has nothing to derive and the run converges on nil -- the
	// queue must not re-run a task whose object is gone into a dead letter.
	row, err := findObjectByID(ctx, s.objects, objectID)
	if err != nil {
		if hasCode(err, ErrObjectNotFound.Code) {
			observability.FromContext(ctx).Info("thumbnail derive skipped",
				"object_id", objectID, "reason", "object not found")
			return nil
		}
		return err
	}
	if row.State != ObjectStateCompleted {
		// A deleting object is owned by the delete protocol, which removes
		// derivative rows itself; an uploading one was never finalized and
		// may still grow or vanish. Neither has a thumbnail to derive.
		observability.FromContext(ctx).Info("thumbnail derive skipped",
			"object_id", objectID, "reason", "object not completed")
		return nil
	}
	if row.MIME == nil || row.Size == nil {
		// Completed rows always carry the finalized metadata, written in the
		// same update that flipped the state. A completed row without it is
		// an internal invariant violation, not a content question.
		return ErrInternal.WithCause(errors.New("storage: completed object row lacks finalized metadata"))
	}
	mime := *row.MIME

	// The idempotent skip, checked before any work: a thumbnail that
	// already exists must not be decoded, downscaled and written again by a
	// re-run. Silent by design -- this is the ordinary shape of a retried or
	// re-driven derive, not an anomaly worth logging.
	rows, err := s.derivatives.listByObject(ctx, objectID)
	if err != nil {
		return err
	}
	for _, d := range rows {
		if d.Kind == DerivativeKindThumbnail {
			return nil
		}
	}

	if !isImageMediaType(mime) {
		observability.FromContext(ctx).Info("thumbnail derive skipped",
			"object_id", objectID, "reason", "object is not an image", "mime", mime)
		return nil
	}
	// The encoder switch runs before the store is even touched: a media
	// type this service cannot re-encode has no thumbnail regardless of the
	// bytes, so the skip must not depend on the store being reachable.
	switch mime {
	case "image/jpeg", "image/png":
	default:
		observability.FromContext(ctx).Info("thumbnail derive skipped",
			"object_id", objectID, "reason", "no encoder for the media type", "mime", mime)
		return nil
	}

	st, err := s.requireStore()
	if err != nil {
		return err
	}

	rc, err := st.GetObject(ctx, row.Key)
	if err != nil {
		// A completed row whose bytes the store cannot produce is the same
		// anomaly OpenContent reports: the row promises content the store no
		// longer holds. The delete protocol can cause this transiently --
		// its byte removal races this read -- and a retry converges once the
		// object's rows are gone.
		return ErrStoreError.WithCause(err)
	}
	// The read is bounded by the finalized size and checked against it: the
	// bytes a completed row names are exactly row.Size, and anything else is
	// a store whose content drifted from the metadata -- refuse it rather
	// than decode bytes the row never accounted for.
	raw, err := io.ReadAll(io.LimitReader(rc, *row.Size+1))
	closeErr := rc.Close()
	if err != nil {
		return ErrStoreError.WithCause(err)
	}
	if closeErr != nil {
		return ErrStoreError.WithCause(closeErr)
	}
	if int64(len(raw)) != *row.Size {
		return ErrStoreError.WithCause(errors.New("storage: stored content size differs from the completed row"))
	}

	// The pixel ceiling is enforced against the header before any full
	// decode, so an image the transfer pipeline would have refused cannot
	// be decoded into memory by the worker. The errors here
	// (ErrPixelLimitExceeded, ErrImageUnreadable) already speak the service
	// vocabulary.
	if _, _, err = decodeImageFacts(raw, s.maxPixels); err != nil {
		return err
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return ErrImageUnreadable.WithCause(err)
	}

	thumb := downscaleToMaxEdge(img, s.maxEdge)
	buf := new(bytes.Buffer)
	switch mime {
	case "image/jpeg":
		if err = jpeg.Encode(buf, thumb, &jpeg.Options{Quality: thumbnailJPEGQuality}); err != nil {
			return ErrInternal.WithCause(fmt.Errorf("storage: encode jpeg thumbnail: %w", err))
		}
	case "image/png":
		if err = png.Encode(buf, thumb); err != nil {
			return ErrInternal.WithCause(fmt.Errorf("storage: encode png thumbnail: %w", err))
		}
	}

	derivKey, err := DerivativeKey(pkgcore.TenantID(row.GetTenantID()), objectID, DerivativeKindThumbnail)
	if err != nil {
		return ErrInternal.WithCause(err)
	}
	if err = st.PutObject(ctx, derivKey, bytes.NewReader(buf.Bytes())); err != nil {
		return ErrStoreError.WithCause(err)
	}

	w, h := thumb.Bounds().Dx(), thumb.Bounds().Dy()
	derivative := ObjectDerivative{
		ID:          uuid.NewString(),
		TenantModel: dbkit.TenantModel{TenantID: string(row.GetTenantID())},
		ObjectID:    objectID,
		Kind:        DerivativeKindThumbnail,
		Key:         derivKey,
		MIME:        mime,
		Size:        int64(buf.Len()),
		Width:       &w,
		Height:      &h,
	}
	// The insert is where this service converges with a delete that marked
	// or removed the object while the bytes above were decoded and written:
	// insertDerivativeIfAbsent's object-state gate (repository.go) refuses --
	// in the same transaction as the insert -- when the object's own row is
	// gone or no longer completed. That atomicity is what the re-read this
	// service used to perform here after its byte write could not give: no
	// interleaving can slip a row past a completed deletion anymore. A
	// refused insert means the bytes this run just wrote have no row that
	// can reference them, so they are dropped (best effort -- a failed drop
	// leaves at worst orphaned bytes, never a row pointing at them) and the
	// run converges on nil, no different from a derive that found nothing
	// to do.
	refused, err := s.derivatives.insertDerivativeIfAbsent(ctx, derivative)
	if err != nil {
		return err
	}
	if refused {
		s.dropDerivativeBytes(ctx, st, objectID, derivKey)
		return nil
	}
	observability.FromContext(ctx).Info("thumbnail derived",
		"object_id", objectID, "kind", DerivativeKindThumbnail,
		"width", w, "height", h, "size", int64(buf.Len()), "mime", mime)
	return nil
}

// dropDerivativeBytes removes the derivative bytes key names from the store,
// best effort: a failed removal is warned about, never failed -- the caller
// is already converging on "nothing derived", and the only cost of the
// orphaned bytes is storage.
func (s *DeriveService) dropDerivativeBytes(ctx context.Context, st pkgcore.ObjectStore, objectID, key string) {
	if err := st.DeleteObject(ctx, key); err != nil {
		observability.FromContext(ctx).Warn("thumbnail byte cleanup failed after the object disappeared",
			"object_id", objectID, "key", key, "error", err)
	}
}

// deriveHandler is the jobs.Handler claiming taskTypeDeriveThumbnail, the
// task the completion pipeline enqueues for every finalized image object.
// Its Handle decodes the task payload and runs DeriveThumbnail; the
// skip-on-nothing semantics above are what keep a task whose object was
// deleted meanwhile from dead-lettering.
type deriveHandler struct {
	svc *DeriveService
}

// Type returns the task type this handler claims -- the type the completion
// pipeline enqueues under, and the string jobs matches at dispatch.
func (h deriveHandler) Type() string { return taskTypeDeriveThumbnail }

// Handle runs one thumbnail-derive task. The task's payload is the
// deriveThumbnailTaskPayload JSON the completion pipeline enqueues; a
// payload that does not decode, or that names no object, is a task-shape
// violation and fails the job (the queue retries and eventually dead-letters
// such a task -- it can never succeed by re-running). Every other outcome
// is the service's own: nil for derived-or-skipped, the typed service error
// for a real failure.
func (h deriveHandler) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	var payload deriveThumbnailTaskPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return jobs.Result{}, fmt.Errorf("storage: undecodable thumbnail-derive task payload: %w", err)
	}
	if payload.ObjectID == "" {
		return jobs.Result{}, errors.New("storage: thumbnail-derive task payload carries an empty object_id")
	}
	if err := h.svc.DeriveThumbnail(ctx, payload.ObjectID); err != nil {
		return jobs.Result{}, err
	}
	return jobs.Result{}, nil
}

// downscaleToMaxEdge shrinks src so that its longer edge is at most maxEdge,
// preserving the aspect ratio. When src already fits, it is returned
// unchanged -- but still re-encoded by the caller, so a thumbnail of a small
// image is a uniform-quality copy in its source format, never the original
// bytes themselves. A non-positive maxEdge resolves to
// defaultDerivativeMaxEdge.
//
// Both edges shrink by integer floor division, so the result is at most
// maxEdge on the longer side and strictly smaller than the source on the
// side the division scaled -- the derivation never upscales, whatever edge
// the host configured.
func downscaleToMaxEdge(src image.Image, maxEdge int) image.Image {
	if maxEdge <= 0 {
		maxEdge = defaultDerivativeMaxEdge
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxEdge && h <= maxEdge {
		return src
	}
	var dstW, dstH int
	if w >= h {
		dstW = maxEdge
		dstH = int(int64(h) * int64(maxEdge) / int64(w))
	} else {
		dstH = maxEdge
		dstW = int(int64(w) * int64(maxEdge) / int64(h))
	}
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}
	return boxDownscaleTo(src, dstW, dstH)
}

// boxDownscaleTo resamples src to exactly dstW by dstH pixels by exact area
// averaging: every output pixel averages every source pixel its rectangle
// covers. The rectangles partition the source at integer boundaries --
// output column dx covers source columns [dx*srcW/dstW, (dx+1)*srcW/dstW),
// and rows likewise -- so no source pixel is sampled twice and none is
// skipped: a hand-rolled area average, no halving chain, no interpolation
// kernel, and no image/draw dependency. The caller must pass dstW <= srcW
// and dstH <= srcH (downscaleToMaxEdge does); the rectangles of an
// upscaling call would leave source pixels unread instead of duplicated.
//
// The average runs in premultiplied 16-bit space, one output row's
// accumulators at a time -- the area rectangle of an output row spans whole
// source rows, so a source row contributes to exactly one output row and the
// accumulator buffer never holds more than one output row (dstW * 4
// uint64s), however large the source. Each source pixel is sampled exactly
// once per output pass, so the cost is one read per source pixel
// regardless of the target size. Averaging premultiplied values keeps a
// semi-transparent source pixel's color weighted by its own opacity, and the
// straight color is recovered after the division, so a PNG with transparency
// comes out without the dark halos a naive straight-color average would
// smear along its edges.
func boxDownscaleTo(src image.Image, dstW, dstH int) *image.NRGBA {
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))

	// The source-column span of each output column, the same for every
	// output row.
	x0 := make([]int, dstW)
	x1 := make([]int, dstW)
	for dx := 0; dx < dstW; dx++ {
		x0[dx] = dx * srcW / dstW
		x1[dx] = (dx + 1) * srcW / dstW
	}

	// One output row's premultiplied accumulators: dstW pixels, four
	// 16-bit channels each, widened to uint64 so a huge source rectangle
	// cannot overflow the sum.
	acc := make([]uint64, dstW*4)
	for dy := 0; dy < dstH; dy++ {
		y0 := dy * srcH / dstH
		y1 := (dy + 1) * srcH / dstH
		clear(acc)
		for sy := y0; sy < y1; sy++ {
			for dx := 0; dx < dstW; dx++ {
				off := dx * 4
				for sx := x0[dx]; sx < x1[dx]; sx++ {
					r, g, bl, a := src.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
					acc[off] += uint64(r)
					acc[off+1] += uint64(g)
					acc[off+2] += uint64(bl)
					acc[off+3] += uint64(a)
				}
			}
		}
		for dx := 0; dx < dstW; dx++ {
			// The number of source pixels one output pixel averages. Both
			// spans are non-negative -- the rectangles partition the source --
			// and their product is at most the source's pixel count, so the
			// widened arithmetic below can neither overflow nor wrap.
			n := uint64(y1-y0) * uint64(x1[dx]-x0[dx]) //nolint:gosec // G115: non-negative span product, bounded by the source pixel count (see the two lines above)
			off := dx * 4
			if n == 0 {
				continue // unreachable under the dst <= src precondition
			}
			alpha := acc[off+3] / n
			o := dst.PixOffset(dx, dy)
			if alpha == 0 {
				// Fully transparent: straight RGB is conventionally zero and
				// the unpremultiply below would divide by a zero alpha.
				dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = 0, 0, 0, 0
				continue
			}
			dst.Pix[o] = channelByte(unpremultiply(acc[off]/n, alpha))
			dst.Pix[o+1] = channelByte(unpremultiply(acc[off+1]/n, alpha))
			dst.Pix[o+2] = channelByte(unpremultiply(acc[off+2]/n, alpha))
			dst.Pix[o+3] = channelByte(alpha)
		}
	}
	return dst
}

// unpremultiply converts one channel of a premultiplied 16-bit pixel mean
// back to the straight 16-bit value the NRGBA output stores, dividing by the
// pixel's own mean alpha. The multiplication cannot overflow: the
// premultiplied mean of a channel never exceeds the alpha mean (each
// premultiplied sample is at most its own alpha sample), and both means fit
// in 16 bits, so p * 0xffff <= 0xffff * 0xffff < 2^32, comfortably inside a
// uint64. The caller guarantees alpha is non-zero.
func unpremultiply(p, alpha uint64) uint64 {
	return p * 0xffff / alpha
}

// channelByte narrows one straight 16-bit channel value to the 8-bit byte an
// NRGBA pixel stores, keeping the top 8 bits of the 16-bit value.
func channelByte(v uint64) uint8 {
	//nolint:gosec // G115: v is a straight 16-bit channel value -- at most 0xffff by the invariants on unpremultiply's inputs and the mean division above
	return uint8(v >> 8)
}
