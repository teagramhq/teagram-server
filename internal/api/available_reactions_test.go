package api_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/reactionassets"
	"github.com/teagramhq/teagram-server/internal/store"
)

const standardReactionEmoticons = "👍 👎 ❤ 🔥 🥰 👏 😁 🤔 🤯 😱 🤬 😢 🎉 🤩 🤮 💩 🙏 👌 🕊 🤡 🥱 🥴 😍 🐳 ❤‍🔥 🌚 🌭 💯 🤣 ⚡ 🍌 🏆 💔 🤨 😐 🍓 🍾 💋 🖕 😈 😴 😭 🤓 👻 👨‍💻 👀 🎃 🙈 😇 😨 🤝 ✍ 🤗 🫡 🎅 🎄 ☃ 💅 🤪 🗿 🆒 💘 🙉 🦄 😘 💊 🙊 😎 👾 🤷‍♂ 🤷 🤷‍♀ 😡"

func availableReactionsResponse(t *testing.T, s *store.Store, userID int64, hash int) tg.MessagesAvailableReactionsClass {
	t.Helper()
	body := dispatchChatMethod(t, fullChannelDispatcher(s), userID, "messages.getAvailableReactions", &tg.MessagesGetAvailableReactionsRequest{Hash: hash})
	var full tg.MessagesAvailableReactions
	if err := full.Decode(&bin.Buffer{Buf: body}); err == nil {
		return &full
	}
	var unchanged tg.MessagesAvailableReactionsNotModified
	if err := unchanged.Decode(&bin.Buffer{Buf: body}); err == nil {
		return &unchanged
	}
	t.Fatal("messages.getAvailableReactions returned neither availableReactions nor notModified")
	return nil
}

