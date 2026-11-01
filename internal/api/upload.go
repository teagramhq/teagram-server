package api

import (
	"errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// maxFileParts bounds a part index using the smallest part size real clients
// choose. The byte total in the store remains the per-file size gate.
func (h *handlers) maxFileParts() int {
	return int((h.maxFileBytes + store.MinPartBytesForRowCap - 1) / store.MinPartBytesForRowCap)
}

// handleSaveFilePart serves upload.saveFilePart: it stores one part of an
// in-flight upload under the caller's account, bounded by the per-file and
// per-user caps the store enforces inside its transaction.
func (h *handlers) handleSaveFilePart(r *mtproto.Request) (bin.Encoder, error) {
	if h.uploadsDisabled {
		return nil, errUploadsDisabled
	}
	var req tg.UploadSaveFilePartRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if req.FileID == 0 || req.FilePart < 0 {
		return nil, errFilePartInvalid
	}
	if req.FilePart >= h.maxFileParts() {
		return nil, errFilePartInvalid
	}
	if err := h.saveUploadPart(r, req.FileID, req.FilePart, req.Bytes); err != nil {
		return nil, err
	}
	return &tg.BoolTrue{}, nil
}

// handleSaveBigFilePart serves upload.saveBigFilePart. It rejects a declared
// upload that already exceeds the per-file cap before writing its first part.
// The store also checks the accumulated bytes in every save transaction, so a
// client that changes its declared total between calls cannot exceed the cap.
func (h *handlers) handleSaveBigFilePart(r *mtproto.Request) (bin.Encoder, error) {
	if h.uploadsDisabled {
		return nil, errUploadsDisabled
	}
	var req tg.UploadSaveBigFilePartRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if req.FileID == 0 || req.FilePart < 0 {
		return nil, errFilePartInvalid
	}
	if req.FileTotalParts <= 0 {
		return nil, errFilePartInvalid
	}
	if req.FilePart >= req.FileTotalParts {
		return nil, errFilePartInvalid
	}
	if req.FileTotalParts > h.maxFileParts() || declaredUploadTooLarge(req.FileTotalParts, len(req.Bytes), h.maxFileBytes) {
		if err := h.checkUploadPartRateLimit(r, req.Bytes); err != nil {
			return nil, err
		}
		if err := h.discardOversizedUpload(r, req.FileID); err != nil {
			return nil, err
		}
		return nil, errFilePartTooBig
	}
	if req.FilePart >= h.maxFileParts() {
		return nil, errFilePartInvalid
	}
	if err := h.saveUploadPart(r, req.FileID, req.FilePart, req.Bytes); err != nil {
		return nil, err
	}
	return &tg.BoolTrue{}, nil
}

// saveUploadPart is the rate limits, store call and error mapping both save
// handlers share.
//
// The limit is checked here rather than in each handler so the two surfaces
// cannot drift onto separate budgets — they write the same rows, and a budget
// each would let an account double its part rate by alternating. It is checked
// before the store call, so a denied part costs the parts table nothing: the
// caps are enforced by writing first and reading the sums back, so reaching the
// store at all is the write amplification this limit exists to bound.
func (h *handlers) saveUploadPart(r *mtproto.Request, fileID int64, part int, payload []byte) error {
	if err := h.checkUploadPartRateLimit(r, payload); err != nil {
		return err
	}
	err := h.store.SaveUploadPart(r.Ctx, r.UserID, fileID, part, payload, h.maxFileBytes)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrPartTooLarge), errors.Is(err, store.ErrFileTooLarge):
		if err := h.discardOversizedUpload(r, fileID); err != nil {
			return err
		}
		return errFilePartTooBig
	// ErrTooManyParts is the row-count half of the same outstanding cap as
	// ErrUploadQuota, and clears the same way, so it reports the same error.
	case errors.Is(err, store.ErrUploadQuota), errors.Is(err, store.ErrTooManyParts):
		return errStorageQuota
	default:
		h.log.Error("save file part", "user_id", r.UserID, "err", err)
		return errInternal
	}
}

func (h *handlers) checkUploadPartRateLimit(r *mtproto.Request, payload []byte) error {
	if err := h.checkRateLimit(r, "save_file_part", h.rateLimitSaveFilePart); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	result, err := h.store.CheckRateLimitCost(r.Ctx, r.UserID, "save_file_part_bytes", h.rateLimitUploadBytes, len(payload))
	return h.finishRateLimitCost(r, "save_file_part", result, err)
}

func (h *handlers) discardOversizedUpload(r *mtproto.Request, fileID int64) error {
	if _, err := h.store.DeleteUploadParts(r.Ctx, r.UserID, fileID); err != nil {
		h.log.Error("discard oversized upload", "user_id", r.UserID, "file_id", fileID, "err", err)
		return errInternal
	}
	return nil
}

func declaredUploadTooLarge(totalParts, partSize int, maxFileBytes int64) bool {
	if totalParts <= 0 || partSize <= 0 {
		return false
	}
	return int64(totalParts) > maxFileBytes/int64(partSize)
}
