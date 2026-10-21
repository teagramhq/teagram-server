package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"testing"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/config"
)

func TestBasicGroupEditChatPhoto(t *testing.T) {
	t.Parallel()
	testSmokeBasicGroupPhoto(t)
}

func testSmokeBasicGroupPhoto(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.photoThumbs = buildSmokePhotoThumbSupervisor(t)
	})
	const phoneA, phoneB, phoneC, phoneD, phoneE = "+15551022861", "+15551022862", "+15551022863", "+15551022864", "+15551022865"
	seedSmokeUsers(t, f, phoneA, phoneB, phoneC, phoneD, phoneE)
	a := newSmokeClient(t, f, "group photo creator", phoneA)
	b := newSmokeClient(t, f, "group photo member", phoneB)
	c := newSmokeClient(t, f, "group photo offline member", phoneC)
	d := newSmokeClient(t, f, "group photo later member", phoneD)
	e := newSmokeClient(t, f, "group photo outsider", phoneE)

	var chatID int64
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		created, err := client.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
			Title: "Group photo", Users: []tg.InputUserClass{inputUser(a.id, b.id), inputUser(a.id, c.id)},
		})
		if err != nil {
			return err
		}
		updates, ok := created.Updates.(*tg.Updates)
		if !ok || len(updates.Chats) != 1 {
			return fmt.Errorf("createChat result = %T with %d chats, want Updates with one chat", created.Updates, len(updates.Chats))
		}
		chat, ok := updates.Chats[0].(*tg.Chat)
		if !ok {
			return fmt.Errorf("createChat chat = %T, want *tg.Chat", updates.Chats[0])
		}
		chatID = chat.ID
		return nil
	}); err != nil {
		t.Fatalf("create group for photo: %v", err)
	}
	for label, client := range map[string]*smokeClient{"member": b, "offline member": c} {
		if _, err := client.seen.waitService(f.ctx, &tg.MessageActionChatCreate{}); err != nil {
			t.Fatalf("%s receive group create: %v", label, err)
		}
	}
	var offlineState *tg.UpdatesState
	if err := c.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		offlineState, err = client.UpdatesGetState(ctx)
		return err
	}); err != nil {
		t.Fatalf("capture offline member state: %v", err)
	}
	c.stopClient(t)

	bodyA := smokeJPEGAtUploadSize(t, smokePhotoPartCount)
	bodyB := distinctSmokePhotoBody(t, bodyA, 0x31)
	bodyC := distinctSmokePhotoBody(t, bodyA, 0x72)
	photoA := uploadSmokeGroupPhoto(t, a, chatID, 1022861, bodyA)
	assertSmokePhotoAction(t, photoA, bodyA, "creator upload response")
	creatorA, err := a.seen.waitService(f.ctx, &tg.MessageActionChatEditPhoto{})
	if err != nil {
		t.Fatalf("creator receives own photo event: %v", err)
	}
	assertGroupPhotoService(t, creatorA.svc.Action, photoA, bodyA, "creator own photo event")
	memberA, err := b.seen.waitService(f.ctx, &tg.MessageActionChatEditPhoto{})
	if err != nil {
		t.Fatalf("member receives creator photo: %v", err)
	}
	assertGroupPhotoService(t, memberA.svc.Action, photoA, bodyA, "member live creator photo")

	foreignBody := distinctSmokePhotoBody(t, bodyA, 0x45)
	const foreignFileID = int64(1022865)
	if err := uploadSmokeGroupParts(e, foreignFileID, foreignBody, smokePhotoPartCount); err != nil {
		t.Fatalf("outsider upload photo parts: %v", err)
	}
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesEditChatPhoto(ctx, &tg.MessagesEditChatPhotoRequest{
			ChatID: chatID, Photo: uploadedSmokeGroupPhoto(foreignFileID, 1, foreignBody),
		})
		return err
	}); err == nil || !tgErrorIs(err, "MEDIA_INVALID") {
		t.Fatalf("creator using outsider upload = %v, want MEDIA_INVALID", err)
	}
	if err := e.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesEditChatPhoto(ctx, &tg.MessagesEditChatPhotoRequest{ChatID: chatID, Photo: &tg.InputChatPhotoEmpty{}})
		return err
	}); err == nil || !tgErrorIs(err, "PEER_ID_INVALID") {
		t.Fatalf("outsider clear = %v, want PEER_ID_INVALID", err)
	}

	photoB := uploadSmokeGroupPhoto(t, b, chatID, 1022862, bodyB)
	assertSmokePhotoAction(t, photoB, bodyB, "member replacement response")
	creatorB, err := a.seen.waitService(f.ctx, &tg.MessageActionChatEditPhoto{})
	if err != nil {
		t.Fatalf("creator receives member replacement: %v", err)
	}
	assertGroupPhotoService(t, creatorB.svc.Action, photoB, bodyB, "creator live member replacement")

	// C reconnects from its last state and receives both immutable photo actions
	// in owner PTS order, including the member's replacement.
	clientC := f.savedSessionClient(c.session)
	var differenceC tg.UpdatesDifferenceClass
	if err := clientC.Run(f.ctx, func(ctx context.Context) error {
		var err error
		differenceC, err = clientC.API().UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{
			Pts: offlineState.Pts, Qts: offlineState.Qts, Date: offlineState.Date,
		})
		if err != nil {
			return err
		}
		full, ok := differenceC.(*tg.UpdatesDifference)
		if !ok {
			return fmt.Errorf("offline photo difference = %T, want *tg.UpdatesDifference", differenceC)
		}
		assertGroupChatPhoto(t, findGroupChat(t, full.Chats, chatID), photoB.ID, "offline difference chat")
		return nil
	}); err != nil {
		t.Fatalf("offline member photo difference: %v", err)
	}
	fullDifferenceC, ok := differenceC.(*tg.UpdatesDifference)
	if !ok {
		t.Fatalf("offline photo difference = %T, want *tg.UpdatesDifference", differenceC)
	}
	if got := photoIDsFromDifference(fullDifferenceC); len(got) != 2 || got[0] != photoA.ID || got[1] != photoB.ID {
		t.Fatalf("offline photo actions = %v, want ordered photo ids [%d %d]", got, photoA.ID, photoB.ID)
	}

	assertGroupPhotoReads(t, f.ctx, b, chatID, photoB, bodyB, "member current photo")
	assertCurrentPhotoReads(t, f.ctx, b, chatID, photoB, bodyB, "member current photo")
	assertGroupChatPhotoReads(t, f.ctx, b, chatID, photoB, "member")

	clearResult := editSmokeGroupPhoto(t, a, chatID, &tg.InputChatPhotoEmpty{})
	assertDeletePhotoAction(t, clearResult, "creator clear response")
	memberClear, err := b.seen.waitService(f.ctx, &tg.MessageActionChatDeletePhoto{})
	if err != nil {
		t.Fatalf("member receives photo clear: %v", err)
	}
	if _, ok := memberClear.svc.Action.(*tg.MessageActionChatDeletePhoto); !ok {
		t.Fatalf("member clear action = %T, want delete photo", memberClear.svc.Action)
	}
	assertEmptyChatPhotoInUpdates(t, clearResult, chatID, "clear response")
	assertGroupChatPhotoReadsEmpty(t, f.ctx, b, chatID, "after clear")
	stateBeforeNoopA, stateBeforeNoopB := smokeState(t, f.ctx, a), smokeState(t, f.ctx, b)
	noopResult := editSmokeGroupPhoto(t, a, chatID, &tg.InputChatPhotoEmpty{})
	noop, ok := noopResult.(*tg.Updates)
	if !ok || len(noop.Updates) != 0 {
		t.Fatalf("already-empty clear = %#v, want empty Updates", noopResult)
	}
	if afterA, afterB := smokeState(t, f.ctx, a), smokeState(t, f.ctx, b); afterA.Pts != stateBeforeNoopA.Pts || afterB.Pts != stateBeforeNoopB.Pts {
		t.Fatalf("already-empty clear changed PTS to %d/%d, want %d/%d", afterA.Pts, afterB.Pts, stateBeforeNoopA.Pts, stateBeforeNoopB.Pts)
	}

	stateBeforeRemoval := smokeState(t, f.ctx, b)
	b.stopClient(t)
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesDeleteChatUser(ctx, &tg.MessagesDeleteChatUserRequest{
			ChatID: chatID, UserID: inputUser(a.id, b.id),
		})
		return err
	}); err != nil {
		t.Fatalf("creator removes member before later photo: %v", err)
	}
	photoC := uploadSmokeGroupPhoto(t, a, chatID, 1022863, bodyC)
	assertSmokePhotoAction(t, photoC, bodyC, "later creator photo response")

	// B's reconnect catches the removal event but never receives photo C, which
	// was selected after its membership ended.
	clientB := f.savedSessionClient(b.session)
	if err := clientB.Run(f.ctx, func(ctx context.Context) error {
		difference, err := clientB.API().UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{
			Pts: stateBeforeRemoval.Pts, Qts: stateBeforeRemoval.Qts, Date: stateBeforeRemoval.Date,
		})
		if err != nil {
			return err
		}
		full, ok := difference.(*tg.UpdatesDifference)
		if !ok {
			return fmt.Errorf("removed member difference = %T, want *tg.UpdatesDifference", difference)
		}
		if ids := photoIDsFromDifference(full); len(ids) != 0 {
			return fmt.Errorf("removed member later photo ids = %v, want none", ids)
		}
		for _, chatClass := range full.Chats {
			if chat, ok := chatClass.(*tg.Chat); ok && chat.ID == chatID {
				assertGroupChatPhoto(t, chat, 0, "removed member difference chat")
			}
		}
		for _, message := range full.NewMessages {
			service, ok := message.(*tg.MessageService)
			if ok {
				if action, ok := service.Action.(*tg.MessageActionChatDeleteUser); ok && action.UserID == b.id {
					return nil
				}
			}
		}
		return errors.New("removed member difference omitted its delete-user service event")
	}); err != nil {
		t.Fatalf("removed member reconnect: %v", err)
	}
	assertLocationInvalid(t, f.ctx, f.savedSessionClient(b.session), &tg.InputPeerPhotoFileLocation{
		Peer: &tg.InputPeerChat{ChatID: chatID}, PhotoID: photoC.ID, Big: true,
	}, "removed member current peer photo")
	assertLocationInvalid(t, f.ctx, f.savedSessionClient(b.session), &tg.InputPhotoFileLocation{
		ID: photoC.ID, AccessHash: photoC.AccessHash, ThumbSize: "w",
	}, "removed member current generic photo")
	assertPhotoLocationBytes(t, f.ctx, f.savedSessionClient(b.session), photoA, bodyA, "removed member frozen first photo")
	assertPhotoLocationBytes(t, f.ctx, f.savedSessionClient(b.session), photoB, bodyB, "removed member frozen replacement photo")

	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesAddChatUser(ctx, &tg.MessagesAddChatUserRequest{
			ChatID: chatID, UserID: inputUser(a.id, d.id), FwdLimit: 0,
		})
		return err
	}); err != nil {
		t.Fatalf("creator adds later member: %v", err)
	}
	addService, err := d.seen.waitService(f.ctx, &tg.MessageActionChatAddUser{})
	if err != nil {
		t.Fatalf("new member receives add service: %v", err)
	}
	if _, ok := addService.svc.Action.(*tg.MessageActionChatAddUser); !ok {
		t.Fatalf("new member add action = %T, want add user", addService.svc.Action)
	}
	assertGroupChatPhotoReads(t, f.ctx, d, chatID, photoC, "new member")
	assertChatPhotoInHistoryAndDialogs(t, f.ctx, d, chatID, photoC.ID)
	assertCurrentPhotoReads(t, f.ctx, d, chatID, photoC, bodyC, "new member without old photo message")
	assertGroupPhotoReads(t, f.ctx, d, chatID, photoC, bodyC, "new member current photo")
	assertNoPhotoServiceFor(t, f.ctx, d, chatID, photoC.ID)

	foreignState := smokeState(t, f.ctx, a)
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesEditChatPhoto(ctx, &tg.MessagesEditChatPhotoRequest{
			ChatID: chatID, Photo: uploadedSmokeGroupPhoto(foreignFileID, 1, foreignBody),
		})
		return err
	}); err == nil || !tgErrorIs(err, "MEDIA_INVALID") {
		t.Fatalf("foreign upload retry = %v, want MEDIA_INVALID", err)
	}
	if got := smokeState(t, f.ctx, a).Pts; got != foreignState.Pts {
		t.Fatalf("foreign upload changed creator PTS to %d, want %d", got, foreignState.Pts)
	}
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.PhotosGetUserPhotos(ctx, &tg.PhotosGetUserPhotosRequest{
			UserID: inputUser(a.id, a.id), Offset: 0, MaxID: 0, Limit: 10,
		})
		if err != nil {
			return err
		}
		if count := profilePhotoCount(result); count != 0 {
			return fmt.Errorf("group photo upload changed personal gallery count to %d", count)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify group upload leaves personal gallery unchanged: %v", err)
	}

	finalClear := editSmokeGroupPhoto(t, a, chatID, &tg.InputChatPhotoEmpty{})
	assertDeletePhotoAction(t, finalClear, "creator clears photo C")
	if _, err := d.seen.waitService(f.ctx, &tg.MessageActionChatDeletePhoto{}); err != nil {
		t.Fatalf("new member receives final clear: %v", err)
	}
	assertEmptyChatPhotoInUpdates(t, finalClear, chatID, "final clear response")
	assertGroupChatPhotoReadsEmpty(t, f.ctx, d, chatID, "new member after clear")
	assertSmokeLocationInvalid(t, f.ctx, d, &tg.InputPeerPhotoFileLocation{
		Peer: &tg.InputPeerChat{ChatID: chatID}, PhotoID: photoC.ID, Big: false,
	}, "new member cleared peer-photo lane")
	assertSmokeLocationInvalid(t, f.ctx, d, &tg.InputPhotoFileLocation{
		ID: photoC.ID, AccessHash: photoC.AccessHash, ThumbSize: "w",
	}, "new member without frozen copy after clear")
	// C received the immutable photo C service copy while it belonged to the
	// group, so clearing only closes the live peer-photo lane for that owner.
	assertPhotoLocationBytes(t, f.ctx, f.savedSessionClient(c.session), photoC, bodyC, "remaining member frozen photo after clear")
}

