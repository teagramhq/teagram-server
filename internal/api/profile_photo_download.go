package api

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// This file is the profile-photo gallery's download lane. photos.getUserPhotos
// puts the owner id in a tagged file_reference and a viewer-bound credential in
// access_hash; upload.getFile verifies that pair before it reaches the live
// gallery gate. The owner hint is not an authorization by itself.
//
// The lane is its own entitlement and it widens no existing one:
//
//   - files.access_hash is not consulted here. The gallery credential is the
//     viewer-bound MAC internal/photohash issues for (viewer, owner, file), and
//     a message photo's raw hash verifies nowhere in it.
//   - user_photos is not consulted by the message lane's gate. Being shown a
//     peer's avatar therefore buys no read of that peer's message media, and
//     having received a file buys no read of anyone's avatar.
//   - the resource discipline is upload.getFile's, shared in serveFileChunk:
//     same in-flight slot, same two rate-limit budgets, same window bounds. The
//     gallery lane cannot be used to read more bytes per second than a
//     download already permitted.
//
// The authorization itself is one live statement (see
// store.ProfilePhotoForDownload) and nothing is held across the blob read: no
// transaction, and in particular no per-owner advisory lock. The messaging lane
// owns that key, and an avatar read that took the photo owner's key would let
// one viewer's hammering queue the owner's own DM and group sends.

// profilePhotoGet is one gallery download: what the wire location carries once
// its peer and photo identity are parsed.
type profilePhotoGet struct {
	// peer names the target whose avatar is requested, in the wire form a
	// location carries. It is resolved with the viewer's own peer hashes, so the
	// target identity here is the same one every other read of that peer uses:
	// InputPeerSelf is the viewer, and an access hash that was not derived
	// for this viewer names no target.
	peer tg.InputPeerClass
	// photoID is the file id the gallery entry names.
	photoID int64
	// credential is the viewer-bound profile-photo access_hash that peer was
	// shown. It is not files.access_hash and the two are never interchangeable:
	// it is bound to the viewer, so a capability copied out of one account's
	// gallery view is inert in another's.
	credential int64
	offset     int64
	limit      int
}

const profilePhotoFileReferenceTag = "tgpf\x01"

// profilePhotoFileReference carries the gallery owner beside the photo
// credential. InputPhotoFileLocation has no peer field, so this authenticated
// hint lets upload.getFile verify the viewer/owner/photo MAC before reading the
// live gallery row. The owner is also returned in photos.getUserPhotos' Users.
func profilePhotoFileReference(ownerID int64) []byte {
	reference := []byte(profilePhotoFileReferenceTag)
	return binary.BigEndian.AppendUint64(reference, uint64(ownerID)) //nolint:gosec // reinterprets a positive database id
}

func profilePhotoOwnerFromFileReference(reference []byte) (int64, bool) {
	if len(reference) != len(profilePhotoFileReferenceTag)+8 ||
		string(reference[:len(profilePhotoFileReferenceTag)]) != profilePhotoFileReferenceTag {
		return 0, false
	}
	ownerID := int64(binary.BigEndian.Uint64(reference[len(profilePhotoFileReferenceTag):])) //nolint:gosec // wire bits reinterpreted as int64
	return ownerID, true
}

// handleGetProfilePhotoFile serves one byte range of one live gallery photo to
// the viewer holding that photo's capability.
func (h *handlers) handleGetProfilePhotoFile(ctx context.Context, viewerID int64, req profilePhotoGet) (bin.Encoder, error) {
	if viewerID == 0 {
		// Answered before the peer is resolved, the window is checked, the
		// credential is verified and the gate runs, so an unauthenticated session
		// learns nothing about what the lane accepts.
		return nil, errAuthKeyUnreg
	}
	ownerID, err := h.peerUserID(req.peer, viewerID)
	if err != nil {
		// Answered as a location that cannot be served, not as PEER_ID_INVALID:
		// a peer this viewer cannot name does identify a target the request is not
		// allowed to read, which is what LOCATION_INVALID means on this lane.
		// Keeping one answer for every way the identity, the capability and the
		// row can fail to line up is what stops the lane from reporting how far a
		// forged request got.
		return nil, errLocationInvalid
	}
	if err := checkDownloadWindow(req.offset, req.limit); err != nil {
		return nil, err
	}
	missingBlobIsFault := false
	return h.serveFileChunk(
		&mtproto.Request{Ctx: ctx, UserID: viewerID},
		req.photoID, req.offset, req.limit,
		h.galleryFileGate(ownerID, req.credential),
		// A gallery photo's bytes are reclaimable once nothing live references
		// them, so a body that vanishes after admission is a race, not a fault.
		&missingBlobIsFault,
	)
}

