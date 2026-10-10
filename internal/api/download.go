package api

import (
	"context"
	"errors"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// maxDownloadChunk caps one upload.getFile reply. It is the protocol maximum,
// and it is also what bounds the per-request buffer: the read window is sized
// from the request's limit and the remaining file bytes, never from the file's
// size, so serving 4 KiB out of a 100 MiB file allocates 4 KiB.
const maxDownloadChunk = 1024 * 1024

const getFileInFlightSurface = "upload_get_file_in_flight"

const defaultGetFileLeaseTTL = 2*mtproto.DefaultRPCDeadline + time.Second

func getFileLeaseTTL(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 {
			return remaining + time.Second
		}
	}
	return defaultGetFileLeaseTTL
}

func (h *handlers) releaseGetFileLease(ctx context.Context, lease *store.LimitLease) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := h.store.ReleaseLimitLease(releaseCtx, lease); err != nil {
		h.log.Error("release get file in-flight lease", "err", err)
	}
}

func (h *handlers) keepGetFileLeaseAlive(
	lease *store.LimitLease,
	ttl time.Duration,
	cancelOperation context.CancelFunc,
) (stop func(), renewalErrors <-chan error) {
	if lease == nil {
		return func() {}, nil
	}

	interval := ttl / 3
	if interval <= 0 {
		interval = ttl
	}
	stopRenewal := make(chan struct{})
	renewalDone := make(chan struct{})
	errorCh := make(chan error, 1)
	renewalErrors = errorCh
	go func() {
		defer close(renewalDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopRenewal:
				return
			case <-ticker.C:
				select {
				case <-stopRenewal:
					return
				default:
				}
				renewCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := h.store.RenewLimitLease(renewCtx, lease, ttl)
				cancel()
				if err != nil {
					select {
					case <-stopRenewal:
						return
					default:
					}
					h.log.Error("renew get file in-flight lease", "err", err)
					select {
					case errorCh <- err:
					default:
					}
					cancelOperation()
					continue
				}
			}
		}
	}()

	return func() {
		close(stopRenewal)
		<-renewalDone
	}, renewalErrors
}

// checkGetFileRateLimit admits an authorized upload.getFile through both the
// per-account and cluster-wide aggregate budgets. The account reservation is
// refunded when the aggregate budget rejects the call, so the two limits remain
// independent and an aggregate denial does not spend the account's budget.
//
// Every download lane goes through this one function and its two budgets. The
// gallery lane's avatar reads are upload.getFile work, so they are charged to
// the same allowance: giving a second lane its own budget would double what one
// account may read out of the blob store.
func (h *handlers) checkGetFileRateLimit(r *mtproto.Request) error {
	reservation, denied, err := h.store.ReserveRateLimit(
		r.Ctx, r.UserID, "upload_get_file", h.rateLimitGetFile,
	)
	if err != nil {
		h.log.Error("get file rate limit")
		return errInternal
	}
	if denied != nil {
		h.recordRateLimitDenial("upload_get_file")
		return floodWaitForDuration(denied.Wait)
	}

	_, denied, err = h.store.ReserveRateLimit(
		r.Ctx, 0, "upload_get_file_replica", h.rateLimitGetFileReplica,
	)
	if err != nil {
		h.log.Error("get file aggregate rate limit", "err", err)
		if reservation != nil {
			if refundErr := h.store.RefundRateLimit(r.Ctx, r.UserID, "upload_get_file", reservation); refundErr != nil {
				h.log.Error("get file rate limit refund")
				return errInternal
			}
		}
		return errInternal
	}
	if denied == nil {
		return nil
	}
	if reservation != nil {
		if err := h.store.RefundRateLimit(r.Ctx, r.UserID, "upload_get_file", reservation); err != nil {
			h.log.Error("get file rate limit refund")
			return errInternal
		}
	}
	h.recordRateLimitDenial("upload_get_file")
	return floodWaitForDuration(denied.Wait)
}

func floodWaitForDuration(wait time.Duration) error {
	seconds := int(wait / time.Second)
	if wait%time.Second != 0 {
		seconds++
	}
	return FloodWaitError(seconds)
}

// downloadLocation is the identity one upload.getFile location names: the file
// id, the credential the lane checks against it, and the thumbnail size
// requested. The lane decides what each of them means.
type downloadLocation struct {
	fileID     int64
	accessHash int64
	thumbSize  string
	photo      bool
}

