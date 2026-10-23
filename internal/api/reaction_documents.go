package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/reactionassets"
	"github.com/teagramhq/teagram-server/internal/store"
)

type reactionDocumentIdentity struct {
	id         int64
	accessHash int64
}

// Built-in reaction documents need a nonzero date for Teagram Desktop to apply their fields.
const reactionDocumentDate = 1_700_000_000

type builtInReactionDocument struct {
	file store.File
	data []byte
}

type reactionDocumentCatalog struct {
	hash      int
	reactions []tg.AvailableReaction
	documents map[reactionDocumentIdentity]builtInReactionDocument
}

var (
	reactionDocumentCatalogOnce sync.Once
	reactionDocumentCatalogVal  *reactionDocumentCatalog
	errReactionDocumentCatalog  error
)

func getReactionDocumentCatalog() (*reactionDocumentCatalog, error) {
	reactionDocumentCatalogOnce.Do(func() {
		reactionDocumentCatalogVal, errReactionDocumentCatalog = buildReactionDocumentCatalog()
	})
	return reactionDocumentCatalogVal, errReactionDocumentCatalog
}

func buildReactionDocumentCatalog() (*reactionDocumentCatalog, error) {
	entries, err := reactionassets.Manifest()
	if err != nil {
		return nil, fmt.Errorf("read reaction asset manifest: %w", err)
	}
	if len(entries) != len(chatReactionsAll) {
		return nil, fmt.Errorf("reaction asset manifest has %d entries, want %d", len(entries), len(chatReactionsAll))
	}

	catalog := &reactionDocumentCatalog{
		reactions: make([]tg.AvailableReaction, 0, len(entries)),
		documents: make(map[reactionDocumentIdentity]builtInReactionDocument, len(entries)*2),
	}
	hashInput := make([]byte, 0, len(entries)*64)
	// Metadata is part of the response too; changing it must invalidate client caches.
	hashInput = strconv.AppendInt(hashInput, int64(reactionDocumentDate), 10)
	hashInput = append(hashInput, 0)
	for i, emoji := range chatReactionsAll {
		entry := entries[i]
		if entry.Emoji != emoji {
			return nil, fmt.Errorf("reaction asset %d is %q, want %q", i, entry.Emoji, emoji)
		}
		stem := strings.TrimSuffix(entry.SVG, ".svg")
		if stem == entry.SVG || stem == "" || path.Base(entry.SVG) != entry.SVG {
			return nil, fmt.Errorf("reaction %q has invalid asset name %q", emoji, entry.SVG)
		}
		staticName := stem + ".webp"
		animationAssetName := stem + ".tgs"
		// The web client recognizes TGS stickers by this filename.
		animationFileName := "AnimatedSticker.tgs"
		staticData, err := reactionassets.Files.ReadFile("static/" + staticName)
		if err != nil {
			return nil, fmt.Errorf("read reaction %q WebP: %w", emoji, err)
		}
		animationData, err := reactionassets.Files.ReadFile("tgs/" + animationAssetName)
		if err != nil {
			return nil, fmt.Errorf("read reaction %q TGS: %w", emoji, err)
		}
		if len(staticData) == 0 || len(animationData) == 0 {
			return nil, fmt.Errorf("reaction %q has empty artwork", emoji)
		}

		staticID := -int64(2*i + 1)
		animationID := -int64(2*i + 2)
		staticDoc := newReactionDocument(emoji, staticID, staticName, "image/webp", staticData)
		animationDoc := newReactionDocument(emoji, animationID, animationFileName, "application/x-tgsticker", animationData)
		reaction := tg.AvailableReaction{
			Reaction:          emoji,
			Title:             emoji,
			StaticIcon:        staticDoc,
			AppearAnimation:   animationDoc,
			SelectAnimation:   animationDoc,
			ActivateAnimation: animationDoc,
			EffectAnimation:   animationDoc,
		}
		reaction.SetCenterIcon(staticDoc)
		reaction.SetAroundAnimation(animationDoc)
		catalog.reactions = append(catalog.reactions, reaction)

		catalog.documents[reactionDocumentIdentity{staticDoc.ID, staticDoc.AccessHash}] = builtInReactionDocument{
			file: reactionStoreFile(staticDoc, staticData),
			data: staticData,
		}
		catalog.documents[reactionDocumentIdentity{animationDoc.ID, animationDoc.AccessHash}] = builtInReactionDocument{
			file: reactionStoreFile(animationDoc, animationData),
			data: animationData,
		}

		hashInput = append(hashInput, emoji...)
		hashInput = append(hashInput, 0)
		hashInput = append(hashInput, staticName...)
		hashInput = append(hashInput, 0)
		hashInput = append(hashInput, staticData...)
		hashInput = append(hashInput, animationFileName...)
		hashInput = append(hashInput, 0)
		hashInput = append(hashInput, animationData...)
	}

	catalog.hash = int(crc32.ChecksumIEEE(hashInput) & 0x7fffffff)
	if catalog.hash == 0 {
		catalog.hash = 1
	}
	return catalog, nil
}