// handleGetPeerProfilePhotoFile serves the current avatar location included in
// a normal tg.User object. InputPeerPhotoFileLocation carries no gallery MAC,
// so its access is limited to the selected photo id for a peer the viewer can
// already name with their own derived peer hash.
func (h *handlers) handleGetPeerProfilePhotoFile(
	r *mtproto.Request, peer tg.InputPeerClass, photoID int64, big bool, offset int64, limit int,
) (bin.Encoder, error) {
	if _, ok := peer.(*tg.InputPeerChannel); ok {
		peerType, channelID, err := h.inputPeer(peer, r.UserID)
		if err != nil || peerType != store.PeerTypeChannel || channelID <= 0 {
			return nil, errLocationInvalid
		}
		gate := func(ctx context.Context, fileID, viewerID int64) (authorizedDownload, error) {
			file, err := h.store.CurrentChannelPeerPhotoForDownload(ctx, channelID, fileID, viewerID)
			switch {
			case errors.Is(err, store.ErrFileNotFound):
				return authorizedDownload{}, errLocationInvalid
			case err != nil:
				h.log.Error("current channel photo for download", "channel_id", channelID, "user_id", viewerID, "err", err)
				return authorizedDownload{}, errInternal
			}
			if !big {
				derivatives, derivativeErr := h.store.PhotoDerivativeForDownload(ctx, fileID, file.AccessHash, viewerID)
				switch {
				case errors.Is(derivativeErr, store.ErrFileNotFound):
				case derivativeErr != nil:
					h.log.Error("current channel photo derivative for download", "channel_id", channelID, "user_id", viewerID, "err", derivativeErr)
					return authorizedDownload{}, errInternal
				default:
					file.PhotoDerivatives = derivatives
					return authorizedDownload{
						file: file,
						photoDerivativeChunk: func(ctx context.Context, offset, limit int64) ([]byte, error) {
							return h.store.PhotoDerivativeChunkForDownload(ctx, fileID, file.AccessHash, viewerID, offset, limit)
						},
					}, nil
				}
			}
			return authorizedDownload{file: file}, nil
		}
		missingBlobIsFault := true
		return h.serveFileChunk(r, photoID, offset, limit, gate, &missingBlobIsFault)
	}
	if chatPeer, ok := peer.(*tg.InputPeerChat); ok {
		if chatPeer.ChatID <= 0 {
			return nil, errLocationInvalid
		}
		gate := func(ctx context.Context, fileID, viewerID int64) (authorizedDownload, error) {
			file, err := h.store.CurrentChatPeerPhotoForDownload(ctx, chatPeer.ChatID, fileID, viewerID)
			switch {
			case errors.Is(err, store.ErrFileNotFound):
				return authorizedDownload{}, errLocationInvalid
			case err != nil:
				h.log.Error("current chat photo for download", "chat_id", chatPeer.ChatID, "user_id", viewerID, "err", err)
				return authorizedDownload{}, errInternal
			}
			if !big {
				derivatives, derivativeErr := h.store.PhotoDerivativeForDownload(ctx, fileID, file.AccessHash, viewerID)
				switch {
				case errors.Is(derivativeErr, store.ErrFileNotFound):
					// The photo remains usable at its original size when no m derivative
					// was produced for it.
				case derivativeErr != nil:
					h.log.Error("current chat photo derivative for download", "chat_id", chatPeer.ChatID, "user_id", viewerID, "err", derivativeErr)
					return authorizedDownload{}, errInternal
				default:
					file.PhotoDerivatives = derivatives
					return authorizedDownload{
						file: file,
						photoDerivativeChunk: func(ctx context.Context, offset, limit int64) ([]byte, error) {
							return h.store.PhotoDerivativeChunkForDownload(ctx, fileID, file.AccessHash, viewerID, offset, limit)
						},
					}, nil
				}
			}
			return authorizedDownload{file: file}, nil
		}
		missingBlobIsFault := true
		return h.serveFileChunk(r, photoID, offset, limit, gate, &missingBlobIsFault)
	}

	ownerID, err := h.peerUserID(peer, r.UserID)
	if err != nil {
		return nil, errLocationInvalid
	}
	gate := func(ctx context.Context, fileID, viewerID int64) (authorizedDownload, error) {
		file, err := h.store.CurrentProfilePhotoForDownload(ctx, ownerID, fileID, viewerID)
		switch {
		case errors.Is(err, store.ErrFileNotFound):
			return authorizedDownload{}, errLocationInvalid
		case err != nil:
			h.log.Error("current profile photo for download", "user_id", viewerID, "err", err)
			return authorizedDownload{}, errInternal
		}
		return authorizedDownload{file: file}, nil
	}
	missingBlobIsFault := false
	return h.serveFileChunk(r, photoID, offset, limit, gate, &missingBlobIsFault)
}

