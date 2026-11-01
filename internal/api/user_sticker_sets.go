package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/stickervalidation"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	stickerSetCreateRateSurface    = "sticker_set_create"
	stickerSetShortNameRateSurface = stickerSetRateLimitSurface
	stickerSetLookupRateSurface    = stickerSetRateLimitSurface
	maxUserStickerEmojiRunes       = 20
	maxUserStickerKeywords         = 20
	maxUserStickerKeywordBytes     = 64
)

var (
	stickerShortNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	botTokenPattern         = regexp.MustCompile(`\d{5,}:[A-Za-z0-9_-]{30,}`)
)

func validStickerShortName(name string) bool {
	return stickerShortNamePattern.MatchString(name)
}

func (h *handlers) stickerSetNameExists(r *mtproto.Request, name string, catalog *stickerSetCatalog) (bool, error) {
	if _, ok := catalog.byShortName[strings.ToLower(name)]; ok {
		return true, nil
	}
	return h.store.UserStickerSetShortNameExists(r.Ctx, name)
}

func (h *handlers) userStickerSetByRef(r *mtproto.Request, ref tg.InputStickerSetClass) (store.UserStickerSet, error) {
	switch value := ref.(type) {
	case *tg.InputStickerSetShortName:
		if value == nil || len(value.ShortName) > maxStickerSetShortNameBytes {
			return store.UserStickerSet{}, store.ErrStickerSetNotFound
		}
		return h.store.UserStickerSetByShortName(r.Ctx, value.ShortName)
	case *tg.InputStickerSetID:
		if value == nil || value.ID <= 0 || value.AccessHash == 0 {
			return store.UserStickerSet{}, store.ErrStickerSetNotFound
		}
		return h.store.UserStickerSetByID(r.Ctx, value.ID, value.AccessHash)
	default:
		return store.UserStickerSet{}, store.ErrStickerSetNotFound
	}
}

func (h *handlers) resolvedStickerSetID(r *mtproto.Request, catalog *stickerSetCatalog, ref tg.InputStickerSetClass) (int64, error) {
	if set, ok := catalog.resolve(ref); ok {
		return set.set.ID, nil
	}
	set, err := h.userStickerSetByRef(r, ref)
	if err == nil {
		return set.ID, nil
	}
	if errors.Is(err, store.ErrStickerSetNotFound) {
		return 0, errStickerSetInvalid
	}
	h.log.Error("resolve user sticker set", "user_id", r.UserID, "err", err)
	return 0, errInternal
}

func installedStickerSetDate(installed []store.InstalledStickerSet, id int64) *time.Time {
	for _, set := range installed {
		if set.ID == id {
			return &set.InstalledAt
		}
	}
	return nil
}

func userStickerSetSummaryTL(set store.UserStickerSet, ownerID int64, dcID int, installedAt *time.Time) tg.StickerSet {
	result := tg.StickerSet{
		ID: set.ID, AccessHash: set.AccessHash, Title: set.Title,
		ShortName: set.ShortName, Count: set.ItemCount, Hash: set.Hash,
		Masks: set.Kind == "mask", Emojis: set.Kind == "emojis", TextColor: set.TextColor,
		Creator: set.CreatorID == ownerID,
	}
	if installedAt != nil {
		result.SetInstalledDate(int(installedAt.Unix()))
	}
	if set.Thumbnail != nil {
		result.SetThumbs([]tg.PhotoSizeClass{&tg.PhotoSize{
			Type: "s", W: set.Thumbnail.Width, H: set.Thumbnail.Height, Size: int(set.Thumbnail.Size),
		}})
		result.SetThumbDCID(dcID)
		result.SetThumbVersion(set.ThumbVersion)
	}
	if set.ThumbDocumentID != 0 {
		result.SetThumbDocumentID(set.ThumbDocumentID)
	}
	result.SetFlags()
	return result
}

func userStickerSetTL(set store.UserStickerSet, ownerID int64, dcID int, installedAt *time.Time) *tg.MessagesStickerSet {
	result := &tg.MessagesStickerSet{
		Set:       userStickerSetSummaryTL(set, ownerID, dcID, installedAt),
		Packs:     make([]tg.StickerPack, 0, len(set.Items)),
		Keywords:  make([]tg.StickerKeyword, 0, len(set.Items)),
		Documents: make([]tg.DocumentClass, 0, len(set.Items)),
	}
	packIndexes := make(map[string]int, len(set.Items))
	for _, item := range set.Items {
		document := userStickerDocumentTL(set, item, dcID)
		result.Documents = append(result.Documents, document)
		if index, ok := packIndexes[item.Emoji]; ok {
			result.Packs[index].Documents = append(result.Packs[index].Documents, item.FileID)
		} else {
			packIndexes[item.Emoji] = len(result.Packs)
			result.Packs = append(result.Packs, tg.StickerPack{Emoticon: item.Emoji, Documents: []int64{item.FileID}})
		}
		if item.Keywords != "" {
			keywords := strings.Split(item.Keywords, ",")
			result.Keywords = append(result.Keywords, tg.StickerKeyword{DocumentID: item.FileID, Keyword: keywords})
		}
	}
	return result
}

