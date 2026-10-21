package store_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func chatPhotoFile(t *testing.T, s *store.Store, ownerID int64) store.File {
	t.Helper()
	file, err := s.AllocateAndCompletePhotoFile(
		context.Background(), ownerID, 19, "image/jpeg", "group-photo.jpg", 1<<30,
		func(store.File) (store.PhotoDimensions, error) {
			return store.PhotoDimensions{Width: 640, Height: 480}, nil
		},
	)
	if err != nil {
		t.Fatalf("assemble photo: %v", err)
	}
	return file
}

func chatPhotoFileWithBytes(t *testing.T, s *store.Store, ownerID int64) store.File {
	t.Helper()
	ctx := context.Background()
	body := []byte("validated photo bytes")
	file, err := s.AllocateAndCompletePhotoFile(ctx, ownerID, int64(len(body)), "image/jpeg", "group-photo.jpg", 1<<30,
		func(file store.File) (store.PhotoDimensions, error) {
			_, err := store.BlobsOf(s).Put(ctx, blob.Key(file.ID), bytes.NewReader(body))
			return store.PhotoDimensions{Width: 640, Height: 480}, err
		},
	)
	if err != nil {
		t.Fatalf("assemble photo with bytes: %v", err)
	}
	return file
}

func TestSetChatPhotoPersistsCurrentPhotoAndOneHistoryCopyPerMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	a := mustUser(t, s, "+15559150001")
	b := mustUser(t, s, "+15559150002")
	chat, err := s.CreateChat(ctx, a.ID, "Photo", []int64{b.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	photo := chatPhotoFile(t, s, a.ID)

	changed, updated, sender, perOwner, err := s.SetChatPhoto(ctx, chat.ID, a.ID, photo.ID)
	if err != nil {
		t.Fatalf("select current photo: %v", err)
	}
	if !changed {
		t.Fatal("select current photo returned unchanged")
	}
	if updated.Version != chat.Version+1 {
		t.Errorf("chat version = %d, want %d", updated.Version, chat.Version+1)
	}
	if int16(sender.Action) != 6 || sender.FileID != photo.ID || sender.FanoutID == 0 {
		t.Fatalf("sender photo action = %+v, want immutable photo file %d", sender, photo.ID)
	}
	if len(perOwner) != 2 {
		t.Fatalf("per-owner PTS = %v, want one entry for both members", perOwner)
	}
	for _, member := range []store.User{a, b} {
		if perOwner[member.ID] != 1 || ptsOf(t, s, member.ID) != 1 {
			t.Errorf("owner %d PTS = %d/%d, want 1/1", member.ID, perOwner[member.ID], ptsOf(t, s, member.ID))
		}
		message, found, err := s.MessageByOwnerLocal(ctx, member.ID, 1)
		if err != nil || !found {
			t.Fatalf("owner %d photo history: found=%v err=%v", member.ID, found, err)
		}
		if int16(message.Action) != 6 || message.FileID != photo.ID || message.FanoutID != sender.FanoutID {
			t.Errorf("owner %d photo history = %+v, want shared action/file/fanout", member.ID, message)
		}
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	})
	var currentFile, currentUploader int64
	if err := conn.QueryRow(ctx, `SELECT current_photo_file_id, current_photo_uploader_id FROM chats WHERE id = $1`, chat.ID).Scan(&currentFile, &currentUploader); err != nil {
		t.Fatalf("read current chat photo: %v", err)
	}
	if currentFile != photo.ID || currentUploader != a.ID {
		t.Fatalf("current photo = %d/%d, want file/uploader %d/%d", currentFile, currentUploader, photo.ID, a.ID)
	}
}

