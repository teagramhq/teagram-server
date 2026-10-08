package api

import (
	"context"
	"errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// This file is the profile-photo gallery's download lane. It is deliberately
// not registered on the dispatcher: no photo RPC is exposed yet, hydration does
// not put a gallery photo in front of a client, and the slice that registers
// this path is a separate decision. What lives here is the authorization, and
// the only caller today is the synthetic test path, which is the point — the
// security-relevant part of an exposure is the gate, and it is provable before
// anything can reach it from the wire.
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
	return h.serveFileChunk(
		&mtproto.Request{Ctx: ctx, UserID: viewerID},
		req.photoID, req.offset, req.limit,
		h.galleryFileGate(ownerID, req.credential),
		// A gallery photo's bytes are reclaimable once nothing live references
		// them, so a body that vanishes after admission is a race, not a fault.
		false,
	)
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
	return func(ctx context.Context, fileID, viewerID int64) (store.File, error) {
		if !h.photos.Verify(viewerID, ownerID, fileID, credential) {
			// Not logged: this path is reachable by anyone who can ask for a
			// photo, and a rejection is a client mistake, not a server event.
			return store.File{}, errLocationInvalid
		}
		file, err := h.store.ProfilePhotoForDownload(ctx, ownerID, fileID, viewerID)
		switch {
		case errors.Is(err, store.ErrFileNotFound):
			return store.File{}, errLocationInvalid
		case err != nil:
			h.log.Error("profile photo for download", "user_id", viewerID, "err", err)
			return store.File{}, errInternal
		}
		return file, nil
	}
}
