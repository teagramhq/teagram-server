package api_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/store"
)

func editChatPhotoForTest(
	s *store.Store, userID int64, blobs blob.Store, chatID int64, photo tg.InputChatPhotoClass,
) (bin.Encoder, error) {
	return api.EditChatPhotoForTest(s, userID, blobs, api.TestMaxUserStorageBytes,
		&tg.MessagesEditChatPhotoRequest{ChatID: chatID, Photo: photo})
}

func statePtsForTest(t *testing.T, s *store.Store, userID int64) int {
	t.Helper()
	result, err := api.GetStateForTest(s, userID)
	if err != nil {
		t.Fatalf("get state for user %d: %v", userID, err)
	}
	state, ok := result.(*tg.UpdatesState)
	if !ok {
		t.Fatalf("state for user %d = %T, want *tg.UpdatesState", userID, result)
	}
	return state.Pts
}

func TestEditChatPhotoRejectsUnsupportedInputsAndForeignUploads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	blobs := newBlobs(t)
	creator, err := s.CreateUser(ctx, "+15551228861")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551228862")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	outsider, err := s.CreateUser(ctx, "+15551228863")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Group", []int64{member.ID})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	video := &tg.InputChatUploadedPhoto{}
	video.SetVideo(&tg.InputFile{ID: 22861, Parts: 1, Name: "video.mp4"})
	videoTimestamp := &tg.InputChatUploadedPhoto{}
	videoTimestamp.SetVideoStartTs(1.0)
	videoMarkup := &tg.InputChatUploadedPhoto{}
	videoMarkup.SetVideoEmojiMarkup(&tg.VideoSizeEmojiMarkup{EmojiID: 1, BackgroundColors: []int{0x102030}})
	unknownFlags := &tg.InputChatUploadedPhoto{}
	unknownFlags.SetFile(&tg.InputFile{ID: 22864, Parts: 1, Name: "photo.jpg"})
	unknownFlags.Flags.Set(5)
	big := &tg.InputChatUploadedPhoto{}
	big.SetFile(&tg.InputFileBig{ID: 22862, Parts: 1, Name: "photo.jpg"})
	for _, tc := range []struct {
		name  string
		photo tg.InputChatPhotoClass
		code  string
	}{
		{name: "existing photo reselection", photo: &tg.InputChatPhoto{ID: &tg.InputPhotoEmpty{}}, code: "INPUT_REQUEST_INVALID"},
		{name: "video mode", photo: video, code: "INPUT_REQUEST_INVALID"},
		{name: "video timestamp mode", photo: videoTimestamp, code: "INPUT_REQUEST_INVALID"},
		{name: "video emoji markup mode", photo: videoMarkup, code: "INPUT_REQUEST_INVALID"},
		{name: "unsupported future flag", photo: unknownFlags, code: "INPUT_REQUEST_INVALID"},
		{name: "missing ordinary file", photo: &tg.InputChatUploadedPhoto{}, code: "INPUT_REQUEST_INVALID"},
		{name: "big file constructor", photo: big, code: "MEDIA_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := editChatPhotoForTest(s, creator.ID, blobs, chat.ID, tc.photo)
			if got := rpcMessage(t, err); got != tc.code {
				t.Fatalf("editChatPhoto error = %s, want %s", got, tc.code)
			}
		})
	}

	if _, err := editChatPhotoForTest(s, outsider.ID, blobs, chat.ID, &tg.InputChatPhotoEmpty{}); rpcMessage(t, err) != "PEER_ID_INVALID" {
		t.Fatalf("outsider clear = %v, want PEER_ID_INVALID", err)
	}
	if _, err := editChatPhotoForTest(s, outsider.ID, blobs, chat.ID+1000, &tg.InputChatPhotoEmpty{}); rpcMessage(t, err) != "PEER_ID_INVALID" {
		t.Fatalf("absent chat clear = %v, want PEER_ID_INVALID", err)
	}
	if _, err := api.EditChatPhotoForTest(s, 0, blobs, api.TestMaxUserStorageBytes,
		&tg.MessagesEditChatPhotoRequest{ChatID: chat.ID, Photo: &tg.InputChatPhotoEmpty{}}); rpcMessage(t, err) != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("prelogin clear = %v, want AUTH_KEY_UNREGISTERED", err)
	}
	if _, _, err := s.SetChatDefaultBannedRights(ctx, chat.ID, creator.ID, []string{"change_info"}); err != nil {
		t.Fatalf("restrict group change_info: %v", err)
	}
	if _, err := editChatPhotoForTest(s, member.ID, blobs, chat.ID, &tg.InputChatPhotoEmpty{}); rpcMessage(t, err) != "CHAT_ADMIN_REQUIRED" {
		t.Fatalf("restricted member clear = %v, want CHAT_ADMIN_REQUIRED", err)
	}
	// The creator keeps the explicit authority exemption and an authorized
	// already-empty clear remains a no-op even under the default restriction.
	result, err := editChatPhotoForTest(s, creator.ID, blobs, chat.ID, &tg.InputChatPhotoEmpty{})
	if err != nil {
		t.Fatalf("creator clear with change_info restricted: %v", err)
	}
	updates, ok := result.(*tg.Updates)
	if !ok || len(updates.Updates) != 0 {
		t.Fatalf("already-empty clear = %#v, want empty Updates", result)
	}

	memberBody := jpegPhotoPayload(t, 640, 480)
	creatorBody := jpegPhotoPayload(t, 480, 640)
	const sharedClientFileID = int64(22863)
	saveParts(t, s, member.ID, sharedClientFileID, memberBody)
	saveParts(t, s, creator.ID, sharedClientFileID, creatorBody)
	if _, err := editChatPhotoForTest(s, creator.ID, blobs, chat.ID, uploadedGroupPhoto(sharedClientFileID, creatorBody)); err != nil {
		t.Fatalf("creator selects its own upload with colliding member upload id: %v", err)
	}
	chatAfterOwnUpload, ok, err := s.ChatByID(ctx, chat.ID)
	if err != nil || !ok || chatAfterOwnUpload.CurrentPhotoFileID == nil {
		t.Fatalf("chat after colliding upload: ok=%v photo=%v err=%v", ok, chatAfterOwnUpload.CurrentPhotoFileID, err)
	}
	files, err := s.FilesByIDs(ctx, []int64{*chatAfterOwnUpload.CurrentPhotoFileID})
	if err != nil {
		t.Fatalf("load selected colliding upload: %v", err)
	}
	selected, ok := files[*chatAfterOwnUpload.CurrentPhotoFileID]
	if !ok || selected.UploaderID != creator.ID {
		t.Fatalf("selected colliding upload = %+v, want creator-owned file", selected)
	}
	gotBytes, err := blobs.ReadAt(ctx, blob.Key(selected.ID), 0, int64(len(creatorBody)))
	if err != nil || !bytes.Equal(gotBytes, creatorBody) {
		t.Fatalf("selected colliding upload bytes = %d, %v; want creator's %d-byte JPEG", len(gotBytes), err, len(creatorBody))
	}
	if n, _, _, err := s.UploadPartsSummary(ctx, member.ID, sharedClientFileID); err != nil || n != 1 {
		t.Fatalf("member's colliding upload staging = %d parts, %v; want one retained part", n, err)
	}

	const foreignFileID = int64(22865)
	saveParts(t, s, member.ID, foreignFileID, memberBody)
	chatBeforeForeign, ok, err := s.ChatByID(ctx, chat.ID)
	if err != nil || !ok {
		t.Fatalf("chat before foreign upload attempt: ok=%v err=%v", ok, err)
	}
	ptsA, ptsB := statePtsForTest(t, s, creator.ID), statePtsForTest(t, s, member.ID)
	_, err = editChatPhotoForTest(s, creator.ID, blobs, chat.ID, uploadedGroupPhoto(foreignFileID, memberBody))
	if rpcMessage(t, err) != "MEDIA_INVALID" {
		t.Fatalf("foreign upload edit = %v, want MEDIA_INVALID", err)
	}
	chatAfterForeign, ok, err := s.ChatByID(ctx, chat.ID)
	if err != nil || !ok {
		t.Fatalf("chat after foreign upload attempt: ok=%v err=%v", ok, err)
	}
	if chatAfterForeign.Version != chatBeforeForeign.Version || chatAfterForeign.CurrentPhotoFileID == nil || *chatAfterForeign.CurrentPhotoFileID != *chatBeforeForeign.CurrentPhotoFileID {
		t.Fatalf("foreign upload changed current chat state: version/photo=%d/%v, want %d/%v", chatAfterForeign.Version, chatAfterForeign.CurrentPhotoFileID, chatBeforeForeign.Version, chatBeforeForeign.CurrentPhotoFileID)
	}
	if statePtsForTest(t, s, creator.ID) != ptsA || statePtsForTest(t, s, member.ID) != ptsB {
		t.Fatalf("foreign upload changed PTS: creator/member=%d/%d, want %d/%d", statePtsForTest(t, s, creator.ID), statePtsForTest(t, s, member.ID), ptsA, ptsB)
	}
	if n, _, _, err := s.UploadPartsSummary(ctx, member.ID, sharedClientFileID); err != nil || n != 1 {
		t.Fatalf("foreign owner's upload staging = %d parts, %v; want one retained part", n, err)
	}
}