// galleryFileGate is the gallery lane's authorization, in the order the checks
// have to run in: the credential first, because it costs a MAC and a
// constant-time compare and answers for every forged capability without
// touching the database; then one live statement for the facts that can change
// underneath a capability that is genuinely valid — the gallery entry, its
// owner's existence, the target's block policy, and the file's published state.
//
// The credential is a capability, never an identity: the viewer is the
// authenticated session's account, and it is bound into the MAC exactly as the
// owner and file id are. Verifying proves the server issued this capability to
// this account for this photo; it waives none of the live checks.
func (h *handlers) galleryFileGate(ownerID, credential int64) downloadGate {
	return func(ctx context.Context, fileID, viewerID int64) (authorizedDownload, error) {
		if !h.photos.Verify(viewerID, ownerID, fileID, credential) {
			// Not logged: this path is reachable by anyone who can ask for a
			// photo, and a rejection is a client mistake, not a server event.
			return authorizedDownload{}, errLocationInvalid
		}
		file, err := h.store.ProfilePhotoForDownload(ctx, ownerID, fileID, viewerID)
		switch {
		case errors.Is(err, store.ErrFileNotFound):
			return authorizedDownload{}, errLocationInvalid
		case err != nil:
			h.log.Error("profile photo for download", "user_id", viewerID, "err", err)
			return authorizedDownload{}, errInternal
		}
		return authorizedDownload{file: file}, nil
	}
}

// galleryPhotoFileGate validates an InputPhotoFileLocation size and selects
// the gallery's own derivative byte read. The returned m metadata and its bytes
// are each checked against the live gallery policy; message entitlements never
// authorize a gallery derivative.
func (h *handlers) galleryPhotoFileGate(ownerID, credential int64, thumbSize string) downloadGate {
	galleryGate := h.galleryFileGate(ownerID, credential)
	return func(ctx context.Context, fileID, viewerID int64) (authorizedDownload, error) {
		authorized, err := galleryGate(ctx, fileID, viewerID)
		if err != nil {
			return authorizedDownload{}, err
		}
		if thumbSize != "m" {
			if thumbSize != photoSizeType(authorized.file.Width, authorized.file.Height) {
				return authorizedDownload{}, errLocationInvalid
			}
			return authorized, nil
		}

		derivatives, err := h.store.ProfilePhotoDerivativeForDownload(ctx, ownerID, fileID, viewerID)
		switch {
		case errors.Is(err, store.ErrFileNotFound):
			return authorizedDownload{}, errLocationInvalid
		case err != nil:
			h.log.Error("profile photo derivative for download", "user_id", viewerID, "err", err)
			return authorizedDownload{}, errInternal
		}
		authorized.file.PhotoDerivatives = derivatives
		authorized.photoDerivativeChunk = func(ctx context.Context, offset, limit int64) ([]byte, error) {
			return h.store.ProfilePhotoDerivativeChunkForDownload(ctx, ownerID, fileID, viewerID, offset, limit)
		}
		return authorized, nil
	}
}
