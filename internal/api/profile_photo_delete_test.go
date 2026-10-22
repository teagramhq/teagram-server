package api_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func dispatchProfilePhotoMutation(
	t *testing.T,
	h mtproto.Handler,
	userID int64,
	key crypto.AuthKey,
	sessionID, msgID int64,
	request bin.Encoder,
) []byte {
	t.Helper()
	var body bin.Buffer
	if err := request.Encode(&body); err != nil {
		t.Fatalf("encode %T: %v", request, err)
	}
	transport := &settingsDispatcherTransport{}
	conn := mtproto.NewTestConn(transport, key)
	if err := h.OnMessage(conn, &mtproto.Request{
		AuthKeyID: key.ID,
		UserID:    userID,
		SessionID: sessionID,
		MsgID:     msgID,
		Buf:       &body,
		Ctx:       context.Background(),
	}); err != nil {
		t.Fatalf("dispatch %T: %v", request, err)
	}
	return transport.result(t, key, msgID)
}

func addCompleteProfilePhotoReceipt(t *testing.T, f *galleryFixture, ownerID, fileID int64) {
	t.Helper()
	digest := sha256.Sum256([]byte("receipt"))
	if _, err := f.writer().Exec(context.Background(), `
		INSERT INTO profile_upload_receipt (
			user_id, client_file_id, file_id, state,
			request_size, part_count, payload_digest, media_mode
		) SELECT $1, $2, $2, 1, size, 1, $3, 'photo'
		  FROM files WHERE id = $2 AND uploader_id = $1`, ownerID, fileID, digest[:]); err != nil {
		t.Fatalf("insert complete profile-photo receipt: %v", err)
	}
}

func decodePhotosPhoto(t *testing.T, body []byte) *tg.PhotosPhoto {
	t.Helper()
	var result tg.PhotosPhoto
	if err := result.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode profile-photo response: %v", err)
	}
	return &result
}

func assertPhotoEmptyResponse(t *testing.T, body []byte) {
	t.Helper()
	result := decodePhotosPhoto(t, body)
	if _, ok := result.Photo.(*tg.PhotoEmpty); !ok {
		t.Fatalf("clear response photo = %T, want *tg.PhotoEmpty", result.Photo)
	}
	if len(result.Users) != 1 {
		t.Fatalf("clear response users = %d, want one self user", len(result.Users))
	}
	user, ok := result.Users[0].(*tg.User)
	if !ok {
		t.Fatalf("clear response user = %T, want *tg.User", result.Users[0])
	}
	if photo, hasPhoto := user.GetPhoto(); hasPhoto {
		t.Fatalf("clear response self user still has photo %T/%+v", photo, photo)
	}
	assertEncodes(t, result)
}

func profileFileIDs(files []store.File) []int64 {
	ids := make([]int64, len(files))
	for i, file := range files {
		ids[i] = file.ID
	}
	return ids
}

func profilePhotoDispatcherWithUpdateLimit(s *store.Store, limit store.RateLimitConfig) mtproto.Handler {
	return api.New(
		s, 2, &tg.Config{}, slog.New(slog.DiscardHandler), false,
		api.TestMaxFileBytes, nil, 1, pgtest.PeerDeriver(), pgtest.PhotoDeriver(),
		config.RateLimitsConfig{UpdateProfile: limit}, config.RegistrationInvite,
	)
}

func assertProfileRPCError(t *testing.T, body []byte, code int, message string) {
	t.Helper()
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode RPC error: %v", err)
	}
	if rpc.ErrorCode != code || rpc.ErrorMessage != message {
		t.Fatalf("RPC error = %d %q, want %d %q", rpc.ErrorCode, rpc.ErrorMessage, code, message)
	}
}