func TestSetChatPhotoReplaceClearKeepsFrozenMemberHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	a := mustUser(t, s, "+15559150021")
	b := mustUser(t, s, "+15559150022")
	chat, err := s.CreateChat(ctx, a.ID, "Photo history", []int64{b.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	photoA := chatPhotoFileWithBytes(t, s, a.ID)
	photoB := chatPhotoFileWithBytes(t, s, b.ID)
	for _, op := range []struct {
		callerID int64
		fileID   int64
		wantFile int64
		wantAct  store.ChatAction
	}{
		{callerID: a.ID, fileID: photoA.ID, wantFile: photoA.ID, wantAct: store.ChatActionEditPhoto},
		{callerID: b.ID, fileID: photoB.ID, wantFile: photoB.ID, wantAct: store.ChatActionEditPhoto},
		{callerID: a.ID, fileID: 0, wantAct: store.ChatActionDeletePhoto},
	} {
		changed, updated, sender, perOwner, err := s.SetChatPhoto(ctx, chat.ID, op.callerID, op.fileID)
		if err != nil {
			t.Fatalf("photo mutation %d by %d: %v", op.fileID, op.callerID, err)
		}
		if !changed {
			t.Fatalf("photo mutation %d returned unchanged", op.fileID)
		}
		if updated.Version != chat.Version+1 || int16(sender.Action) != int16(op.wantAct) || sender.FileID != op.wantFile {
			t.Fatalf("photo mutation %d = chat version %d, sender %+v; want version %d/action %d/file %d",
				op.fileID, updated.Version, sender, chat.Version+1, op.wantAct, op.wantFile)
		}
		for _, member := range []store.User{a, b} {
			if perOwner[member.ID] != len(eventsOf(t, s, member.ID, 0)) {
				t.Errorf("owner %d returned PTS %d, event history has %d entries", member.ID, perOwner[member.ID], len(eventsOf(t, s, member.ID, 0)))
			}
		}
		chat = updated
	}

	current, ok, err := s.ChatByID(ctx, chat.ID)
	if err != nil || !ok {
		t.Fatalf("reload chat: ok=%v err=%v", ok, err)
	}
	if current.CurrentPhotoFileID != nil || current.CurrentPhotoUploaderID != nil {
		t.Fatalf("current photo after clear = %v/%v, want empty", current.CurrentPhotoFileID, current.CurrentPhotoUploaderID)
	}
	if current.Version != chat.Version || ptsOf(t, s, a.ID) != 3 || ptsOf(t, s, b.ID) != 3 {
		t.Fatalf("final version/PTS = %d/%d/%d, want %d/3/3", current.Version, ptsOf(t, s, a.ID), ptsOf(t, s, b.ID), chat.Version)
	}
	for _, member := range []store.User{a, b} {
		events := eventsOf(t, s, member.ID, 0)
		if len(events) != 3 {
			t.Fatalf("owner %d events = %+v, want three ordered photo actions", member.ID, events)
		}
		wantFiles := []int64{photoA.ID, photoB.ID, 0}
		wantActions := []store.ChatAction{store.ChatActionEditPhoto, store.ChatActionEditPhoto, store.ChatActionDeletePhoto}
		for i, event := range events {
			if event.Pts != i+1 || event.LocalID != int64(i+1) {
				t.Errorf("owner %d event %d = %+v, want PTS/local id %d", member.ID, i, event, i+1)
			}
			message, found, err := s.MessageByOwnerLocal(ctx, member.ID, event.LocalID)
			if err != nil || !found {
				t.Fatalf("owner %d history %d: found=%v err=%v", member.ID, event.LocalID, found, err)
			}
			if message.Action != wantActions[i] || message.FileID != wantFiles[i] {
				t.Errorf("owner %d history %d = action %d/file %d, want %d/%d", member.ID, event.LocalID, message.Action, message.FileID, wantActions[i], wantFiles[i])
			}
		}
	}

	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, a.ID, photoA.ID); !errors.Is(err, store.ErrChatPhotoUnavailable) {
		t.Fatalf("reselect prior photo A: %v, want unavailable", err)
	}
	if _, _, _, err := s.RemoveChatUser(ctx, chat.ID, b.ID, a.ID); err != nil {
		t.Fatalf("remove former member: %v", err)
	}
	if member, err := s.IsMember(ctx, chat.ID, b.ID); err != nil || member {
		t.Fatalf("former member status = %v/%v, want false", member, err)
	}
	for _, photo := range []store.File{photoA, photoB} {
		if _, err := s.FileForDownload(ctx, photo.ID, photo.AccessHash, b.ID); err != nil {
			t.Errorf("former member's retained photo %d: %v", photo.ID, err)
		}
	}
	counts := sweep(t, s, future())
	if counts.Erased != 0 || !rowPresent(t, s, photoA.ID) || !rowPresent(t, s, photoB.ID) || !blobPresent(t, s, photoA.ID) || !blobPresent(t, s, photoB.ID) {
		t.Fatalf("erasure removed retained history: counts=%+v rows=%v/%v blobs=%v/%v", counts, rowPresent(t, s, photoA.ID), rowPresent(t, s, photoB.ID), blobPresent(t, s, photoA.ID), blobPresent(t, s, photoB.ID))
	}
}