func userStickerDocumentTL(set store.UserStickerSet, item store.UserStickerSetItem, dcID int) *tg.Document {
	attributes := make([]tg.DocumentAttributeClass, 0, 4)
	attributes = append(attributes, &tg.DocumentAttributeImageSize{W: item.Width, H: item.Height})
	setRef := &tg.InputStickerSetID{ID: set.ID, AccessHash: set.AccessHash}
	if item.Format == string(stickervalidation.FormatTGS) {
		attributes = append(attributes, &tg.DocumentAttributeAnimated{})
	}
	if item.Format == string(stickervalidation.FormatWEBM) {
		attributes = append(attributes, &tg.DocumentAttributeVideo{
			Nosound:  true,
			Duration: float64(item.Duration) / float64(time.Second),
			W:        item.Width,
			H:        item.Height,
		})
	}
	if set.Kind == "emojis" {
		attributes = append(attributes, &tg.DocumentAttributeCustomEmoji{
			Free: true, TextColor: set.TextColor, Alt: item.Emoji, Stickerset: setRef,
		})
	} else {
		sticker := &tg.DocumentAttributeSticker{Alt: item.Emoji, Stickerset: setRef}
		if set.Kind == "mask" {
			sticker.SetMask(true)
			if item.MaskCoords != "" && item.MaskCoords != "null" {
				var coords tg.MaskCoords
				if err := json.Unmarshal([]byte(item.MaskCoords), &coords); err == nil {
					sticker.SetMaskCoords(coords)
				}
			}
		}
		attributes = append(attributes, sticker)
	}
	filename := "sticker." + strings.ToLower(item.Format)
	attributes = append(attributes, &tg.DocumentAttributeFilename{FileName: filename})
	return &tg.Document{
		ID: item.FileID, AccessHash: item.AccessHash,
		FileReference: binary.BigEndian.AppendUint64(nil, uint64(item.FileID)), //nolint:gosec // Positive file id is an opaque document reference.
		Date:          int(item.Date.Unix()), MimeType: stickerMIME(item.Format), Size: item.Size,
		DCID: dcID, Attributes: attributes,
	}
}

func stickerMIME(format string) string {
	switch format {
	case string(stickervalidation.FormatWEBP):
		return "image/webp"
	case string(stickervalidation.FormatTGS):
		return "application/x-tgsticker"
	case string(stickervalidation.FormatWEBM):
		return "video/webm"
	default:
		return "application/octet-stream"
	}
}

func userStickerSetResponse(set store.UserStickerSet, ownerID int64, dcID int, installedAt *time.Time) *tg.MessagesStickerSet {
	return userStickerSetTL(set, ownerID, dcID, installedAt)
}

func (h *handlers) addUserStickerMedia(ctx context.Context, media map[int64]tg.MessageMediaClass, documentIDs []int64) error {
	if len(documentIDs) == 0 {
		return nil
	}
	sets, err := h.store.UserStickerSetsByFileIDs(ctx, documentIDs)
	if err != nil {
		return fmt.Errorf("load user sticker message media: %w", err)
	}
	for id, document := range sets {
		media[id] = &tg.MessageMediaDocument{Document: userStickerDocumentTL(document.Set, document.Item, h.dcID)}
	}
	return nil
}

