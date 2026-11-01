package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/reactionassets"
	"github.com/teagramhq/teagram-server/internal/stickerbundle"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	twemojiReactionSetShortName = "twemoji_reactions"
	twemojiReactionSetTitle     = "Twemoji Reactions"
	twemojiReactionDocumentDate = 1_735_689_600 // 2025-01-01 00:00:00 UTC.
	stickerSetRateLimitSurface  = "sticker_set_mutation"
	maxStickerSetShortNameBytes = 64
)

var stickerSetMutationRateLimit = store.RateLimitConfig{Limit: 60, Window: time.Minute}

type builtInStickerSet struct {
	set       tg.StickerSet
	documents []*tg.Document
	packs     []tg.StickerPack
}

type stickerSetCatalog struct {
	byShortName   map[string]*builtInStickerSet
	byID          map[int64]*builtInStickerSet
	featured      []*builtInStickerSet
	documents     map[reactionDocumentIdentity]builtInReactionDocument
	documentsByID map[int64]*tg.Document
}

// StickerSetCatalog is a fully validated catalogue view suitable for sharing
// with the handlers created for one server process.
type StickerSetCatalog = stickerSetCatalog

var (
	stickerSetCatalogOnce sync.Once
	stickerSetCatalogVal  *stickerSetCatalog
	errStickerSetCatalog  error
)

func getStickerSetCatalog() (*stickerSetCatalog, error) {
	stickerSetCatalogOnce.Do(func() {
		stickerSetCatalogVal, errStickerSetCatalog = buildStickerSetCatalog(nil)
	})
	return stickerSetCatalogVal, errStickerSetCatalog
}

func (h *handlers) stickerSetCatalog() (*stickerSetCatalog, error) {
	if h.stickerSets != nil {
		return h.stickerSets, nil
	}
	return getStickerSetCatalog()
}

// LoadStickerSetCatalog combines the built-in set with a previously validated
// optional bundle. Bundle files stay on disk and are read by range on demand.
func LoadStickerSetCatalog(bundle *stickerbundle.Bundle) (*StickerSetCatalog, error) {
	return buildStickerSetCatalog(bundle)
}

func buildStickerSetCatalog(bundles ...*stickerbundle.Bundle) (*stickerSetCatalog, error) {
	if len(bundles) > 1 {
		return nil, errors.New("sticker catalogue accepts at most one generated bundle")
	}
	var bundle *stickerbundle.Bundle
	if len(bundles) == 1 {
		bundle = bundles[0]
	}
	catalog, err := buildTwemojiStickerSetCatalog()
	if err != nil {
		return nil, err
	}
	if bundle != nil {
		if err := appendStickerBundle(catalog, bundle); err != nil {
			return nil, err
		}
	}
	reactions, err := getReactionDocumentCatalog()
	if err != nil {
		return nil, fmt.Errorf("load standard reaction documents for sticker collision check: %w", err)
	}
	if err := validateStickerSetCatalogIdentities(catalog, reactions); err != nil {
		return nil, err
	}
	return catalog, nil
}

