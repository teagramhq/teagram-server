package api_test

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- Telegram inputFile requires MD5 for uploaded photos.
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSanitizeMIME(t *testing.T) {
	t.Parallel()
	const fallback = "application/octet-stream"
	tests := map[string]string{
		"image/png":                 "image/png",
		"text/plain; charset=utf-8": fallback,
		"image/png\r\nX: y":         fallback,
		"":                          fallback,
		"png":                       fallback,
		"a/b/c":                     fallback,
		"image/":                    fallback,
		"/png":                      fallback,
		strings.Repeat("a", 128) + "/" + strings.Repeat("b", 127): fallback,
	}
	for in, want := range tests {
		if got := api.SanitizeMIME(in); got != want {
			t.Errorf("SanitizeMIME(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeFileName(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"hello.txt":              "hello.txt",
		"réunion notes.pdf":      "réunion notes.pdf",
		"nul\x00.txt":            "",
		"two\nlines.txt":         "",
		"bell\x07.txt":           "",
		"c1\u009f.txt":           "",
		"annexe\u202egnp.exe":    "",
		"isolate\u2066.txt":      "",
		strings.Repeat("a", 256): "",
	}
	for in, want := range tests {
		if got := api.SanitizeFileName(in); got != want {
			t.Errorf("SanitizeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// saveParts uploads each payload as one part of fileID under userID, through
// the handler that testHandlers builds for this store's blob backend, so the
// parts land where assembly will look for them.
func saveParts(t *testing.T, s *store.Store, userID, fileID int64, payloads ...[]byte) {
	t.Helper()
	for i, p := range payloads {
		if _, err := api.SaveFilePartForTest(s, userID, &tg.UploadSaveFilePartRequest{
			FileID: fileID, FilePart: i, Bytes: p,
		}); err != nil {
			t.Fatalf("save part %d: %v", i, err)
		}
	}
}

func jpegPhotoPayload(t *testing.T, width, height int) []byte {
	t.Helper()
	image := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			image.SetRGBA(x, y, color.RGBA{R: 70, G: 120, B: 180, A: 255})
		}
	}
	var payload bytes.Buffer
	if err := jpeg.Encode(&payload, image, nil); err != nil {
		t.Fatalf("encode test JPEG: %v", err)
	}
	return payload.Bytes()
}

func jpegPhotoMD5(payload []byte) string {
	sum := md5.Sum(payload) // #nosec G401 -- Telegram inputFile requires MD5 for uploaded photos.
	return hex.EncodeToString(sum[:])
}

// uploadedDocument builds an uploaded document input for sendMedia.
func uploadedDocument(fileID int64, parts int, name, mime string) *tg.InputMediaUploadedDocument {
	return &tg.InputMediaUploadedDocument{
		File:     &tg.InputFile{ID: fileID, Parts: parts, Name: name},
		MimeType: mime,
	}
}

func uploadedPhoto(fileID int64, parts int, name, checksum string) *tg.InputMediaUploadedPhoto {
	return &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
		ID: fileID, Parts: parts, Name: name, MD5Checksum: checksum,
	}}
}

// newBlobs opens a blob store rooted in the test's own temporary directory.
func newBlobs(t *testing.T) blob.Store {
	t.Helper()
	b, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	return b
}

type blockingAssembledPut struct {
	blob.Store

	key         string
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func (b *blockingAssembledPut) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if strings.HasPrefix(key, blob.PartsPrefix) {
		return b.Store.Put(ctx, key, r)
	}
	b.key = key
	return b.Store.Put(ctx, key, &blockingReader{
		Reader:  r,
		ctx:     ctx,
		started: b.started,
		release: b.release,
		once:    &b.startOnce,
	})
}

func (b *blockingAssembledPut) unblock() {
	b.releaseOnce.Do(func() { close(b.release) })
}

type blockingReader struct {
	io.Reader

	ctx     context.Context
	started chan<- struct{}
	release <-chan struct{}
	once    *sync.Once
}

func (r *blockingReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	select {
	case <-r.release:
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
	return r.Reader.Read(p)
}

// messageOf returns the message the first updateNewMessage in enc carries,
// for a test that needs the local id the send allocated.
func messageOf(t *testing.T, enc any) *tg.Message {
	t.Helper()
	update := messageUpdateOf(t, enc)
	message, ok := update.Message.(*tg.Message)
	if !ok {
		t.Fatalf("message type = %T, want *tg.Message", update.Message)
	}
	return message
}

func messageUpdateOf(t *testing.T, enc any) *tg.UpdateNewMessage {
	t.Helper()
	ups, ok := enc.(*tg.Updates)
	if !ok {
		t.Fatalf("result type = %T, want *tg.Updates", enc)
	}
	for _, u := range ups.Updates {
		nm, ok := u.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		return nm
	}
	t.Fatal("no updateNewMessage in the result")
	return nil
}

// documentOf returns the document the first updateNewMessage in enc carries.
func documentOf(t *testing.T, enc any) *tg.Document {
	t.Helper()
	ups, ok := enc.(*tg.Updates)
	if !ok {
		t.Fatalf("result type = %T, want *tg.Updates", enc)
	}
	for _, u := range ups.Updates {
		nm, ok := u.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		m, ok := nm.Message.(*tg.Message)
		if !ok {
			t.Fatalf("message type = %T, want *tg.Message", nm.Message)
		}
		media, ok := m.Media.(*tg.MessageMediaDocument)
		if !ok {
			t.Fatalf("media type = %T, want *tg.MessageMediaDocument", m.Media)
		}
		doc, ok := media.Document.(*tg.Document)
		if !ok {
			t.Fatalf("document type = %T, want *tg.Document", media.Document)
		}
		return doc
	}
	t.Fatal("no updateNewMessage in reply")
	return nil
}

func TestPartsReaderConcatenates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551296001")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	saveParts(t, s, u.ID, 700, []byte("hello "), []byte("wide "), []byte("world"))

	r, err := api.NewPartsReaderForTest(s, u.ID, 700)
	if err != nil {
		t.Fatalf("parts reader: %v", err)
	}
	sized, ok := r.(interface{ Size() int64 })
	if !ok {
		t.Fatal("parts reader does not expose its known size")
	}
	if sized.Size() != int64(len("hello wide world")) {
		t.Fatalf("parts reader size = %d, want %d", sized.Size(), len("hello wide world"))
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello wide world" {
		t.Errorf("read = %q", got)
	}

	// Zero parts is end of stream, not an error and not a spin.
	r, err = api.NewPartsReaderForTest(s, u.ID, 701)
	if err != nil {
		t.Fatalf("empty parts reader: %v", err)
	}
	empty, err := io.ReadAll(r)
	if err != nil || len(empty) != 0 {
		t.Errorf("empty read = %q err=%v", empty, err)
	}
}

func TestSendMediaToUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	blobs := newBlobs(t)
	a, err := s.CreateUser(ctx, "+15551296011")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551296012")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	saveParts(t, s, a.ID, 555, []byte("hello "), []byte("world"))

	enc, err := api.SendMediaForTest(s, a.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(a.ID, b.ID),
		Media:    uploadedDocument(555, 2, "hello.txt", "text/plain"),
		Message:  "look",
		RandomID: 42,
	})
	if err != nil {
		t.Fatalf("send media: %v", err)
	}
	assertEncodes(t, enc)
	doc := documentOf(t, enc)
	if doc.Size != 11 {
		t.Errorf("Size = %d, want 11", doc.Size)
	}
	if doc.MimeType != "text/plain" {
		t.Errorf("MimeType = %q", doc.MimeType)
	}
	name, ok := doc.Attributes[0].(*tg.DocumentAttributeFilename)
	if !ok || name.FileName != "hello.txt" {
		t.Errorf("attributes = %#v", doc.Attributes)
	}

	// The recipient reads the same document off their own history.
	hist, err := api.GetHistoryForTest(s, b.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(b.ID, a.ID),
	})
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	msgs, ok := hist.(*tg.MessagesMessages)
	if !ok || len(msgs.Messages) != 1 {
		t.Fatalf("history = %#v", hist)
	}
	inbox, ok := msgs.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("history message type = %T", msgs.Messages[0])
	}
	media, ok := inbox.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("history media type = %T", inbox.Media)
	}
	inboxDoc, ok := media.Document.(*tg.Document)
	if !ok || inboxDoc.ID != doc.ID {
		t.Fatalf("history document = %#v", media.Document)
	}

	// The bytes are the ones that were uploaded, at the file id's key.
	body, err := blobs.ReadAt(ctx, blob.Key(doc.ID), 0, 64)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(body, []byte("hello world")) {
		t.Errorf("blob = %q, want %q", body, "hello world")
	}

	// The parts are consumed: assembly is not repeatable off the same upload.
	n, _, _, err := s.UploadPartsSummary(ctx, a.ID, 555)
	if err != nil {
		t.Fatalf("parts summary: %v", err)
	}
	if n != 0 {
		t.Errorf("parts left = %d, want 0", n)
	}
	// The part objects go with the rows: nothing of the upload is left in the
	// store.
	if got := partObjects(t, blobs); len(got) != 0 {
		t.Errorf("part objects left = %d, want 0", len(got))
	}
}

