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

func userStickerSetSummaryTL(set store.UserStickerSet, ownerID int64, installedAt *time.Time) tg.StickerSet {
	result := tg.StickerSet{
		ID: set.ID, AccessHash: set.AccessHash, Title: set.Title,
		ShortName: set.ShortName, Count: set.ItemCount, Hash: set.Hash,
		Masks: set.Kind == "mask", Emojis: set.Kind == "emojis", TextColor: set.TextColor,
		Creator: set.CreatorID == ownerID,
	}
	if installedAt != nil {
		result.SetInstalledDate(int(installedAt.Unix()))
	}
	result.SetFlags()
	return result
}

func userStickerSetTL(set store.UserStickerSet, ownerID int64, dcID int, installedAt *time.Time) *tg.MessagesStickerSet {
	result := &tg.MessagesStickerSet{
		Set:       userStickerSetSummaryTL(set, ownerID, installedAt),
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
	if errors.Is(err, store.ErrStickerDocumentInvalid) || errors.Is(err, store.ErrStickerFileAlreadyUsed) || errors.Is(err, store.ErrStickerFileBlocked) {
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

// handleAddStickerToSetUnsupported keeps the mutation entry point rate limited
// while set editing remains outside this feature's scope.
func (h *handlers) handleAddStickerToSetUnsupported(_ *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	var req tg.StickersAddStickerToSetRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, nil, errAuthKeyUnreg
	}
	if err := h.checkRateLimit(r, stickerSetRateLimitSurface, stickerSetMutationRateLimit); err != nil {
		return nil, nil, err
	}
	return nil, nil, errMethodNotImpl
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
