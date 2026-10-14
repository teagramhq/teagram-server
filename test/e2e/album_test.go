package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

func albumPhotoItem(photo *tg.Photo, randomID int64) tg.InputSingleMedia {
	return tg.InputSingleMedia{
		Media:    &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference}},
		RandomID: randomID,
	}
}

func albumDocumentItem(doc *tg.Document, randomID int64) tg.InputSingleMedia {
	return tg.InputSingleMedia{
		Media:    &tg.InputMediaDocument{ID: &tg.InputDocument{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}},
		RandomID: randomID,
	}
}

func sendAlbum(t *testing.T, ctx context.Context, mc *mediaClient, peer tg.InputPeerClass, items ...tg.InputSingleMedia) (tg.UpdatesClass, error) {
	t.Helper()
	var result tg.UpdatesClass
	err := execMedia(t, ctx, mc.cmds, func(ctx context.Context, c *tg.Client) error {
		var err error
		result, err = c.MessagesSendMultiMedia(ctx, &tg.MessagesSendMultiMediaRequest{Peer: peer, MultiMedia: items})
		return err
	})
	return result, err
}

func mustSendAlbum(t *testing.T, ctx context.Context, mc *mediaClient, peer tg.InputPeerClass, items ...tg.InputSingleMedia) []*tg.Message {
	t.Helper()
	result, err := sendAlbum(t, ctx, mc, peer, items...)
	if err != nil {
		t.Fatalf("sendMultiMedia: %v", err)
	}
	messages := albumResultMessages(t, result)
	if len(messages) != len(items) {
		t.Fatalf("sendMultiMedia returned %d messages, want %d", len(messages), len(items))
	}
	assertOneAlbum(t, messages, "sender result")
	return messages
}

func albumResultMessages(t *testing.T, result tg.UpdatesClass) []*tg.Message {
	t.Helper()
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("sendMultiMedia result = %T, want *tg.Updates", result)
	}
	messages := make([]*tg.Message, 0, len(updates.Updates)/2)
	for _, update := range updates.Updates {
		var class tg.MessageClass
		switch u := update.(type) {
		case *tg.UpdateNewMessage:
			class = u.Message
		case *tg.UpdateNewChannelMessage:
			class = u.Message
		default:
			continue
		}
		message, ok := class.(*tg.Message)
		if !ok {
			t.Fatalf("album update message = %T, want *tg.Message", class)
		}
		messages = append(messages, message)
	}
	return messages
}

// assertOneAlbum checks every message carries the same non-zero grouped_id and
// returns it.
func assertOneAlbum(t *testing.T, messages []*tg.Message, label string) int64 {
	t.Helper()
	if len(messages) < 2 {
		t.Fatalf("%s has %d messages, want an album", label, len(messages))
	}
	groupedID, ok := messages[0].GetGroupedID()
	if !ok || groupedID == 0 {
		t.Fatalf("%s first message has no grouped_id", label)
	}
	for i, message := range messages {
		if got, ok := message.GetGroupedID(); !ok || got != groupedID {
			t.Fatalf("%s message %d grouped_id = (%d, %v), want %d", label, i, got, ok, groupedID)
		}
	}
	return groupedID
}

func albumHistory(t *testing.T, ctx context.Context, mc *mediaClient, peer tg.InputPeerClass) []*tg.Message {
	t.Helper()
	var messages []*tg.Message
	execChat(t, ctx, mc.cmds, func(ctx context.Context, c *tg.Client) error {
		result, err := c.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 50})
		if err != nil {
			return err
		}
		var classes []tg.MessageClass
		switch page := result.(type) {
		case *tg.MessagesMessages:
			classes = page.Messages
		case *tg.MessagesMessagesSlice:
			classes = page.Messages
		case *tg.MessagesChannelMessages:
			classes = page.Messages
		default:
			return fmt.Errorf("history = %T", result)
		}
		for _, class := range classes {
			if message, ok := class.(*tg.Message); ok && message.Media != nil {
				messages = append(messages, message)
			}
		}
		return nil
	})
	return messages
}