func (h *handlers) handleCreateStickerSet(_ *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	var req tg.StickersCreateStickerSetRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, nil, errAuthKeyUnreg
	}
	if err := h.checkRateLimit(r, stickerSetCreateRateSurface, h.rateLimitCreateStickerSet); err != nil {
		return nil, nil, err
	}
	if len(req.Stickers) == 0 {
		return nil, nil, errStickersEmpty
	}
	maxStickers := store.MaxUserStickerCount
	kind := "regular"
	if req.Masks && req.Emojis {
		return nil, nil, errStickerSetInvalid
	}
	if req.Masks {
		kind = "mask"
	}
	if req.Emojis {
		kind = "emojis"
		maxStickers = store.MaxUserEmojiCount
	}
	if req.TextColor && kind != "emojis" {
		return nil, nil, errStickerSetInvalid
	}
	if _, hasThumb := req.GetThumb(); hasThumb {
		return nil, nil, rpcErr(400, "THUMBNAIL_INVALID")
	}
	if len(req.Stickers) > maxStickers {
		return nil, nil, errStickersTooMuch
	}
	if !validStickerSetTitle(req.Title) {
		return nil, nil, errStickerSetTitleInvalid
	}
	if !validStickerShortName(req.ShortName) {
		return nil, nil, errShortNameInvalid
	}
	software, _ := req.GetSoftware()
	if len(software) > 256 || botTokenPattern.MatchString(software) || !utf8.ValidString(software) || strings.ContainsRune(software, 0) {
		return nil, nil, errStickerSetInvalid
	}
	creatorID, err := h.inputUserID(req.UserID, r.UserID)
	if err != nil || creatorID != r.UserID {
		return nil, nil, errPeerIDInvalid
	}
	catalog, err := h.stickerSetCatalog()
	if err != nil {
		h.log.Error("load sticker set catalogue", "err", err)
		return nil, nil, errInternal
	}
	occupied, err := h.stickerSetNameExists(r, req.ShortName, catalog)
	if err != nil {
		h.log.Error("check sticker set name", "err", err)
		return nil, nil, errInternal
	}
	if occupied {
		return nil, nil, errShortNameOccupied
	}

	refs := make([]store.StickerFileRef, 0, len(req.Stickers))
	keywordsByPosition := make([]string, len(req.Stickers))
	maskCoordsByPosition := make([]string, len(req.Stickers))
	seen := make(map[int64]bool, len(req.Stickers))
	for position, input := range req.Stickers {
		document, ok := input.Document.(*tg.InputDocument)
		if !ok || document == nil || document.ID <= 0 || document.AccessHash == 0 || seen[document.ID] {
			return nil, nil, errStickerDocumentInvalid
		}
		seen[document.ID] = true
		keywords, _ := input.GetKeywords()
		if !validUserStickerEmoji(input.Emoji) {
			return nil, nil, errStickerEmojiInvalid
		}
		if !validUserStickerKeywords(keywords) {
			return nil, nil, errStickerSetInvalid
		}
		if kind == "mask" && keywords != "" {
			return nil, nil, errStickerSetInvalid
		}
		coords, hasCoords := input.GetMaskCoords()
		if hasCoords {
			if kind != "mask" || !validMaskCoords(coords) {
				return nil, nil, errStickerSetInvalid
			}
			encoded, marshalErr := json.Marshal(coords)
			if marshalErr != nil {
				return nil, nil, errStickerSetInvalid
			}
			maskCoordsByPosition[position] = string(encoded)
		}
		keywordsByPosition[position] = keywords
		refs = append(refs, store.StickerFileRef{ID: document.ID, AccessHash: document.AccessHash})
	}

	files, err := h.store.StickerFilesForSet(r.Ctx, creatorID, refs)
	if errors.Is(err, store.ErrStickerDocumentInvalid) {
		return nil, nil, errStickerDocumentInvalid
	}
	if err != nil {
		h.log.Error("resolve stored sticker files", "user_id", creatorID, "err", err)
		return nil, nil, errInternal
	}
	if len(files) != len(req.Stickers) {
		return nil, nil, errStickerDocumentInvalid
	}
	for _, file := range files {
		if file.Size <= 0 {
			return nil, nil, errStickerDocumentInvalid
		}
		if file.Size > store.MaxStoredStickerBytes {
			return nil, nil, rpcErr(400, "STICKER_FILE_SIZE")
		}
	}

	items := make([]store.UserStickerSetItem, 0, len(req.Stickers))
	target := stickervalidation.TargetSticker
	switch kind {
	case "mask":
		target = stickervalidation.TargetMaskSticker
	case "emojis":
		target = stickervalidation.TargetCustomEmoji
	}
	for position, input := range req.Stickers {
		file := files[position]
		data, readErr := h.store.ReadStickerFileForSet(r.Ctx, file)
		if errors.Is(readErr, store.ErrStickerFileTooLarge) {
			return nil, nil, rpcErr(400, "STICKER_FILE_SIZE")
		}
		if errors.Is(readErr, store.ErrStickerDocumentInvalid) {
			return nil, nil, errStickerDocumentInvalid
		}
		if readErr != nil {
			h.log.Error("read stored sticker bytes", "user_id", creatorID, "file_id", file.ID, "err", readErr)
			return nil, nil, errInternal
		}
		metadata, validateErr := stickervalidation.Validate(data, target)
		if validateErr != nil {
			if validationErr, ok := errors.AsType[*stickervalidation.ValidationError](validateErr); ok {
				return nil, nil, rpcErr(400, validationErr.Reason)
			}
			return nil, nil, errStickerDocumentInvalid
		}
		items = append(items, store.UserStickerSetItem{
			FileID: file.ID, AccessHash: file.AccessHash, Size: file.Size, Date: file.Date,
			Emoji: input.Emoji, Keywords: keywordsByPosition[position], Format: string(metadata.Format),
			Width: metadata.Width, Height: metadata.Height, Duration: int64(metadata.Duration), MaskCoords: maskCoordsByPosition[position],
		})
	}
	set, err := h.store.CreateUserStickerSet(r.Ctx, store.UserStickerSet{
		CreatorID: creatorID, Title: req.Title, ShortName: req.ShortName,
		Software: software, Kind: kind, TextColor: req.TextColor,
	}, items)
	if errors.Is(err, store.ErrStickerSetNameOccupied) {
		return nil, nil, errShortNameOccupied
	}
	if errors.Is(err, store.ErrStickerSetLimit) {
		return nil, nil, errStickersTooMuch
	}
	if errors.Is(err, store.ErrStickerDocumentInvalid) || errors.Is(err, store.ErrStickerFileAlreadyUsed) {
		return nil, nil, errStickerDocumentInvalid
	}
	if err != nil {
		h.log.Error("create user sticker set", "user_id", creatorID, "short_name", req.ShortName, "err", err)
		return nil, nil, errInternal
	}
	return userStickerSetResponse(set, r.UserID, h.dcID, nil), nil, nil
}