func buildTwemojiStickerSetCatalog() (*stickerSetCatalog, error) {
	entries, err := reactionassets.Manifest()
	if err != nil {
		return nil, fmt.Errorf("read twemoji sticker manifest: %w", err)
	}
	if len(entries) != 73 {
		return nil, fmt.Errorf("twemoji sticker manifest has %d entries, want 73", len(entries))
	}
	setInput := make([]byte, 0, 32+len(twemojiReactionSetTitle)+len(twemojiReactionSetShortName))
	setInput = appendHashPart(setInput, []byte(twemojiReactionSetShortName))
	setInput = appendHashPart(setInput, []byte(twemojiReactionSetTitle))
	for i, entry := range entries {
		stem := strings.TrimSuffix(entry.SVG, ".svg")
		if stem == entry.SVG || stem == "" || path.Base(entry.SVG) != entry.SVG {
			return nil, fmt.Errorf("twemoji sticker %d has invalid asset name %q", i, entry.SVG)
		}
		filename := stem + ".webp"
		data, err := reactionassets.Files.ReadFile("static/" + filename)
		if err != nil {
			return nil, fmt.Errorf("read twemoji sticker %q WebP: %w", entry.Emoji, err)
		}
		if len(data) < 12 || !bytes.Equal(data[:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WEBP")) {
			return nil, fmt.Errorf("twemoji sticker %q is not a WebP image", entry.Emoji)
		}
		setInput = appendHashPart(setInput, []byte(entry.Emoji))
		setInput = appendHashPart(setInput, []byte(filename))
		setInput = appendHashPart(setInput, data)
	}
	setDigest := namespacedDigest("teagram-sticker-set-v1", setInput)
	setID := negativeContentID(setDigest[:8])
	setAccessHash := positiveContentHash(setDigest[8:16])
	setHash := int(binary.BigEndian.Uint32(setDigest[24:28]) & 0x7fffffff)
	if setHash == 0 {
		setHash = 1
	}

	set := &builtInStickerSet{
		set: tg.StickerSet{
			ID:         setID,
			AccessHash: setAccessHash,
			Title:      twemojiReactionSetTitle,
			ShortName:  twemojiReactionSetShortName,
			Count:      len(entries),
			Hash:       setHash,
		},
		documents: make([]*tg.Document, 0, len(entries)),
		packs:     make([]tg.StickerPack, 0, len(entries)),
	}
	set.set.SetFlags()

	catalog := &stickerSetCatalog{
		byShortName:   map[string]*builtInStickerSet{strings.ToLower(set.set.ShortName): set},
		byID:          map[int64]*builtInStickerSet{set.set.ID: set},
		featured:      []*builtInStickerSet{set},
		documents:     make(map[reactionDocumentIdentity]builtInReactionDocument, len(entries)),
		documentsByID: make(map[int64]*tg.Document, len(entries)),
	}
	for _, entry := range entries {
		stem := strings.TrimSuffix(entry.SVG, ".svg")
		filename := stem + ".webp"
		data, err := reactionassets.Files.ReadFile("static/" + filename)
		if err != nil {
			return nil, fmt.Errorf("read twemoji sticker %q WebP: %w", entry.Emoji, err)
		}
		digest := namespacedDigest("teagram-sticker-document-v1", appendHashPart(nil,
			[]byte(twemojiReactionSetShortName), []byte(entry.Emoji), []byte(filename), data,
		))
		id := negativeContentID(digest[:8])
		accessHash := positiveContentHash(digest[8:16])
		document := &tg.Document{
			ID:            id,
			AccessHash:    accessHash,
			FileReference: bytes.Clone(digest[16:]),
			Date:          twemojiReactionDocumentDate,
			MimeType:      "image/webp",
			Size:          int64(len(data)),
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeImageSize{W: 512, H: 512},
				&tg.DocumentAttributeSticker{
					Alt:        entry.Emoji,
					Stickerset: &tg.InputStickerSetID{ID: set.set.ID, AccessHash: set.set.AccessHash},
				},
				&tg.DocumentAttributeFilename{FileName: filename},
			},
		}
		set.documents = append(set.documents, document)
		set.packs = append(set.packs, tg.StickerPack{Emoticon: entry.Emoji, Documents: []int64{id}})
		catalog.documentsByID[id] = document
		catalog.documents[reactionDocumentIdentity{id: id, accessHash: accessHash}] = builtInReactionDocument{
			file: reactionStoreFile(document, data),
			data: data,
		}
	}
	return catalog, nil
}

