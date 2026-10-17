package api

import (
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

const maxUserPhotosPage = 100

// handleGetUserPhotos serves photos.getUserPhotos from the owner's stored
// profile-photo gallery. Each returned photo carries a viewer-bound gallery
// credential, separate from the raw file hash used by message media.
func (h *handlers) handleGetUserPhotos(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.PhotosGetUserPhotosRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if req.Offset < -1 || req.MaxID < 0 || (req.Offset == -1 && req.MaxID == 0) {
		return nil, errOffsetInvalid
	}
	if req.Limit < 1 || req.Limit > maxUserPhotosPage {
		return nil, errLimitInvalid
	}

	ownerID, err := h.inputUserID(req.UserID, r.UserID)
	if err != nil {
		return nil, err
	}
	users, err := h.store.UsersByID(r.Ctx, []int64{ownerID})
	if err != nil {
		h.log.Error("get user photos: load owner", "user_id", r.UserID, "peer_id", ownerID, "err", err)
		return nil, errInternal
	}
	owner, ok := users[ownerID]
	if !ok {
		if ownerID == r.UserID {
			return nil, errAuthKeyUnreg
		}
		return nil, errPeerIDInvalid
	}

	contacts, err := h.contactStatesForUsers(r.Ctx, r.UserID, []int64{ownerID})
	if err != nil {
		h.log.Error("get user photos: load contact state", "user_id", r.UserID, "peer_id", ownerID, "err", err)
		return nil, errInternal
	}
	visibility, err := h.lastSeenVisibilityForUsers(r.Ctx, []store.User{owner}, r.UserID)
	if err != nil {
		h.log.Error("get user photos: evaluate status privacy", "user_id", r.UserID, "peer_id", ownerID, "err", err)
		return nil, errInternal
	}
	user := h.userToTL(owner, r.UserID, ownerID == r.UserID, contacts[ownerID], visibility[ownerID])

	files, total, err := h.store.ProfilePhotosForOwner(r.Ctx, ownerID, r.UserID, req.Offset, req.MaxID, req.Limit)
	if err != nil {
		h.log.Error("get user photos: load gallery", "user_id", r.UserID, "peer_id", ownerID, "err", err)
		return nil, errInternal
	}
	photos := make([]tg.PhotoClass, 0, len(files))
	for _, file := range files {
		photo := h.photoToTL(file)
		photo.AccessHash = h.photos.Derive(r.UserID, ownerID, file.ID)
		photos = append(photos, photo)
	}
	responseUsers := []tg.UserClass{user}
	if len(photos) == 0 {
		return &tg.PhotosPhotos{Photos: photos, Users: responseUsers}, nil
	}
	return &tg.PhotosPhotosSlice{Count: int(total), Photos: photos, Users: responseUsers}, nil
}