func TestPhotosDeletePhotosRemovesOnlyValidOwnEntriesAndClearsCurrent(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144700001", "+1555144700002")
	retained := f.photoID
	current := f.addStoredPhoto(t, f.owner)
	f.addEntry(t, f.owner, current)
	addCompleteProfilePhotoReceipt(t, f, f.owner, current)
	f.selectCurrentPhoto(t, f.owner, current)
	foreign := f.addStoredPhoto(t, f.viewer)
	f.addEntry(t, f.viewer, foreign)
	deleted := f.addStoredPhoto(t, f.owner)
	f.addEntry(t, f.owner, deleted)
	f.deleteEntry(t, f.owner, deleted)
	rawFile := f.addStoredPhoto(t, f.owner)
	var rawHash int64
	if err := f.writer().QueryRow(context.Background(), `SELECT access_hash FROM files WHERE id = $1`, rawFile).Scan(&rawHash); err != nil {
		t.Fatalf("read raw file access hash: %v", err)
	}
	ownerBefore := f.selectionSnapshot(f.owner)
	viewerBefore := f.selectionSnapshot(f.viewer)

	h := fullChannelDispatcher(f.s)
	key := testKey()
	response := dispatchProfilePhotoMutation(t, h, f.owner, key, 123, 1<<32, &tg.PhotosDeletePhotosRequest{
		ID: []tg.InputPhotoClass{
			&tg.InputPhoto{ID: current, AccessHash: f.credential(f.owner, f.owner, current)},
			&tg.InputPhoto{ID: current, AccessHash: f.credential(f.owner, f.owner, current)},
			&tg.InputPhoto{ID: foreign, AccessHash: f.credential(f.owner, f.viewer, foreign)},
			&tg.InputPhoto{ID: rawFile, AccessHash: rawHash},
			&tg.InputPhoto{ID: 999999999, AccessHash: 11},
			&tg.InputPhoto{ID: deleted, AccessHash: f.credential(f.owner, f.owner, deleted)},
			&tg.InputPhoto{ID: retained, AccessHash: f.credential(f.owner, f.owner, retained) ^ 1},
			&tg.InputPhotoEmpty{},
		},
	})
	var deletedIDs tg.LongVector
	if err := deletedIDs.Decode(&bin.Buffer{Buf: response}); err != nil {
		t.Fatalf("decode photos.deletePhotos result: %v", err)
	}
	if len(deletedIDs.Elems) != 1 || deletedIDs.Elems[0] != current {
		t.Fatalf("deleted ids = %v, want [%d]", deletedIDs.Elems, current)
	}

	ownerAfter := f.selectionSnapshot(f.owner)
	if ownerAfter.galleryRows != ownerBefore.galleryRows-1 || ownerAfter.current != 0 || ownerAfter.revision != ownerBefore.revision+1 {
		t.Fatalf("profile after delete = %+v, want gallery -1, no current, revision +1 from %+v", ownerAfter, ownerBefore)
	}
	if ownerAfter.fileRows != ownerBefore.fileRows || ownerAfter.charged != ownerBefore.charged {
		t.Fatalf("delete changed stored-file quota: before=%+v after=%+v", ownerBefore, ownerAfter)
	}
	if viewerAfter := f.selectionSnapshot(f.viewer); viewerAfter != viewerBefore {
		t.Fatalf("delete changed other account: before=%+v after=%+v", viewerBefore, viewerAfter)
	}
	var receiptState int16
	if err := f.writer().QueryRow(context.Background(), `
		SELECT state FROM profile_upload_receipt WHERE user_id = $1 AND client_file_id = $2`, f.owner, current).Scan(&receiptState); err != nil {
		t.Fatalf("read deleted photo receipt: %v", err)
	}
	if receiptState != 2 {
		t.Fatalf("deleted photo receipt state = %d, want terminal state 2", receiptState)
	}
	if _, err := f.s.ProfilePhotoForDownload(context.Background(), f.owner, current, f.viewer); err == nil {
		t.Fatal("deleted photo remains available through the gallery download gate")
	}
	if _, err := f.s.CurrentProfilePhotoForDownload(context.Background(), f.owner, current, f.viewer); err == nil {
		t.Fatal("deleted current photo remains available through the peer-photo gate")
	}
	if _, err := f.call(t, f.viewer, f.owner, current, 0, len(galleryPayload)); rpcMessage(t, err) != "LOCATION_INVALID" {
		t.Fatalf("deleted cached gallery location error = %v, want LOCATION_INVALID", err)
	}
	peerLocation := dispatchProfilePhotoMutation(t, fullChannelDispatcher(f.s), f.viewer, testKey(), 123, 1<<32,
		&tg.UploadGetFileRequest{
			Location: &tg.InputPeerPhotoFileLocation{Peer: api.InputPeerUser(f.viewer, f.owner), PhotoID: current, Big: true},
			Offset:   0, Limit: len(galleryPayload),
		})
	var peerRPC mt.RPCError
	if err := peerRPC.Decode(&bin.Buffer{Buf: peerLocation}); err != nil {
		t.Fatalf("decode deleted peer-photo location reply: %v", err)
	}
	if peerRPC.ErrorMessage != "LOCATION_INVALID" {
		t.Fatalf("deleted peer-photo location error = %q, want LOCATION_INVALID", peerRPC.ErrorMessage)
	}
	photos, count, err := f.s.ProfilePhotosForOwner(context.Background(), f.owner, f.viewer, 0, 0, 10)
	if err != nil || count != 1 || len(photos) != 1 || photos[0].ID != retained {
		t.Fatalf("remaining gallery = ids %v, count %d, err %v; want [%d], count 1", profileFileIDs(photos), count, err, retained)
	}
	refresh, refreshCount, err := f.s.ProfilePhotosForOwner(context.Background(), f.owner, f.viewer, -1, current, 1)
	if err != nil || refreshCount != 1 || len(refresh) != 0 {
		t.Fatalf("deleted cached cursor = ids %v, count %d, err %v; want [], count 1", profileFileIDs(refresh), refreshCount, err)
	}
	var fileRows int64
	if err := f.writer().QueryRow(context.Background(), `SELECT count(*) FROM files WHERE id = $1 AND uploader_id = $2`, current, f.owner).Scan(&fileRows); err != nil {
		t.Fatalf("read retained file row: %v", err)
	}
	if fileRows != 1 {
		t.Fatalf("deleted photo file rows = %d, want retained row", fileRows)
	}
	_, retryErr := f.s.ProfileUpload(context.Background(), store.ProfileUploadRequest{
		OwnerID: f.owner, ClientFileID: current, Parts: 1, MimeType: "image/jpeg", FileName: "avatar.jpg",
		MaxFileBytes: 1 << 20, MaxUserBytes: api.TestMaxUserStorageBytes, GalleryCap: 20,
		Assemble: func(store.ProfileAssembly) (store.PhotoDimensions, error) {
			return store.PhotoDimensions{Width: 64, Height: 48}, nil
		},
	})
	if !errors.Is(retryErr, store.ErrProfilePhotoUnavailable) {
		t.Fatalf("retry of deleted photo upload = %v, want unavailable", retryErr)
	}
	uploadRetry := &tg.PhotosUploadProfilePhotoRequest{}
	uploadRetry.SetFile(&tg.InputFile{ID: current, Parts: 1, Name: "avatar.jpg", MD5Checksum: "00000000000000000000000000000000"})
	retryBody := dispatchProfilePhotoMutation(t, h, f.owner, key, 123, (1<<32)+4, uploadRetry)
	var retryRPC mt.RPCError
	if err := retryRPC.Decode(&bin.Buffer{Buf: retryBody}); err != nil {
		t.Fatalf("decode deleted upload retry reply: %v", err)
	}
	if retryRPC.ErrorCode != 400 || retryRPC.ErrorMessage != "MEDIA_INVALID" {
		t.Fatalf("deleted upload retry = %d %q, want 400 MEDIA_INVALID", retryRPC.ErrorCode, retryRPC.ErrorMessage)
	}
	if afterRetry := f.selectionSnapshot(f.owner); afterRetry != ownerAfter {
		t.Fatalf("deleted upload retry changed profile state: before=%+v after=%+v", ownerAfter, afterRetry)
	}
}