// partObjects lists the keys under the parts prefix that blobs still holds.
func partObjects(t *testing.T, blobs blob.Store) []string {
	t.Helper()
	dir, ok := blobLocalDir(t, blobs)
	if !ok {
		t.Skip("blob backend is not local; object listing is not available")
	}
	entries, err := os.ReadDir(dir + "/parts")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read parts dir: %v", err)
	}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			keys = append(keys, blob.PartsPrefix+e.Name())
		}
	}
	return keys
}

// blobLocalDir reports the root directory of a local blob backend.
func blobLocalDir(t *testing.T, b blob.Store) (string, bool) {
	t.Helper()
	l, ok := b.(*blob.Local)
	if !ok {
		return "", false
	}
	return l.RootDir(), true
}

func TestSendMediaSanitizesStoredMIME(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551296091")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551296092")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	saveParts(t, s, a.ID, 564, []byte("plain"))

	// The wiring, not the pure function: a mime type with a space is stored as
	// the generic type, and the reply echoes what was stored.
	enc, err := api.SendMediaForTest(s, a.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(a.ID, b.ID),
		Media:    uploadedDocument(564, 1, "two\nlines.txt", "text/plain; charset=utf-8"),
		RandomID: 50,
	})
	if err != nil {
		t.Fatalf("send media: %v", err)
	}
	doc := documentOf(t, enc)
	if doc.MimeType != "application/octet-stream" {
		t.Errorf("MimeType = %q, want application/octet-stream", doc.MimeType)
	}
	// The file name is unusable, so the document carries no name attribute.
	if len(doc.Attributes) != 0 {
		t.Errorf("attributes = %#v, want none", doc.Attributes)
	}
}

func TestSendMediaResendIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	a, err := s.CreateUser(ctx, "+15551296101")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551296102")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	saveParts(t, s, a.ID, 565, []byte("once"))

	req := &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(a.ID, b.ID),
		Media:    uploadedDocument(565, 1, "once.txt", "text/plain"),
		Message:  "look",
		RandomID: 51,
	}
	first, err := api.SendMediaForTest(s, a.ID, newBlobs(t), api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("send media: %v", err)
	}
	firstDoc := documentOf(t, first)

	// A client that lost the reply resends. Assembly already consumed the
	// parts, so a resend that reached it would report MEDIA_INVALID for a
	// message that was delivered.
	second, err := api.SendMediaForTest(s, a.ID, newBlobs(t), api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	secondDoc := documentOf(t, second)
	if secondDoc.ID != firstDoc.ID {
		t.Errorf("resend document = %d, want %d", secondDoc.ID, firstDoc.ID)
	}
	// No second message and no second file: the resend cost nothing.
	if n := countFiles(t, ctx, dsn); n != 1 {
		t.Errorf("files rows = %d, want 1", n)
	}
	hist, err := api.GetHistoryForTest(s, b.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(b.ID, a.ID),
	})
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	msgs, ok := hist.(*tg.MessagesMessages)
	if !ok || len(msgs.Messages) != 1 {
		t.Fatalf("history = %#v, want one message", hist)
	}
}

func TestSendMediaSubtypeRightsFollowOriginalFileOnResend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	a, err := s.CreateUser(ctx, "+15551296301")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551296302")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	c, err := s.CreateUser(ctx, "+15551296303")
	if err != nil {
		t.Fatalf("user c: %v", err)
	}

	const clientFileID = 7788
	const randomID = 7789
	saveParts(t, s, a.ID, clientFileID, []byte("sticker bytes"))
	sticker := uploadedDocument(clientFileID, 1, "sticker.tgs", "application/x-tgsticker")
	sticker.Attributes = []tg.DocumentAttributeClass{
		&tg.DocumentAttributeSticker{Alt: "⭐", Stickerset: &tg.InputStickerSetEmpty{}},
	}
	first, err := api.SendMediaForTest(s, a.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(a.ID, b.ID), Media: sticker, RandomID: randomID,
	})
	if err != nil {
		t.Fatalf("send sticker: %v", err)
	}
	fileID := documentOf(t, first).ID

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	var rights []string
	if err := conn.QueryRow(ctx, `SELECT subtype_rights FROM files WHERE id = $1`, fileID).Scan(&rights); err != nil {
		t.Fatalf("read stored subtype rights: %v", err)
	}
	if len(rights) != 1 || rights[0] != "send_stickers" {
		t.Fatalf("stored subtype rights = %v, want [send_stickers]", rights)
	}
	forward, err := api.ForwardMessagesForTest(s, a.ID, &tg.MessagesForwardMessagesRequest{
		ToPeer: api.InputPeerUser(a.ID, c.ID), FromPeer: api.InputPeerUser(a.ID, b.ID),
		ID: []int{messageOf(t, first).ID}, RandomID: []int64{randomID + 1},
	})
	if err != nil {
		t.Fatalf("forward sticker: %v", err)
	}
	if got := documentOf(t, forward).ID; got != fileID {
		t.Fatalf("forward file id = %d, want original %d", got, fileID)
	}
	if err := conn.QueryRow(ctx, `SELECT subtype_rights FROM files WHERE id = $1`, fileID).Scan(&rights); err != nil {
		t.Fatalf("read subtype rights after forward: %v", err)
	}
	if len(rights) != 1 || rights[0] != "send_stickers" {
		t.Fatalf("stored subtype rights after forward = %v, want [send_stickers]", rights)
	}

	// A retry with the same random ID keeps the original classification, even
	// when it supplies no document attributes and no matching upload parts.
	retry, err := api.SendMediaForTest(s, a.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:  api.InputPeerUser(a.ID, b.ID),
		Media: uploadedDocument(clientFileID+1, 1, "retry.bin", "application/octet-stream"), RandomID: randomID,
	})
	if err != nil {
		t.Fatalf("resend without attributes: %v", err)
	}
	if got := documentOf(t, retry).ID; got != fileID {
		t.Fatalf("resend file id = %d, want original %d", got, fileID)
	}
	if err := conn.QueryRow(ctx, `SELECT subtype_rights FROM files WHERE id = $1`, fileID).Scan(&rights); err != nil {
		t.Fatalf("read subtype rights after resend: %v", err)
	}
	if len(rights) != 1 || rights[0] != "send_stickers" {
		t.Fatalf("stored subtype rights after resend = %v, want [send_stickers]", rights)
	}
	if n := countFiles(t, ctx, dsn); n != 1 {
		t.Fatalf("files rows after forward and resend = %d, want 1", n)
	}
}

