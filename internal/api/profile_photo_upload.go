package api

import (
	"context"
	"errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

const maxProfilePhotoGalleryEntries = 20

// handleUploadProfilePhoto stores an uploaded JPEG in the caller's profile
// gallery and selects it as the current avatar. The gallery store owns the
// receipt, quota charge, file publication and current-photo transition as one
// operation; this handler only supplies the validated byte assembly.
func (h *handlers) handleUploadProfilePhoto(r *mtproto.Request) (bin.Encoder, error) {
	if h.uploadsDisabled {
		return nil, errUploadsDisabled
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.PhotosUploadProfilePhotoRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if req.GetFallback() {
		return nil, errInputRequestInvalid
	}
	if _, ok := req.GetBot(); ok {
		return nil, errInputRequestInvalid
	}
	if _, ok := req.GetVideo(); ok {
		return nil, errInputRequestInvalid
	}
	if _, ok := req.GetVideoStartTs(); ok {
		return nil, errInputRequestInvalid
	}
	if _, ok := req.GetVideoEmojiMarkup(); ok {
		return nil, errInputRequestInvalid
	}
	input, ok := req.GetFile()
	if !ok || input == nil {
		return nil, errMediaInvalid
	}
	fileID, parts, name, checksum, err := inputPhotoFileParts(input)
	if err != nil {
		return nil, err
	}
	if int64(parts) > (maxPhotoJPEGBytes+store.MaxPartBytes-1)/store.MaxPartBytes {
		return nil, errFilePartInvalid
	}
	name = sanitizeFileName(name)
	if name == "" {
		name = "profile.jpg"
	}
	if err := h.checkRateLimit(r, "update_profile", h.rateLimitUpdateProfile); err != nil {
		return nil, err
	}

	result, err := h.store.ProfileUpload(r.Ctx, store.ProfileUploadRequest{
		OwnerID:      r.UserID,
		ClientFileID: fileID,
		Parts:        parts,
		MimeType:     "image/jpeg",
		FileName:     name,
		MaxFileBytes: maxPhotoJPEGBytes,
		MaxUserBytes: h.maxUserStorageBytes,
		GalleryCap:   maxProfilePhotoGalleryEntries,
		Assemble: func(assembly store.ProfileAssembly) (store.PhotoDimensions, error) {
			reader := newPartsReader(r.Ctx, h.store, assembly.Parts, assembly.Total)
			dimensions, written, assembleErr := h.putAndValidateJPEG(
				r.Ctx, blob.Key(assembly.File.ID), reader, assembly.Total, checksum, nil,
			)
			reader.stopAndWait()
			if assembleErr != nil {
				return store.PhotoDimensions{}, assembleErr
			}
			if written != assembly.Total {
				return store.PhotoDimensions{}, errMediaInvalid
			}
			return store.PhotoDimensions{
				Width:  int32(dimensions.width),  //nolint:gosec // validateJPEG caps dimensions at maxPhotoDimension.
				Height: int32(dimensions.height), //nolint:gosec // validateJPEG caps dimensions at maxPhotoDimension.
			}, nil
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrProfilePartsIncomplete), errors.Is(err, store.ErrProfileUploadConflict),
			errors.Is(err, store.ErrUploadPartMissing), errors.Is(err, store.ErrProfilePhotoUnavailable):
			return nil, errMediaInvalid
		case errors.Is(err, store.ErrProfileGalleryCap):
			return nil, errProfilePhotosTooMuch
		case errors.Is(err, store.ErrStorageQuota):
			return nil, errFileQuota
		case errors.Is(err, store.ErrUploadQuota):
			return nil, errStorageQuota
		case errors.Is(err, store.ErrInvalidPhotoDimensions):
			return nil, errPhotoInvalidDimensions
		case photoValidationRPCError(err) != nil:
			return nil, photoValidationRPCError(err)
		default:
			h.log.Error("upload profile photo", "user_id", r.UserID, "file_id", fileID, "err", err)
			return nil, errInternal
		}
	}
	if _, err := h.store.DeleteUploadParts(r.Ctx, r.UserID, fileID); err != nil {
		h.log.Error("delete profile photo upload parts", "user_id", r.UserID, "file_id", fileID, "err", err)
	}

	photo := h.photoToTL(result.File)
	photo.AccessHash = h.photos.Derive(r.UserID, r.UserID, result.File.ID)
	photo.FileReference = profilePhotoFileReference(r.UserID)
	loadedUser, found, loadErr := h.store.UserByID(r.Ctx, r.UserID)
	if loadErr != nil || !found {
		h.log.Error("upload profile photo: load owner", "user_id", r.UserID, "err", loadErr)
		return nil, errInternal
	}
	user := h.userToTL(loadedUser, r.UserID, true, store.Contact{}, store.LastSeenVisibility{})
	user.SetPhoto(userProfilePhoto(result.File.ID, h.dcID))
	return &tg.PhotosPhoto{Photo: photo, Users: []tg.UserClass{user}}, nil
}

func userProfilePhoto(photoID int64, dcID int) *tg.UserProfilePhoto {
	return &tg.UserProfilePhoto{PhotoID: photoID, DCID: dcID}
}

func (h *handlers) attachCurrentProfilePhotos(ctx context.Context, viewerID int64, owners []store.User, rendered []*tg.User) error {
	if len(owners) != len(rendered) {
		return errors.New("profile photo hydration: owners and users have different lengths")
	}
	ids := make([]int64, len(owners))
	for i, owner := range owners {
		ids[i] = owner.ID
	}
	photos, err := h.store.CurrentProfilePhotoIDsForViewer(ctx, ids, viewerID)
	if err != nil {
		return err
	}
	for i, owner := range owners {
		if photoID := photos[owner.ID]; photoID > 0 {
			rendered[i].SetPhoto(userProfilePhoto(photoID, h.dcID))
		}
	}
	return nil
}