func distinctSmokePhotoBody(t *testing.T, body []byte, value byte) []byte {
	t.Helper()
	variant := bytes.Clone(body)
	if len(variant) < 16 {
		t.Fatal("smoke photo is too short to vary metadata padding")
	}
	variant[8] = value
	if _, err := jpeg.Decode(bytes.NewReader(variant)); err != nil {
		t.Fatalf("decode varied JPEG metadata: %v", err)
	}
	return variant
}

func uploadSmokeGroupParts(client *smokeClient, fileID int64, body []byte, parts int) error {
	return client.call(client.lifecycle.ctx, func(ctx context.Context, api *tg.Client) error {
		if len(body) != parts*smokePhotoPartSize {
			return fmt.Errorf("photo upload body is %d bytes for %d parts", len(body), parts)
		}
		for part := range parts {
			start := part * smokePhotoPartSize
			ok, err := api.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
				FileID: fileID, FilePart: part, Bytes: body[start : start+smokePhotoPartSize],
			})
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("upload part %d returned false", part)
			}
		}
		return nil
	})
}

func uploadedSmokeGroupPhoto(fileID int64, parts int, body []byte) *tg.InputChatUploadedPhoto {
	photo := &tg.InputChatUploadedPhoto{}
	photo.SetFile(&tg.InputFile{ID: fileID, Parts: parts, Name: "group.jpg", MD5Checksum: smokePhotoMD5(body)})
	return photo
}

