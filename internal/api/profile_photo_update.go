package api

import (
	"errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// handleUpdateProfilePhoto selects one of the caller's live gallery entries as
// the current profile photo. The self-bound MAC is checked before rate
// admission or any database lookup; the store then serializes and rechecks the
// live gallery row before changing the current pointer.
func (h *handlers) handleUpdateProfilePhoto(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.PhotosUpdateProfilePhotoRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if req.GetFallback() {
		return nil, errInputRequestInvalid
	}
	if _, ok := req.GetBot(); ok {
		return nil, errInputRequestInvalid
	}
	input, ok := req.GetIDAsNotEmpty()
	if !ok || input == nil || !h.photos.Verify(r.UserID, r.UserID, input.ID, input.AccessHash) {
		return nil, errPhotoIDInvalid
	}
	if err := h.checkRateLimit(r, "update_profile", h.rateLimitUpdateProfile); err != nil {
		return nil, err
	}

	file, _, err := h.store.SelectProfilePhoto(r.Ctx, r.UserID, input.ID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrProfilePhotoUnavailable):
			return nil, errPhotoIDInvalid
		case errors.Is(err, store.ErrProfilePhotoBusy):
			return nil, FloodWaitError(1)
		default:
			h.log.Error("update profile photo", "user_id", r.UserID, "photo_id", input.ID, "err", err)
			return nil, errInternal
		}
	}

	photo := h.photoToTL(file)
	photo.AccessHash = h.photos.Derive(r.UserID, r.UserID, file.ID)
	photo.FileReference = profilePhotoFileReference(r.UserID)
	loadedUser, found, loadErr := h.store.UserByID(r.Ctx, r.UserID)
	if loadErr != nil || !found {
		h.log.Error("update profile photo: load owner", "user_id", r.UserID, "err", loadErr)
		return nil, errInternal
	}
	user := h.userToTL(loadedUser, r.UserID, true, store.Contact{}, store.LastSeenVisibility{})
	user.SetPhoto(userProfilePhoto(file.ID, h.dcID))
	return &tg.PhotosPhoto{Photo: photo, Users: []tg.UserClass{user}}, nil
}