func appendStickerBundle(catalog *stickerSetCatalog, bundle *stickerbundle.Bundle) error {
	for _, pack := range bundle.Packs {
		if _, exists := catalog.byShortName[strings.ToLower(pack.ShortName)]; exists {
			return fmt.Errorf("sticker bundle pack %q collides with an existing sticker set", pack.ShortName)
		}
		setIdentity := namespacedDigest("teagram-bundle-sticker-set-identity-v1", appendHashPart(nil, []byte(pack.ShortName)))
		setContent := appendHashPart(nil, []byte(pack.ShortName), []byte(pack.Title), []byte(pack.Attribution))
		for _, sticker := range pack.Stickers {
			setContent = appendHashPart(setContent, []byte(sticker.Emoji), []byte(sticker.File), []byte(strconv.FormatInt(sticker.Size, 10)), sticker.SHA256[:])
		}
		setDigest := namespacedDigest("teagram-bundle-sticker-set-content-v1", setContent)
		setHash := int(binary.BigEndian.Uint32(setDigest[24:28]) & 0x7fffffff)
		if setHash == 0 {
			setHash = 1
		}
		set := &builtInStickerSet{
			set: tg.StickerSet{
				ID:         negativeContentID(setIdentity[:8]),
				AccessHash: positiveContentHash(setIdentity[8:16]),
				Title:      pack.Title,
				ShortName:  pack.ShortName,
				Count:      len(pack.Stickers),
				Hash:       setHash,
			},
			documents: make([]*tg.Document, 0, len(pack.Stickers)),
			packs:     make([]tg.StickerPack, 0, len(pack.Stickers)),
		}
		set.set.SetFlags()
		for _, sticker := range pack.Stickers {
			filename := path.Base(sticker.File)
			digest := namespacedDigest("teagram-bundle-sticker-document-v1", appendHashPart(nil,
				[]byte(pack.ShortName), []byte(sticker.Emoji), []byte(sticker.File), sticker.SHA256[:],
			))
			attributes := make([]tg.DocumentAttributeClass, 0, 3)
			attributes = append(attributes,
				&tg.DocumentAttributeImageSize{W: 512, H: 512},
				&tg.DocumentAttributeSticker{
					Alt:        sticker.Emoji,
					Stickerset: &tg.InputStickerSetID{ID: set.set.ID, AccessHash: set.set.AccessHash},
				},
				&tg.DocumentAttributeFilename{FileName: filename},
			)
			document := &tg.Document{
				ID:            negativeContentID(digest[:8]),
				AccessHash:    positiveContentHash(digest[8:16]),
				FileReference: bytes.Clone(digest[16:]),
				Date:          twemojiReactionDocumentDate,
				MimeType:      sticker.MimeType,
				Size:          sticker.Size,
				Attributes:    attributes,
			}
			set.documents = append(set.documents, document)
			set.packs = append(set.packs, tg.StickerPack{Emoticon: sticker.Emoji, Documents: []int64{document.ID}})
			catalog.documentsByID[document.ID] = document
			file := reactionStoreFile(document, nil)
			file.Size = sticker.Size
			catalog.documents[reactionDocumentIdentity{id: document.ID, accessHash: document.AccessHash}] = builtInReactionDocument{
				file: file,
				path: sticker.Path,
			}
		}
		catalog.byShortName[strings.ToLower(set.set.ShortName)] = set
		catalog.byID[set.set.ID] = set
		catalog.featured = append(catalog.featured, set)
	}
	return nil
}

func appendHashPart(dst []byte, parts ...[]byte) []byte {
	for _, part := range parts {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		dst = append(dst, size[:]...)
		dst = append(dst, part...)
	}
	return dst
}

func namespacedDigest(namespace string, input []byte) [32]byte {
	content := appendHashPart([]byte(namespace), input)
	return sha256.Sum256(content)
}

func negativeContentID(prefix []byte) int64 {
	const maxSignedLong = uint64(1<<63 - 1)
	value := binary.BigEndian.Uint64(prefix[:8]) & maxSignedLong
	if value == 0 {
		value = 1
	}
	return -int64(value)
}

func positiveContentHash(prefix []byte) int64 {
	const maxSignedLong = uint64(1<<63 - 1)
	value := binary.BigEndian.Uint64(prefix[:8]) & maxSignedLong
	if value == 0 {
		value = 1
	}
	return int64(value)
}