func uploadSmokeGroupPhoto(t *testing.T, client *smokeClient, chatID, fileID int64, body []byte) *tg.Photo {
	t.Helper()
	if err := uploadSmokeGroupParts(client, fileID, body, smokePhotoPartCount); err != nil {
		t.Fatalf("upload group photo parts: %v", err)
	}
	result := editSmokeGroupPhoto(t, client, chatID, uploadedSmokeGroupPhoto(fileID, smokePhotoPartCount, body))
	return photoFromEditUpdates(t, result)
}

func editSmokeGroupPhoto(t *testing.T, client *smokeClient, chatID int64, photo tg.InputChatPhotoClass) tg.UpdatesClass {
	t.Helper()
	var result tg.UpdatesClass
	if err := client.call(client.lifecycle.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.MessagesEditChatPhoto(ctx, &tg.MessagesEditChatPhotoRequest{ChatID: chatID, Photo: photo})
		return err
	}); err != nil {
		t.Fatalf("messages.editChatPhoto: %v", err)
	}
	return result
}

func photoFromEditUpdates(t *testing.T, result tg.UpdatesClass) *tg.Photo {
	t.Helper()
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("editChatPhoto result = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		newMessage, ok := update.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		service, ok := newMessage.Message.(*tg.MessageService)
		if !ok {
			continue
		}
		action, ok := service.Action.(*tg.MessageActionChatEditPhoto)
		if !ok {
			continue
		}
		photo, ok := action.Photo.(*tg.Photo)
		if !ok {
			t.Fatalf("photo edit service photo = %T, want *tg.Photo", action.Photo)
		}
		return photo
	}
	t.Fatalf("editChatPhoto response has no photo service action")
	return nil
}