func TestUpdateProfilePhotoEmptyDeduplicatesByTransportRequest(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144700101", "+1555144700102")
	current := f.addStoredPhoto(t, f.owner)
	f.addEntry(t, f.owner, current)
	addCompleteProfilePhotoReceipt(t, f, f.owner, current)
	f.selectCurrentPhoto(t, f.owner, current)
	before := f.selectionSnapshot(f.owner)

	h := fullChannelDispatcher(f.s)
	key := testKey()
	const sessionID = int64(123)
	const clearMsgID = int64(1 << 32)
	clearRequest := &tg.PhotosUpdateProfilePhotoRequest{ID: &tg.InputPhotoEmpty{}}
	first := dispatchProfilePhotoMutation(t, h, f.owner, key, sessionID, clearMsgID, clearRequest)
	assertPhotoEmptyResponse(t, first)
	afterClear := f.selectionSnapshot(f.owner)
	if afterClear.galleryRows != before.galleryRows-1 || afterClear.current != 0 || afterClear.revision != before.revision+1 {
		t.Fatalf("profile after clear = %+v, want one removed entry, no current, revision +1 from %+v", afterClear, before)
	}
	if afterClear.fileRows != before.fileRows || afterClear.charged != before.charged {
		t.Fatalf("clear changed stored-file quota: before=%+v after=%+v", before, afterClear)
	}

	selected := f.photoID
	selectResult := dispatchProfilePhotoMutation(t, h, f.owner, key, sessionID, clearMsgID+4, &tg.PhotosUpdateProfilePhotoRequest{
		ID: &tg.InputPhoto{ID: selected, AccessHash: f.credential(f.owner, f.owner, selected)},
	})
	selectedReply := decodePhotosPhoto(t, selectResult)
	selectedPhoto, ok := selectedReply.Photo.(*tg.Photo)
	if !ok || selectedPhoto.ID != selected {
		t.Fatalf("selection after clear result = %T/%+v, want photo %d", selectedReply.Photo, selectedReply.Photo, selected)
	}
	selectedState := f.selectionSnapshot(f.owner)

	// A new connection has no connection-local replay cache. The durable receipt
	// for the same auth key, session and msg_id must return the first empty result
	// without clearing the photo selected since then.
	replay := dispatchProfilePhotoMutation(t, h, f.owner, key, sessionID, clearMsgID, clearRequest)
	assertPhotoEmptyResponse(t, replay)
	if afterReplay := f.selectionSnapshot(f.owner); afterReplay != selectedState {
		t.Fatalf("replayed clear changed newer selection: before=%+v after=%+v", selectedState, afterReplay)
	}

	newClear := dispatchProfilePhotoMutation(t, h, f.owner, key, sessionID, clearMsgID+8, clearRequest)
	assertPhotoEmptyResponse(t, newClear)
	afterNewClear := f.selectionSnapshot(f.owner)
	if afterNewClear.galleryRows != selectedState.galleryRows-1 || afterNewClear.current != 0 || afterNewClear.revision != selectedState.revision+1 {
		t.Fatalf("profile after fresh clear = %+v, want one removed entry, no current, revision +1 from %+v", afterNewClear, selectedState)
	}
	noCurrent := dispatchProfilePhotoMutation(t, h, f.owner, key, sessionID, clearMsgID+12, clearRequest)
	assertPhotoEmptyResponse(t, noCurrent)
	if afterNoCurrent := f.selectionSnapshot(f.owner); afterNoCurrent != afterNewClear {
		t.Fatalf("clear with no current photo changed profile state: before=%+v after=%+v", afterNewClear, afterNoCurrent)
	}
}