func validUserStickerEmoji(emoji string) bool {
	return utf8.ValidString(emoji) && emoji != "" && !strings.ContainsRune(emoji, 0) && utf8.RuneCountInString(emoji) <= maxUserStickerEmojiRunes
}

func validUserStickerKeywords(keywords string) bool {
	if !utf8.ValidString(keywords) || strings.ContainsRune(keywords, 0) {
		return false
	}
	if keywords == "" {
		return true
	}
	terms := strings.Split(keywords, ",")
	if len(terms) > maxUserStickerKeywords {
		return false
	}
	for _, term := range terms {
		if len(term) > maxUserStickerKeywordBytes {
			return false
		}
	}
	return true
}

func (h *handlers) checkStickerSetMutation(r *mtproto.Request) error {
	if r.UserID == 0 {
		return errAuthKeyUnreg
	}
	return h.checkRateLimit(r, stickerSetRateLimitSurface, stickerSetMutationRateLimit)
}

func (h *handlers) ownedUserStickerSet(r *mtproto.Request, ref tg.InputStickerSetClass) (store.UserStickerSet, error) {
	set, err := h.userStickerSetByRef(r, ref)
	if errors.Is(err, store.ErrStickerSetNotFound) {
		return store.UserStickerSet{}, errStickerSetInvalid
	}
	if err != nil {
		h.log.Error("resolve user sticker set for mutation", "user_id", r.UserID, "err", err)
		return store.UserStickerSet{}, errInternal
	}
	if set.CreatorID != r.UserID {
		return store.UserStickerSet{}, errStickerSetInvalid
	}
	return set, nil
}

func userStickerDocumentRef(input tg.InputDocumentClass) (store.StickerFileRef, bool) {
	document, ok := input.(*tg.InputDocument)
	if !ok || document == nil || document.ID <= 0 || document.AccessHash == 0 {
		return store.StickerFileRef{}, false
	}
	return store.StickerFileRef{ID: document.ID, AccessHash: document.AccessHash}, true
}

func stickerTargetForSet(kind string) (stickervalidation.Target, int, bool) {
	switch kind {
	case "regular":
		return stickervalidation.TargetSticker, store.MaxUserStickerCount, true
	case "mask":
		return stickervalidation.TargetMaskSticker, store.MaxUserStickerCount, true
	case "emojis":
		return stickervalidation.TargetCustomEmoji, store.MaxUserEmojiCount, true
	default:
		return "", 0, false
	}
}