func TestChatPhotoNoopAndRetriesStillCheckLockedAuthority(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	a := mustUser(t, s, "+15559150031")
	b := mustUser(t, s, "+15559150032")
	outsider := mustUser(t, s, "+15559150033")
	chat := chatWith(t, s, a, b)
	photoB := chatPhotoFile(t, s, b.ID)
	changed, _, _, pts, err := s.SetChatPhoto(ctx, chat.ID, a.ID, 0)
	if err != nil || changed || pts != nil {
		t.Fatalf("authorized empty clear = changed %v, pts %v, err %v; want no-op", changed, pts, err)
	}
	if _, _, err := s.SetChatDefaultBannedRights(ctx, chat.ID, a.ID, []string{"change_info"}); err != nil {
		t.Fatalf("restrict change_info: %v", err)
	}
	if err := s.CheckChatPhotoEditPermission(ctx, chat.ID, b.ID); !errors.Is(err, store.ErrChatAdminRequired) {
		t.Fatalf("restricted member preflight = %v, want ErrChatAdminRequired", err)
	}
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, b.ID, 0); !errors.Is(err, store.ErrChatAdminRequired) {
		t.Fatalf("restricted empty clear = %v, want ErrChatAdminRequired", err)
	}
	if _, _, err := s.SetChatAdmin(ctx, chat.ID, b.ID, a.ID, true); err != nil {
		t.Fatalf("make restricted member an admin: %v", err)
	}
	beforeVersion := chatVersion(t, s, chat.ID)
	beforeAPts, beforeBPts := ptsOf(t, s, a.ID), ptsOf(t, s, b.ID)
	beforeAEvents, beforeBEvents := len(eventsOf(t, s, a.ID, 0)), len(eventsOf(t, s, b.ID, 0))
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, b.ID, photoB.ID); !errors.Is(err, store.ErrChatAdminRequired) {
		t.Fatalf("restricted admin photo edit = %v, want ErrChatAdminRequired", err)
	}
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, outsider.ID, photoB.ID); !errors.Is(err, store.ErrNotMember) {
		t.Fatalf("outsider photo edit = %v, want ErrNotMember", err)
	}
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID+1000, outsider.ID, 0); !errors.Is(err, store.ErrNotMember) {
		t.Fatalf("missing chat photo clear = %v, want ErrNotMember", err)
	}
	if chatVersion(t, s, chat.ID) != beforeVersion || ptsOf(t, s, a.ID) != beforeAPts || ptsOf(t, s, b.ID) != beforeBPts {
		t.Fatalf("rejected operations changed version or PTS: version=%d pts=%d/%d", chatVersion(t, s, chat.ID), ptsOf(t, s, a.ID), ptsOf(t, s, b.ID))
	}
	if len(eventsOf(t, s, a.ID, 0)) != beforeAEvents || len(eventsOf(t, s, b.ID, 0)) != beforeBEvents {
		t.Fatal("rejected operations wrote recipient events")
	}
	changed, empty, _, emptyPts, err := s.SetChatPhoto(ctx, chat.ID, a.ID, 0)
	if err != nil || changed || emptyPts != nil || empty.Version != beforeVersion {
		t.Fatalf("creator empty clear under restriction = changed %v version %d PTS %v err %v", changed, empty.Version, emptyPts, err)
	}

	if _, _, err := s.SetChatDefaultBannedRights(ctx, chat.ID, a.ID, []string{}); err != nil {
		t.Fatalf("remove change_info restriction: %v", err)
	}
	changed, selectedByMember, _, _, err := s.SetChatPhoto(ctx, chat.ID, b.ID, photoB.ID)
	if err != nil || !changed || selectedByMember.CurrentPhotoUploaderID == nil || *selectedByMember.CurrentPhotoUploaderID != b.ID {
		t.Fatalf("unrestricted member photo edit = changed %v uploader %v err %v", changed, selectedByMember.CurrentPhotoUploaderID, err)
	}
	if _, _, err := s.SetChatDefaultBannedRights(ctx, chat.ID, a.ID, []string{"change_info"}); err != nil {
		t.Fatalf("restore change_info restriction: %v", err)
	}
	retryVersion := chatVersion(t, s, chat.ID)
	retryAPts, retryBPts := ptsOf(t, s, a.ID), ptsOf(t, s, b.ID)
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, b.ID, photoB.ID); !errors.Is(err, store.ErrChatAdminRequired) {
		t.Fatalf("restricted retry of the already-selected photo = %v, want ErrChatAdminRequired", err)
	}
	if chatVersion(t, s, chat.ID) != retryVersion || ptsOf(t, s, a.ID) != retryAPts || ptsOf(t, s, b.ID) != retryBPts {
		t.Fatal("unauthorized same-photo retry changed version or PTS")
	}

	// The creator keeps change_info authority even when the default restriction
	// is set, including on a real selection and its corresponding clear.
	photoA := chatPhotoFile(t, s, a.ID)
	changed, selected, _, _, err := s.SetChatPhoto(ctx, chat.ID, a.ID, photoA.ID)
	if err != nil || !changed || selected.CurrentPhotoFileID == nil || *selected.CurrentPhotoFileID != photoA.ID {
		t.Fatalf("creator photo edit under restriction = changed %v current %v err %v", changed, selected.CurrentPhotoFileID, err)
	}
	changed, cleared, _, _, err := s.SetChatPhoto(ctx, chat.ID, a.ID, 0)
	if err != nil || !changed || cleared.CurrentPhotoFileID != nil {
		t.Fatalf("creator clear under restriction = changed %v current %v err %v", changed, cleared.CurrentPhotoFileID, err)
	}

	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, a.ID, photoB.ID); !errors.Is(err, store.ErrChatPhotoUnavailable) {
		t.Fatalf("creator selecting another member's upload = %v, want unavailable", err)
	}
}