func TestBasicGroupPhotoDownloadsUseLiveSelectionAndMessageHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	blobs := &galleryBlobs{Store: newBlobs(t)}
	creator, err := s.CreateUser(ctx, "+15551228871")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551228872")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	joiner, err := s.CreateUser(ctx, "+15551228873")
	if err != nil {
		t.Fatalf("create joiner: %v", err)
	}
	outsider, err := s.CreateUser(ctx, "+15551228874")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Photo downloads", []int64{member.ID})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	assemble := func(ownerID int64, original, preview []byte) store.File {
		t.Helper()
		file, derivativesStored, err := s.AllocateAndCompletePhotoFileWithDerivatives(ctx, ownerID,
			int64(len(original)), "image/jpeg", "group.jpg", api.TestMaxUserStorageBytes,
			func(file store.File) (store.PhotoAssembly, error) {
				if _, err := blobs.Put(ctx, blob.Key(file.ID), bytes.NewReader(original)); err != nil {
					return store.PhotoAssembly{}, err
				}
				return store.PhotoAssembly{Dimensions: store.PhotoDimensions{Width: 640, Height: 480},
					Derivatives: &store.PhotoDerivativeInput{MWidth: 320, MHeight: 240, MBytes: preview, Stripped: []byte{1, 1, 1}}}, nil
			})
		if err != nil || !derivativesStored {
			t.Fatalf("assemble group photo: derivatives=%v err=%v", derivativesStored, err)
		}
		return file
	}
	peerLocation := func(viewerID int64, file store.File, big bool) *tg.InputPeerPhotoFileLocation {
		return &tg.InputPeerPhotoFileLocation{Peer: api.InputPeerChat(viewerID, chat.ID), PhotoID: file.ID, Big: big}
	}
	getFile := func(viewerID int64, location tg.InputFileLocationClass, limit int) ([]byte, error) {
		result, err := api.GetFileForTest(s, viewerID, blobs, &tg.UploadGetFileRequest{Location: location, Limit: limit})
		if err != nil {
			return nil, err
		}
		file, ok := result.(*tg.UploadFile)
		if !ok {
			return nil, errors.New("getFile returned a non-upload-file response")
		}
		return file.Bytes, nil
	}
	assertBytes := func(label string, viewerID int64, location tg.InputFileLocationClass, want []byte) {
		t.Helper()
		got, err := getFile(viewerID, location, len(want))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s download = %q, %v; want %q", label, got, err, want)
		}
	}
	assertLocationInvalid := func(label string, viewerID int64, location tg.InputFileLocationClass) {
		t.Helper()
		_, err := getFile(viewerID, location, 1<<20)
		if err == nil {
			t.Fatalf("%s download succeeded, want LOCATION_INVALID", label)
		}
		if rpcMessage(t, err) != "LOCATION_INVALID" {
			t.Fatalf("%s download = %v, want LOCATION_INVALID", label, err)
		}
	}

	originalA, previewA := []byte("group photo A original"), []byte("group photo A derivative")
	photoA := assemble(creator.ID, originalA, previewA)
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, creator.ID, photoA.ID); err != nil {
		t.Fatalf("select photo A: %v", err)
	}
	assertBytes("peer photo small", member.ID, peerLocation(member.ID, photoA, false), previewA)
	assertBytes("peer photo big", member.ID, peerLocation(member.ID, photoA, true), originalA)
	assertBytes("photo location original", member.ID, &tg.InputPhotoFileLocation{
		ID: photoA.ID, AccessHash: photoA.AccessHash, ThumbSize: "x",
	}, originalA)
	assertBytes("photo location derivative", member.ID, &tg.InputPhotoFileLocation{
		ID: photoA.ID, AccessHash: photoA.AccessHash, ThumbSize: "m",
	}, previewA)
	assertLocationInvalid("wrong photo hash", member.ID, &tg.InputPhotoFileLocation{
		ID: photoA.ID, AccessHash: photoA.AccessHash ^ 1, ThumbSize: "x",
	})
	assertLocationInvalid("document location substitution", member.ID, &tg.InputDocumentFileLocation{
		ID: photoA.ID, AccessHash: photoA.AccessHash,
	})
	assertLocationInvalid("outsider peer photo", outsider.ID, peerLocation(outsider.ID, photoA, true))

	originalB, previewB := []byte("group photo B original"), []byte("group photo B derivative")
	photoB := assemble(creator.ID, originalB, previewB)
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, creator.ID, photoB.ID); err != nil {
		t.Fatalf("replace with photo B: %v", err)
	}
	assertLocationInvalid("stale peer photo A", member.ID, peerLocation(member.ID, photoA, true))
	assertBytes("frozen history photo A", member.ID, &tg.InputPhotoFileLocation{
		ID: photoA.ID, AccessHash: photoA.AccessHash, ThumbSize: "x",
	}, originalA)

	if added, _, _, err := s.AddChatUser(ctx, chat.ID, joiner.ID, creator.ID); err != nil || !added {
		t.Fatalf("add member after photo B: added=%v err=%v", added, err)
	}
	assertBytes("joiner live photo B", joiner.ID, &tg.InputPhotoFileLocation{
		ID: photoB.ID, AccessHash: photoB.AccessHash, ThumbSize: "x",
	}, originalB)
	if _, err := s.FileForDownload(ctx, photoB.ID, photoB.AccessHash, joiner.ID); !errors.Is(err, store.ErrFileNotFound) {
		t.Fatalf("joiner received a historical photo B copy: %v", err)
	}

	removed, _, _, err := s.RemoveChatUser(ctx, chat.ID, member.ID, creator.ID)
	if err != nil || !removed {
		t.Fatalf("remove historical member: removed=%v err=%v", removed, err)
	}
	assertLocationInvalid("removed member live photo B", member.ID, peerLocation(member.ID, photoB, true))
	assertBytes("removed member frozen photo B", member.ID, &tg.InputPhotoFileLocation{
		ID: photoB.ID, AccessHash: photoB.AccessHash, ThumbSize: "x",
	}, originalB)

	originalC, previewC := []byte("group photo C original"), []byte("group photo C derivative")
	photoC := assemble(creator.ID, originalC, previewC)
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, creator.ID, photoC.ID); err != nil {
		t.Fatalf("replace with photo C after removal: %v", err)
	}
	assertLocationInvalid("removed member later peer photo", member.ID, peerLocation(member.ID, photoC, true))
	assertLocationInvalid("removed member later generic photo", member.ID, &tg.InputPhotoFileLocation{
		ID: photoC.ID, AccessHash: photoC.AccessHash, ThumbSize: "x",
	})
	assertBytes("joiner current derivative after earlier join", joiner.ID, &tg.InputPhotoFileLocation{
		ID: photoC.ID, AccessHash: photoC.AccessHash, ThumbSize: "m",
	}, previewC)

	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, creator.ID, 0); err != nil {
		t.Fatalf("clear current photo: %v", err)
	}
	assertLocationInvalid("cleared peer photo", joiner.ID, peerLocation(joiner.ID, photoC, false))
	// The joiner received a photoC service copy before clear. Its ordinary
	// message capability remains valid even though the peer-photo live lane is
	// now empty.
	assertBytes("frozen photo history after clear", joiner.ID, &tg.InputPhotoFileLocation{
		ID: photoC.ID, AccessHash: photoC.AccessHash, ThumbSize: "x",
	}, originalC)
}

