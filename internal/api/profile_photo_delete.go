package api

import (
	"context"
	"errors"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

const maxProfilePhotoDeleteInputs = 100

// handleDeleteProfilePhotos removes only live gallery entries authorized by
// the caller's self-bound photo hash. Invalid elements are omitted uniformly,
// while the fixed vector cap is checked before rate admission or database work.
func (h *handlers) handleDeleteProfilePhotos(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.PhotosDeletePhotosRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	inputs := req.GetID()
	if len(inputs) > maxProfilePhotoDeleteInputs {
		return nil, errLimitInvalid
	}
	ids := make([]int64, 0, len(inputs))
	seen := make(map[int64]struct{}, len(inputs))
	for _, item := range inputs {
		photo, ok := item.(*tg.InputPhoto)
		if !ok || photo == nil || photo.ID < 1 || !h.photos.Verify(r.UserID, r.UserID, photo.ID, photo.AccessHash) {
			continue
		}
		if _, ok := seen[photo.ID]; ok {
			continue
		}
		seen[photo.ID] = struct{}{}
		ids = append(ids, photo.ID)
	}
	if len(ids) == 0 {
		return &tg.LongVector{Elems: []int64{}}, nil
	}

	mutationCtx, cancel := context.WithTimeout(r.Ctx, store.ProfilePhotoSelectionWait)
	defer cancel()
	if err := h.checkRateLimitCostWithDeadline(r, mutationCtx, "update_profile", h.rateLimitUpdateProfile, 1); err != nil {
		return nil, err
	}
	deleted, err := h.store.DeleteProfilePhotos(mutationCtx, r.UserID, ids)
	if err != nil {
		if errors.Is(err, store.ErrProfilePhotoBusy) ||
			(r.Ctx.Err() == nil && errors.Is(mutationCtx.Err(), context.DeadlineExceeded)) {
			return nil, FloodWaitError(1)
		}
		h.log.Error("delete profile photos", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	return &tg.LongVector{Elems: deleted}, nil
}

// handleClearProfilePhoto removes exactly the current entry. The store binds
// this clear to the authenticated transport request and serializes its receipt
// check with rate admission, so a retry cannot remove a later selection or
// spend another token while the first clear is still committing.
func (h *handlers) handleClearProfilePhoto(r *mtproto.Request) (bin.Encoder, error) {
	mutationCtx, cancel := context.WithTimeout(r.Ctx, store.ProfilePhotoClearWait)
	defer cancel()
	authKeyID := mtproto.AuthKeyIDInt64(r.AuthKeyID)
	if err := h.store.ClearCurrentProfilePhoto(
		mutationCtx, r.UserID, authKeyID, r.SessionID, r.MsgID, h.rateLimitUpdateProfile,
	); err != nil {
		if errors.Is(err, store.ErrProfilePhotoBusy) ||
			(r.Ctx.Err() == nil && errors.Is(mutationCtx.Err(), context.DeadlineExceeded)) {
			return nil, FloodWaitError(1)
		}
		if rateLimited, ok := errors.AsType[*store.RateLimitedError](err); ok {
			h.recordRateLimitDenial(rateLimited.Surface)
			return nil, FloodWaitError(int(rateLimited.Wait / time.Second))
		}
		if errors.Is(err, store.ErrRateLimitStorage) {
			h.log.Error("clear profile photo: rate limit charge", "user_id", r.UserID,
				"surface", "update_profile", "err", err)
			return nil, errInternal
		}
		h.log.Error("clear profile photo", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}

	loadedUser, found, err := h.store.UserByID(r.Ctx, r.UserID)
	if err != nil || !found {
		h.log.Error("clear profile photo: load owner", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	user := h.userToTL(loadedUser, r.UserID, true, store.Contact{}, store.LastSeenVisibility{})
	return &tg.PhotosPhoto{Photo: &tg.PhotoEmpty{}, Users: []tg.UserClass{user}}, nil
}