func assertSmokePhotoAction(t *testing.T, photo *tg.Photo, body []byte, label string) {
	t.Helper()
	if photo == nil || photo.ID <= 0 || photo.AccessHash == 0 || len(photo.FileReference) == 0 {
		t.Fatalf("%s photo identity = %#v, want id, hash and file reference", label, photo)
	}
	if photo.ID <= 0 {
		t.Fatalf("%s photo id = %d, want positive", label, photo.ID)
	}
	message := &tg.Message{Media: &tg.MessageMediaPhoto{Photo: photo}}
	assertSmokePhoto(t, message, body)
}

func assertGroupPhotoService(t *testing.T, action tg.MessageActionClass, want *tg.Photo, body []byte, label string) {
	t.Helper()
	edit, ok := action.(*tg.MessageActionChatEditPhoto)
	if !ok {
		t.Fatalf("%s action = %T, want edit photo", label, action)
	}
	photo, ok := edit.Photo.(*tg.Photo)
	if !ok {
		t.Fatalf("%s action photo = %T, want *tg.Photo", label, edit.Photo)
	}
	assertSmokePhotoAction(t, photo, body, label)
	if photo.ID != want.ID || photo.AccessHash != want.AccessHash || !bytes.Equal(photo.FileReference, want.FileReference) {
		t.Fatalf("%s photo = id/hash/reference %d/%d/%x, want %d/%d/%x", label, photo.ID, photo.AccessHash, photo.FileReference, want.ID, want.AccessHash, want.FileReference)
	}
}