func TestBasicGroupPeerPhotoReadAdmittedBeforeClearCanFinish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	blobs := &galleryBlobs{Store: newBlobs(t)}
	creator, err := s.CreateUser(ctx, "+15551228875")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551228876")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Photo read admission", []int64{member.ID})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	body := jpegPhotoPayload(t, 640, 480)
	photo, derivativesStored, err := s.AllocateAndCompletePhotoFileWithDerivatives(ctx, creator.ID,
		int64(len(body)), "image/jpeg", "group.jpg", api.TestMaxUserStorageBytes,
		func(file store.File) (store.PhotoAssembly, error) {
			if _, err := blobs.Put(ctx, blob.Key(file.ID), bytes.NewReader(body)); err != nil {
				return store.PhotoAssembly{}, err
			}
			return store.PhotoAssembly{Dimensions: store.PhotoDimensions{Width: 640, Height: 480},
				Derivatives: &store.PhotoDerivativeInput{MWidth: 320, MHeight: 240, MBytes: []byte("preview"), Stripped: []byte{1, 1, 1}}}, nil
		})
	if err != nil || !derivativesStored {
		t.Fatalf("assemble group photo: derivatives=%v err=%v", derivativesStored, err)
	}
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, creator.ID, photo.ID); err != nil {
		t.Fatalf("select group photo: %v", err)
	}

	entered := blobs.pauseReads()
	t.Cleanup(blobs.releaseReads)
	readResult := make(chan struct {
		bytes []byte
		err   error
	}, 1)
	go func() {
		result, err := api.GetFileForTest(s, member.ID, blobs, &tg.UploadGetFileRequest{
			Location: &tg.InputPeerPhotoFileLocation{
				Peer: &tg.InputPeerChat{ChatID: chat.ID}, PhotoID: photo.ID, Big: true,
			},
			Limit: len(body),
		})
		if err != nil {
			readResult <- struct {
				bytes []byte
				err   error
			}{err: err}
			return
		}
		file, ok := result.(*tg.UploadFile)
		if !ok {
			readResult <- struct {
				bytes []byte
				err   error
			}{err: errors.New("getFile returned a non-upload-file response")}
			return
		}
		readResult <- struct {
			bytes []byte
			err   error
		}{bytes: file.Bytes}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("peer photo read never reached blob storage")
	}

	clearCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, _, _, err := s.SetChatPhoto(clearCtx, chat.ID, creator.ID, 0); err != nil {
		t.Fatalf("clear photo while admitted read is paused: %v", err)
	}
	blobs.releaseReads()
	select {
	case read := <-readResult:
		if read.err != nil || !bytes.Equal(read.bytes, body) {
			t.Fatalf("admitted peer photo read = %d bytes, %v; want %d bytes", len(read.bytes), read.err, len(body))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admitted peer photo read did not finish after blob release")
	}

	_, err = api.GetFileForTest(s, member.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPeerPhotoFileLocation{
			Peer: &tg.InputPeerChat{ChatID: chat.ID}, PhotoID: photo.ID, Big: true,
		},
		Limit: len(body),
	})
	if got := rpcMessage(t, err); got != "LOCATION_INVALID" {
		t.Fatalf("peer photo read admitted after clear = %s, want LOCATION_INVALID", got)
	}
}