func TestSetChatPhotoRejectsPreviouslyReferencedOrNonPhotoFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	a := mustUser(t, s, "+15559150041")
	b := mustUser(t, s, "+15559150042")
	chat := chatWith(t, s, a, b)

	foreign := chatPhotoFile(t, s, b.ID)
	document := storedFile(t, s, a.ID)
	unassembled, err := s.AllocateFile(ctx, a.ID, 13, "image/jpeg", "not-stored.jpg", 1<<30)
	if err != nil {
		t.Fatalf("allocate unstored file: %v", err)
	}
	for _, file := range []store.File{foreign, document, unassembled} {
		if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, a.ID, file.ID); !errors.Is(err, store.ErrChatPhotoUnavailable) {
			t.Errorf("ineligible file %d selection = %v, want unavailable", file.ID, err)
		}
	}

	gallery := chatPhotoFile(t, s, a.ID)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $3)`, a.ID, gallery.ID, 901); err != nil {
		t.Fatalf("insert gallery photo: %v", err)
	}
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, a.ID, gallery.ID); !errors.Is(err, store.ErrChatPhotoUnavailable) {
		t.Fatalf("gallery photo selection = %v, want unavailable", err)
	}

	grant := chatPhotoFile(t, s, a.ID)
	if _, err := conn.Exec(ctx, `INSERT INTO file_upload_grants (file_id, owner_id) VALUES ($1, $2)`, grant.ID, a.ID); err != nil {
		t.Fatalf("insert upload grant: %v", err)
	}
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, a.ID, grant.ID); !errors.Is(err, store.ErrChatPhotoUnavailable) {
		t.Fatalf("upload grant photo selection = %v, want unavailable", err)
	}

	previous := chatPhotoFile(t, s, a.ID)
	if _, _, _, err := s.SendChatMessage(ctx, store.FanOut{ChatID: chat.ID, FromID: a.ID, FileID: previous.ID, RandomID: 90}); err != nil {
		t.Fatalf("send existing photo as media: %v", err)
	}
	beforeVersion := chatVersion(t, s, chat.ID)
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, a.ID, previous.ID); !errors.Is(err, store.ErrChatPhotoUnavailable) {
		t.Fatalf("previously messaged photo selection = %v, want unavailable", err)
	}
	if chatVersion(t, s, chat.ID) != beforeVersion || ptsOf(t, s, a.ID) != 1 || ptsOf(t, s, b.ID) != 1 {
		t.Fatalf("ineligible selection changed chat or PTS: version=%d pts=%d/%d", chatVersion(t, s, chat.ID), ptsOf(t, s, a.ID), ptsOf(t, s, b.ID))
	}
}

func TestDeleteChatClearsPhotoAndReleasesHistoryRetention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	a := mustUser(t, s, "+15559150051")
	b := mustUser(t, s, "+15559150052")
	chat := chatWith(t, s, a, b)
	photo := chatPhotoFileWithBytes(t, s, a.ID)
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, a.ID, photo.ID); err != nil {
		t.Fatalf("select photo: %v", err)
	}

	deletion, err := s.DeleteChat(ctx, chat.ID, a.ID)
	if err != nil {
		t.Fatalf("delete chat: %v", err)
	}
	if len(deletion.RemovalEvents) != 2 {
		t.Fatalf("removal events = %+v, want both members", deletion.RemovalEvents)
	}
	deleted, ok, err := s.ChatByID(ctx, chat.ID)
	if err != nil || !ok {
		t.Fatalf("reload deleted chat: ok=%v err=%v", ok, err)
	}
	if deleted.CurrentPhotoFileID != nil || deleted.CurrentPhotoUploaderID != nil {
		t.Fatalf("deleted chat current photo = %v/%v, want empty", deleted.CurrentPhotoFileID, deleted.CurrentPhotoUploaderID)
	}
	for _, member := range []store.User{a, b} {
		if _, err := s.FileForDownload(ctx, photo.ID, photo.AccessHash, member.ID); !errors.Is(err, store.ErrFileNotFound) {
			t.Errorf("member %d retained deleted-chat photo access: %v", member.ID, err)
		}
	}
	counts := sweep(t, s, future())
	if counts.Erased != 1 || rowPresent(t, s, photo.ID) || blobPresent(t, s, photo.ID) {
		t.Fatalf("deleted chat photo erasure = %+v, row=%v blob=%v", counts, rowPresent(t, s, photo.ID), blobPresent(t, s, photo.ID))
	}
}

func TestChatPhotoFanoutFailureRollsBackPointerVersionAndEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	a := mustUser(t, s, "+15559150061")
	b := mustUser(t, s, "+15559150062")
	chat := chatWith(t, s, a, b)
	photo := chatPhotoFile(t, s, a.ID)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `
		CREATE FUNCTION reject_chat_photo_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected message event failure'; END;
		$$;
		CREATE TRIGGER reject_chat_photo_event BEFORE INSERT ON message_events
		FOR EACH ROW EXECUTE FUNCTION reject_chat_photo_event();
	`); err != nil {
		t.Fatalf("install event failure trigger: %v", err)
	}

	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, a.ID, photo.ID); err == nil {
		t.Fatal("photo mutation succeeded despite fan-out event failure")
	}
	unchanged, ok, err := s.ChatByID(ctx, chat.ID)
	if err != nil || !ok {
		t.Fatalf("reload chat after rollback: ok=%v err=%v", ok, err)
	}
	if unchanged.CurrentPhotoFileID != nil || unchanged.Version != chat.Version {
		t.Fatalf("failed mutation left chat at version %d/photo %v, want %d/empty", unchanged.Version, unchanged.CurrentPhotoFileID, chat.Version)
	}
	for _, member := range []store.User{a, b} {
		if ptsOf(t, s, member.ID) != 0 || len(eventsOf(t, s, member.ID, 0)) != 0 {
			t.Errorf("owner %d has PTS/events after rollback: %d/%+v", member.ID, ptsOf(t, s, member.ID), eventsOf(t, s, member.ID, 0))
		}
		if _, found := msgOpt(t, s, member.ID, 1); found {
			t.Errorf("owner %d has a service copy after rollback", member.ID)
		}
	}
}

func TestChatPhotoLosesToConcurrentChangeInfoRestriction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	a := mustUser(t, s, "+15559150071")
	b := mustUser(t, s, "+15559150072")
	chat := chatWith(t, s, a, b)
	photo := chatPhotoFile(t, s, b.ID)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	})
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rights transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // no-op after commit
	var lockedChat int64
	if err := tx.QueryRow(ctx, `SELECT id FROM chats WHERE id = $1 FOR UPDATE`, chat.ID).Scan(&lockedChat); err != nil {
		t.Fatalf("lock chat row: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, b.ID, photo.ID)
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		if err := conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'
				  AND query LIKE '%FROM chats WHERE id = $1 FOR UPDATE%'
			)
		`).Scan(&waiting); err != nil {
			t.Fatalf("check chat lock waiter: %v", err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("photo mutation did not wait on the held chat row")
	}
	if _, err := tx.Exec(ctx, `UPDATE chats SET default_banned_rights = ARRAY['change_info']::text[], version = version + 1 WHERE id = $1`, chat.ID); err != nil {
		t.Fatalf("restrict change_info under chat lock: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit restriction: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrChatAdminRequired) {
			t.Fatalf("serialized photo mutation = %v, want ErrChatAdminRequired", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("photo mutation did not finish after the rights transaction committed")
	}
	unchanged, ok, err := s.ChatByID(ctx, chat.ID)
	if err != nil || !ok {
		t.Fatalf("reload chat after rights race: ok=%v err=%v", ok, err)
	}
	if unchanged.CurrentPhotoFileID != nil || ptsOf(t, s, a.ID) != 0 || ptsOf(t, s, b.ID) != 0 {
		t.Fatalf("photo won after restriction: current=%v pts=%d/%d", unchanged.CurrentPhotoFileID, ptsOf(t, s, a.ID), ptsOf(t, s, b.ID))
	}
}

func TestChatPhotoAttachFailsAfterFileRowIsDeleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	a := mustUser(t, s, "+15559150081")
	b := mustUser(t, s, "+15559150082")
	chat := chatWith(t, s, a, b)
	photo := chatPhotoFile(t, s, a.ID)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	})
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin file erase transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // no-op after commit
	var lockedFile int64
	if err := tx.QueryRow(ctx, `SELECT id FROM files WHERE id = $1 FOR UPDATE`, photo.ID).Scan(&lockedFile); err != nil {
		t.Fatalf("lock file row: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, a.ID, photo.ID)
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		if err := conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'
				  AND query LIKE '%LockChatPhotoFileForAttach%'
			)
		`).Scan(&waiting); err != nil {
			t.Fatalf("check file lock waiter: %v", err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		select {
		case err := <-done:
			t.Fatalf("photo mutation returned before acquiring the held file row: %v", err)
		default:
		}
		t.Fatal("photo mutation did not wait on the held file row")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM files WHERE id = $1`, photo.ID); err != nil {
		t.Fatalf("erase unreferenced photo row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit file erase: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrChatPhotoUnavailable) {
			t.Fatalf("photo attachment after erase = %v, want unavailable", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("photo mutation did not finish after file erasure")
	}
	unchanged, ok, err := s.ChatByID(ctx, chat.ID)
	if err != nil || !ok {
		t.Fatalf("reload chat after file race: ok=%v err=%v", ok, err)
	}
	if unchanged.CurrentPhotoFileID != nil || ptsOf(t, s, a.ID) != 0 || ptsOf(t, s, b.ID) != 0 {
		t.Fatalf("erased file was attached: current=%v pts=%d/%d", unchanged.CurrentPhotoFileID, ptsOf(t, s, a.ID), ptsOf(t, s, b.ID))
	}
}

func TestChatPhotoReferenceCountsAndUnassembledErasureProtection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	owner := mustUser(t, s, "+15559150091")
	chat := chatWith(t, s, owner)
	unassembled, err := s.AllocateFile(ctx, owner.ID, 17, "image/jpeg", "unassembled.jpg", 1<<30)
	if err != nil {
		t.Fatalf("allocate unassembled file: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `UPDATE chats SET current_photo_file_id = $2, current_photo_uploader_id = $3 WHERE id = $1`, chat.ID, unassembled.ID, owner.ID); err != nil {
		t.Fatalf("install unassembled current-photo reference: %v", err)
	}
	scan := scanAll(t, s, future())
	if scan.Counts.SkippedChatRef != 1 || scan.Counts.Unassembled != 0 {
		t.Fatalf("scan of current unassembled reference = %+v, want one chat ref and no reclaim candidate", scan.Counts)
	}
	if counts, err := s.SweepMediaErasure(ctx, future(), store.ErasureScanBatch); err != nil || counts.UnassembledErased != 0 || !rowPresent(t, s, unassembled.ID) {
		t.Fatalf("unassembled photo reference sweep = %+v, row=%v, err=%v", counts, rowPresent(t, s, unassembled.ID), err)
	}
	if _, err := conn.Exec(ctx, `UPDATE chats SET current_photo_file_id = NULL, current_photo_uploader_id = NULL WHERE id = $1`, chat.ID); err != nil {
		t.Fatalf("clear unassembled reference: %v", err)
	}
	counts, err := s.SweepMediaErasure(ctx, future(), store.ErasureScanBatch)
	if err != nil || counts.UnassembledErased != 1 || rowPresent(t, s, unassembled.ID) {
		t.Fatalf("unassembled cleanup after clear = %+v, row=%v, err=%v", counts, rowPresent(t, s, unassembled.ID), err)
	}
}

func TestChatCurrentPhotoForeignKeyRefusesDeletionUntilCleared(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	a := mustUser(t, s, "+15559150101")
	b := mustUser(t, s, "+15559150102")
	chat := chatWith(t, s, a, b)
	photo := chatPhotoFile(t, s, a.ID)
	if _, _, _, _, err := s.SetChatPhoto(ctx, chat.ID, a.ID, photo.ID); err != nil {
		t.Fatalf("select photo: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	})
	_, err = conn.Exec(ctx, `DELETE FROM files WHERE id = $1`, photo.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "chats_current_photo_file_owned_by_uploader" {
		t.Fatalf("delete selected photo = %v, want RESTRICT foreign-key failure", err)
	}
}

func TestGenericFanOutCannotForgeChatPhotoServiceAction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	a := mustUser(t, s, "+15559150011")
	b := mustUser(t, s, "+15559150012")
	chat := chatWith(t, s, a, b)
	photo := chatPhotoFile(t, s, a.ID)

	sender, _, _, err := s.SendChatMessage(ctx, store.FanOut{
		ChatID: chat.ID, FromID: a.ID, Action: store.ChatAction(6), FileID: photo.ID,
	})
	if err == nil {
		t.Fatal("generic fan-out accepted a photo service action without the locked photo mutation")
	}
	if sender.Action != store.ChatActionNone {
		t.Errorf("forged photo action returned sender copy: %+v", sender)
	}
	for _, member := range []store.User{a, b} {
		if ptsOf(t, s, member.ID) != 0 {
			t.Errorf("owner %d PTS changed after forged photo action", member.ID)
		}
	}
}

func TestBasicGroupPhotoSchemaHasRestrictiveIndexedReference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	})
	var columns int
	if err := conn.QueryRow(ctx, `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'chats'
		  AND column_name IN ('current_photo_file_id', 'current_photo_uploader_id')
	`).Scan(&columns); err != nil {
		t.Fatalf("inspect current photo columns: %v", err)
	}
	if columns != 2 {
		t.Fatalf("chat current-photo columns = %d, want 2", columns)
	}
	var indexDef string
	if err := conn.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = 'chats'
		  AND indexname = 'chats_current_photo_file_id_idx'
	`).Scan(&indexDef); err != nil {
		t.Fatalf("read current-photo reverse index: %v", err)
	}
	if !strings.Contains(indexDef, "current_photo_file_id") {
		t.Fatalf("current-photo reverse index does not cover the file id: %s", indexDef)
	}
	var constraintDef string
	if err := conn.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'chats'::regclass
		  AND conname = 'chats_current_photo_file_owned_by_uploader'
	`).Scan(&constraintDef); err != nil {
		t.Fatalf("read current-photo ownership constraint: %v", err)
	}
	if !strings.Contains(constraintDef, "REFERENCES files(id, uploader_id)") || !strings.Contains(constraintDef, "ON DELETE RESTRICT") {
		t.Fatalf("current-photo constraint = %s, want uploader-owned file with RESTRICT", constraintDef)
	}
}
