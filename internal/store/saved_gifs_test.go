package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func prepareSavedGIFFile(t *testing.T, s *store.Store, uploader, recipient int64, mimeType string, rights []string, stored bool) (store.File, store.Message) {
	t.Helper()
	ctx := context.Background()
	f := allocate(t, s, uploader, 17)
	if _, err := store.StorePool(s).Exec(ctx,
		`UPDATE files SET mime_type = $2, subtype_rights = $3 WHERE id = $1`, f.ID, mimeType, rights); err != nil {
		t.Fatalf("set GIF metadata: %v", err)
	}
	if stored {
		if err := s.MarkFileStored(ctx, f.ID); err != nil {
			t.Fatalf("mark GIF stored: %v", err)
		}
	}
	var message store.Message
	if recipient != 0 {
		created, _, _, _, err := s.SendMessage(ctx, uploader, recipient, "gif", f.ID+1000, f.ID, 0)
		if err != nil {
			t.Fatalf("send GIF source message: %v", err)
		}
		message = created
	}
	return f, message
}

func TestSaveGIFRequiresCurrentAnimatedDocumentEntitlement(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	uploader := mustUser(t, s, "+15559200001")
	recipient := mustUser(t, s, "+15559200002")
	stranger := mustUser(t, s, "+15559200003")

	valid, _ := prepareSavedGIFFile(t, s, uploader.ID, recipient.ID, "video/mp4", []string{"send_gifs"}, true)
	unstored, _ := prepareSavedGIFFile(t, s, uploader.ID, recipient.ID, "video/mp4", []string{"send_gifs"}, false)
	nonAnimated, _ := prepareSavedGIFFile(t, s, uploader.ID, recipient.ID, "video/mp4", []string{}, true)
	wrongMime, _ := prepareSavedGIFFile(t, s, uploader.ID, recipient.ID, "text/plain", []string{"send_gifs"}, true)
	unknownRights, _ := prepareSavedGIFFile(t, s, uploader.ID, recipient.ID, "video/mp4", nil, true)
	imageGIF, _ := prepareSavedGIFFile(t, s, uploader.ID, recipient.ID, "image/gif", []string{"send_gifs"}, true)
	photo := allocate(t, s, uploader.ID, 17)
	if _, err := store.StorePool(s).Exec(ctx, `UPDATE files SET stored = true, media_kind = 'photo', width = 16, height = 16, mime_type = 'image/jpeg', subtype_rights = ARRAY['send_photos']::text[] WHERE id = $1`, photo.ID); err != nil {
		t.Fatalf("mark photo stored: %v", err)
	}
	if _, _, _, _, err := s.SendMessage(ctx, uploader.ID, recipient.ID, "photo", photo.ID+1000, photo.ID, 0); err != nil {
		t.Fatalf("send photo source message: %v", err)
	}

	for _, tc := range []struct {
		name     string
		ownerID  int64
		fileID   int64
		fileHash int64
	}{
		{name: "wrong access hash", ownerID: recipient.ID, fileID: valid.ID, fileHash: valid.AccessHash + 1},
		{name: "unknown file", ownerID: recipient.ID, fileID: valid.ID + 1<<40, fileHash: valid.AccessHash},
		{name: "no current message entitlement", ownerID: stranger.ID, fileID: valid.ID, fileHash: valid.AccessHash},
		{name: "not stored", ownerID: recipient.ID, fileID: unstored.ID, fileHash: unstored.AccessHash},
		{name: "photo", ownerID: recipient.ID, fileID: photo.ID, fileHash: photo.AccessHash},
		{name: "not animated", ownerID: recipient.ID, fileID: nonAnimated.ID, fileHash: nonAnimated.AccessHash},
		{name: "unsupported MIME type", ownerID: recipient.ID, fileID: wrongMime.ID, fileHash: wrongMime.AccessHash},
		{name: "unknown subtype rights", ownerID: recipient.ID, fileID: unknownRights.ID, fileHash: unknownRights.AccessHash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.SaveGIF(ctx, tc.ownerID, tc.fileID, tc.fileHash)
			if !errors.Is(err, store.ErrSavedGIFInvalid) {
				t.Fatalf("save invalid GIF = %v, want ErrSavedGIFInvalid", err)
			}
			got, err := s.SavedGIFs(ctx, tc.ownerID)
			if err != nil {
				t.Fatalf("list after refusal: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("saved GIFs changed after refusal: %+v", got)
			}
		})
	}

	if err := s.SaveGIF(ctx, recipient.ID, valid.ID, valid.AccessHash); err != nil {
		t.Fatalf("save entitled GIF: %v", err)
	}
	if err := s.SaveGIF(ctx, recipient.ID, imageGIF.ID, imageGIF.AccessHash); err != nil {
		t.Fatalf("save entitled image/gif: %v", err)
	}
}