func TestGetAvailableReactionsReturnsStableStandardDocuments(t *testing.T) {
	t.Parallel()
	wantEmoticons := strings.Fields(standardReactionEmoticons)

	firstResult := availableReactionsResponse(t, nil, 7, 0)
	first, ok := firstResult.(*tg.MessagesAvailableReactions)
	if !ok {
		t.Fatalf("hash=0 result = %T, want *tg.MessagesAvailableReactions", firstResult)
	}
	if first.Hash == 0 {
		t.Fatal("available reactions hash is zero")
	}
	const previousPaddedReactionCatalogHash = 303018940
	if first.Hash != previousPaddedReactionCatalogHash {
		t.Errorf("available reactions hash = %d, want prior center-icon hash %d", first.Hash, previousPaddedReactionCatalogHash)
	}
	if len(first.Reactions) != len(wantEmoticons) {
		t.Fatalf("available reactions = %d, want %d", len(first.Reactions), len(wantEmoticons))
	}
	// This asset-only hash omitted the document metadata from older responses.
	const previousReactionCatalogHash = 1491138664
	previousHashResult := availableReactionsResponse(t, nil, 7, previousReactionCatalogHash)
	if _, ok := previousHashResult.(*tg.MessagesAvailableReactions); !ok {
		t.Fatalf("previous reaction hash result = %T, want a refreshed reaction list", previousHashResult)
	}
	const unpaddedReactionCatalogHash = 1976891679
	unpaddedHashResult := availableReactionsResponse(t, nil, 7, unpaddedReactionCatalogHash)
	if _, ok := unpaddedHashResult.(*tg.MessagesAvailableReactions); !ok {
		t.Fatalf("unpadded reaction hash result = %T, want a refreshed reaction list", unpaddedHashResult)
	}

	var missingDates []string
	for i, want := range wantEmoticons {
		reaction := first.Reactions[i]
		if reaction.Reaction != want {
			t.Errorf("reaction %d = %q, want %q", i, reaction.Reaction, want)
		}
		static := requireReactionDocument(t, reaction.StaticIcon, want, "image/webp", ".webp")
		centerIcon, hasCenterIcon := reaction.GetCenterIcon()
		if !hasCenterIcon {
			t.Errorf("reaction %q has no center_icon", want)
		} else {
			centerDocument := requireReactionDocument(t, centerIcon, want, "image/webp", ".webp")
			if centerDocument.ID == static.ID {
				t.Errorf("reaction %q center_icon reuses its full-size static icon document", want)
			}
			if centerDocument.ID != -int64(2*len(wantEmoticons)+i+1) {
				t.Errorf("reaction %q center_icon id = %d, want historical id %d", want, centerDocument.ID, -int64(2*len(wantEmoticons)+i+1))
			}
			if i == 0 && centerDocument.AccessHash != 5531608559122588618 {
				t.Errorf("first center_icon access_hash = %d, want historical hash 5531608559122588618", centerDocument.AccessHash)
			}
			if centerDocument.Date == 0 {
				missingDates = append(missingDates, want+" center")
			}
		}

		animation := requireReactionDocument(t, reaction.AppearAnimation, want, "application/x-tgsticker", ".tgs")
		if static.Date == 0 {
			missingDates = append(missingDates, want+" static")
		}
		if animation.Date == 0 {
			missingDates = append(missingDates, want+" animation")
		}
		if got := reactionFilename(t, animation); got != "AnimatedSticker.tgs" {
			t.Errorf("reaction %q TGS filename = %q, want client-recognized AnimatedSticker.tgs", want, got)
		}
		for field, class := range map[string]tg.DocumentClass{
			"select_animation":   reaction.SelectAnimation,
			"activate_animation": reaction.ActivateAnimation,
			"effect_animation":   reaction.EffectAnimation,
		} {
			got := requireReactionDocument(t, class, want, "application/x-tgsticker", ".tgs")
			if !sameReactionDocument(got, animation) {
				t.Errorf("reaction %q %s does not reference its TGS document", want, field)
			}
		}
		around, ok := reaction.GetAroundAnimation()
		if !ok {
			t.Errorf("reaction %q has no around_animation", want)
		} else if got := requireReactionDocument(t, around, want, "application/x-tgsticker", ".tgs"); !sameReactionDocument(got, animation) {
			t.Errorf("reaction %q around_animation does not reference its TGS document", want)
		}
	}
	if len(missingDates) > 0 {
		t.Errorf("reaction document dates are zero, so Teagram Desktop ignores their fields: %s", strings.Join(missingDates, ", "))
	}

	secondResult := availableReactionsResponse(t, nil, 7, 0)
	second, ok := secondResult.(*tg.MessagesAvailableReactions)
	if !ok {
		t.Fatalf("second hash=0 result = %T, want *tg.MessagesAvailableReactions", secondResult)
	}
	if second.Hash != first.Hash {
		t.Fatalf("available reactions hash changed between handlers: %d != %d", second.Hash, first.Hash)
	}
	for i := range first.Reactions {
		firstStatic := requireReactionDocument(t, first.Reactions[i].StaticIcon, first.Reactions[i].Reaction, "image/webp", ".webp")
		secondStatic := requireReactionDocument(t, second.Reactions[i].StaticIcon, second.Reactions[i].Reaction, "image/webp", ".webp")
		firstAnimation := requireReactionDocument(t, first.Reactions[i].AppearAnimation, first.Reactions[i].Reaction, "application/x-tgsticker", ".tgs")
		secondAnimation := requireReactionDocument(t, second.Reactions[i].AppearAnimation, second.Reactions[i].Reaction, "application/x-tgsticker", ".tgs")
		if !sameReactionDocument(firstStatic, secondStatic) || !sameReactionDocument(firstAnimation, secondAnimation) {
			t.Errorf("reaction %q document identity changed between handlers", first.Reactions[i].Reaction)
		}
	}

	unchanged := availableReactionsResponse(t, nil, 7, first.Hash)
	if _, ok := unchanged.(*tg.MessagesAvailableReactionsNotModified); !ok {
		t.Fatalf("current hash result = %T, want *tg.MessagesAvailableReactionsNotModified", unchanged)
	}
}