func TestUpdateProfilePhotoEmptyReplayDoesNotSpendRateLimit(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144700121", "+1555144700122")
	current := f.addStoredPhoto(t, f.owner)
	f.addEntry(t, f.owner, current)
	addCompleteProfilePhotoReceipt(t, f, f.owner, current)
	f.selectCurrentPhoto(t, f.owner, current)
	h := profilePhotoDispatcherWithUpdateLimit(f.s, store.RateLimitConfig{Limit: 1, Window: time.Hour})
	key := testKey()
	const sessionID = int64(123)
	const clearMsgID = int64(1 << 32)
	clearRequest := &tg.PhotosUpdateProfilePhotoRequest{ID: &tg.InputPhotoEmpty{}}

	first := dispatchProfilePhotoMutation(t, h, f.owner, key, sessionID, clearMsgID, clearRequest)
	assertPhotoEmptyResponse(t, first)
	f.selectCurrentPhoto(t, f.owner, f.photoID)
	selectedState := f.selectionSnapshot(f.owner)

	// A durable receipt replay returns its first result without spending another
	// update_profile token or clearing the photo selected after the first call.
	replay := dispatchProfilePhotoMutation(t, h, f.owner, key, sessionID, clearMsgID, clearRequest)
	assertPhotoEmptyResponse(t, replay)
	if afterReplay := f.selectionSnapshot(f.owner); afterReplay != selectedState {
		t.Fatalf("replayed clear changed newer selection or revision: before=%+v after=%+v", selectedState, afterReplay)
	}

	// A different transport request is fresh and remains subject to the spent
	// budget; refusal must leave the current profile photo untouched.
	fresh := dispatchProfilePhotoMutation(t, h, f.owner, key, sessionID, clearMsgID+4, clearRequest)
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: fresh}); err != nil {
		t.Fatalf("decode fresh clear rate-limit reply: %v", err)
	}
	if rpc.ErrorCode != 420 || !strings.HasPrefix(rpc.ErrorMessage, "FLOOD_WAIT_") {
		t.Fatalf("fresh clear error = %d %q, want 420 FLOOD_WAIT", rpc.ErrorCode, rpc.ErrorMessage)
	}
	if afterFresh := f.selectionSnapshot(f.owner); afterFresh != selectedState {
		t.Fatalf("rate-limited fresh clear changed profile state: before=%+v after=%+v", selectedState, afterFresh)
	}
}

