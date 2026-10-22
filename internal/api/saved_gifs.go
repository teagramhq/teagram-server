package api

import (
	"encoding/binary"
	"errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func (h *handlers) handleSaveGif(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesSaveGifRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	document, ok := req.ID.(*tg.InputDocument)
	if !ok || document == nil || document.ID <= 0 {
		return nil, tgerr.New(400, "GIF_ID_INVALID")
	}
	if req.Unsave {
		if err := h.store.UnsaveGIF(r.Ctx, r.UserID, document.ID); err != nil {
			if errors.Is(err, store.ErrSavedGIFInvalid) {
				return nil, tgerr.New(400, "GIF_ID_INVALID")
			}
			return nil, err
		}
		return &tg.BoolTrue{}, nil
	}
	if err := h.store.SaveGIF(r.Ctx, r.UserID, document.ID, document.AccessHash); err != nil {
		if errors.Is(err, store.ErrSavedGIFInvalid) {
			return nil, tgerr.New(400, "GIF_ID_INVALID")
		}
		return nil, err
	}
	return &tg.BoolTrue{}, nil
}

func (h *handlers) handleGetSavedGifs(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetSavedGifsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	files, err := h.store.SavedGIFs(r.Ctx, r.UserID)
	if err != nil {
		return nil, err
	}
	hash := savedGIFsHash(files)
	if req.Hash != 0 && req.Hash == hash {
		return &tg.MessagesSavedGifsNotModified{}, nil
	}
	gifs := make([]tg.DocumentClass, 0, len(files))
	for _, file := range files {
		gifs = append(gifs, h.documentToTL(file))
	}
	return &tg.MessagesSavedGifs{Hash: hash, Gifs: gifs}, nil
}

func savedGIFsHash(files []store.File) int64 {
	if len(files) == 0 {
		return 0
	}

	const (
		offset = uint64(14695981039346656037)
		prime  = uint64(1099511628211)
	)
	hash := offset
	for _, file := range files {
		var id [8]byte
		binary.LittleEndian.PutUint64(id[:], uint64(file.ID)) //nolint:gosec // G115: persisted file IDs are positive.
		for _, value := range id {
			hash ^= uint64(value)
			hash *= prime
		}
	}
	if hash == 0 {
		return 1
	}
	return int64(hash) //nolint:gosec // G115: the protocol hash is an opaque 64-bit value.
}