func requireReactionDocument(t *testing.T, class tg.DocumentClass, emoji, mimeType, extension string) *tg.Document {
	t.Helper()
	document, ok := class.(*tg.Document)
	if !ok {
		t.Fatalf("reaction %q document = %T, want *tg.Document", emoji, class)
	}
	if document.ID >= 0 {
		t.Errorf("reaction %q document id = %d, want reserved negative id", emoji, document.ID)
	}
	if document.AccessHash == 0 {
		t.Errorf("reaction %q document has zero access hash", emoji)
	}
	if len(document.FileReference) == 0 {
		t.Errorf("reaction %q document has empty file reference", emoji)
	}
	if document.MimeType != mimeType {
		t.Errorf("reaction %q MIME type = %q, want %q", emoji, document.MimeType, mimeType)
	}
	if document.Size <= 0 {
		t.Errorf("reaction %q size = %d, want positive size", emoji, document.Size)
	}

	imageSizeFound, stickerFound, filenameFound := false, false, false
	for _, attribute := range document.Attributes {
		switch value := attribute.(type) {
		case *tg.DocumentAttributeImageSize:
			imageSizeFound = true
			if value.W != 512 || value.H != 512 {
				t.Errorf("reaction %q image size = %dx%d, want 512x512", emoji, value.W, value.H)
			}
		case *tg.DocumentAttributeSticker:
			stickerFound = true
			if value.Alt != emoji {
				t.Errorf("reaction %q sticker alt = %q", emoji, value.Alt)
			}
			if _, ok := value.Stickerset.(*tg.InputStickerSetEmpty); !ok {
				t.Errorf("reaction %q sticker set = %T, want empty sticker set", emoji, value.Stickerset)
			}
		case *tg.DocumentAttributeFilename:
			filenameFound = true
			if !strings.HasSuffix(value.FileName, extension) {
				t.Errorf("reaction %q filename = %q, want suffix %q", emoji, value.FileName, extension)
			}
		}
	}
	if !imageSizeFound || !stickerFound || !filenameFound {
		t.Errorf("reaction %q document attributes: image-size=%t sticker=%t filename=%t", emoji, imageSizeFound, stickerFound, filenameFound)
	}
	return document
}

func sameReactionDocument(left, right *tg.Document) bool {
	return left.ID == right.ID && left.AccessHash == right.AccessHash && left.Size == right.Size && left.MimeType == right.MimeType
}

func TestAvailableReactionDocumentsDownloadForAnySignedInUser(t *testing.T) {
	t.Parallel()
	s, blobs, caller, _, _ := downloadFixture(t, "+15551048001", "+15551048002")
	result := availableReactionsResponse(t, s, caller.ID, 0)
	full, ok := result.(*tg.MessagesAvailableReactions)
	if !ok {
		t.Fatalf("available reactions result = %T", result)
	}
	if len(full.Reactions) != len(strings.Fields(standardReactionEmoticons)) {
		t.Fatalf("available reactions = %d, want %d", len(full.Reactions), len(strings.Fields(standardReactionEmoticons)))
	}
	entries, err := reactionassets.Manifest()
	if err != nil {
		t.Fatalf("read reaction asset manifest: %v", err)
	}
	if len(entries) != len(full.Reactions) {
		t.Fatalf("reaction asset entries = %d, want %d", len(entries), len(full.Reactions))
	}

	getFile := api.GetFileSeqForTestWithLimits(s, blobs, store.RateLimitConfig{}, store.RateLimitConfig{})
	for i, reaction := range full.Reactions {
		entry := entries[i]
		if reaction.Reaction != entry.Emoji {
			t.Fatalf("reaction %d = %q, asset manifest emoji = %q", i, reaction.Reaction, entry.Emoji)
		}
		centerIcon, ok := reaction.GetCenterIcon()
		if !ok {
			t.Fatalf("reaction %q has no center_icon", reaction.Reaction)
		}
		assetStem := strings.TrimSuffix(entry.SVG, ".svg")
		for _, item := range []struct {
			class     tg.DocumentClass
			dir       string
			mimeType  string
			extension string
		}{
			{reaction.StaticIcon, "static", "image/webp", ".webp"},
			{centerIcon, "center", "image/webp", ".webp"},
			{reaction.AppearAnimation, "tgs", "application/x-tgsticker", ".tgs"},
		} {
			document := requireReactionDocument(t, item.class, reaction.Reaction, item.mimeType, item.extension)
			filename := assetStem + item.extension
			want, err := os.ReadFile(filepath.Join("..", "reactionassets", item.dir, filename))
			if err != nil {
				t.Fatalf("read committed %s asset %s: %v", item.dir, filename, err)
			}
			if int64(len(want)) != document.Size {
				t.Fatalf("reaction %q %s size = %d, committed file size = %d", reaction.Reaction, item.dir, document.Size, len(want))
			}
			response, err := getFile(caller.ID, &tg.UploadGetFileRequest{
				Location: &tg.InputDocumentFileLocation{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference},
				Offset:   0,
				Limit:    int(document.Size),
			})
			if err != nil {
				t.Fatalf("download reaction %q %s: %v", reaction.Reaction, item.dir, err)
			}
			file, ok := response.(*tg.UploadFile)
			if !ok {
				t.Fatalf("download reaction %q %s = %T, want *tg.UploadFile", reaction.Reaction, item.dir, response)
			}
			if !bytes.Equal(file.Bytes, want) {
				t.Fatalf("download reaction %q %s differs from committed asset", reaction.Reaction, item.dir)
			}
		}
	}
}