func TestSendMediaClassifiesAcceptedDocumentAttributes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	a, err := s.CreateUser(ctx, "+15551296311")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551296312")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}

	sticker := &tg.DocumentAttributeSticker{Alt: "⭐", Stickerset: &tg.InputStickerSetEmpty{}}
	customEmoji := &tg.DocumentAttributeCustomEmoji{Alt: "⭐", Stickerset: &tg.InputStickerSetEmpty{}}
	tests := []struct {
		name       string
		fileName   string
		mimeType   string
		attributes []tg.DocumentAttributeClass
		wantNull   bool
		wantRights []string
	}{
		{name: "empty attributes", wantRights: []string{}},
		{
			name: "filename and image size are generic",
			attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: "photo.png"},
				&tg.DocumentAttributeImageSize{W: 2, H: 3},
			},
			wantRights: []string{},
		},
		{
			name:       "filename and MIME do not infer subtype",
			fileName:   "sticker.gif",
			mimeType:   "video/mp4",
			wantRights: []string{},
		},
		{name: "sticker", attributes: []tg.DocumentAttributeClass{sticker}, wantRights: []string{"send_stickers"}},
		{name: "custom emoji", attributes: []tg.DocumentAttributeClass{customEmoji}, wantRights: []string{"send_stickers"}},
		{name: "animated", attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAnimated{}}, wantRights: []string{"send_gifs"}},
		{name: "video", attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{}}, wantRights: []string{"send_videos"}},
		{name: "round video", attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{RoundMessage: true}}, wantRights: []string{"send_roundvideos"}},
		{name: "audio", attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAudio{}}, wantRights: []string{"send_audios"}},
		{name: "voice", attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAudio{Voice: true}}, wantRights: []string{"send_voices"}},
		{
			name: "combined and duplicate subtypes are sorted and unique",
			attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeAudio{Voice: true}, sticker, &tg.DocumentAttributeAnimated{}, sticker,
				&tg.DocumentAttributeVideo{RoundMessage: true},
			},
			wantRights: []string{"send_gifs", "send_roundvideos", "send_stickers", "send_voices"},
		},
		{
			name:       "unrecognized attribute stays unknown",
			attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeHasStickers{}},
			wantNull:   true,
		},
		{
			name:       "unrecognized attribute makes combined set unknown",
			attributes: []tg.DocumentAttributeClass{sticker, &tg.DocumentAttributeHasStickers{}},
			wantNull:   true,
		},
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientFileID := int64(7900 + i)
			saveParts(t, s, a.ID, clientFileID, []byte("document"))
			fileName := tt.fileName
			if fileName == "" {
				fileName = "document.bin"
			}
			mimeType := tt.mimeType
			if mimeType == "" {
				mimeType = "application/octet-stream"
			}
			media := uploadedDocument(clientFileID, 1, fileName, mimeType)
			media.Attributes = tt.attributes
			result, err := api.SendMediaForTest(s, a.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
				Peer: api.InputPeerUser(a.ID, b.ID), Media: media, RandomID: int64(7950 + i),
			})
			if err != nil {
				t.Fatalf("send media: %v", err)
			}

			fileID := documentOf(t, result).ID
			var isNull bool
			var rights []string
			if err := conn.QueryRow(ctx, `SELECT subtype_rights IS NULL, subtype_rights FROM files WHERE id = $1`, fileID).Scan(&isNull, &rights); err != nil {
				t.Fatalf("read subtype rights: %v", err)
			}
			if isNull != tt.wantNull {
				t.Fatalf("subtype rights null = %v, want %v (value %v)", isNull, tt.wantNull, rights)
			}
			if !tt.wantNull {
				if len(rights) != len(tt.wantRights) {
					t.Fatalf("subtype rights = %v, want %v", rights, tt.wantRights)
				}
				for i := range rights {
					if rights[i] != tt.wantRights[i] {
						t.Fatalf("subtype rights = %v, want %v", rights, tt.wantRights)
					}
				}
			}
		})
	}
}

func TestSendMediaToChat(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	blobs := newBlobs(t)
	users, chat := chatWith(t, s, "+15551296021", "+15551296022", "+15551296023")
	saveParts(t, s, users[0].ID, 556, []byte("chat bytes"))

	enc, err := api.SendMediaForTest(s, users[0].ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     &tg.InputPeerChat{ChatID: chat.ID},
		Media:    uploadedDocument(556, 1, "notes.txt", "text/plain"),
		Message:  "for everyone",
		RandomID: 43,
	})
	if err != nil {
		t.Fatalf("send media: %v", err)
	}
	assertEncodes(t, enc)
	doc := documentOf(t, enc)

	// Every member's own copy carries the document.
	for _, u := range users[1:] {
		hist, herr := api.GetHistoryForTest(s, u.ID, &tg.MessagesGetHistoryRequest{
			Peer: &tg.InputPeerChat{ChatID: chat.ID},
		})
		if herr != nil {
			t.Fatalf("get history %d: %v", u.ID, herr)
		}
		msgs, ok := hist.(*tg.MessagesMessages)
		if !ok || len(msgs.Messages) == 0 {
			t.Fatalf("history %d = %#v", u.ID, hist)
		}
		m, ok := msgs.Messages[0].(*tg.Message)
		if !ok {
			t.Fatalf("history %d message type = %T", u.ID, msgs.Messages[0])
		}
		media, ok := m.Media.(*tg.MessageMediaDocument)
		if !ok {
			t.Fatalf("history %d media type = %T", u.ID, m.Media)
		}
		got, ok := media.Document.(*tg.Document)
		if !ok || got.ID != doc.ID {
			t.Fatalf("history %d document = %#v", u.ID, media.Document)
		}
	}
}

func TestSendMediaUploadedPhotoRejectsMissingChecksum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551296031")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551296032")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	saveParts(t, s, a.ID, 557, []byte("not really a png"))

	_, err = api.SendMediaForTest(s, a.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(a.ID, b.ID),
		Media: &tg.InputMediaUploadedPhoto{
			File: &tg.InputFile{ID: 557, Parts: 1, Name: "photo.png"},
		},
		RandomID: 44,
	})
	rpcError(t, err, "MEDIA_INVALID")
}