func (h *handlers) handleAddStickerToSet(_ *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	var req tg.StickersAddStickerToSetRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if err := h.checkStickerSetMutation(r); err != nil {
		return nil, nil, err
	}
	if h.stickerImportMode == config.StickerImportOff {
		return nil, nil, errStickerImportDisabled
	}
	set, err := h.ownedUserStickerSet(r, req.Stickerset)
	if err != nil {
		return nil, nil, err
	}
	target, maxItems, ok := stickerTargetForSet(set.Kind)
	if !ok {
		return nil, nil, errStickerSetInvalid
	}
	if set.ItemCount >= maxItems {
		return nil, nil, errStickersTooMuch
	}
	ref, ok := userStickerDocumentRef(req.Sticker.Document)
	if !ok {
		return nil, nil, errStickerDocumentInvalid
	}
	if !validUserStickerEmoji(req.Sticker.Emoji) {
		return nil, nil, errStickerEmojiInvalid
	}
	keywords, _ := req.Sticker.GetKeywords()
	if !validUserStickerKeywords(keywords) || (set.Kind == "mask" && keywords != "") {
		return nil, nil, errStickerSetInvalid
	}
	maskCoords := ""
	coords, hasCoords := req.Sticker.GetMaskCoords()
	if hasCoords {
		if set.Kind != "mask" || !validMaskCoords(coords) {
			return nil, nil, errStickerSetInvalid
		}
		encoded, marshalErr := json.Marshal(coords)
		if marshalErr != nil {
			return nil, nil, errStickerSetInvalid
		}
		maskCoords = string(encoded)
	}
	files, err := h.store.StickerFilesForUserSet(r.Ctx, r.UserID, set.ID, []store.StickerFileRef{ref})
	if errors.Is(err, store.ErrStickerDocumentInvalid) {
		return nil, nil, errStickerDocumentInvalid
	}
	if err != nil {
		h.log.Error("resolve stored sticker file for add", "user_id", r.UserID, "file_id", ref.ID, "err", err)
		return nil, nil, errInternal
	}
	if len(files) != 1 || files[0].Size <= 0 {
		return nil, nil, errStickerDocumentInvalid
	}
	if files[0].Size > store.MaxStoredStickerBytes {
		return nil, nil, rpcErr(400, "STICKER_FILE_SIZE")
	}
	data, err := h.store.ReadStickerFileForSet(r.Ctx, files[0])
	if errors.Is(err, store.ErrStickerFileTooLarge) {
		return nil, nil, rpcErr(400, "STICKER_FILE_SIZE")
	}
	if errors.Is(err, store.ErrStickerDocumentInvalid) {
		return nil, nil, errStickerDocumentInvalid
	}
	if err != nil {
		h.log.Error("read stored sticker bytes for add", "user_id", r.UserID, "file_id", ref.ID, "err", err)
		return nil, nil, errInternal
	}
	metadata, err := stickervalidation.Validate(data, target)
	if err != nil {
		if validationErr, ok := errors.AsType[*stickervalidation.ValidationError](err); ok {
			return nil, nil, rpcErr(400, validationErr.Reason)
		}
		return nil, nil, errStickerDocumentInvalid
	}
	updated, err := h.store.AddUserStickerSetItem(r.Ctx, r.UserID, set.ID, store.UserStickerSetItem{
		FileID: files[0].ID, AccessHash: files[0].AccessHash, Size: files[0].Size, Date: files[0].Date,
		Emoji: req.Sticker.Emoji, Keywords: keywords, Format: string(metadata.Format),
		Width: metadata.Width, Height: metadata.Height, Duration: int64(metadata.Duration), MaskCoords: maskCoords,
	})
	if errors.Is(err, store.ErrStickerSetNotFound) {
		return nil, nil, errStickerSetInvalid
	}
	if errors.Is(err, store.ErrStickerSetLimit) {
		return nil, nil, errStickersTooMuch
	}
	if errors.Is(err, store.ErrStickerDocumentInvalid) || errors.Is(err, store.ErrStickerFileAlreadyUsed) {
		return nil, nil, errStickerDocumentInvalid
	}
	if err != nil {
		h.log.Error("add sticker to user set", "user_id", r.UserID, "set_id", set.ID, "err", err)
		return nil, nil, errInternal
	}
	return userStickerSetResponse(updated, r.UserID, h.dcID, nil), nil, nil
}