func albumPhotoOf(t *testing.T, message *tg.Message) *tg.Photo {
	t.Helper()
	media, ok := message.Media.(*tg.MessageMediaPhoto)
	if !ok {
		t.Fatalf("message %d media = %T, want photo", message.ID, message.Media)
	}
	photo, ok := media.Photo.(*tg.Photo)
	if !ok {
		t.Fatalf("message %d photo = %T", message.ID, media.Photo)
	}
	return photo
}

func downloadAlbumPhoto(t *testing.T, ctx context.Context, mc *mediaClient, photo *tg.Photo) []byte {
	t.Helper()
	original, ok := photo.Sizes[len(photo.Sizes)-1].(*tg.PhotoSize)
	if !ok {
		t.Fatalf("photo original = %T", photo.Sizes[len(photo.Sizes)-1])
	}
	var out []byte
	execChat(t, ctx, mc.cmds, func(ctx context.Context, c *tg.Client) error {
		res, err := c.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: original.Type},
			Limit:    mediaPartSize,
		})
		if err != nil {
			return err
		}
		file, ok := res.(*tg.UploadFile)
		if !ok {
			return fmt.Errorf("getFile = %T", res)
		}
		out = file.Bytes
		return nil
	})
	return out
}

func albumConn(t *testing.T, ctx context.Context, mc *mediaClient) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, mc.dsn)
	if err != nil {
		t.Fatalf("connect album fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close album fixture: %v", err)
		}
	})
	return conn
}

func albumGrantCount(t *testing.T, ctx context.Context, conn *pgx.Conn, ownerID int64) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM file_upload_grants WHERE owner_id = $1`, ownerID).Scan(&n); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	return n
}

func albumRowCount(t *testing.T, ctx context.Context, conn *pgx.Conn) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM messages WHERE grouped_id IS NOT NULL) +
		(SELECT count(*) FROM channel_messages WHERE grouped_id IS NOT NULL)`).Scan(&n); err != nil {
		t.Fatalf("count album rows: %v", err)
	}
	return n
}