// parseDownloadLocation reads the (id, access_hash, thumb) triple one
// supported input file location carries. A document location names no
// thumbnail, and a thumbnail size on one asks for bytes this server does not
// generate.
//
// loc.FileReference is deliberately not read, not compared and not
// validated: it is a placeholder echoed on output and ignored on input, and
// half-validating it would make it an oracle. Do not "complete" it.
func parseDownloadLocation(loc tg.InputFileLocationClass) (downloadLocation, error) {
	switch l := loc.(type) {
	case *tg.InputDocumentFileLocation:
		if l.ThumbSize != "" {
			return downloadLocation{}, errLocationInvalid
		}
		return downloadLocation{fileID: l.ID, accessHash: l.AccessHash}, nil
	case *tg.InputPhotoFileLocation:
		return downloadLocation{
			fileID:     l.ID,
			accessHash: l.AccessHash,
			thumbSize:  l.ThumbSize,
			photo:      true,
		}, nil
	default:
		return downloadLocation{}, errLocationInvalid
	}
}

// checkDownloadWindow rejects a window that cannot be walked. Both lanes answer
// identically because both are upload.getFile, and the bound on a reply is what
// bounds the per-request buffer.
func checkDownloadWindow(offset int64, limit int) error {
	if limit <= 0 || limit > maxDownloadChunk || offset < 0 {
		return errLocationInvalid
	}
	return nil
}

// downloadGate turns one request's file identity into the file this lane may
// serve, or into the error the client gets. It runs inside the in-flight slot
// and before the window is computed, so no lane reaches a range check, a
// rate-limit reservation, or a blob read holding a file it was not given.
//
// The gate returns the client-facing error itself and logs anything that is a
// server event: a lane's rejections are specific, and only the lane knows which
// of them are client mistakes worth no log line.
type downloadGate func(ctx context.Context, fileID, callerID int64) (store.File, error)

// serveFileChunk is the plumbing every download lane shares: the per-account
// in-flight slot and its renewal, the lane's authorization, the window, the
// account and replica rate limits, and one blob read. It is one function so a
// lane cannot be added that ships half the resource discipline, and so the two
// lanes' answers to a bad window, an exhausted budget and a busy slot stay
// identical by construction.
//
// missingBlobIsFault is what a body missing under a row that says stored means
// on this lane. On the message lane it is a server fault: a file a live message
// names is retained by the eraser, so a stored row whose object is gone is a
// broken server. On the gallery lane it is the same LOCATION_INVALID every other
// rejection gives, and it is not logged: a gallery entry's lifetime is the
// owner's delete decision, and the eraser reclaims a photo's bytes once nothing
// live references them, so a reclaim landing between the gate and this read is
// a race that ends in an ordinary read failure. The bytes the gate admitted may
// finish; the next call is denied.
//
// Nothing here holds a database session across the object read. The in-flight
// lease is a row this function commits and renews from its own goroutine, a
// lane's gate is a statement that has already committed, and ReadAt runs on
// operationCtx with no transaction and no advisory lock held by this request.
func (h *handlers) serveFileChunk(
	r *mtproto.Request, fileID, offset int64, limit int,
	gate downloadGate, missingBlobIsFault bool,
) (result bin.Encoder, retErr error) {
	if r.UserID == 0 {
		// Backstop, not the lane's check: every lane answers authentication before
		// it parses a location, validates a window, resolves a peer or verifies a
		// credential, so no request shape reaches the in-flight slot, the
		// database, the rate limiter or the object store unauthenticated.
		return nil, errAuthKeyUnreg
	}
	leaseTTL := getFileLeaseTTL(r.Ctx)
	lease, denied, err := h.store.TryAcquireLimitLease(
		r.Ctx, r.UserID, getFileInFlightSurface, 1, leaseTTL,
	)
	if err != nil {
		h.log.Error("acquire get file in-flight lease", "err", err)
		return nil, errInternal
	}
	if denied != nil {
		return nil, errDownloadBusy
	}
	operationCtx, cancelOperation := context.WithCancel(r.Ctx)
	stopLeaseRenewal, renewalErrors := h.keepGetFileLeaseAlive(lease, leaseTTL, cancelOperation)
	defer func() {
		stopLeaseRenewal()
		cancelOperation()
		h.releaseGetFileLease(r.Ctx, lease)
		select {
		case <-renewalErrors:
			if retErr == nil {
				result = nil
				retErr = errInternal
			}
		default:
		}
	}()
	rateLimitRequest := *r
	rateLimitRequest.Ctx = operationCtx

	file, err := gate(operationCtx, fileID, r.UserID)
	if err != nil {
		return nil, err
	}

	downloadSize := file.Size
	if derivatives := file.PhotoDerivatives; derivatives != nil {
		downloadSize = int64(derivatives.MSize)
	}

	// A window running past the end is served short rather than rejected:
	// upload.getFile is how a client walks a file in fixed-size windows, and the
	// last window is short by definition. offset == size is legal and returns
	// zero bytes, so a client that has read to the end gets an empty reply.
	if offset > downloadSize {
		return nil, errLocationInvalid
	}
	n := int64(limit)
	if remaining := downloadSize - offset; n > remaining {
		n = remaining
	}
	if err := h.checkGetFileRateLimit(&rateLimitRequest); err != nil {
		return nil, err
	}

	var b []byte
	if file.PhotoDerivatives != nil {
		b, err = h.store.PhotoDerivativeChunkForDownload(
			operationCtx, file.ID, file.AccessHash, r.UserID, offset, n,
		)
		if errors.Is(err, store.ErrFileNotFound) {
			return nil, errLocationInvalid
		}
		if err != nil {
			h.log.Error("read photo derivative", "file_id", file.ID, "err", err)
			return nil, errInternal
		}
	} else {
		b, err = h.blobs.ReadAt(operationCtx, blob.Key(file.ID), offset, n)
		if err != nil {
			if missingBlobIsFault || !errors.Is(err, blob.ErrNotFound) {
				// ErrNotFound here means the row says stored but the body is gone: a
				// server fault, not a client one.
				h.log.Error("read file blob", "file_id", file.ID, "err", err)
				return nil, errInternal
			}
			// The gallery lane's eraser race: the object was reclaimed after this
			// request was admitted. The client is told the location is not
			// servable, which is what every other rejection of it says.
			return nil, errLocationInvalid
		}
	}

	var fileType tg.StorageFileTypeClass = &tg.StorageFileUnknown{}
	if file.Kind == store.FileKindPhoto {
		fileType = &tg.StorageFileJpeg{}
	}
	return &tg.UploadFile{
		// Documents retain storage.fileUnknown; photos are served as JPEG only
		// after the upload validator has identified and checked that format.
		Type:  fileType,
		Mtime: int(file.Date.Unix()),
		Bytes: b,
	}, nil
}