func (h *handlers) handleRemoveStickerFromSet(_ *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	var req tg.StickersRemoveStickerFromSetRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if err := h.checkStickerSetMutation(r); err != nil {
		return nil, nil, err
	}
	ref, ok := userStickerDocumentRef(req.Sticker)
	if !ok {
		return nil, nil, errStickerDocumentInvalid
	}
	set, err := h.store.RemoveUserStickerSetItem(r.Ctx, r.UserID, ref.ID, ref.AccessHash)
	if errors.Is(err, store.ErrStickerSetNotFound) {
		return nil, nil, errStickerSetInvalid
	}
	if errors.Is(err, store.ErrStickerDocumentInvalid) {
		return nil, nil, errStickerDocumentInvalid
	}
	if errors.Is(err, store.ErrStickerSetEmpty) {
		return nil, nil, errStickersEmpty
	}
	if err != nil {
		h.log.Error("remove sticker from user set", "user_id", r.UserID, "file_id", ref.ID, "err", err)
		return nil, nil, errInternal
	}
	return userStickerSetResponse(set, r.UserID, h.dcID, nil), nil, nil
}

func (h *handlers) handleChangeStickerPosition(_ *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	var req tg.StickersChangeStickerPositionRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if err := h.checkStickerSetMutation(r); err != nil {
		return nil, nil, err
	}
	ref, ok := userStickerDocumentRef(req.Sticker)
	if !ok {
		return nil, nil, errStickerDocumentInvalid
	}
	set, err := h.store.ChangeUserStickerSetItemPosition(r.Ctx, r.UserID, ref.ID, ref.AccessHash, req.Position)
	if errors.Is(err, store.ErrStickerSetNotFound) {
		return nil, nil, errStickerSetInvalid
	}
	if errors.Is(err, store.ErrStickerDocumentInvalid) {
		return nil, nil, errStickerDocumentInvalid
	}
	if err != nil {
		h.log.Error("change user sticker position", "user_id", r.UserID, "file_id", ref.ID, "err", err)
		return nil, nil, errInternal
	}
	return userStickerSetResponse(set, r.UserID, h.dcID, nil), nil, nil
}

func (h *handlers) handleChangeSticker(_ *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	var req tg.StickersChangeStickerRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if err := h.checkStickerSetMutation(r); err != nil {
		return nil, nil, err
	}
	ref, ok := userStickerDocumentRef(req.Sticker)
	if !ok {
		return nil, nil, errStickerDocumentInvalid
	}
	set, err := h.store.UserStickerSetByOwnedSticker(r.Ctx, r.UserID, ref.ID, ref.AccessHash)
	if errors.Is(err, store.ErrStickerSetNotFound) {
		return nil, nil, errStickerSetInvalid
	}
	if err != nil {
		h.log.Error("resolve user sticker set for change", "user_id", r.UserID, "file_id", ref.ID, "err", err)
		return nil, nil, errInternal
	}
	emoji, hasEmoji := req.GetEmoji()
	keywords, hasKeywords := req.GetKeywords()
	coords, hasCoords := req.GetMaskCoords()
	if !hasEmoji && !hasKeywords && !hasCoords {
		return nil, nil, errStickerSetInvalid
	}
	if hasEmoji && !validUserStickerEmoji(emoji) {
		return nil, nil, errStickerEmojiInvalid
	}
	if hasKeywords && (!validUserStickerKeywords(keywords) || (set.Kind == "mask" && keywords != "")) {
		return nil, nil, errStickerSetInvalid
	}
	var maskCoords *string
	if hasCoords {
		if set.Kind != "mask" || !validMaskCoords(coords) {
			return nil, nil, errStickerSetInvalid
		}
		encoded, marshalErr := json.Marshal(coords)
		if marshalErr != nil {
			return nil, nil, errStickerSetInvalid
		}
		value := string(encoded)
		maskCoords = &value
	}
	var emojiUpdate, keywordsUpdate *string
	if hasEmoji {
		emojiUpdate = &emoji
	}
	if hasKeywords {
		keywordsUpdate = &keywords
	}
	updated, err := h.store.ChangeUserStickerSetItem(r.Ctx, r.UserID, ref.ID, ref.AccessHash, store.UserStickerSetItemUpdate{
		Emoji: emojiUpdate, Keywords: keywordsUpdate, MaskCoords: maskCoords,
	})
	if errors.Is(err, store.ErrStickerSetNotFound) {
		return nil, nil, errStickerSetInvalid
	}
	if errors.Is(err, store.ErrStickerDocumentInvalid) {
		return nil, nil, errStickerDocumentInvalid
	}
	if err != nil {
		h.log.Error("change user sticker metadata", "user_id", r.UserID, "file_id", ref.ID, "err", err)
		return nil, nil, errInternal
	}
	return userStickerSetResponse(updated, r.UserID, h.dcID, nil), nil, nil
}