func uploadedGroupPhoto(clientFileID int64, body []byte) *tg.InputChatUploadedPhoto {
	photo := &tg.InputChatUploadedPhoto{}
	photo.SetFile(&tg.InputFile{
		ID: clientFileID, Parts: 1, Name: "group.jpg", MD5Checksum: jpegPhotoMD5(body),
	})
	return photo
}

func TestEditChatPhotoRechecksRightsAfterPausedPut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551228881")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551228882")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Photo rights race", []int64{member.ID})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	blobs := &blockingAssembledPut{Store: newBlobs(t), started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(blobs.unblock)
	body := jpegPhotoPayload(t, 640, 480)
	const clientFileID = int64(22881)
	saveParts(t, s, member.ID, clientFileID, body)

	result := make(chan error, 1)
	go func() {
		_, err := api.EditChatPhotoForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes,
			&tg.MessagesEditChatPhotoRequest{ChatID: chat.ID, Photo: uploadedGroupPhoto(clientFileID, body)})
		result <- err
	}()
	select {
	case <-blobs.started:
	case <-time.After(10 * time.Second):
		t.Fatal("photo upload never reached assembled blob Put")
	}

	// The upload is paused in blob I/O. A group mutation must still be able to
	// acquire the chat row, and it wins authority before the photo's final write.
	messageCtx, cancelMessage := context.WithTimeout(ctx, 5*time.Second)
	_, _, duplicate, err := s.SendChatMessage(messageCtx, store.FanOut{
		ChatID: chat.ID, FromID: creator.ID, Text: "message during photo upload", RandomID: 2288101,
	})
	cancelMessage()
	if err != nil {
		t.Fatalf("send message while photo Put is paused: %v", err)
	}
	if duplicate {
		t.Fatal("message during photo Put was unexpectedly deduplicated")
	}
	rightsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := s.SetChatDefaultBannedRights(rightsCtx, chat.ID, creator.ID, []string{"change_info"}); err != nil {
		t.Fatalf("restrict change_info while photo Put is paused: %v", err)
	}
	before, ok, err := s.ChatByID(ctx, chat.ID)
	if err != nil || !ok {
		t.Fatalf("chat after restriction: ok=%v err=%v", ok, err)
	}
	ptsCreator, ptsMember := statePtsForTest(t, s, creator.ID), statePtsForTest(t, s, member.ID)
	blobs.unblock()
	if err := <-result; rpcMessage(t, err) != "CHAT_ADMIN_REQUIRED" {
		t.Fatalf("photo edit after restriction = %v, want CHAT_ADMIN_REQUIRED", err)
	}
	after, ok, err := s.ChatByID(ctx, chat.ID)
	if err != nil || !ok {
		t.Fatalf("chat after refused edit: ok=%v err=%v", ok, err)
	}
	if after.Version != before.Version || after.CurrentPhotoFileID != nil {
		t.Fatalf("refused upload changed chat version/photo to %d/%v, want %d/empty", after.Version, after.CurrentPhotoFileID, before.Version)
	}
	if gotCreator, gotMember := statePtsForTest(t, s, creator.ID), statePtsForTest(t, s, member.ID); gotCreator != ptsCreator || gotMember != ptsMember {
		t.Fatalf("refused upload changed PTS to %d/%d, want %d/%d", gotCreator, gotMember, ptsCreator, ptsMember)
	}
}