// TestSendMultiMediaRefusesReferencesWithoutGrant: another account's grant, a
// guessed id, a file the caller received and a file the caller sent and then
// deleted all answer the identical MEDIA_INVALID and write nothing, and the
// victim's grant still works afterwards.
func TestSendMultiMediaRefusesReferencesWithoutGrant(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	clients := bootMediaEnv(t, ctx, "+15551318501", "+15551318502")
	a, b := clients[0], clients[1]
	conn := albumConn(t, ctx, a)

	a1 := uploadMediaDocument(t, ctx, a, b.id, 0x5A1B0001, "a1.bin", []byte("a1"))
	a2 := uploadMediaDocument(t, ctx, a, b.id, 0x5A1B0002, "a2.bin", []byte("a2"))
	b1 := uploadMediaDocument(t, ctx, b, a.id, 0x5A1B0011, "b1.bin", []byte("b1"))

	guessed := *a1
	guessed.ID += 1000
	for name, foreign := range map[string]*tg.Document{"other account's grant": a1, "guessed id": &guessed} {
		_, err := sendAlbum(t, ctx, b, peerUser(b.id, a.id), albumDocumentItem(b1, 1), albumDocumentItem(foreign, 2))
		assertRPCError(t, err, "MEDIA_INVALID")
		if n := albumRowCount(t, ctx, conn); n != 0 {
			t.Fatalf("%s wrote %d album rows", name, n)
		}
	}

	received := mustSendAlbum(t, ctx, a, peerUser(a.id, b.id), albumDocumentItem(a1, 11), albumDocumentItem(a2, 12))
	if n := albumGrantCount(t, ctx, conn, a.id); n != 0 {
		t.Fatalf("victim grants after own send = %d, want consumed", n)
	}
	receivedDoc := documentOf(t, albumHistory(t, ctx, b, peerUser(b.id, a.id))[0])
	_, err := sendAlbum(t, ctx, b, peerUser(b.id, a.id), albumDocumentItem(b1, 3), albumDocumentItem(receivedDoc, 4))
	assertRPCError(t, err, "MEDIA_INVALID")

	b2 := uploadMediaDocument(t, ctx, b, a.id, 0x5A1B0012, "b2.bin", []byte("b2"))
	b3 := uploadMediaDocument(t, ctx, b, a.id, 0x5A1B0013, "b3.bin", []byte("b3"))
	sent := mustSendAlbum(t, ctx, b, peerUser(b.id, a.id), albumDocumentItem(b2, 21), albumDocumentItem(b3, 22))
	execChat(t, ctx, b.cmds, func(ctx context.Context, c *tg.Client) error {
		_, err := c.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{Revoke: true, ID: []int{sent[0].ID, sent[1].ID}})
		return err
	})
	_, err = sendAlbum(t, ctx, b, peerUser(b.id, a.id), albumDocumentItem(b1, 5), albumDocumentItem(b2, 6))
	assertRPCError(t, err, "MEDIA_INVALID")

	if n := albumGrantCount(t, ctx, conn, b.id); n != 1 {
		t.Fatalf("caller grants after refusals = %d, want b1 unconsumed", n)
	}
	if n := albumRowCount(t, ctx, conn); n != 2*len(received)+2*len(sent) {
		t.Fatalf("album rows = %d, want only the two sent albums", n)
	}
	for _, location := range []*tg.Document{b2, b3} {
		err := execMedia(t, ctx, b.cmds, func(ctx context.Context, c *tg.Client) error {
			_, err := c.UploadGetFile(ctx, &tg.UploadGetFileRequest{
				Location: &tg.InputDocumentFileLocation{ID: location.ID, AccessHash: location.AccessHash, FileReference: location.FileReference},
				Limit:    mediaPartSize,
			})
			return err
		})
		assertRPCError(t, err, "LOCATION_INVALID")
	}
}