func TestSavedGIFsTrackCurrentFileEntitlement(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	for index, path := range []string{"delete for everyone", "own delete", "TTL expiry", "channel ban", "channel leave", "channel history floor"} {
		t.Run(path, func(t *testing.T) {
			uploader := mustUser(t, s, fmt.Sprintf("+155592%05d", index+10))
			viewer := mustUser(t, s, fmt.Sprintf("+155593%05d", index+10))
			channelPath := path == "channel ban" || path == "channel leave" || path == "channel history floor"
			recipientID := viewer.ID
			if channelPath {
				recipientID = 0
			}
			file, message := prepareSavedGIFFile(t, s, uploader.ID, recipientID, "video/mp4", []string{"send_gifs"}, true)
			var revoke func()
			switch path {
			case "delete for everyone":
				revoke = func() {
					if _, err := s.DeleteMessages(ctx, uploader.ID, []int64{message.LocalID}, true); err != nil {
						t.Fatalf("delete source for everyone: %v", err)
					}
				}
			case "own delete":
				revoke = func() {
					if _, err := s.DeleteMessages(ctx, viewer.ID, []int64{message.PeerLocalID}, false); err != nil {
						t.Fatalf("delete own source copy: %v", err)
					}
				}
			case "TTL expiry":
				revoke = func() {
					if _, err := store.StorePool(s).Exec(ctx,
						`UPDATE messages SET expires_at = statement_timestamp() - interval '1 second' WHERE owner_id = $1 AND local_id = $2`, viewer.ID, message.PeerLocalID); err != nil {
						t.Fatalf("expire source message: %v", err)
					}
				}
			default:
				channelID, localID, err := store.SeedChannelPost(ctx, s, uploader.ID, viewer.ID, file.ID)
				if err != nil {
					t.Fatalf("seed channel post: %v", err)
				}
				switch path {
				case "channel ban":
					revoke = func() {
						until := time.Now().Add(time.Hour)
						if err := store.SetChannelBan(ctx, s, channelID, viewer.ID, &until); err != nil {
							t.Fatalf("ban channel viewer: %v", err)
						}
					}
				case "channel leave":
					revoke = func() {
						if _, err := s.LeaveChannel(ctx, channelID, viewer.ID); err != nil {
							t.Fatalf("leave source channel: %v", err)
						}
					}
				case "channel history floor":
					revoke = func() {
						if _, err := store.StorePool(s).Exec(ctx,
							`UPDATE channel_participants SET available_min_id = $3 WHERE channel_id = $1 AND user_id = $2`, channelID, viewer.ID, localID); err != nil {
							t.Fatalf("advance channel history floor: %v", err)
						}
					}
				}
			}

			if err := s.SaveGIF(ctx, viewer.ID, file.ID, file.AccessHash); err != nil {
				t.Fatalf("save source GIF: %v", err)
			}
			if _, err := s.FileForDownload(ctx, file.ID, file.AccessHash, viewer.ID); err != nil {
				t.Fatalf("download before revocation: %v", err)
			}
			revoke()
			got, err := s.SavedGIFs(ctx, viewer.ID)
			if err != nil {
				t.Fatalf("list after revocation: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("saved GIF remained after %s: %+v", path, got)
			}
			if _, err := s.FileForDownload(ctx, file.ID, file.AccessHash, viewer.ID); !errors.Is(err, store.ErrFileNotFound) {
				t.Fatalf("download after %s = %v, want ErrFileNotFound", path, err)
			}
		})
	}
}

func TestSaveGIFMovesToFrontCapsAt200AndUnsaveIsIdempotent(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner := mustUser(t, s, "+15559400001")
	files := make([]store.File, 201)
	for i := range files {
		f := allocate(t, s, owner.ID, 17)
		if _, err := store.StorePool(s).Exec(ctx,
			`UPDATE files SET mime_type = 'video/mp4', subtype_rights = ARRAY['send_gifs']::text[] WHERE id = $1`, f.ID); err != nil {
			t.Fatalf("set GIF metadata: %v", err)
		}
		if err := s.MarkFileStored(ctx, f.ID); err != nil {
			t.Fatalf("mark GIF %d stored: %v", i, err)
		}
		if _, _, _, _, err := s.SendMessage(ctx, owner.ID, owner.ID, "gif", int64(155940001+i), f.ID, 0); err != nil {
			t.Fatalf("send GIF source %d: %v", i, err)
		}
		files[i] = f
		if i < 2 {
			if err := s.SaveGIF(ctx, owner.ID, f.ID, f.AccessHash); err != nil {
				t.Fatalf("save GIF %d: %v", i, err)
			}
		}
	}
	if err := s.SaveGIF(ctx, owner.ID, files[0].ID, files[0].AccessHash); err != nil {
		t.Fatalf("resave oldest GIF: %v", err)
	}
	for i := 2; i < len(files); i++ {
		if err := s.SaveGIF(ctx, owner.ID, files[i].ID, files[i].AccessHash); err != nil {
			t.Fatalf("save GIF %d: %v", i, err)
		}
	}
	var savedCount int64
	if err := store.StorePool(s).QueryRow(ctx,
		`SELECT count(*) FROM saved_gifs WHERE owner_id = $1`, owner.ID,
	).Scan(&savedCount); err != nil {
		t.Fatalf("count saved GIF rows: %v", err)
	}
	if savedCount != 200 {
		t.Fatalf("saved GIF row count = %d, want 200", savedCount)
	}
	var oldestPresent bool
	if err := store.StorePool(s).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM saved_gifs WHERE owner_id = $1 AND file_id = $2)`, owner.ID, files[1].ID,
	).Scan(&oldestPresent); err != nil {
		t.Fatalf("check oldest saved GIF row: %v", err)
	}
	if oldestPresent {
		t.Fatalf("oldest non-resaved GIF %d remains in saved_gifs after reaching the cap", files[1].ID)
	}

	got, err := s.SavedGIFs(ctx, owner.ID)
	if err != nil {
		t.Fatalf("list saved GIFs: %v", err)
	}
	if len(got) != 200 {
		t.Fatalf("saved GIF count = %d, want 200", len(got))
	}
	if got[0].ID != files[len(files)-1].ID || got[len(got)-1].ID != files[0].ID {
		t.Fatalf("saved GIF boundaries = first %d, last %d, want first %d and last %d", got[0].ID, got[len(got)-1].ID, files[len(files)-1].ID, files[0].ID)
	}
	for _, listed := range got {
		if listed.ID == files[1].ID {
			t.Fatalf("oldest non-resaved GIF %d survived the 200-entry cap", listed.ID)
		}
	}
	if err := s.UnsaveGIF(ctx, owner.ID, files[0].ID); err != nil {
		t.Fatalf("unsave GIF: %v", err)
	}
	if err := s.UnsaveGIF(ctx, owner.ID, files[0].ID); err != nil {
		t.Fatalf("repeat unsave GIF: %v", err)
	}
	if err := s.UnsaveGIF(ctx, owner.ID, files[1].ID); err != nil {
		t.Fatalf("unsave GIF not present in the list: %v", err)
	}
	got, err = s.SavedGIFs(ctx, owner.ID)
	if err != nil {
		t.Fatalf("list after unsave: %v", err)
	}
	if len(got) != 199 {
		t.Fatalf("saved GIF count after unsave = %d, want 199", len(got))
	}
}

func TestSavedGIFInBasicChatRemainsReadableAfterLeaving(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	uploader := mustUser(t, s, "+15559410001")
	viewer := mustUser(t, s, "+15559410002")
	file, _ := prepareSavedGIFFile(t, s, uploader.ID, 0, "video/mp4", []string{"send_gifs"}, true)
	chat, err := s.CreateChat(ctx, uploader.ID, "GIF source", []int64{viewer.ID})
	if err != nil {
		t.Fatalf("create source chat: %v", err)
	}
	if _, _, _, err := s.SendChatMessage(ctx, store.FanOut{
		ChatID: chat.ID, FromID: uploader.ID, FileID: file.ID, RandomID: 9941001,
	}); err != nil {
		t.Fatalf("send GIF to source chat: %v", err)
	}
	if err := s.SaveGIF(ctx, viewer.ID, file.ID, file.AccessHash); err != nil {
		t.Fatalf("save GIF from source chat: %v", err)
	}
	if removed, _, _, err := s.RemoveChatUser(ctx, chat.ID, viewer.ID, viewer.ID); err != nil || !removed {
		t.Fatalf("leave source chat = removed %t, err %v", removed, err)
	}
	got, err := s.SavedGIFs(ctx, viewer.ID)
	if err != nil || len(got) != 1 || got[0].ID != file.ID {
		t.Fatalf("saved GIF after basic chat leave = %+v, err %v, want the original GIF", got, err)
	}
	if _, err := s.FileForDownload(ctx, file.ID, file.AccessHash, viewer.ID); err != nil {
		t.Fatalf("download after basic chat leave: %v", err)
	}
}

func TestSavedGIFBookmarkDoesNotBlockMediaErasure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	owner := mustUser(t, s, "+15559420001")
	peer := mustUser(t, s, "+15559420002")
	file := allocate(t, s, owner.ID, 17)
	if _, err := store.StorePool(s).Exec(ctx,
		`UPDATE files SET mime_type = 'video/mp4', subtype_rights = ARRAY['send_gifs']::text[] WHERE id = $1`, file.ID); err != nil {
		t.Fatalf("set GIF metadata: %v", err)
	}
	body := []byte("erasable GIF bytes")
	if _, err := blobs.Put(ctx, blob.Key(file.ID), bytes.NewReader(body)); err != nil {
		t.Fatalf("store GIF bytes: %v", err)
	}
	if err := s.MarkFileStored(ctx, file.ID); err != nil {
		t.Fatalf("mark GIF stored: %v", err)
	}
	message, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, "GIF", 9942001, file.ID, 0) //nolint:dogsled // the message id is the revoke target
	if err != nil {
		t.Fatalf("send GIF source: %v", err)
	}
	if err := s.SaveGIF(ctx, peer.ID, file.ID, file.AccessHash); err != nil {
		t.Fatalf("save GIF: %v", err)
	}
	if _, err := s.DeleteMessages(ctx, owner.ID, []int64{message.LocalID}, true); err != nil {
		t.Fatalf("delete GIF source: %v", err)
	}
	counts, err := s.SweepMediaErasure(ctx, time.Now().Add(time.Hour), store.ErasureScanBatch)
	if err != nil {
		t.Fatalf("erase revoked GIF: %v", err)
	}
	if counts.Erased != 1 {
		t.Fatalf("erased files = %d, want 1 despite saved bookmark", counts.Erased)
	}
	if _, err := s.FileForDownload(ctx, file.ID, file.AccessHash, peer.ID); !errors.Is(err, store.ErrFileNotFound) {
		t.Fatalf("download erased GIF = %v, want ErrFileNotFound", err)
	}
	got, err := s.SavedGIFs(ctx, peer.ID)
	if err != nil || len(got) != 0 {
		t.Fatalf("saved GIFs after erasure = %+v, err %v, want empty", got, err)
	}
}