func assertDeletePhotoAction(t *testing.T, result tg.UpdatesClass, label string) {
	t.Helper()
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("%s result = %T, want *tg.Updates", label, result)
	}
	for _, update := range updates.Updates {
		newMessage, ok := update.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		service, ok := newMessage.Message.(*tg.MessageService)
		if !ok {
			continue
		}
		if _, ok := service.Action.(*tg.MessageActionChatDeletePhoto); ok {
			return
		}
	}
	t.Fatalf("%s result has no delete-photo service action", label)
}

func assertEmptyChatPhotoInUpdates(t *testing.T, result tg.UpdatesClass, chatID int64, label string) {
	t.Helper()
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("%s result = %T, want *tg.Updates", label, result)
	}
	assertGroupChatPhoto(t, findGroupChat(t, updates.Chats, chatID), 0, label)
}

func assertGroupChatPhoto(t *testing.T, chat *tg.Chat, photoID int64, label string) {
	t.Helper()
	if chat == nil {
		t.Fatalf("%s has no Chat", label)
	}
	photo := chat.GetPhoto()
	if photoID == 0 {
		if _, ok := photo.(*tg.ChatPhotoEmpty); !ok {
			t.Fatalf("%s chat photo = %T, want empty", label, photo)
		}
		return
	}
	full, ok := photo.(*tg.ChatPhoto)
	if !ok || full.GetPhotoID() != photoID {
		t.Fatalf("%s chat photo = %#v, want photo id %d", label, photo, photoID)
	}
	if stripped, ok := full.GetStrippedThumb(); !ok || len(stripped) == 0 {
		t.Fatalf("%s chat photo has no stripped preview", label)
	}
}

func findGroupChat(t *testing.T, chats []tg.ChatClass, chatID int64) *tg.Chat {
	t.Helper()
	for _, class := range chats {
		chat, ok := class.(*tg.Chat)
		if ok && chat.ID == chatID {
			return chat
		}
	}
	t.Fatalf("group %d not found in %d chats", chatID, len(chats))
	return nil
}