// TestSendMultiMediaGrantsAreSingleUse covers sequential and concurrent reuse,
// expiry, erasure between upload and send, and one bad item in three.
func TestSendMultiMediaGrantsAreSingleUse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	clients := bootMediaEnv(t, ctx, "+15551318511", "+15551318512")
	a, b := clients[0], clients[1]
	conn := albumConn(t, ctx, a)
	body := smokeJPEGAtUploadSize(t, smokePhotoPartCount)

	p1 := uploadMediaPhoto(t, ctx, a, b.id, 0x5A1C0001, "p1.jpg", body)
	p2 := uploadMediaPhoto(t, ctx, a, b.id, 0x5A1C0002, "p2.jpg", body)
	mustSendAlbum(t, ctx, a, peerUser(a.id, b.id), albumPhotoItem(p1, 1), albumPhotoItem(p2, 2))
	_, err := sendAlbum(t, ctx, a, peerUser(a.id, b.id), albumPhotoItem(p1, 3), albumPhotoItem(p2, 4))
	assertRPCError(t, err, "MEDIA_INVALID")

	c1 := uploadMediaDocument(t, ctx, a, b.id, 0x5A1C0011, "c1.bin", []byte("c1"))
	c2 := uploadMediaDocument(t, ctx, a, b.id, 0x5A1C0012, "c2.bin", []byte("c2"))
	errs := make([]error, 2)
	execChat(t, ctx, a.cmds, func(ctx context.Context, c *tg.Client) error {
		var wg sync.WaitGroup
		for i := range errs {
			wg.Go(func() {
				_, errs[i] = c.MessagesSendMultiMedia(ctx, &tg.MessagesSendMultiMediaRequest{
					Peer:       peerUser(a.id, b.id),
					MultiMedia: []tg.InputSingleMedia{albumDocumentItem(c1, int64(100+2*i)), albumDocumentItem(c2, int64(101+2*i))},
				})
			})
		}
		wg.Wait()
		return nil
	})
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("concurrent sends = %v, want exactly one success", errs)
	}
	for _, err := range errs {
		if err != nil && !tgerr.Is(err, "MEDIA_INVALID") {
			t.Fatalf("losing concurrent send = %v, want MEDIA_INVALID", err)
		}
	}
	rowsBefore := albumRowCount(t, ctx, conn)

	e1 := uploadMediaDocument(t, ctx, a, b.id, 0x5A1C0021, "e1.bin", []byte("e1"))
	e2 := uploadMediaDocument(t, ctx, a, b.id, 0x5A1C0022, "e2.bin", []byte("e2"))
	if _, err = conn.Exec(ctx, `UPDATE file_upload_grants SET created_at = now() - interval '7 hours' WHERE file_id = $1`, e1.ID); err != nil {
		t.Fatalf("age grant: %v", err)
	}
	_, err = sendAlbum(t, ctx, a, peerUser(a.id, b.id), albumDocumentItem(e1, 31), albumDocumentItem(e2, 32))
	assertRPCError(t, err, "MEDIA_INVALID")

	if _, err = a.store.SweepMediaErasure(ctx, time.Now().Add(time.Hour), store.ErasureScanBatch); err != nil {
		t.Fatalf("erase unsent uploads: %v", err)
	}
	if n := albumGrantCount(t, ctx, conn, a.id); n != 0 {
		t.Fatalf("grants after erasure = %d, want 0", n)
	}
	_, err = sendAlbum(t, ctx, a, peerUser(a.id, b.id), albumDocumentItem(e2, 33), albumDocumentItem(e1, 34))
	assertRPCError(t, err, "MEDIA_INVALID")
	if n := albumRowCount(t, ctx, conn); n != rowsBefore {
		t.Fatalf("album rows after expired and erased sends = %d, want %d", n, rowsBefore)
	}

	g := []*tg.Document{
		uploadMediaDocument(t, ctx, a, b.id, 0x5A1C0031, "g1.bin", []byte("g1")),
		uploadMediaDocument(t, ctx, a, b.id, 0x5A1C0032, "g2.bin", []byte("g2")),
		uploadMediaDocument(t, ctx, a, b.id, 0x5A1C0033, "g3.bin", []byte("g3")),
	}
	bad := *g[2]
	bad.AccessHash++
	_, err = sendAlbum(t, ctx, a, peerUser(a.id, b.id), albumDocumentItem(g[0], 41), albumDocumentItem(g[1], 42), albumDocumentItem(&bad, 43))
	assertRPCError(t, err, "MEDIA_INVALID")
	if n := albumGrantCount(t, ctx, conn, a.id); n != 3 {
		t.Fatalf("grants after one bad item = %d, want 3", n)
	}
	if n := albumRowCount(t, ctx, conn); n != rowsBefore {
		t.Fatalf("album rows after one bad item = %d, want %d", n, rowsBefore)
	}
	mustSendAlbum(t, ctx, a, peerUser(a.id, b.id), albumDocumentItem(g[0], 41), albumDocumentItem(g[1], 42), albumDocumentItem(g[2], 43))
}