func newReactionDocument(emoji string, id int64, filename, mimeType string, data []byte) *tg.Document {
	accessHash, fileReference := reactionDocumentIdentityHash(id, filename, data)
	return &tg.Document{
		ID:            id,
		AccessHash:    accessHash,
		FileReference: fileReference,
		Date:          reactionDocumentDate,
		MimeType:      mimeType,
		Size:          int64(len(data)),
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeImageSize{W: 512, H: 512},
			&tg.DocumentAttributeSticker{Alt: emoji, Stickerset: &tg.InputStickerSetEmpty{}},
			&tg.DocumentAttributeFilename{FileName: filename},
		},
	}
}

func reactionDocumentIdentityHash(id int64, filename string, data []byte) (int64, []byte) {
	input := make([]byte, 0, len("teagram-reaction-document-v1")+1+20+1+len(filename)+1+len(data))
	input = append(input, "teagram-reaction-document-v1"...)
	input = append(input, 0)
	input = strconv.AppendInt(input, id, 10)
	input = append(input, 0)
	input = append(input, filename...)
	input = append(input, 0)
	input = append(input, data...)
	digest := sha256.Sum256(input)
	const maxSignedLong = uint64(1<<63 - 1)
	accessHash := int64(binary.BigEndian.Uint64(digest[:8]) & maxSignedLong)
	if accessHash == 0 {
		accessHash = 1
	}
	return accessHash, append([]byte(nil), digest[16:]...)
}

func reactionStoreFile(document *tg.Document, data []byte) store.File {
	filename := ""
	for _, attribute := range document.Attributes {
		if value, ok := attribute.(*tg.DocumentAttributeFilename); ok {
			filename = value.FileName
			break
		}
	}
	return store.File{
		ID:         document.ID,
		AccessHash: document.AccessHash,
		Size:       int64(len(data)),
		MimeType:   document.MimeType,
		FileName:   filename,
		Kind:       store.FileKindDocument,
		Stored:     true,
		Date:       time.Unix(0, 0).UTC(),
	}
}

func (c *reactionDocumentCatalog) responseReactions(dcID int) ([]tg.AvailableReaction, error) {
	result := make([]tg.AvailableReaction, len(c.reactions))
	for i, source := range c.reactions {
		reaction := source
		static, ok := reaction.StaticIcon.(*tg.Document)
		if !ok {
			return nil, fmt.Errorf("reaction %q static icon has type %T", reaction.Reaction, reaction.StaticIcon)
		}
		staticCopy := *static
		staticCopy.DCID = dcID
		reaction.StaticIcon = &staticCopy
		reaction.SetCenterIcon(&staticCopy)

		animation, ok := reaction.AppearAnimation.(*tg.Document)
		if !ok {
			return nil, fmt.Errorf("reaction %q appear animation has type %T", reaction.Reaction, reaction.AppearAnimation)
		}
		animationCopy := *animation
		animationCopy.DCID = dcID
		reaction.AppearAnimation = &animationCopy
		reaction.SelectAnimation = &animationCopy
		reaction.ActivateAnimation = &animationCopy
		reaction.EffectAnimation = &animationCopy
		reaction.SetAroundAnimation(&animationCopy)
		result[i] = reaction
	}
	return result, nil
}

func (h *handlers) builtInDocumentFileGate(loc downloadLocation) downloadGate {
	return func(_ context.Context, fileID, _ int64) (authorizedDownload, error) {
		catalog, err := getReactionDocumentCatalog()
		if err != nil {
			h.log.Error("load reaction documents", "err", err)
			return authorizedDownload{}, errInternal
		}
		document, ok := catalog.documents[reactionDocumentIdentity{id: fileID, accessHash: loc.accessHash}]
		if ok {
			return builtInDocumentDownload(document), nil
		}
		stickerSets, err := getStickerSetCatalog()
		if err != nil {
			h.log.Error("load sticker set documents", "err", err)
			return authorizedDownload{}, errInternal
		}
		document, ok = stickerSets.documents[reactionDocumentIdentity{id: fileID, accessHash: loc.accessHash}]
		if !ok {
			return authorizedDownload{}, errLocationInvalid
		}
		return builtInDocumentDownload(document), nil
	}
}

func builtInDocumentDownload(document builtInReactionDocument) authorizedDownload {
	return authorizedDownload{
		file: document.file,
		readChunk: func(ctx context.Context, offset, limit int64) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			size := int64(len(document.data))
			if offset < 0 || limit < 0 || offset > size || limit > size-offset {
				return nil, fmt.Errorf("built-in document read window outside %d byte asset", size)
			}
			return document.data[int(offset):int(offset+limit)], nil
		},
	}
}