func assertGroupChatPhotoReads(t *testing.T, ctx context.Context, client *smokeClient, chatID int64, want *tg.Photo, label string) {
	t.Helper()
	var fullResult *tg.MessagesChatFull
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		fullResult, err = api.MessagesGetFullChat(ctx, chatID)
		return err
	}); err != nil {
		t.Fatalf("%s getFullChat: %v", label, err)
	}
	full, ok := fullResult.FullChat.(*tg.ChatFull)
	if !ok {
		t.Fatalf("%s full chat = %T, want *tg.ChatFull", label, fullResult.FullChat)
	}
	photo, ok := full.GetChatPhoto()
	photoValue, isPhoto := photo.(*tg.Photo)
	if !ok || !isPhoto || photoValue.ID != want.ID || photoValue.AccessHash != want.AccessHash {
		t.Fatalf("%s ChatFull photo = %#v/%v, want id/hash %d/%d", label, photo, ok, want.ID, want.AccessHash)
	}
	assertGroupChatPhoto(t, findGroupChat(t, fullResult.Chats, chatID), want.ID, label+" ChatFull.Chat")

	var dialogs tg.MessagesDialogsClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		dialogs, err = api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}, Limit: 20})
		return err
	}); err != nil {
		t.Fatalf("%s getDialogs: %v", label, err)
	}
	assertGroupChatPhoto(t, findGroupChat(t, chatsFromDialogs(t, dialogs), chatID), want.ID, label+" dialogs")

	var history tg.MessagesMessagesClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		history, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: &tg.InputPeerChat{ChatID: chatID}, Limit: 20})
		return err
	}); err != nil {
		t.Fatalf("%s getHistory: %v", label, err)
	}
	assertGroupChatPhoto(t, findGroupChat(t, chatsFromMessages(t, history), chatID), want.ID, label+" history")
}

func assertGroupChatPhotoReadsEmpty(t *testing.T, ctx context.Context, client *smokeClient, chatID int64, label string) {
	t.Helper()
	var fullResult *tg.MessagesChatFull
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		fullResult, err = api.MessagesGetFullChat(ctx, chatID)
		return err
	}); err != nil {
		t.Fatalf("%s getFullChat: %v", label, err)
	}
	full, ok := fullResult.FullChat.(*tg.ChatFull)
	if !ok {
		t.Fatalf("%s full chat = %T, want *tg.ChatFull", label, fullResult.FullChat)
	}
	photo, ok := full.GetChatPhoto()
	if !ok {
		t.Fatalf("%s full chat has no chat_photo", label)
	}
	if _, ok := photo.(*tg.PhotoEmpty); !ok {
		t.Fatalf("%s full chat photo = %T, want empty", label, photo)
	}
	assertGroupChatPhoto(t, findGroupChat(t, fullResult.Chats, chatID), 0, label+" ChatFull.Chat")

	var dialogs tg.MessagesDialogsClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		dialogs, err = api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}, Limit: 20})
		return err
	}); err != nil {
		t.Fatalf("%s getDialogs: %v", label, err)
	}
	assertGroupChatPhoto(t, findGroupChat(t, chatsFromDialogs(t, dialogs), chatID), 0, label+" dialogs")

	var history tg.MessagesMessagesClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		history, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: &tg.InputPeerChat{ChatID: chatID}, Limit: 20})
		return err
	}); err != nil {
		t.Fatalf("%s getHistory: %v", label, err)
	}
	assertGroupChatPhoto(t, findGroupChat(t, chatsFromMessages(t, history), chatID), 0, label+" history")
}

func chatsFromDialogs(t *testing.T, dialogs tg.MessagesDialogsClass) []tg.ChatClass {
	t.Helper()
	switch value := dialogs.(type) {
	case *tg.MessagesDialogs:
		return value.Chats
	case *tg.MessagesDialogsSlice:
		return value.Chats
	default:
		t.Fatalf("dialogs result = %T, want messages.dialogs or slice", dialogs)
		return nil
	}
}

func chatsFromMessages(t *testing.T, messages tg.MessagesMessagesClass) []tg.ChatClass {
	t.Helper()
	switch value := messages.(type) {
	case *tg.MessagesMessages:
		return value.Chats
	case *tg.MessagesMessagesSlice:
		return value.Chats
	default:
		t.Fatalf("messages result = %T, want messages.messages or slice", messages)
		return nil
	}
}