func validateStickerSetCatalog(set *builtInStickerSet, reactions *reactionDocumentCatalog) error {
	if set.set.ID >= 0 || set.set.AccessHash == 0 || set.set.Hash == 0 || set.set.Count != len(set.documents) {
		return errors.New("twemoji sticker set has invalid identity or count")
	}
	identities := make(map[int64]string, len(reactions.documents)*2+len(set.documents)*2+3)
	addIdentity := func(value int64, description string) error {
		if existing, ok := identities[value]; ok {
			return fmt.Errorf("built-in sticker identity collision: %s collides with %s", description, existing)
		}
		identities[value] = description
		return nil
	}
	for identity := range reactions.documents {
		if err := addIdentity(identity.id, fmt.Sprintf("reaction document id %d", identity.id)); err != nil {
			return err
		}
		if err := addIdentity(identity.accessHash, fmt.Sprintf("reaction document access hash %d", identity.accessHash)); err != nil {
			return err
		}
	}
	if err := addIdentity(set.set.ID, fmt.Sprintf("sticker set id %d", set.set.ID)); err != nil {
		return err
	}
	if err := addIdentity(set.set.AccessHash, fmt.Sprintf("sticker set access hash %d", set.set.AccessHash)); err != nil {
		return err
	}
	if err := addIdentity(int64(set.set.Hash), fmt.Sprintf("sticker set hash %d", set.set.Hash)); err != nil {
		return err
	}
	seenDocuments := make(map[int64]struct{}, len(set.documents))
	for _, document := range set.documents {
		if document.ID >= 0 {
			return fmt.Errorf("built-in sticker document id %d is not in the reserved negative range", document.ID)
		}
		if _, ok := seenDocuments[document.ID]; ok {
			return fmt.Errorf("built-in sticker document id %d is duplicated", document.ID)
		}
		seenDocuments[document.ID] = struct{}{}
		if err := addIdentity(document.ID, fmt.Sprintf("sticker document id %d", document.ID)); err != nil {
			return err
		}
		if err := addIdentity(document.AccessHash, fmt.Sprintf("sticker document access hash %d", document.AccessHash)); err != nil {
			return err
		}
	}
	return nil
}