func TestSendMediaUploadedPhotoToPrivateUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	blobs := newBlobs(t)
	sender, err := s.CreateUser(ctx, "+15551296033")
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551296034")
	if err != nil {
		t.Fatalf("recipient: %v", err)
	}

	body := jpegPhotoPayload(t, 640, 480)
	saveParts(t, s, sender.ID, 560, body)
	enc, err := api.SendMediaForTest(s, sender.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(sender.ID, recipient.ID),
		Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
			ID: 560, Parts: 1, Name: "219343.jpg", MD5Checksum: jpegPhotoMD5(body),
		}},
		Message:  "",
		RandomID: 560,
	})
	if err != nil {
		t.Fatalf("send photo: %v", err)
	}

	message := messageOf(t, enc)
	photo := photoOfMessage(t, message)
	if photo.ID == 0 || photo.AccessHash == 0 || len(photo.FileReference) == 0 {
		t.Fatalf("photo identifiers = id %d, access hash %d, file reference %x", photo.ID, photo.AccessHash, photo.FileReference)
	}
	if len(photo.Sizes) != 1 {
		t.Fatalf("photo sizes = %d, want one original size", len(photo.Sizes))
	}
	size, ok := photo.Sizes[0].(*tg.PhotoSize)
	if !ok {
		t.Fatalf("photo size = %T, want *tg.PhotoSize", photo.Sizes[0])
	}
	if size.W != 640 || size.H != 480 || size.Type != "x" {
		t.Fatalf("photo size = %+v, want 640x480 type x", size)
	}

	history, err := api.GetHistoryForTest(s, recipient.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(recipient.ID, sender.ID),
	})
	if err != nil {
		t.Fatalf("recipient history: %v", err)
	}
	historyMessages, ok := history.(*tg.MessagesMessages)
	if !ok || len(historyMessages.Messages) != 1 {
		t.Fatalf("recipient history = %#v, want one message", history)
	}
	recipientMessage, ok := historyMessages.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("recipient message = %T, want *tg.Message", historyMessages.Messages[0])
	}
	recipientPhoto := photoOfMessage(t, recipientMessage)
	if recipientPhoto.ID != photo.ID || recipientPhoto.AccessHash != photo.AccessHash || recipientMessage.Message != "" {
		t.Fatalf("recipient photo/message = id %d hash %d caption %q, want sender photo id %d hash %d and empty caption", recipientPhoto.ID, recipientPhoto.AccessHash, recipientMessage.Message, photo.ID, photo.AccessHash)
	}

	fileResult, err := api.GetFileForTest(s, recipient.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{
			ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: size.Type,
		},
		Limit: len(body),
	})
	if err != nil {
		t.Fatalf("recipient photo download: %v", err)
	}
	uploadFile, ok := fileResult.(*tg.UploadFile)
	if !ok {
		t.Fatalf("photo download = %T, want *tg.UploadFile", fileResult)
	}
	if _, ok := uploadFile.Type.(*tg.StorageFileJpeg); !ok {
		t.Fatalf("photo download type = %T, want *tg.StorageFileJpeg", uploadFile.Type)
	}
	if !bytes.Equal(uploadFile.Bytes, body) {
		t.Fatalf("recipient photo download differs from uploaded JPEG (%d bytes)", len(uploadFile.Bytes))
	}
	stranger, err := s.CreateUser(ctx, "+15551296047")
	if err != nil {
		t.Fatalf("create stranger: %v", err)
	}
	for _, tc := range []struct {
		name     string
		userID   int64
		location tg.InputFileLocationClass
	}{
		{name: "unknown photo id", userID: recipient.ID, location: &tg.InputPhotoFileLocation{ID: photo.ID + 1000, AccessHash: photo.AccessHash, ThumbSize: size.Type}},
		{name: "wrong photo hash", userID: recipient.ID, location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash + 1, ThumbSize: size.Type}},
		{name: "wrong photo size", userID: recipient.ID, location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "y"}},
		{name: "photo without live message entitlement", userID: stranger.ID, location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: size.Type}},
		{name: "document location naming photo", userID: recipient.ID, location: &tg.InputDocumentFileLocation{ID: photo.ID, AccessHash: photo.AccessHash}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := api.GetFileForTest(s, tc.userID, blobs, &tg.UploadGetFileRequest{Location: tc.location, Limit: 1024})
			rpcError(t, err, "LOCATION_INVALID")
		})
	}
}

func TestSendMediaUploadedPhotoToBasicGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	blobs := newBlobs(t)
	users, chat := chatWith(t, s, "+15551296035", "+15551296036", "+15551296037")
	body := jpegPhotoPayload(t, 640, 480)
	saveParts(t, s, users[0].ID, 563, body)

	result, err := api.SendMediaForTest(s, users[0].ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(users[0].ID, chat.ID), Media: uploadedPhoto(563, 1, "219343.jpg", jpegPhotoMD5(body)), RandomID: 563,
	})
	if err != nil {
		t.Fatalf("send group photo: %v", err)
	}
	photo := photoOfMessage(t, messageOf(t, result))
	files, err := s.FilesByIDs(ctx, []int64{photo.ID})
	if err != nil {
		t.Fatalf("load photo metadata: %v", err)
	}
	file, ok := files[photo.ID]
	if !ok || file.Kind != store.FileKindPhoto || file.Width != 640 || file.Height != 480 || !slices.Equal(file.SubtypeRights, []string{"send_photos"}) {
		t.Fatalf("stored group photo = %+v (found %v), want photo metadata with send_photos", file, ok)
	}

	for _, recipient := range users[1:] {
		history, err := api.GetHistoryForTest(s, recipient.ID, &tg.MessagesGetHistoryRequest{Peer: &tg.InputPeerChat{ChatID: chat.ID}})
		if err != nil {
			t.Fatalf("recipient %d history: %v", recipient.ID, err)
		}
		messages, ok := history.(*tg.MessagesMessages)
		if !ok || len(messages.Messages) != 1 {
			t.Fatalf("recipient %d history = %#v, want one message", recipient.ID, history)
		}
		message, ok := messages.Messages[0].(*tg.Message)
		if !ok {
			t.Fatalf("recipient %d message = %T, want *tg.Message", recipient.ID, messages.Messages[0])
		}
		gotPhoto := photoOfMessage(t, message)
		if gotPhoto.ID != photo.ID || message.Message != "" {
			t.Fatalf("recipient %d photo/message = id %d caption %q, want photo id %d and empty caption", recipient.ID, gotPhoto.ID, message.Message, photo.ID)
		}
		fileResult, err := api.GetFileForTest(s, recipient.ID, blobs, &tg.UploadGetFileRequest{
			Location: &tg.InputPhotoFileLocation{ID: gotPhoto.ID, AccessHash: gotPhoto.AccessHash, ThumbSize: "x"},
			Limit:    len(body),
		})
		if err != nil {
			t.Fatalf("recipient %d photo download: %v", recipient.ID, err)
		}
		uploadFile, ok := fileResult.(*tg.UploadFile)
		if !ok {
			t.Fatalf("recipient %d download = %T, want *tg.UploadFile", recipient.ID, fileResult)
		}
		if !bytes.Equal(uploadFile.Bytes, body) {
			t.Fatalf("recipient %d download = %d bytes, want the uploaded JPEG unchanged", recipient.ID, len(uploadFile.Bytes))
		}
	}
}