func waitForProfilePhotoActivity(
	t *testing.T,
	f *galleryFixture,
	deadline time.Time,
	queryFragment, state, waitEvent string,
) {
	t.Helper()
	for time.Now().Before(deadline) {
		var count int64
		err := f.observer().QueryRow(context.Background(), `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND query LIKE '%' || $1 || '%'
			  AND ($2 = '' OR state = $2)
			  AND ($3 = '' OR wait_event_type = $3)
		`, queryFragment, state, waitEvent).Scan(&count)
		if err != nil {
			t.Fatalf("read pg_stat_activity for %q: %v", queryFragment, err)
		}
		if count > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pg_stat_activity query %q (state %q, wait event %q)",
		queryFragment, state, waitEvent)
}

func TestUpdateProfilePhotoEmptyConcurrentReplayDoesNotSpendRateLimit(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144700131", "+1555144700132")
	addCompleteProfilePhotoReceipt(t, f, f.owner, f.photoID)
	f.selectCurrentPhoto(t, f.owner, f.photoID)
	before := f.selectionSnapshot(f.owner)
	h := profilePhotoDispatcherWithUpdateLimit(f.s, store.RateLimitConfig{Limit: 2, Window: time.Hour})
	key := testKey()
	const sessionID = int64(123)
	const clearMsgID = int64(1 << 32)
	clearRequest := &tg.PhotosUpdateProfilePhotoRequest{ID: &tg.InputPhotoEmpty{}}

	blocker, err := f.writer().Begin(context.Background())
	if err != nil {
		t.Fatalf("begin upload-receipt lock: %v", err)
	}
	var lockedClientFileID int64
	if err := blocker.QueryRow(context.Background(), `
		SELECT client_file_id FROM profile_upload_receipt
		WHERE user_id = $1 AND client_file_id = $2
		FOR UPDATE
	`, f.owner, f.photoID).Scan(&lockedClientFileID); err != nil {
		t.Fatalf("lock current upload receipt: %v", err)
	}
	if lockedClientFileID != f.photoID {
		t.Fatalf("locked client file id = %d, want %d", lockedClientFileID, f.photoID)
	}
	blockerOpen := true
	defer func() {
		if blockerOpen {
			if err := blocker.Rollback(context.Background()); err != nil {
				t.Errorf("release upload-receipt lock: %v", err)
			}
		}
	}()

	deadline := time.Now().Add(store.ProfilePhotoSelectionWait - 25*time.Millisecond)
	firstDone := make(chan []byte, 1)
	go func() {
		firstDone <- dispatchProfilePhotoMutation(t, h, f.owner, key, sessionID, clearMsgID, clearRequest)
	}()
	waitForProfilePhotoActivity(t, f, deadline, "LockProfileUploadReceiptsForDeletion", "active", "Lock")

	replayDone := make(chan []byte, 1)
	go func() {
		replayDone <- dispatchProfilePhotoMutation(t, h, f.owner, key, sessionID, clearMsgID, clearRequest)
	}()
	// Both implementations reach the per-owner lock while the first clear is
	// paused. The replay must wait there before rate admission as well as before
	// mutating the gallery.
	waitForProfilePhotoActivity(t, f, deadline, "pg_try_advisory_xact_lock", "idle in transaction", "")
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatalf("release upload-receipt lock: %v", err)
	}
	blockerOpen = false

	awaitReply := func(name string, done <-chan []byte) []byte {
		t.Helper()
		select {
		case body := <-done:
			return body
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s clear reply", name)
			return nil
		}
	}
	assertPhotoEmptyResponse(t, awaitReply("first", firstDone))
	assertPhotoEmptyResponse(t, awaitReply("replay", replayDone))

	var tokenCount int32
	if err := f.writer().QueryRow(context.Background(), `
		SELECT token_count FROM rate_limits WHERE subject_id = $1 AND surface = 'update_profile'
	`, f.owner).Scan(&tokenCount); err != nil {
		t.Fatalf("read clear rate-limit count: %v", err)
	}
	if tokenCount != 1 {
		t.Fatalf("concurrent same-tuple clears consumed %d tokens, want one", tokenCount)
	}
	if after := f.selectionSnapshot(f.owner); after.galleryRows != before.galleryRows-1 || after.current != 0 || after.revision != before.revision+1 {
		t.Fatalf("profile after concurrent clear replay = %+v, want one removed entry, no current, revision +1 from %+v", after, before)
	}
}