func (h *handlers) handleSetStickerSetThumb(_ *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	var req tg.StickersSetStickerSetThumbRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if err := h.checkStickerSetMutation(r); err != nil {
		return nil, nil, err
	}
	set, err := h.ownedUserStickerSet(r, req.Stickerset)
	if err != nil {
		return nil, nil, err
	}
	thumbInput, hasThumb := req.GetThumb()
	thumbDocumentID, hasThumbDocumentID := req.GetThumbDocumentID()
	var thumbnail *store.UserStickerSetThumbnail
	if set.Kind == "emojis" {
		if hasThumb {
			return nil, nil, errStickerSetInvalid
		}
		if !hasThumbDocumentID {
			thumbDocumentID = 0
		}
	} else {
		if hasThumbDocumentID {
			return nil, nil, errStickerSetInvalid
		}
		thumbDocumentID = 0
		if hasThumb {
			ref, ok := userStickerDocumentRef(thumbInput)
			if !ok {
				return nil, nil, errStickerDocumentInvalid
			}
			files, fileErr := h.store.StickerFilesForSet(r.Ctx, r.UserID, []store.StickerFileRef{ref})
			if errors.Is(fileErr, store.ErrStickerDocumentInvalid) {
				return nil, nil, errStickerDocumentInvalid
			}
			if fileErr != nil {
				h.log.Error("resolve sticker set thumbnail", "user_id", r.UserID, "file_id", ref.ID, "err", fileErr)
				return nil, nil, errInternal
			}
			if len(files) != 1 || files[0].Size <= 0 {
				return nil, nil, errStickerDocumentInvalid
			}
			data, readErr := h.store.ReadStickerFileForSet(r.Ctx, files[0])
			if errors.Is(readErr, store.ErrStickerFileTooLarge) {
				return nil, nil, rpcErr(400, "STICKER_FILE_SIZE")
			}
			if errors.Is(readErr, store.ErrStickerDocumentInvalid) {
				return nil, nil, errStickerDocumentInvalid
			}
			if readErr != nil {
				h.log.Error("read sticker set thumbnail", "user_id", r.UserID, "file_id", ref.ID, "err", readErr)
				return nil, nil, errInternal
			}
			metadata, validateErr := stickervalidation.Validate(data, stickervalidation.TargetSetThumbnail)
			if validateErr != nil {
				if validationErr, ok := errors.AsType[*stickervalidation.ValidationError](validateErr); ok {
					return nil, nil, rpcErr(400, validationErr.Reason)
				}
				return nil, nil, errStickerDocumentInvalid
			}
			thumbnail = &store.UserStickerSetThumbnail{
				FileID: files[0].ID, AccessHash: files[0].AccessHash, Size: files[0].Size, Format: string(metadata.Format),
				Width: metadata.Width, Height: metadata.Height, Duration: int64(metadata.Duration),
			}
		}
	}
	updated, err := h.store.SetUserStickerSetThumb(r.Ctx, r.UserID, set.ID, thumbnail, thumbDocumentID)
	if errors.Is(err, store.ErrStickerSetNotFound) {
		return nil, nil, errStickerSetInvalid
	}
	if errors.Is(err, store.ErrStickerDocumentInvalid) {
		return nil, nil, errStickerDocumentInvalid
	}
	if err != nil {
		h.log.Error("set user sticker thumbnail", "user_id", r.UserID, "set_id", set.ID, "err", err)
		return nil, nil, errInternal
	}
	return userStickerSetResponse(updated, r.UserID, h.dcID, nil), nil, nil
}

func (h *handlers) handleRenameStickerSet(_ *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	var req tg.StickersRenameStickerSetRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if err := h.checkStickerSetMutation(r); err != nil {
		return nil, nil, err
	}
	if !validStickerSetTitle(req.Title) {
		return nil, nil, errStickerSetTitleInvalid
	}
	set, err := h.ownedUserStickerSet(r, req.Stickerset)
	if err != nil {
		return nil, nil, err
	}
	updated, err := h.store.RenameUserStickerSet(r.Ctx, r.UserID, set.ID, req.Title)
	if errors.Is(err, store.ErrStickerSetNotFound) {
		return nil, nil, errStickerSetInvalid
	}
	if err != nil {
		h.log.Error("rename user sticker set", "user_id", r.UserID, "set_id", set.ID, "err", err)
		return nil, nil, errInternal
	}
	return userStickerSetResponse(updated, r.UserID, h.dcID, nil), nil, nil
}