func assertChatPhotoInHistoryAndDialogs(t *testing.T, ctx context.Context, client *smokeClient, chatID, photoID int64) {
	t.Helper()
	var dialogs tg.MessagesDialogsClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		dialogs, err = api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}, Limit: 20})
		return err
	}); err != nil {
		t.Fatalf("new member dialogs: %v", err)
	}
	assertGroupChatPhoto(t, findGroupChat(t, chatsFromDialogs(t, dialogs), chatID), photoID, "new member dialogs")
	var history tg.MessagesMessagesClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		history, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: &tg.InputPeerChat{ChatID: chatID}, Limit: 20})
		return err
	}); err != nil {
		t.Fatalf("new member history: %v", err)
	}
	assertGroupChatPhoto(t, findGroupChat(t, chatsFromMessages(t, history), chatID), photoID, "new member history")
}

func assertNoPhotoServiceFor(t *testing.T, ctx context.Context, client *smokeClient, chatID, photoID int64) {
	t.Helper()
	var history tg.MessagesMessagesClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		history, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: &tg.InputPeerChat{ChatID: chatID}, Limit: 20})
		return err
	}); err != nil {
		t.Fatalf("new member history service check: %v", err)
	}
	var classes []tg.MessageClass
	switch value := history.(type) {
	case *tg.MessagesMessages:
		classes = value.Messages
	case *tg.MessagesMessagesSlice:
		classes = value.Messages
	default:
		t.Fatalf("new member history = %T, want message list", history)
	}
	for _, class := range classes {
		service, ok := class.(*tg.MessageService)
		if !ok {
			continue
		}
		edit, ok := service.Action.(*tg.MessageActionChatEditPhoto)
		if !ok {
			continue
		}
		photo, ok := edit.Photo.(*tg.Photo)
		if ok && photo.ID == photoID {
			t.Fatalf("new member history owns pre-join photo service copy %d", photoID)
		}
	}
}

func assertGroupPhotoReads(t *testing.T, ctx context.Context, client *smokeClient, chatID int64, photo *tg.Photo, body []byte, label string) {
	t.Helper()
	peerLocation := &tg.InputPeerPhotoFileLocation{Peer: &tg.InputPeerChat{ChatID: chatID}, PhotoID: photo.ID, Big: true}
	assertFileBytes(t, ctx, client, peerLocation, body, label+" peer big")
	peerLocation = &tg.InputPeerPhotoFileLocation{Peer: &tg.InputPeerChat{ChatID: chatID}, PhotoID: photo.ID, Big: false}
	var small tg.UploadFileClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		small, err = api.UploadGetFile(ctx, &tg.UploadGetFileRequest{Location: peerLocation, Offset: 0, Limit: 1 << 20})
		return err
	}); err != nil {
		t.Fatalf("%s peer small: %v", label, err)
	}
	smallFile, ok := small.(*tg.UploadFile)
	if !ok {
		t.Fatalf("%s peer small = %T, want *tg.UploadFile", label, small)
	}
	m, ok := photo.Sizes[1].(*tg.PhotoSize)
	if !ok || m.Type != "m" || len(smallFile.Bytes) != m.Size {
		t.Fatalf("%s peer small bytes=%d advertised=%#v", label, len(smallFile.Bytes), photo.Sizes[1])
	}
	decoded, err := jpeg.Decode(bytes.NewReader(smallFile.Bytes))
	if err != nil {
		t.Fatalf("%s peer small is not JPEG: %v", label, err)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != m.W || bounds.Dy() != m.H {
		t.Fatalf("%s peer small dimensions=%dx%d, advertised %dx%d", label, bounds.Dx(), bounds.Dy(), m.W, m.H)
	}
}

func assertCurrentPhotoReads(t *testing.T, ctx context.Context, client *smokeClient, chatID int64, photo *tg.Photo, body []byte, label string) {
	t.Helper()
	var result tg.UploadFileClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: "w"},
			Offset:   0, Limit: len(body),
		})
		return err
	}); err != nil {
		t.Fatalf("%s generic original: %v", label, err)
	}
	original, ok := result.(*tg.UploadFile)
	if !ok || !bytes.Equal(original.Bytes, body) {
		t.Fatalf("%s generic original = %T with %d bytes, want %d bytes", label, result, len(fileBytes(result)), len(body))
	}
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: "m"},
			Offset:   0, Limit: 1 << 20,
		})
		return err
	}); err != nil {
		t.Fatalf("%s generic derivative: %v", label, err)
	}
	file, ok := result.(*tg.UploadFile)
	if !ok {
		t.Fatalf("%s generic derivative = %T, want *tg.UploadFile", label, result)
	}
	if m, ok := photo.Sizes[1].(*tg.PhotoSize); !ok || len(file.Bytes) != m.Size {
		t.Fatalf("%s generic derivative bytes=%d advertised=%#v", label, len(file.Bytes), photo.Sizes[1])
	}
}

