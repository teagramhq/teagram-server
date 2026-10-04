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
					errorCh <- err
					cancelOperation()
					return
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

// handleGetFile serves upload.getFile: one byte range of one stored file, to a
// caller the store's gate says owns a live message referencing it.
func (h *handlers) handleGetFile(r *mtproto.Request) (result bin.Encoder, retErr error) {
	var req tg.UploadGetFileRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	// Only this one location type. InputPhotoFileLocation is rejected because M5
	// stores no photos, and every other InputFileLocation* because it names
	// something that does not exist here.
	loc, ok := req.Location.(*tg.InputDocumentFileLocation)
	if !ok {
		return nil, errLocationInvalid
	}
	// M5 stores no thumbnails, so a thumb request has no answer and must not
	// silently return the full file.
	if loc.ThumbSize != "" {
		return nil, errLocationInvalid
	}
	if req.Limit <= 0 || req.Limit > maxDownloadChunk || req.Offset < 0 {
		return nil, errLocationInvalid
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

	// loc.FileReference is deliberately not read, not compared and not
	// validated: it is a placeholder echoed on output and ignored on input, and
	// half-validating it would make it an oracle. Do not "complete" it.
	file, err := h.store.FileForDownload(operationCtx, loc.ID, loc.AccessHash, r.UserID)
	switch {
	case errors.Is(err, store.ErrFileNotFound):
		// A rejection is a client mistake, not a server event, and this path is
		// reachable by anyone: it is not logged.
		return nil, errLocationInvalid
	case err != nil:
		h.log.Error("file for download", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}

	// A window running past the end is served short rather than rejected:
	// upload.getFile is how a client walks a file in fixed-size windows, and the
	// last window is short by definition. offset == size is legal and returns
	// zero bytes, so a client that has read to the end gets an empty reply.
	if req.Offset > file.Size {
		return nil, errLocationInvalid
	}
	n := int64(req.Limit)
	if remaining := file.Size - req.Offset; n > remaining {
		n = remaining
	}
	if err := h.checkGetFileRateLimit(&rateLimitRequest); err != nil {
		return nil, err
	}

	b, err := h.blobs.ReadAt(operationCtx, blob.Key(file.ID), req.Offset, n)
	if err != nil {
		// ErrNotFound here means the row says stored but the body is gone: a
		// server fault, not a client one.
		h.log.Error("read file blob", "file_id", file.ID, "err", err)
		return nil, errInternal
	}

	return &tg.UploadFile{
		// storage.fileUnknown is the honest answer: the type field describes the
		// file's format, and the server never decodes an uploaded file, so it
		// cannot name one.
		Type:  &tg.StorageFileUnknown{},
		Mtime: int(file.Date.Unix()),
		Bytes: b,
	}, nil
}