func TestSendMediaPhotoForeignUploadIDKeepsOwnerParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	blobs := newBlobs(t)
	owner, err := s.CreateUser(ctx, "+15551296048")
	if err != nil {
		t.Fatalf("upload owner: %v", err)
	}
	sender, err := s.CreateUser(ctx, "+15551296049")
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551296050")
	if err != nil {
		t.Fatalf("recipient: %v", err)
	}
	body := jpegPhotoPayload(t, 640, 480)
	const fileID, randomID = int64(5681), int64(5682)
	saveParts(t, s, owner.ID, fileID, body)
	_, err = api.SendMediaForTest(s, sender.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(sender.ID, recipient.ID), Media: uploadedPhoto(fileID, 1, "219343.jpg", jpegPhotoMD5(body)), RandomID: randomID,
	})
	rpcError(t, err, "MEDIA_INVALID")
	if _, found, err := s.MessageByRandomID(ctx, sender.ID, randomID); err != nil || found {
		t.Fatalf("foreign upload message = found %v, err=%v; want no message", found, err)
	}
	if count, _, _, err := s.UploadPartsSummary(ctx, owner.ID, fileID); err != nil || count != 1 {
		t.Fatalf("owner upload parts after foreign send = %d, err=%v; want one retained part", count, err)
	}
}

func TestBasicGroupPhotoRightsApplyToSendsAndForwards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	sender, err := s.CreateUser(ctx, "+15551296044")
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551296045")
	if err != nil {
		t.Fatalf("recipient: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551296046")
	if err != nil {
		t.Fatalf("group member: %v", err)
	}
	chat, err := s.CreateChat(ctx, recipient.ID, "Photo rights", []int64{member.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close conn: %v", err)
		}
	}()

	body := jpegPhotoPayload(t, 640, 480)
	saveParts(t, s, sender.ID, 567, body)
	source, err := api.SendMediaForTest(s, sender.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(sender.ID, member.ID), Media: uploadedPhoto(567, 1, "219343.jpg", jpegPhotoMD5(body)), RandomID: 567,
	})
	if err != nil {
		t.Fatalf("send photo source: %v", err)
	}
	photo := photoOfMessage(t, messageOf(t, source))
	files, err := s.FilesByIDs(ctx, []int64{photo.ID})
	if err != nil {
		t.Fatalf("load source photo metadata: %v", err)
	}
	if rights := files[photo.ID].SubtypeRights; !slices.Equal(rights, []string{"send_photos"}) {
		t.Fatalf("photo subtype rights = %v, want [send_photos]", rights)
	}
	peerHistory, err := api.GetHistoryForTest(s, member.ID, &tg.MessagesGetHistoryRequest{Peer: api.InputPeerUser(member.ID, sender.ID)})
	if err != nil {
		t.Fatalf("load photo source history: %v", err)
	}
	historyMessages, ok := peerHistory.(*tg.MessagesMessages)
	if !ok || len(historyMessages.Messages) != 1 {
		t.Fatalf("source history = %#v, want one message", peerHistory)
	}
	peerMessage, ok := historyMessages.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("source message = %T, want *tg.Message", historyMessages.Messages[0])
	}

	setChatDefaultRights(t, conn, chat.ID, "send_photos")
	before := basicChatWriteStats(t, conn, chat.ID)
	saveParts(t, s, member.ID, 568, body)
	_, err = api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(member.ID, chat.ID), Media: uploadedPhoto(568, 1, "219343.jpg", jpegPhotoMD5(body)), RandomID: 568,
	})
	rpcError(t, err, "CHAT_WRITE_FORBIDDEN")
	assertChatWriteStats(t, conn, chat.ID, before)
	if n, _, _, err := s.UploadPartsSummary(ctx, member.ID, 568); err != nil || n != 1 {
		t.Fatalf("denied photo upload parts = %d, err=%v, want one unconsumed part", n, err)
	}
	_, err = api.ForwardMessagesForTest(s, member.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: api.InputPeerUser(member.ID, sender.ID), ID: []int{peerMessage.ID},
		ToPeer: api.InputPeerChat(member.ID, chat.ID), RandomID: []int64{569},
	})
	rpcError(t, err, "CHAT_WRITE_FORBIDDEN")
	assertChatWriteStats(t, conn, chat.ID, before)

	setChatDefaultRights(t, conn, chat.ID, "send_docs")
	saveParts(t, s, member.ID, 571, body)
	sent, err := api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(member.ID, chat.ID), Media: uploadedPhoto(571, 1, "219343.jpg", jpegPhotoMD5(body)), RandomID: 571,
	})
	if err != nil {
		t.Fatalf("send photo under send_docs restriction: %v", err)
	}
	sentPhoto := photoOfMessage(t, messageOf(t, sent))
	sentFiles, err := s.FilesByIDs(ctx, []int64{sentPhoto.ID})
	if err != nil {
		t.Fatalf("load sent photo metadata: %v", err)
	}
	if got, ok := sentFiles[sentPhoto.ID]; !ok || got.Kind != store.FileKindPhoto || got.Width != 640 || got.Height != 480 {
		t.Fatalf("sent photo metadata = %+v (found %v), want 640x480 photo", got, ok)
	}
	assertChatWriteStats(t, conn, chat.ID, chatWriteStats{messages: before.messages + 2, events: before.events + 2})
	forwarded, err := api.ForwardMessagesForTest(s, member.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: api.InputPeerUser(member.ID, sender.ID), ID: []int{peerMessage.ID},
		ToPeer: api.InputPeerChat(member.ID, chat.ID), RandomID: []int64{570},
	})
	if err != nil {
		t.Fatalf("forward photo under send_docs restriction: %v", err)
	}
	if got := photoOfMessage(t, messageOf(t, forwarded)).ID; got != photo.ID {
		t.Fatalf("forwarded photo id = %d, want original %d", got, photo.ID)
	}
	assertChatWriteStats(t, conn, chat.ID, chatWriteStats{messages: before.messages + 4, events: before.events + 4})
}