func TestUpdateProfilePhotoEmptyRejectsFallbackAndBotBeforeRateOrLock(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144700151", "+1555144700152")
	addCompleteProfilePhotoReceipt(t, f, f.owner, f.photoID)
	f.selectCurrentPhoto(t, f.owner, f.photoID)
	before := f.selectionSnapshot(f.owner)
	h := profilePhotoDispatcherWithUpdateLimit(f.s, store.RateLimitConfig{Limit: 1, Window: time.Hour})
	key := testKey()
	if _, err := f.writer().Exec(context.Background(), `SELECT pg_advisory_lock($1::int, $2::int)`, profilePhotoLockDomainForTest, f.owner); err != nil {
		t.Fatalf("hold profile-domain lock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			if _, err := f.writer().Exec(context.Background(), `SELECT pg_advisory_unlock($1::int, $2::int)`, profilePhotoLockDomainForTest, f.owner); err != nil {
				t.Errorf("release profile-domain lock: %v", err)
			}
		}
	}()

	fallback := &tg.PhotosUpdateProfilePhotoRequest{ID: &tg.InputPhotoEmpty{}}
	fallback.SetFallback(true)
	started := time.Now()
	fallbackReply := dispatchProfilePhotoMutation(t, h, f.owner, key, 123, 1<<32, fallback)
	assertProfileRPCError(t, fallbackReply, 400, "INPUT_REQUEST_INVALID")
	bot := &tg.PhotosUpdateProfilePhotoRequest{ID: &tg.InputPhotoEmpty{}}
	bot.SetBot(&tg.InputUserSelf{})
	botReply := dispatchProfilePhotoMutation(t, h, f.owner, key, 123, (1<<32)+4, bot)
	assertProfileRPCError(t, botReply, 400, "INPUT_REQUEST_INVALID")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("early clear refusals took %s with the profile lock held, want no lock wait", elapsed)
	}
	if after := f.selectionSnapshot(f.owner); after != before {
		t.Fatalf("fallback/bot refusals changed profile state: before=%+v after=%+v", before, after)
	}
	if _, err := f.writer().Exec(context.Background(), `SELECT pg_advisory_unlock($1::int, $2::int)`, profilePhotoLockDomainForTest, f.owner); err != nil {
		t.Fatalf("release profile-domain lock before valid clear: %v", err)
	}
	locked = false
	valid := dispatchProfilePhotoMutation(t, h, f.owner, key, 123, (1<<32)+8,
		&tg.PhotosUpdateProfilePhotoRequest{ID: &tg.InputPhotoEmpty{}})
	assertPhotoEmptyResponse(t, valid)
}