// TestSendMultiMediaGroupAndChannel: a basic-group album reaches every member
// as one album, a channel photo album reaches subscribers, and the rights
// answers are sendMedia's.
func TestSendMultiMediaGroupAndChannel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	clients := bootMediaEnv(t, ctx, "+15551318521", "+15551318522", "+15551318523")
	a, b, c := clients[0], clients[1], clients[2]
	conn := albumConn(t, ctx, a)
	body := smokeJPEGAtUploadSize(t, smokePhotoPartCount)

	var chatID int64
	execChat(t, ctx, a.cmds, func(ctx context.Context, client *tg.Client) error {
		created, err := client.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
			Title: "album group", Users: []tg.InputUserClass{inputUser(a.id, b.id), inputUser(a.id, c.id)},
		})
		if err != nil {
			return err
		}
		updates, ok := created.Updates.(*tg.Updates)
		if !ok || len(updates.Chats) != 1 {
			return fmt.Errorf("createChat result = %T", created.Updates)
		}
		group, ok := updates.Chats[0].(*tg.Chat)
		if !ok {
			return fmt.Errorf("createChat chat = %T", updates.Chats[0])
		}
		chatID = group.ID
		return nil
	})
	chat := &tg.InputPeerChat{ChatID: chatID}
	photo := uploadMediaPhoto(t, ctx, a, b.id, 0x5A1D0001, "g.jpg", body)
	doc := uploadMediaDocument(t, ctx, a, b.id, 0x5A1D0002, "g.bin", []byte("group document"))
	sent := mustSendAlbum(t, ctx, a, chat, albumPhotoItem(photo, 1), albumDocumentItem(doc, 2))
	groupedID, _ := sent[0].GetGroupedID()
	for _, member := range []*mediaClient{b, c} {
		history := albumHistory(t, ctx, member, chat)
		if got := assertOneAlbum(t, history, "member history"); got != groupedID || len(history) != 2 {
			t.Fatalf("member history album = %d with %d items, want %d with 2", got, len(history), groupedID)
		}
	}
	history := albumHistory(t, ctx, b, chat)
	for _, message := range history {
		switch message.Media.(type) {
		case *tg.MessageMediaPhoto:
			if got := downloadAlbumPhoto(t, ctx, b, albumPhotoOf(t, message)); !bytes.Equal(got, body) {
				t.Fatalf("member photo download returned %d bytes, want %d", len(got), len(body))
			}
		default:
			if got := downloadDocument(t, ctx, b, documentOf(t, message)); string(got) != "group document" {
				t.Fatalf("member document download = %q", got)
			}
		}
	}

	bp := uploadMediaPhoto(t, ctx, b, a.id, 0x5A1D0011, "b.jpg", body)
	bd := uploadMediaDocument(t, ctx, b, a.id, 0x5A1D0012, "b.bin", []byte("b"))
	ban := func(rights tg.ChatBannedRights) {
		execChat(t, ctx, a.cmds, func(ctx context.Context, client *tg.Client) error {
			_, err := client.MessagesEditChatDefaultBannedRights(ctx, &tg.MessagesEditChatDefaultBannedRightsRequest{Peer: chat, BannedRights: rights})
			return err
		})
	}
	ban(tg.ChatBannedRights{SendPhotos: true})
	_, err := sendAlbum(t, ctx, b, chat, albumPhotoItem(bp, 11), albumDocumentItem(bd, 12))
	assertRPCError(t, err, "CHAT_WRITE_FORBIDDEN")
	sendMediaErr := execMedia(t, ctx, b.cmds, func(ctx context.Context, client *tg.Client) error {
		parts := uploadPartsOn(ctx, client, 0x5A1D0013, body)
		_, err := client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: chat, RandomID: 13,
			Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{ID: 0x5A1D0013, Parts: parts, Name: "s.jpg"}},
		})
		return err
	})
	assertRPCError(t, sendMediaErr, "CHAT_WRITE_FORBIDDEN")
	ban(tg.ChatBannedRights{SendDocs: true})
	_, err = sendAlbum(t, ctx, b, chat, albumPhotoItem(bp, 11), albumDocumentItem(bd, 12))
	assertRPCError(t, err, "CHAT_WRITE_FORBIDDEN")
	if n := albumGrantCount(t, ctx, conn, b.id); n != 2 {
		t.Fatalf("grants after refused group albums = %d, want 2", n)
	}
	ban(tg.ChatBannedRights{})
	mustSendAlbum(t, ctx, b, chat, albumPhotoItem(bp, 11), albumDocumentItem(bd, 12))

	var channelID int64
	var channelHash int64
	execChat(t, ctx, a.cmds, func(ctx context.Context, client *tg.Client) error {
		created, err := client.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{Title: "album channel", Broadcast: true})
		if err != nil {
			return err
		}
		updates, ok := created.(*tg.Updates)
		if !ok || len(updates.Chats) != 1 {
			return fmt.Errorf("createChannel result = %T", created)
		}
		channel, ok := updates.Chats[0].(*tg.Channel)
		if !ok {
			return fmt.Errorf("createChannel chat = %T", updates.Chats[0])
		}
		channelID, channelHash = channel.ID, channel.AccessHash
		return nil
	})
	invite, err := a.store.CreateChannelInvite(ctx, channelID, a.id)
	if err != nil {
		t.Fatalf("channel invite: %v", err)
	}
	if _, _, err = a.store.JoinChannelByInvite(ctx, invite, b.id); err != nil {
		t.Fatalf("join channel: %v", err)
	}
	channelPeer := &tg.InputPeerChannel{ChannelID: channelID, AccessHash: channelHash}
	cp1 := uploadMediaPhoto(t, ctx, a, b.id, 0x5A1D0021, "c1.jpg", body)
	cp2 := uploadMediaPhoto(t, ctx, a, b.id, 0x5A1D0022, "c2.jpg", body)
	cd := uploadMediaDocument(t, ctx, a, b.id, 0x5A1D0023, "c.bin", []byte("c"))
	_, err = sendAlbum(t, ctx, a, channelPeer, albumPhotoItem(cp1, 21), albumDocumentItem(cd, 22))
	assertRPCError(t, err, "PEER_ID_INVALID")
	posts := mustSendAlbum(t, ctx, a, channelPeer, albumPhotoItem(cp1, 21), albumPhotoItem(cp2, 22))
	replay := mustSendAlbum(t, ctx, a, channelPeer, albumPhotoItem(cp1, 21), albumPhotoItem(cp2, 22))
	if replay[0].ID != posts[0].ID || replay[1].ID != posts[1].ID {
		t.Fatalf("channel replay ids = %d,%d, want %d,%d", replay[0].ID, replay[1].ID, posts[0].ID, posts[1].ID)
	}
	subscriberView := albumHistory(t, ctx, b, peerChannel(b.id, channelID))
	if len(subscriberView) != 2 {
		t.Fatalf("subscriber channel history has %d media posts, want 2", len(subscriberView))
	}
	assertOneAlbum(t, subscriberView, "subscriber channel history")
	if got := downloadAlbumPhoto(t, ctx, b, albumPhotoOf(t, subscriberView[0])); !bytes.Equal(got, body) {
		t.Fatalf("subscriber channel download returned %d bytes, want %d", len(got), len(body))
	}
}