func TestBuiltInReactionDownloadRejectionsAndRateLimit(t *testing.T) {
	t.Parallel()
	s, blobs, caller, receiver, userDocument := downloadFixture(t, "+15551048011", "+15551048012")
	result := availableReactionsResponse(t, s, caller.ID, 0)
	full, ok := result.(*tg.MessagesAvailableReactions)
	if !ok {
		t.Fatalf("available reactions result = %T, want *tg.MessagesAvailableReactions", result)
	}
	if len(full.Reactions) == 0 {
		t.Fatal("available reactions list is empty")
	}
	document := requireReactionDocument(t, full.Reactions[0].StaticIcon, full.Reactions[0].Reaction, "image/webp", ".webp")

	getFile := api.GetFileSeqForTestWithLimits(s, blobs, store.RateLimitConfig{}, store.RateLimitConfig{})
	request := func(id, accessHash int64, offset int64) error {
		_, err := getFile(caller.ID, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: id, AccessHash: accessHash, FileReference: document.FileReference},
			Offset:   offset,
			Limit:    1024,
		})
		return err
	}
	wrongHash := rpcMessage(t, request(document.ID, document.AccessHash+1, 0))
	unknownFile := rpcMessage(t, request(document.ID-10_000, document.AccessHash, 0))
	if wrongHash != "LOCATION_INVALID" || unknownFile != wrongHash {
		t.Fatalf("wrong built-in hash = %q, unknown file = %q; want identical LOCATION_INVALID errors", wrongHash, unknownFile)
	}
	if got := rpcMessage(t, request(document.ID, document.AccessHash, document.Size+1)); got != "LOCATION_INVALID" {
		t.Fatalf("offset past built-in file = %q, want LOCATION_INVALID", got)
	}
	if got := rpcMessage(t, request(userDocument.ID, document.AccessHash, 0)); got != "LOCATION_INVALID" {
		t.Fatalf("positive user-file id with reaction access hash = %q, want LOCATION_INVALID", got)
	}
	_, sendErr := api.SendMediaForTest(s, caller.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(caller.ID, receiver.ID),
		Media: &tg.InputMediaUploadedDocument{
			File:     &tg.InputFile{ID: document.ID, Parts: 1, Name: "1f44d.webp"},
			MimeType: "image/webp",
		},
		Message:  "reaction document replay",
		RandomID: time.Now().UnixNano(),
	})
	if got := rpcMessage(t, sendErr); got != "MEDIA_INVALID" {
		t.Fatalf("messages.sendMedia with built-in reaction id = %q, want MEDIA_INVALID", got)
	}

	limited := api.GetFileSeqForTestWithLimits(
		s, blobs,
		store.RateLimitConfig{Limit: 1, Window: time.Minute},
		store.RateLimitConfig{},
	)
	limitedRequest := &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference},
		Offset:   0,
		Limit:    1024,
	}
	if _, err := limited(caller.ID, limitedRequest); err != nil {
		t.Fatalf("first built-in download: %v", err)
	}
	if got := rpcMessage(t, func() error {
		_, err := limited(caller.ID, limitedRequest)
		return err
	}()); !strings.HasPrefix(got, "FLOOD_WAIT_") {
		t.Fatalf("second built-in download = %q, want rate limit", got)
	}
}

func reactionFilename(t *testing.T, document *tg.Document) string {
	t.Helper()
	for _, attribute := range document.Attributes {
		if filename, ok := attribute.(*tg.DocumentAttributeFilename); ok {
			return filename.FileName
		}
	}
	t.Fatalf("document %d has no filename attribute", document.ID)
	return ""
}