func TestSendMediaPhotoRejectsInvalidJPEGBeforePublication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	sender, err := s.CreateUser(ctx, "+15551296038")
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551296039")
	if err != nil {
		t.Fatalf("recipient: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close conn: %v", err)
		}
	}()
	cases := []struct {
		name     string
		fileID   int64
		randomID int64
		body     []byte
		want     string
	}{
		{name: "unsupported format", fileID: 5641, randomID: 5641, body: []byte("not a JPEG"), want: "MEDIA_INVALID"},
		{name: "invalid dimensions", fileID: 5642, randomID: 5642, body: jpegPhotoPayload(t, 10001, 1), want: "PHOTO_INVALID_DIMENSIONS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saveParts(t, s, sender.ID, tc.fileID, tc.body)
			_, sendErr := api.SendMediaForTest(s, sender.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
				Peer: api.InputPeerUser(sender.ID, recipient.ID), Media: uploadedPhoto(tc.fileID, 1, "219343.jpg", jpegPhotoMD5(tc.body)), RandomID: tc.randomID,
			})
			rpcError(t, sendErr, tc.want)
			if _, found, lookupErr := s.MessageByRandomID(ctx, sender.ID, tc.randomID); lookupErr != nil || found {
				t.Fatalf("invalid photo message lookup = found %v, err %v; want no message", found, lookupErr)
			}
			var stored int64
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM files WHERE uploader_id = $1 AND stored = true`, sender.ID).Scan(&stored); err != nil {
				t.Fatalf("count stored photo rows: %v", err)
			}
			if stored != 0 {
				t.Fatalf("stored files after rejected photo = %d, want none", stored)
			}
		})
	}
}

func TestSendMediaPhotoRetrySurvivesStoreRecreation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	sender, err := s.CreateUser(ctx, "+15551296040")
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551296041")
	if err != nil {
		t.Fatalf("recipient: %v", err)
	}
	body := jpegPhotoPayload(t, 640, 480)
	saveParts(t, s, sender.ID, 565, body)
	req := &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(sender.ID, recipient.ID), Media: uploadedPhoto(565, 1, "219343.jpg", jpegPhotoMD5(body)), RandomID: 565,
	}
	first, err := api.SendMediaForTest(s, sender.ID, blobs, api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("send photo: %v", err)
	}
	firstUpdate := messageUpdateOf(t, first)
	firstMessage := messageOf(t, first)
	firstPhoto := photoOfMessage(t, firstMessage)

	restarted, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Errorf("close reopened store: %v", err)
		}
	})
	second, err := api.SendMediaForTest(restarted, sender.ID, blobs, api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("retry photo after store recreation: %v", err)
	}
	secondUpdate := messageUpdateOf(t, second)
	secondMessage := messageOf(t, second)
	secondPhoto := photoOfMessage(t, secondMessage)
	if secondUpdate.Pts != firstUpdate.Pts || secondMessage.ID != firstMessage.ID || secondPhoto.ID != firstPhoto.ID {
		t.Fatalf("retry update/photo = pts %d message %v photo %d, want original pts %d message %v photo %d", secondUpdate.Pts, secondUpdate.Message, secondPhoto.ID, firstUpdate.Pts, firstUpdate.Message, firstPhoto.ID)
	}

	history, err := api.GetHistoryForTest(restarted, recipient.ID, &tg.MessagesGetHistoryRequest{Peer: api.InputPeerUser(recipient.ID, sender.ID)})
	if err != nil {
		t.Fatalf("recipient history after recreation: %v", err)
	}
	messages, ok := history.(*tg.MessagesMessages)
	if !ok || len(messages.Messages) != 1 {
		t.Fatalf("recipient history after recreation = %#v, want one message", history)
	}
	historyMessage, ok := messages.Messages[0].(*tg.Message)
	if !ok || photoOfMessage(t, historyMessage).ID != firstPhoto.ID {
		t.Fatalf("recipient photo after recreation = %T, want original photo %d", messages.Messages[0], firstPhoto.ID)
	}
	fileResult, err := api.GetFileForTest(restarted, recipient.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: firstPhoto.ID, AccessHash: firstPhoto.AccessHash, ThumbSize: "x"},
		Limit:    len(body),
	})
	if err != nil {
		t.Fatalf("recipient download after recreation: %v", err)
	}
	if uploadFile, ok := fileResult.(*tg.UploadFile); !ok || !bytes.Equal(uploadFile.Bytes, body) {
		t.Fatalf("recreated-store download = %T, want original %d-byte JPEG", fileResult, len(body))
	}
	if count := countFiles(t, ctx, dsn); count != 1 {
		t.Fatalf("file rows after retry = %d, want one", count)
	}
}

func TestSendMediaPhotoResendsRejectReferencesAndUnsupportedFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	sender, err := s.CreateUser(ctx, "+15551296042")
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551296043")
	if err != nil {
		t.Fatalf("recipient: %v", err)
	}
	body := jpegPhotoPayload(t, 640, 480)
	saveParts(t, s, sender.ID, 566, body)
	req := &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(sender.ID, recipient.ID), Media: uploadedPhoto(566, 1, "219343.jpg", jpegPhotoMD5(body)), RandomID: 566,
	}
	first, err := api.SendMediaForTest(s, sender.ID, blobs, api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("send photo: %v", err)
	}
	photo := photoOfMessage(t, messageOf(t, first))

	unsupported := uploadedPhoto(567, 1, "219343.jpg", jpegPhotoMD5(body))
	unsupported.SetStickers([]tg.InputDocumentClass{&tg.InputDocumentEmpty{}})
	_, err = api.SendMediaForTest(s, sender.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: req.Peer, Media: unsupported, RandomID: req.RandomID,
	})
	rpcError(t, err, "MEDIA_INVALID")

	_, err = api.SendMediaForTest(s, sender.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     req.Peer,
		Media:    &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference}},
		RandomID: req.RandomID,
	})
	rpcError(t, err, "MEDIA_INVALID")
	if count := countFiles(t, ctx, dsn); count != 1 {
		t.Fatalf("file rows after rejected photo resends = %d, want one", count)
	}
	history, err := api.GetHistoryForTest(s, sender.ID, &tg.MessagesGetHistoryRequest{Peer: api.InputPeerUser(sender.ID, recipient.ID)})
	if err != nil {
		t.Fatalf("sender history: %v", err)
	}
	messages, ok := history.(*tg.MessagesMessages)
	if !ok || len(messages.Messages) != 1 {
		t.Fatalf("sender history after rejected resends = %#v, want one message", history)
	}
}

func photoOfMessage(t *testing.T, message *tg.Message) *tg.Photo {
	t.Helper()
	media, ok := message.Media.(*tg.MessageMediaPhoto)
	if !ok {
		t.Fatalf("message media = %T, want *tg.MessageMediaPhoto", message.Media)
	}
	photo, ok := media.Photo.(*tg.Photo)
	if !ok {
		t.Fatalf("photo = %T, want *tg.Photo", media.Photo)
	}
	return photo
}

func TestSendMediaRejectsThumb(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551296041")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551296042")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	saveParts(t, s, a.ID, 558, []byte("body"))

	media := uploadedDocument(558, 1, "clip.bin", "application/octet-stream")
	media.SetThumb(&tg.InputFile{ID: 559, Parts: 1, Name: "thumb.jpg"})
	_, err = api.SendMediaForTest(s, a.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(a.ID, b.ID),
		Media:    media,
		RandomID: 45,
	})
	rpcError(t, err, "MEDIA_INVALID")
}

func TestSendMediaRejectsPartCountMismatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	a, err := s.CreateUser(ctx, "+15551296051")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551296052")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	saveParts(t, s, a.ID, 560, []byte("one"), []byte("two"))

	_, err = api.SendMediaForTest(s, a.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(a.ID, b.ID),
		Media:    uploadedDocument(560, 3, "short.txt", "text/plain"),
		RandomID: 46,
	})
	rpcError(t, err, "MEDIA_INVALID")
	// The row is allocated after the check, so a rejected assembly must not
	// have consumed any of the account's quota.
	if n := countFiles(t, ctx, dsn); n != 0 {
		t.Errorf("files rows = %d, want 0", n)
	}
}

func TestSendMediaRejectsAnotherAccountsUpload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	a, err := s.CreateUser(ctx, "+15551296061")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551296062")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	// b uploads; a names b's file id. The parts are looked up under the
	// caller's own user id, so a's assembly finds nothing.
	saveParts(t, s, b.ID, 561, []byte("b's bytes"))

	_, err = api.SendMediaForTest(s, a.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(a.ID, b.ID),
		Media:    uploadedDocument(561, 1, "stolen.txt", "text/plain"),
		RandomID: 47,
	})
	rpcError(t, err, "MEDIA_INVALID")
	if n := countFiles(t, ctx, dsn); n != 0 {
		t.Errorf("files rows = %d, want 0", n)
	}
}

func TestSendMediaToChatRequiresMembership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	_, chat := chatWith(t, s, "+15551296071", "+15551296072")
	outsider, err := s.CreateUser(ctx, "+15551296073")
	if err != nil {
		t.Fatalf("outsider: %v", err)
	}
	saveParts(t, s, outsider.ID, 562, []byte("intrusion"))

	_, err = api.SendMediaForTest(s, outsider.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     &tg.InputPeerChat{ChatID: chat.ID},
		Media:    uploadedDocument(562, 1, "intrusion.txt", "text/plain"),
		RandomID: 48,
	})
	rpcError(t, err, "PEER_ID_INVALID")
	// Membership is checked before assembly, so no bytes and no row.
	if n := countFiles(t, ctx, dsn); n != 0 {
		t.Errorf("files rows = %d, want 0", n)
	}
}

func TestSendMediaEnforcesStorageQuota(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551296081")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551296082")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	saveParts(t, s, a.ID, 563, []byte("eleven byte"))

	_, err = api.SendMediaForTest(s, a.ID, newBlobs(t), 4, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(a.ID, b.ID),
		Media:    uploadedDocument(563, 1, "big.txt", "text/plain"),
		RandomID: 49,
	})
	rpcError(t, err, "STORAGE_CHECK_FAILED")
}

// A forward whose file row is gone is refused, and reports the invalid message
// id a deleted source already reports. The source message here is still live
// and still names the file, so the handler's own liveness read passes and the
// interlock inside the transaction is the only thing that can refuse.
//
// The send paths reject the same state through the same interlock, asserted in
// the store's own tests: a handler cannot stage it, because handleSendMedia
// answers a resend from its own read before the send transaction opens, and a
// first send assembles the file it then references.
func TestForwardRejectsErasedFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	a, err := s.CreateUser(ctx, "+15551296101")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551296102")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	c, err := s.CreateUser(ctx, "+15551296103")
	if err != nil {
		t.Fatalf("user c: %v", err)
	}
	peerB := api.InputPeerUser(a.ID, b.ID)
	peerC := api.InputPeerUser(a.ID, c.ID)
	saveParts(t, s, a.ID, 565, []byte("eleven byte"))
	if _, err = api.SendMediaForTest(s, a.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: peerB, Media: uploadedDocument(565, 1, "doc.txt", "text/plain"), RandomID: 51,
	}); err != nil {
		t.Fatalf("seed send: %v", err)
	}
	eraseFiles(t, ctx, dsn)

	_, err = api.ForwardMessagesForTest(s, a.ID, &tg.MessagesForwardMessagesRequest{
		ToPeer: peerC, FromPeer: peerB, ID: []int{1}, RandomID: []int64{52},
	})
	rpcError(t, err, "MESSAGE_ID_INVALID")

	msgs, err := s.History(ctx, c.ID, store.PeerTypeUser, a.ID, 0, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("destination holds %d messages, want 0", len(msgs))
	}
}

// eraseFiles removes every files row. Nothing in the shipped server deletes one
// — the eraser is a later stage of M17 — so this is the only way a handler test
// can reach the state the reference interlock refuses.
func eraseFiles(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		if cerr := conn.Close(ctx); cerr != nil {
			t.Errorf("close conn: %v", cerr)
		}
	}()
	tag, err := conn.Exec(ctx, "DELETE FROM files")
	if err != nil {
		t.Fatalf("erase files: %v", err)
	}
	if tag.RowsAffected() == 0 {
		t.Fatal("erase files removed nothing — the test is asserting against a file that was never stored")
	}
}

// countFiles reports how many files rows exist, for the rejection paths that
// must not allocate one.
func countFiles(t *testing.T, ctx context.Context, dsn string) int64 {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		if cerr := conn.Close(ctx); cerr != nil {
			t.Errorf("close conn: %v", cerr)
		}
	}()
	var n int64
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM files").Scan(&n); err != nil {
		t.Fatalf("count files: %v", err)
	}
	return n
}

// Criterion 12 at the boundary a client actually reaches: a resend naming a
// file the eraser has removed is refused, not replayed.
//
// The store-level test for this drives SendMessage directly, which no client
// does. sendMedia answers a transport retry from the caller's stored message
// before it reaches the interlock, and MessageByRandomID has no deleted
// predicate — so the branch above the interlock is where criterion 12 is
// actually decided, and it has to be tested here.
//
// The leak this closes is a per-file erasure oracle, which is why it is not
// merely a consistency point. The same repeated request answers with the
// document while the file row exists and with a plain message once it is gone,
// so the uploader reads off exactly which of their files was erased — and
// therefore which recipient deleted which media — on demand, with none of the
// blunting the randomized sweep interval was accepted as providing.
func TestSendMediaResendAfterErasureIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	blobs := newBlobs(t)
	// One blob store behind both the handler's assembly and the eraser's
	// unlink, the way the process wires them.
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if cerr := s.Close(); cerr != nil {
			t.Errorf("close: %v", cerr)
		}
	})

	a, err := s.CreateUser(ctx, "+15551296201")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551296202")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	saveParts(t, s, a.ID, 7700, []byte("erasable bytes"))

	req := &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(a.ID, b.ID),
		Media:    uploadedDocument(7700, 1, "doc.txt", "text/plain"),
		Message:  "look",
		RandomID: 7701,
	}
	first, err := api.SendMediaForTest(s, a.ID, blobs, api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("send media: %v", err)
	}
	// The control on the oracle: before erasure this same request answers with
	// the document. Without it, an assertion that the resend is refused would
	// pass on a handler that refused every resend.
	doc := documentOf(t, first)
	sent := messageOf(t, first)

	// Both copies deleted, then erased. revoke = true takes the recipient's
	// copy too, through the shipped delete path rather than a direct update.
	if _, err = s.DeleteMessages(ctx, a.ID, []int64{int64(sent.ID)}, true); err != nil {
		t.Fatalf("delete: %v", err)
	}
	counts, err := s.SweepMediaErasure(ctx, time.Now().Add(time.Hour), store.ErasureScanBatch)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if counts.Erased != 1 {
		t.Fatalf("sweep counts = %+v, want one erased — the file under test was not erased", counts)
	}

	// The byte-identical resend a client would send after losing the reply.
	second, err := api.SendMediaForTest(s, a.ID, blobs, api.TestMaxUserStorageBytes, req)
	if err == nil {
		t.Fatalf("resend after erasure returned %#v, want MEDIA_INVALID: the erased file %d was replayed", second, doc.ID)
	}
	rpcError(t, err, "MEDIA_INVALID")

	// Nothing was written by the refusal: no new message for the recipient and
	// no new file row.
	hist, err := api.GetHistoryForTest(s, b.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(b.ID, a.ID),
	})
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	if msgs, ok := hist.(*tg.MessagesMessages); !ok || len(msgs.Messages) != 0 {
		t.Errorf("recipient history = %#v, want empty after a refused resend", hist)
	}
	if n := countFiles(t, ctx, dsn); n != 0 {
		t.Errorf("files rows = %d, want 0 — the refused resend reassembled the upload", n)
	}
}

// deletedOriginalResend runs one arm of the erasure-oracle probe: send media,
// soft-delete both copies through the shipped delete path, optionally let the
// erasure sweep past, then resend the byte-identical request. It returns what
// that second call answered.
//
// The two arms differ in exactly one thing — whether the file behind the
// deleted original still exists — which is the bit an uploader must not be able
// to read off the reply.
func deletedOriginalResend(t *testing.T, phoneA, phoneB string, fileID, randomID int64, runSweep bool) (any, error) {
	t.Helper()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	blobs := newBlobs(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if cerr := s.Close(); cerr != nil {
			t.Errorf("close: %v", cerr)
		}
	})
	a, err := s.CreateUser(ctx, phoneA)
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, phoneB)
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	saveParts(t, s, a.ID, fileID, []byte("erasable bytes"))

	req := &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(a.ID, b.ID),
		Media:    uploadedDocument(fileID, 1, "doc.txt", "text/plain"),
		Message:  "look",
		RandomID: randomID,
	}
	first, err := api.SendMediaForTest(s, a.ID, blobs, api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("send media: %v", err)
	}
	// The control on the whole probe: before anything is deleted this request
	// answers with the document. Without it both arms could be refusals for a
	// reason that has nothing to do with the leak.
	documentOf(t, first)
	sent := messageOf(t, first)

	if _, err = s.DeleteMessages(ctx, a.ID, []int64{int64(sent.ID)}, true); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if runSweep {
		counts, serr := s.SweepMediaErasure(ctx, time.Now().Add(time.Hour), store.ErasureScanBatch)
		if serr != nil {
			t.Fatalf("sweep: %v", serr)
		}
		if counts.Erased != 1 {
			t.Fatalf("sweep counts = %+v, want one erased", counts)
		}
		if n := countFiles(t, ctx, dsn); n != 0 {
			t.Fatalf("files rows = %d after the sweep, want 0 — this arm is not the erased one", n)
		}
	} else if n := countFiles(t, ctx, dsn); n != 1 {
		// Without this the two arms could be identical and the comparison below
		// would be comparing one state with itself.
		t.Fatalf("files rows = %d without a sweep, want 1 — this arm is not the surviving one", n)
	}
	return api.SendMediaForTest(s, a.ID, blobs, api.TestMaxUserStorageBytes, req)
}

// A resend whose original is soft-deleted answers the same way whether or not
// the eraser has taken the file behind it.
//
// Refusing is not the property — indistinguishability is. Two arms that both
// fail but fail differently leave the oracle exactly where it was: the uploader
// still reads off which of their files has been erased, and therefore which
// recipient deleted which media, from one repeated request. So this compares
// the two replies against each other rather than each against a constant.
//
// The surviving-file arm is the one that carried the leak after the first fix.
// The handler stopped short-circuiting on a deleted original, but the
// fall-through reached SendMessage, where the interlock refused only when the
// file row was gone — and when it survived, the store's own dedup replayed the
// deleted original and the handler rendered its document.
func TestSendMediaResendOfDeletedOriginalIsIndistinguishable(t *testing.T) {
	t.Parallel()

	kept, keptErr := deletedOriginalResend(t, "+15551296211", "+15551296212", 7710, 7711, false)
	erased, erasedErr := deletedOriginalResend(t, "+15551296221", "+15551296222", 7720, 7721, true)

	if keptErr == nil {
		t.Fatalf("resend with the file still present returned %#v, want a refusal — the deleted original was replayed", kept)
	}
	if erasedErr == nil {
		t.Fatalf("resend after erasure returned %#v, want a refusal", erased)
	}
	rpcError(t, keptErr, "MEDIA_INVALID")
	rpcError(t, erasedErr, "MEDIA_INVALID")

	// The load-bearing assertion. Anything that distinguishes the two replies —
	// a different code, a different message, one carrying a payload — is the
	// oracle still open.
	if keptErr.Error() != erasedErr.Error() {
		t.Errorf("the arms answer differently: file present -> %q, file erased -> %q; whether the sweep has run is readable from the reply",
			keptErr, erasedErr)
	}
	if kept != nil || erased != nil {
		t.Errorf("a refused resend returned a payload: present -> %#v, erased -> %#v", kept, erased)
	}
}

// A live assembled Put may outlive an intentionally tiny temporary cutoff.
// Its file-row shared hold must make the blob sweep skip the temporary rather
// than unlinking the open path out from under Local.Put.
func TestAssembledPutSurvivesSmallTempCutoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	plain := newBlobs(t)
	blobs := &blockingAssembledPut{
		Store:   plain,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	defer blobs.unblock()

	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	a, err := s.CreateUser(ctx, "+15551296301")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551296302")
	if err != nil {
		t.Fatalf("create recipient: %v", err)
	}
	const clientFileID = 7730
	const body = "a live assembled upload"
	if _, err := api.SaveFilePartBlobsForTest(s, blobs, a.ID, &tg.UploadSaveFilePartRequest{
		FileID: clientFileID, FilePart: 0, Bytes: []byte(body),
	}); err != nil {
		t.Fatalf("save part: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := api.SendMediaForTest(s, a.ID, blobs, api.TestMaxUserStorageBytes,
			&tg.MessagesSendMediaRequest{
				Peer:     api.InputPeerUser(a.ID, b.ID),
				Media:    uploadedDocument(clientFileID, 1, "live.txt", "text/plain"),
				Message:  "live",
				RandomID: clientFileID,
			})
		done <- err
	}()

	<-blobs.started
	local, ok := plain.(*blob.Local)
	if !ok {
		t.Fatalf("blob backend = %T, want *blob.Local", plain)
	}
	tempPath := filepath.Join(local.RootDir(), filepath.FromSlash(blobs.key+blob.TempSuffix))
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(tempPath, old, old); err != nil {
		t.Fatalf("age live temporary: %v", err)
	}

	mediaCounts, err := s.SweepMediaErasure(ctx, time.Now().Add(time.Hour), store.ErasureScanBatch)
	if err != nil {
		t.Fatalf("media sweep: %v", err)
	}
	if mediaCounts.UnassembledConsidered != 1 || mediaCounts.UnassembledContended != 1 || mediaCounts.UnassembledErased != 0 {
		t.Fatalf("media sweep counts = %+v, want one contended unassembled row and no erase", mediaCounts)
	}

	cutoff := time.Now().Add(-time.Minute)
	counts, err := s.SweepBlobErasure(ctx, cutoff, cutoff)
	if err != nil {
		t.Fatalf("blob sweep: %v", err)
	}
	if counts.TempConsidered != 1 || counts.TempContended != 1 || counts.TempUnlinkAttempts != 0 {
		t.Fatalf("temp sweep counts = %+v, want one contended candidate and no unlink", counts)
	}
	if _, err := os.Stat(tempPath); err != nil {
		t.Fatalf("live temporary after sweep: %v", err)
	}

	blobs.unblock()
	if err := <-done; err != nil {
		t.Fatalf("send after small-cutoff sweep: %v", err)
	}
	got, err := blobs.ReadAt(ctx, blobs.key, 0, int64(len(body)))
	if err != nil {
		t.Fatalf("read assembled bytes: %v", err)
	}
	if string(got) != body {
		t.Fatalf("assembled bytes = %q, want %q", got, body)
	}
}