func uploadPartsOn(ctx context.Context, c *tg.Client, fileID int64, payload []byte) int {
	parts := 0
	for offset := 0; offset < len(payload); offset += mediaPartSize {
		if _, err := c.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
			FileID: fileID, FilePart: parts, Bytes: payload[offset:min(offset+mediaPartSize, len(payload))],
		}); err != nil {
			return 0
		}
		parts++
	}
	return parts
}

// TestSendMultiMediaResendsAndBounds: a full resend replays without
// duplicates, a mixed or partial resend and a resend after a deletion are
// refused, the item count is bounded, and a block answers what sendMedia
// answers.
func TestSendMultiMediaResendsAndBounds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	clients := bootMediaEnv(t, ctx, "+15551318531", "+15551318532")
	a, b := clients[0], clients[1]
	conn := albumConn(t, ctx, a)

	d := []*tg.Document{
		uploadMediaDocument(t, ctx, a, b.id, 0x5A1E0001, "r1.bin", []byte("r1")),
		uploadMediaDocument(t, ctx, a, b.id, 0x5A1E0002, "r2.bin", []byte("r2")),
		uploadMediaDocument(t, ctx, a, b.id, 0x5A1E0003, "r3.bin", []byte("r3")),
		uploadMediaDocument(t, ctx, a, b.id, 0x5A1E0004, "r4.bin", []byte("r4")),
	}
	sent := mustSendAlbum(t, ctx, a, peerUser(a.id, b.id), albumDocumentItem(d[0], 1), albumDocumentItem(d[1], 2))
	replay := mustSendAlbum(t, ctx, a, peerUser(a.id, b.id), albumDocumentItem(d[0], 1), albumDocumentItem(d[1], 2))
	if replay[0].ID != sent[0].ID || replay[1].ID != sent[1].ID {
		t.Fatalf("replay ids = %d,%d, want %d,%d", replay[0].ID, replay[1].ID, sent[0].ID, sent[1].ID)
	}
	if n := albumRowCount(t, ctx, conn); n != 4 {
		t.Fatalf("album rows after replay = %d, want 4", n)
	}
	_, err := sendAlbum(t, ctx, a, peerUser(a.id, b.id), albumDocumentItem(d[2], 2), albumDocumentItem(d[3], 3))
	assertRPCError(t, err, "MEDIA_INVALID")
	if n := albumGrantCount(t, ctx, conn, a.id); n != 2 {
		t.Fatalf("grants after mixed resend = %d, want 2", n)
	}

	received := albumHistory(t, ctx, b, peerUser(b.id, a.id))
	execChat(t, ctx, b.cmds, func(ctx context.Context, c *tg.Client) error {
		_, err := c.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{Revoke: true, ID: []int{received[0].ID}})
		return err
	})
	_, err = sendAlbum(t, ctx, a, peerUser(a.id, b.id), albumDocumentItem(d[0], 1), albumDocumentItem(d[1], 2))
	assertRPCError(t, err, "MEDIA_INVALID")

	eleven := make([]tg.InputSingleMedia, 11)
	for i := range eleven {
		eleven[i] = albumDocumentItem(d[2], int64(200+i))
	}
	_, err = sendAlbum(t, ctx, a, peerUser(a.id, b.id), eleven...)
	assertRPCError(t, err, "MULTI_MEDIA_TOO_LONG")
	_, err = sendAlbum(t, ctx, a, peerUser(a.id, b.id), albumDocumentItem(d[2], 300))
	assertRPCError(t, err, "MEDIA_INVALID")
	if n := albumGrantCount(t, ctx, conn, a.id); n != 2 {
		t.Fatalf("grants after bounds refusals = %d, want 2", n)
	}

	execChat(t, ctx, b.cmds, func(ctx context.Context, c *tg.Client) error {
		_, err := c.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peerUser(b.id, a.id)})
		return err
	})
	single := mediaPayload(1024)
	parts := uploadParts(t, ctx, a, 0x5A1E0010, single)
	sendMediaErr := execMedia(t, ctx, a.cmds, func(ctx context.Context, c *tg.Client) error {
		_, err := c.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peerUser(a.id, b.id), RandomID: 400,
			Media: &tg.InputMediaUploadedDocument{File: &tg.InputFile{ID: 0x5A1E0010, Parts: parts, Name: "s.bin"}, MimeType: "application/octet-stream"},
		})
		return err
	})
	_, albumErr := sendAlbum(t, ctx, a, peerUser(a.id, b.id), albumDocumentItem(d[2], 401), albumDocumentItem(d[3], 402))
	sameAnswer := sendMediaErr == nil && albumErr == nil
	if sendMediaErr != nil && albumErr != nil {
		mediaRPC, mediaOK := tgerr.As(sendMediaErr)
		albumRPC, albumOK := tgerr.As(albumErr)
		sameAnswer = mediaOK && albumOK && mediaRPC.Type == albumRPC.Type
	}
	if !sameAnswer {
		t.Fatalf("blocked sender: sendMedia = %v, sendMultiMedia = %v; want the same answer", sendMediaErr, albumErr)
	}
}