func assertPhotoLocationBytes(t *testing.T, ctx context.Context, client *telegram.Client, photo *tg.Photo, body []byte, label string) {
	t.Helper()
	var result tg.UploadFileClass
	if err := client.Run(ctx, func(ctx context.Context) error {
		var err error
		result, err = tg.NewClient(client).UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: "w"},
			Offset:   0, Limit: len(body),
		})
		return err
	}); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	file, ok := result.(*tg.UploadFile)
	if !ok || !bytes.Equal(file.Bytes, body) {
		t.Fatalf("%s = %T with %d bytes, want %d original bytes", label, result, len(fileBytes(result)), len(body))
	}
}

func assertFileBytes(t *testing.T, ctx context.Context, client *smokeClient, location tg.InputFileLocationClass, want []byte, label string) {
	t.Helper()
	var result tg.UploadFileClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.UploadGetFile(ctx, &tg.UploadGetFileRequest{Location: location, Offset: 0, Limit: len(want)})
		return err
	}); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	file, ok := result.(*tg.UploadFile)
	if !ok || !bytes.Equal(file.Bytes, want) {
		t.Fatalf("%s = %T with %d bytes, want %d bytes", label, result, len(fileBytes(result)), len(want))
	}
}

func fileBytes(file tg.UploadFileClass) []byte {
	if upload, ok := file.(*tg.UploadFile); ok {
		return upload.Bytes
	}
	return nil
}

func assertLocationInvalid(t *testing.T, ctx context.Context, client *telegram.Client, location tg.InputFileLocationClass, label string) {
	t.Helper()
	var callErr error
	if err := client.Run(ctx, func(ctx context.Context) error {
		_, callErr = tg.NewClient(client).UploadGetFile(ctx, &tg.UploadGetFileRequest{Location: location, Offset: 0, Limit: 1024})
		return nil
	}); err != nil {
		t.Fatalf("%s run: %v", label, err)
	}
	if callErr == nil || !tgErrorIs(callErr, "LOCATION_INVALID") {
		t.Fatalf("%s = %v, want LOCATION_INVALID", label, callErr)
	}
}

func assertSmokeLocationInvalid(t *testing.T, ctx context.Context, client *smokeClient, location tg.InputFileLocationClass, label string) {
	t.Helper()
	var callErr error
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		_, callErr = api.UploadGetFile(ctx, &tg.UploadGetFileRequest{Location: location, Offset: 0, Limit: 1024})
		return nil
	}); err != nil {
		t.Fatalf("%s request: %v", label, err)
	}
	if callErr == nil || !tgErrorIs(callErr, "LOCATION_INVALID") {
		t.Fatalf("%s = %v, want LOCATION_INVALID", label, callErr)
	}
}

func smokeState(t *testing.T, ctx context.Context, client *smokeClient) *tg.UpdatesState {
	t.Helper()
	var state *tg.UpdatesState
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		state, err = api.UpdatesGetState(ctx)
		return err
	}); err != nil {
		t.Fatalf("get state for %s: %v", client.label, err)
	}
	return state
}

func photoIDsFromDifference(difference *tg.UpdatesDifference) []int64 {
	var ids []int64
	for _, class := range difference.NewMessages {
		service, ok := class.(*tg.MessageService)
		if !ok {
			continue
		}
		edit, ok := service.Action.(*tg.MessageActionChatEditPhoto)
		if !ok {
			continue
		}
		if photo, ok := edit.Photo.(*tg.Photo); ok {
			ids = append(ids, photo.ID)
		}
	}
	return ids
}

func profilePhotoCount(result tg.PhotosPhotosClass) int {
	switch photos := result.(type) {
	case *tg.PhotosPhotos:
		return len(photos.Photos)
	case *tg.PhotosPhotosSlice:
		return photos.Count
	default:
		return -1
	}
}

func tgErrorIs(err error, code string) bool {
	return tgerr.Is(err, code)
}