// handleGetFile serves upload.getFile: one byte range of one stored file, to a
// caller the store's gate says owns a live message referencing it.
func (h *handlers) handleGetFile(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.UploadGetFileRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		// Authentication is answered before anything is parsed or validated: what
		// the location and window checks accept is the authenticated path's answer,
		// and an unauthenticated caller has no business collecting it.
		return nil, errAuthKeyUnreg
	}
	loc, err := parseDownloadLocation(req.Location)
	if err != nil {
		return nil, err
	}
	if err := checkDownloadWindow(req.Offset, req.Limit); err != nil {
		return nil, err
	}
	return h.serveFileChunk(r, loc.fileID, req.Offset, req.Limit, h.messageFileGate(loc), true)
}

// messageFileGate is the message lane's entitlement: the raw files.access_hash
// the caller carries, plus a live message it owns or an un-banned channel post
// naming the file. A photo location must name a stored photo whose size type is
// the one the request asks for, which is what pins a photo's advertised sizes to
// the bytes it serves.
func (h *handlers) messageFileGate(loc downloadLocation) downloadGate {
	return func(ctx context.Context, fileID, callerID int64) (store.File, error) {
		file, err := h.store.FileForDownload(ctx, fileID, loc.accessHash, callerID)
		switch {
		case errors.Is(err, store.ErrFileNotFound):
			// A rejection is a client mistake, not a server event, and this path is
			// reachable by anyone: it is not logged.
			return store.File{}, errLocationInvalid
		case err != nil:
			h.log.Error("file for download", "user_id", callerID, "err", err)
			return store.File{}, errInternal
		}
		if loc.photo {
			if file.Kind != store.FileKindPhoto {
				return store.File{}, errLocationInvalid
			}
			if loc.thumbSize == "m" {
				derivatives, derivativeErr := h.store.PhotoDerivativeForDownload(ctx, fileID, loc.accessHash, callerID)
				switch {
				case errors.Is(derivativeErr, store.ErrFileNotFound):
					return store.File{}, errLocationInvalid
				case derivativeErr != nil:
					h.log.Error("photo derivative for download", "user_id", callerID, "err", derivativeErr)
					return store.File{}, errInternal
				}
				file.PhotoDerivatives = derivatives
				return file, nil
			}
			if loc.thumbSize != photoSizeType(file.Width, file.Height) {
				return store.File{}, errLocationInvalid
			}
			return file, nil
		}
		if file.Kind != store.FileKindDocument {
			return store.File{}, errLocationInvalid
		}
		return file, nil
	}
}