func (h *handlers) handleDeleteStickerSet(_ *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	var req tg.StickersDeleteStickerSetRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if err := h.checkStickerSetMutation(r); err != nil {
		return nil, nil, err
	}
	set, err := h.ownedUserStickerSet(r, req.Stickerset)
	if err != nil {
		return nil, nil, err
	}
	if err = h.store.DeleteUserStickerSet(r.Ctx, r.UserID, set.ID); errors.Is(err, store.ErrStickerSetNotFound) {
		return nil, nil, errStickerSetInvalid
	} else if err != nil {
		h.log.Error("delete user sticker set", "user_id", r.UserID, "set_id", set.ID, "err", err)
		return nil, nil, errInternal
	}
	return &tg.BoolTrue{}, nil, nil
}

func validStickerSetTitle(title string) bool {
	return utf8.ValidString(title) && !strings.ContainsRune(title, 0) && utf8.RuneCountInString(title) > 0 && utf8.RuneCountInString(title) <= 64
}

func validMaskCoords(coords tg.MaskCoords) bool {
	return coords.N >= 0 && coords.N <= 3 &&
		!math.IsNaN(coords.X) && !math.IsInf(coords.X, 0) &&
		!math.IsNaN(coords.Y) && !math.IsInf(coords.Y, 0) &&
		!math.IsNaN(coords.Zoom) && !math.IsInf(coords.Zoom, 0) && coords.Zoom > 0
}

func (h *handlers) handleCheckStickerSetShortName(_ *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	var req tg.StickersCheckShortNameRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, nil, errAuthKeyUnreg
	}
	if err := h.checkRateLimit(r, stickerSetShortNameRateSurface, stickerSetMutationRateLimit); err != nil {
		return nil, nil, err
	}
	if !validStickerShortName(req.ShortName) {
		return nil, nil, errShortNameInvalid
	}
	catalog, err := h.stickerSetCatalog()
	if err != nil {
		h.log.Error("load sticker set catalogue", "err", err)
		return nil, nil, errInternal
	}
	occupied, err := h.stickerSetNameExists(r, req.ShortName, catalog)
	if err != nil {
		h.log.Error("check sticker set short name", "err", err)
		return nil, nil, errInternal
	}
	if occupied {
		return &tg.BoolFalse{}, nil, nil
	}
	return &tg.BoolTrue{}, nil, nil
}

func (h *handlers) handleSuggestStickerSetShortName(_ *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	var req tg.StickersSuggestShortNameRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, nil, errAuthKeyUnreg
	}
	if err := h.checkRateLimit(r, stickerSetShortNameRateSurface, stickerSetMutationRateLimit); err != nil {
		return nil, nil, err
	}
	catalog, err := h.stickerSetCatalog()
	if err != nil {
		h.log.Error("load sticker set catalogue", "err", err)
		return nil, nil, errInternal
	}
	base := stickerNameSlug(req.Title)
	candidates := make([]string, 0, 1000)
	for suffix := range 1000 {
		candidate := base
		if suffix > 0 {
			tail := fmt.Sprintf("_%d", suffix)
			candidate = strings.TrimRight(base[:min(len(base), maxStickerSetShortNameBytes-len(tail))], "_") + tail
		}
		candidates = append(candidates, candidate)
	}
	userOccupied, err := h.store.UserStickerSetOccupiedShortNames(r.Ctx, candidates)
	if err != nil {
		h.log.Error("check suggested sticker set short names", "err", err)
		return nil, nil, errInternal
	}
	for _, candidate := range candidates {
		key := strings.ToLower(candidate)
		if _, occupied := catalog.byShortName[key]; occupied {
			continue
		}
		if _, occupied := userOccupied[key]; !occupied {
			return &tg.StickersSuggestedShortName{ShortName: candidate}, nil, nil
		}
	}
	return nil, nil, rpcErr(400, "SHORT_NAME_OCCUPIED")
}

func stickerNameSlug(title string) string {
	var b strings.Builder
	underscore := false
	for _, r := range title {
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			underscore = false
		} else if b.Len() > 0 && !underscore {
			b.WriteByte('_')
			underscore = true
		}
	}
	base := strings.Trim(b.String(), "_")
	if base == "" || base[0] < 'a' || base[0] > 'z' {
		base = "stickers_" + base
	}
	base = strings.TrimRight(base, "_")
	if len(base) > maxStickerSetShortNameBytes {
		base = strings.TrimRight(base[:maxStickerSetShortNameBytes], "_")
	}
	if base == "" {
		return "stickers"
	}
	return base
}