func TestPhotosDeletePhotosRejectsOversizedVectorBeforeChangingState(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144700201", "+1555144700202")
	f.selectCurrentPhoto(t, f.owner, f.photoID)
	before := f.selectionSnapshot(f.owner)
	ids := make([]tg.InputPhotoClass, 101)
	for i := range ids {
		ids[i] = &tg.InputPhotoEmpty{}
	}
	body := dispatchProfilePhotoMutation(t, fullChannelDispatcher(f.s), f.owner, testKey(), 123, 1<<32,
		&tg.PhotosDeletePhotosRequest{ID: ids})
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode oversized delete reply: %v", err)
	}
	if rpc.ErrorCode != 400 || rpc.ErrorMessage != "LIMIT_INVALID" {
		t.Fatalf("oversized delete = %d %q, want 400 LIMIT_INVALID", rpc.ErrorCode, rpc.ErrorMessage)
	}
	if after := f.selectionSnapshot(f.owner); after != before {
		t.Fatalf("oversized delete changed profile state: before=%+v after=%+v", before, after)
	}
}

func TestPhotosDeletePhotosLockContentionReturnsFloodWaitWithoutChanges(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144700301", "+1555144700302")
	addCompleteProfilePhotoReceipt(t, f, f.owner, f.photoID)
	f.selectCurrentPhoto(t, f.owner, f.photoID)
	before := f.selectionSnapshot(f.owner)
	if _, err := f.writer().Exec(context.Background(), `SELECT pg_advisory_lock($1::int, $2::int)`, profilePhotoLockDomainForTest, f.owner); err != nil {
		t.Fatalf("hold profile-domain lock: %v", err)
	}
	defer func() {
		if _, err := f.writer().Exec(context.Background(), `SELECT pg_advisory_unlock($1::int, $2::int)`, profilePhotoLockDomainForTest, f.owner); err != nil {
			t.Errorf("release profile-domain lock: %v", err)
		}
	}()

	started := time.Now()
	body := dispatchProfilePhotoMutation(t, fullChannelDispatcher(f.s), f.owner, testKey(), 123, 1<<32,
		&tg.PhotosDeletePhotosRequest{ID: []tg.InputPhotoClass{
			&tg.InputPhoto{ID: f.photoID, AccessHash: f.credential(f.owner, f.owner, f.photoID)},
		}})
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode busy delete reply: %v", err)
	}
	if rpc.ErrorCode != 420 || rpc.ErrorMessage != "FLOOD_WAIT_1" {
		t.Fatalf("delete while profile lock is held = %d %q, want 420 FLOOD_WAIT_1", rpc.ErrorCode, rpc.ErrorMessage)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("profile-lock refusal took %s, want a bounded wait under two seconds", elapsed)
	}
	if after := f.selectionSnapshot(f.owner); after != before {
		t.Fatalf("busy delete changed profile state: before=%+v after=%+v", before, after)
	}
}