func validateStickerSetCatalogIdentities(catalog *stickerSetCatalog, reactions *reactionDocumentCatalog) error {
	identities := make(map[int64]string, len(reactions.documents)*2+len(catalog.documents)*2+len(catalog.featured)*3)
	addIdentity := func(value int64, description string) error {
		if existing, ok := identities[value]; ok {
			return fmt.Errorf("built-in sticker identity collision: %s collides with %s", description, existing)
		}
		identities[value] = description
		return nil
	}
	for identity := range reactions.documents {
		if err := addIdentity(identity.id, fmt.Sprintf("reaction document id %d", identity.id)); err != nil {
			return err
		}
		if err := addIdentity(identity.accessHash, fmt.Sprintf("reaction document access hash %d", identity.accessHash)); err != nil {
			return err
		}
	}
	for _, set := range catalog.featured {
		if set.set.ID >= 0 || set.set.AccessHash == 0 || set.set.Hash == 0 || set.set.Count != len(set.documents) {
			return fmt.Errorf("sticker set %q has invalid identity or count", set.set.ShortName)
		}
		if err := addIdentity(set.set.ID, fmt.Sprintf("sticker set %q id %d", set.set.ShortName, set.set.ID)); err != nil {
			return err
		}
		if err := addIdentity(set.set.AccessHash, fmt.Sprintf("sticker set %q access hash %d", set.set.ShortName, set.set.AccessHash)); err != nil {
			return err
		}
		if err := addIdentity(int64(set.set.Hash), fmt.Sprintf("sticker set %q hash %d", set.set.ShortName, set.set.Hash)); err != nil {
			return err
		}
		seenDocuments := make(map[int64]struct{}, len(set.documents))
		for _, document := range set.documents {
			if document.ID >= 0 {
				return fmt.Errorf("sticker set %q document id %d is not in the reserved negative range", set.set.ShortName, document.ID)
			}
			if _, ok := seenDocuments[document.ID]; ok {
				return fmt.Errorf("sticker set %q has duplicate document id %d", set.set.ShortName, document.ID)
			}
			seenDocuments[document.ID] = struct{}{}
			if err := addIdentity(document.ID, fmt.Sprintf("sticker set %q document id %d", set.set.ShortName, document.ID)); err != nil {
				return err
			}
			if err := addIdentity(document.AccessHash, fmt.Sprintf("sticker set %q document access hash %d", set.set.ShortName, document.AccessHash)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *stickerSetCatalog) resolve(ref tg.InputStickerSetClass) (*builtInStickerSet, bool) {
	switch value := ref.(type) {
	case *tg.InputStickerSetShortName:
		if value == nil || len(value.ShortName) > maxStickerSetShortNameBytes {
			return nil, false
		}
		set, ok := c.byShortName[strings.ToLower(value.ShortName)]
		return set, ok
	case *tg.InputStickerSetID:
		if value == nil {
			return nil, false
		}
		set, ok := c.byID[value.ID]
		if !ok || set.set.AccessHash != value.AccessHash {
			return nil, false
		}
		return set, true
	default:
		return nil, false
	}
}

func specialStickerSet(ref tg.InputStickerSetClass) (*builtInStickerSet, bool) {
	switch ref.(type) {
	case *tg.InputStickerSetEmpty,
		*tg.InputStickerSetAnimatedEmoji,
		*tg.InputStickerSetDice,
		*tg.InputStickerSetAnimatedEmojiAnimations,
		*tg.InputStickerSetPremiumGifts,
		*tg.InputStickerSetEmojiGenericAnimations,
		*tg.InputStickerSetEmojiDefaultStatuses,
		*tg.InputStickerSetEmojiDefaultTopicIcons,
		*tg.InputStickerSetEmojiChannelDefaultStatuses,
		*tg.InputStickerSetTonGifts:
	default:
		return nil, false
	}

	identity := make([]byte, 4)
	binary.BigEndian.PutUint32(identity, ref.TypeID())
	if dice, ok := ref.(*tg.InputStickerSetDice); ok {
		if dice == nil {
			return nil, false
		}
		identity = appendHashPart(identity, []byte(dice.Emoticon))
	}
	digest := namespacedDigest("teagram-special-sticker-set-v1", identity)
	hash := int(binary.BigEndian.Uint32(digest[24:28]) & 0x7fffffff)
	if hash == 0 {
		hash = 1
	}
	set := &builtInStickerSet{
		set: tg.StickerSet{
			ID:         negativeContentID(digest[:8]),
			AccessHash: positiveContentHash(digest[8:16]),
			Title:      ref.TypeName(),
			Count:      0,
			Hash:       hash,
		},
		documents: []*tg.Document{},
		packs:     []tg.StickerPack{},
	}
	return set, true
}

func (h *handlers) resolveStickerDocument(ctx context.Context, id, accessHash int64) (bool, error) {
	if id > 0 {
		return h.store.UserStickerDocumentExists(ctx, id, accessHash)
	}
	catalog, err := h.stickerSetCatalog()
	if err != nil {
		return false, err
	}
	_, ok := catalog.documents[reactionDocumentIdentity{id: id, accessHash: accessHash}]
	return ok, nil
}

// validStickerRetryIdentity keeps current catalogue identities exact while
// allowing a retry for a catalogue id that has since gone stale to render its
// stored message as unsupported.
func (h *handlers) validStickerRetryIdentity(ctx context.Context, id, accessHash int64) (bool, error) {
	if id > 0 {
		return h.store.UserStickerDocumentExists(ctx, id, accessHash)
	}
	catalog, err := h.stickerSetCatalog()
	if err != nil {
		return false, err
	}
	document, current := catalog.documentsByID[id]
	return !current || document.AccessHash == accessHash, nil
}

func (set *builtInStickerSet) responseSet(installedAt *time.Time) tg.StickerSet {
	result := set.set
	if installedAt != nil {
		result.SetInstalledDate(int(installedAt.Unix()))
	}
	result.SetFlags()
	return result
}

func (set *builtInStickerSet) responseDocuments(dcID int) []tg.DocumentClass {
	result := make([]tg.DocumentClass, 0, len(set.documents))
	for _, source := range set.documents {
		result = append(result, cloneStickerDocument(source, dcID))
	}
	return result
}

func cloneStickerDocument(source *tg.Document, dcID int) *tg.Document {
	document := *source
	document.DCID = dcID
	document.FileReference = bytes.Clone(source.FileReference)
	document.Attributes = append([]tg.DocumentAttributeClass(nil), source.Attributes...)
	for i, attribute := range document.Attributes {
		switch value := attribute.(type) {
		case *tg.DocumentAttributeImageSize:
			cloned := *value
			document.Attributes[i] = &cloned
		case *tg.DocumentAttributeSticker:
			cloned := *value
			if ref, ok := value.Stickerset.(*tg.InputStickerSetID); ok {
				refCopy := *ref
				cloned.Stickerset = &refCopy
			}
			document.Attributes[i] = &cloned
		case *tg.DocumentAttributeFilename:
			cloned := *value
			document.Attributes[i] = &cloned
		}
	}
	return &document
}

func stickerDocumentMedia(catalog *stickerSetCatalog, documentID int64, dcID int) (tg.MessageMediaClass, bool) {
	if catalog == nil {
		var err error
		catalog, err = getStickerSetCatalog()
		if err != nil {
			slog.Default().Error("load sticker catalogue to render message", "err", err)
			return nil, false
		}
	}
	if catalog == nil {
		return nil, false
	}
	document, ok := catalog.documentsByID[documentID]
	if !ok {
		return nil, false
	}
	return &tg.MessageMediaDocument{Document: cloneStickerDocument(document, dcID)}, true
}

func installedStickerSetsHash(sets []tg.StickerSet) int64 {
	if len(sets) == 0 {
		return 0
	}
	input := make([]byte, 0, len(sets)*12)
	for _, set := range sets {
		var id [8]byte
		binary.BigEndian.PutUint64(id[:], uint64(set.ID)) //nolint:gosec // Negative catalogue IDs use their two's-complement bit pattern.
		input = append(input, id[:]...)
		var hash [4]byte
		binary.BigEndian.PutUint32(hash[:], uint32(set.Hash)) //nolint:gosec // Catalogue hashes are constrained to the signed 31-bit range.
		input = append(input, hash[:]...)
	}
	value := int64(crc32.ChecksumIEEE(input) & 0x7fffffff)
	if value == 0 {
		return 1
	}
	return value
}

func (h *handlers) handleGetStickerSet(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.MessagesGetStickerSetRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if _, ok := req.Stickerset.(*tg.InputStickerSetShortName); ok {
		if err := h.checkRateLimit(r, stickerSetLookupRateSurface, stickerSetMutationRateLimit); err != nil {
			return nil, err
		}
	}
	catalog, err := h.stickerSetCatalog()
	if err != nil {
		h.log.Error("load sticker set catalogue", "err", err)
		return nil, errInternal
	}
	set, ok := catalog.resolve(req.Stickerset)
	var userSet *store.UserStickerSet
	if !ok {
		if special, specialOK := specialStickerSet(req.Stickerset); specialOK {
			set = special
		} else {
			resolved, resolveErr := h.userStickerSetByRef(r, req.Stickerset)
			if errors.Is(resolveErr, store.ErrStickerSetNotFound) {
				return nil, errStickerSetInvalid
			}
			if resolveErr != nil {
				h.log.Error("resolve user sticker set", "user_id", r.UserID, "err", resolveErr)
				return nil, errInternal
			}
			userSet = &resolved
		}
	}
	var setHash int
	var setID int64
	if userSet != nil {
		setHash = userSet.Hash
		setID = userSet.ID
	} else {
		setHash = set.set.Hash
		setID = set.set.ID
	}
	if req.Hash != 0 && req.Hash == setHash {
		return &tg.MessagesStickerSetNotModified{}, nil
	}
	installed, err := h.store.InstalledStickerSets(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("load installed sticker sets", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	installedAt := installedStickerSetDate(installed, setID)
	if userSet != nil {
		return userStickerSetTL(*userSet, r.UserID, h.dcID, installedAt), nil
	}
	return &tg.MessagesStickerSet{
		Set:       set.responseSet(installedAt),
		Packs:     append([]tg.StickerPack(nil), set.packs...),
		Keywords:  []tg.StickerKeyword{},
		Documents: set.responseDocuments(h.dcID),
	}, nil
}

func (h *handlers) handleGetAllStickers(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.MessagesGetAllStickersRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	catalog, err := h.stickerSetCatalog()
	if err != nil {
		h.log.Error("load sticker set catalogue", "err", err)
		return nil, errInternal
	}
	installed, err := h.store.InstalledStickerSets(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("load installed sticker sets", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	userSetIDs := make([]int64, 0, len(installed))
	for _, item := range installed {
		if item.ID > 0 {
			userSetIDs = append(userSetIDs, item.ID)
		}
	}
	userSets, err := h.store.UserStickerSetSummariesByInstalledIDs(r.Ctx, userSetIDs)
	if err != nil {
		h.log.Error("load installed user sticker sets", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	sets := make([]tg.StickerSet, 0, len(installed))
	for _, item := range installed {
		if item.ID < 0 {
			set, ok := catalog.byID[item.ID]
			if !ok {
				continue
			}
			sets = append(sets, set.responseSet(&item.InstalledAt))
			continue
		}
		userSet, ok := userSets[item.ID]
		if !ok {
			continue
		}
		if userSet.Kind == "emojis" {
			continue
		}
		sets = append(sets, userStickerSetSummaryTL(userSet, r.UserID, &item.InstalledAt))
	}
	hash := installedStickerSetsHash(sets)
	if req.Hash != 0 && req.Hash == hash {
		return &tg.MessagesAllStickersNotModified{}, nil
	}
	return &tg.MessagesAllStickers{Hash: hash, Sets: sets}, nil
}

func (h *handlers) handleInstallStickerSet(c *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	if r.UserID == 0 {
		return nil, nil, errAuthKeyUnreg
	}
	var req tg.MessagesInstallStickerSetRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if err := h.checkRateLimit(r, stickerSetRateLimitSurface, stickerSetMutationRateLimit); err != nil {
		return nil, nil, err
	}
	catalog, err := h.stickerSetCatalog()
	if err != nil {
		h.log.Error("load sticker set catalogue", "err", err)
		return nil, nil, errInternal
	}
	setID, resolveErr := h.resolvedStickerSetID(r, catalog, req.Stickerset)
	if resolveErr != nil {
		return nil, nil, resolveErr
	}
	changed, err := h.store.InstallStickerSet(r.Ctx, r.UserID, setID)
	if errors.Is(err, store.ErrStickerSetNotFound) {
		return nil, nil, errStickerSetInvalid
	}
	if errors.Is(err, store.ErrStickerSetInstallLimit) {
		return nil, nil, errStickersTooMuch
	}
	if err != nil {
		h.log.Error("install sticker set", "user_id", r.UserID, "set_id", setID, "err", err)
		return nil, nil, errInternal
	}
	if !changed {
		return &tg.MessagesStickerSetInstallResultSuccess{}, nil, nil
	}
	return &tg.MessagesStickerSetInstallResultSuccess{}, func() {
		h.notifyStickerSetChanged(r)
	}, nil
}

func (h *handlers) handleUninstallStickerSet(c *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	if r.UserID == 0 {
		return nil, nil, errAuthKeyUnreg
	}
	var req tg.MessagesUninstallStickerSetRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if err := h.checkRateLimit(r, stickerSetRateLimitSurface, stickerSetMutationRateLimit); err != nil {
		return nil, nil, err
	}
	catalog, err := h.stickerSetCatalog()
	if err != nil {
		h.log.Error("load sticker set catalogue", "err", err)
		return nil, nil, errInternal
	}
	setID, resolveErr := h.resolvedStickerSetID(r, catalog, req.Stickerset)
	if resolveErr != nil {
		return nil, nil, resolveErr
	}
	changed, err := h.store.UninstallStickerSet(r.Ctx, r.UserID, setID)
	if err != nil {
		h.log.Error("uninstall sticker set", "user_id", r.UserID, "set_id", setID, "err", err)
		return nil, nil, errInternal
	}
	if !changed {
		return &tg.BoolTrue{}, nil, nil
	}
	return &tg.BoolTrue{}, func() {
		h.notifyStickerSetChanged(r)
	}, nil
}

func (h *handlers) notifyStickerSetChanged(r *mtproto.Request) {
	ctx, cancel := store.NotificationContext(r.Ctx)
	defer cancel()
	payload := store.StickerSetChangedPayload(r.UserID, mtproto.AuthKeyIDInt64(r.AuthKeyID), r.SessionID)
	if err := h.store.Notify(ctx, store.ChannelStickerSets, payload); err != nil {
		h.log.Info("notify sticker set update", "user_id", r.UserID, "err", err)
	}
}
