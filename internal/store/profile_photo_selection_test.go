package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestProfilePhotoSelectionChangesCurrentWithoutChangingGallery(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()

	profilePartsT(t, f.s, f.owner, 71, part('a', 100), part('b', 100))
	first, err := f.upload(71, 2, 0, 1<<30)
	if err != nil {
		t.Fatalf("upload first photo: %v", err)
	}
	profilePartsT(t, f.s, f.owner, 72, part('c', 100), part('d', 100))
	second, err := f.upload(72, 2, 0, 1<<30)
	if err != nil {
		t.Fatalf("upload second photo: %v", err)
	}
	before := profileSnapshot(t, ctx, f.dsn, f.owner, 72)
	if !before.currentFile.Valid || before.currentFile.Int64 != second.File.ID {
		t.Fatalf("current photo before selection = %v, want second photo %d", before.currentFile, second.File.ID)
	}
	if before.revision != 2 {
		t.Fatalf("revision before selection = %d, want two upload changes", before.revision)
	}
	beforeGallery, _, err := f.s.ProfilePhotosForOwner(ctx, f.owner, f.owner, 0, 0, 20)
	if err != nil {
		t.Fatalf("read gallery before selection: %v", err)
	}

	selected, changed, err := f.s.SelectProfilePhoto(ctx, f.owner, first.File.ID)
	if err != nil {
		t.Fatalf("select first photo: %v", err)
	}
	if !changed {
		t.Fatal("selecting a different photo reported no change")
	}
	if selected.ID != first.File.ID || !selected.Date.Equal(first.File.Date) {
		t.Fatalf("selected photo = id %d date %s, want id %d date %s", selected.ID, selected.Date, first.File.ID, first.File.Date)
	}
	after := profileSnapshot(t, ctx, f.dsn, f.owner, 72)
	if !after.currentFile.Valid || after.currentFile.Int64 != first.File.ID {
		t.Fatalf("current photo after selection = %v, want first photo %d", after.currentFile, first.File.ID)
	}
	if after.revision != before.revision+1 {
		t.Fatalf("revision after selection = %d, want %d", after.revision, before.revision+1)
	}
	if after.fileRows != before.fileRows || after.storedRows != before.storedRows ||
		after.chargedBytes != before.chargedBytes || after.galleryRows != before.galleryRows ||
		after.receiptRows != before.receiptRows || after.receiptTime != before.receiptTime {
		t.Fatalf("selection changed upload accounting: before=%+v after=%+v", before, after)
	}
	afterGallery, _, err := f.s.ProfilePhotosForOwner(ctx, f.owner, f.owner, 0, 0, 20)
	if err != nil {
		t.Fatalf("read gallery after selection: %v", err)
	}
	if !reflect.DeepEqual(afterGallery, beforeGallery) {
		t.Fatalf("selection changed gallery ordering or entries: before=%+v after=%+v", beforeGallery, afterGallery)
	}

	selected, changed, err = f.s.SelectProfilePhoto(ctx, f.owner, first.File.ID)
	if err != nil {
		t.Fatalf("select already-current photo: %v", err)
	}
	if changed {
		t.Fatal("selecting the current photo reported a write")
	}
	if selected.ID != first.File.ID {
		t.Fatalf("already-current selection returned photo %d, want %d", selected.ID, first.File.ID)
	}
	noOp := profileSnapshot(t, ctx, f.dsn, f.owner, 72)
	if noOp != after {
		t.Fatalf("already-current selection changed state: before=%+v after=%+v", after, noOp)
	}
}

func TestProfilePhotoSelectionRejectsForeignAndMissingEntriesWithoutWrites(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()

	profilePartsT(t, f.s, f.owner, 81, part('a', 100), part('b', 100))
	own, err := f.upload(81, 2, 0, 1<<30)
	if err != nil {
		t.Fatalf("upload owner's photo: %v", err)
	}
	foreign, err := f.s.CreateUser(ctx, "+15559300102")
	if err != nil {
		t.Fatalf("create foreign owner: %v", err)
	}
	profilePartsT(t, f.s, foreign.ID, 82, part('c', 100), part('d', 100))
	foreignPhoto, err := f.s.ProfileUpload(ctx, profileRequest(foreign.ID, 82, 2, 0, 1<<30,
		func(a store.ProfileAssembly) (store.PhotoDimensions, error) {
			return profileWriteBlob(f.s, a)
		}))
	if err != nil {
		t.Fatalf("upload foreign owner's photo: %v", err)
	}
	ownerBefore := profileSnapshot(t, ctx, f.dsn, f.owner, 81)
	foreignBefore := profileSnapshot(t, ctx, f.dsn, foreign.ID, 82)

	for _, fileID := range []int64{foreignPhoto.File.ID, 999999999} {
		if _, _, err := f.s.SelectProfilePhoto(ctx, f.owner, fileID); !errors.Is(err, store.ErrProfilePhotoUnavailable) {
			t.Errorf("select file %d: got %v, want ErrProfilePhotoUnavailable", fileID, err)
		}
	}
	ownerAfter := profileSnapshot(t, ctx, f.dsn, f.owner, 81)
	foreignAfter := profileSnapshot(t, ctx, f.dsn, foreign.ID, 82)
	if ownerAfter != ownerBefore {
		t.Errorf("rejected selections changed caller state: before=%+v after=%+v", ownerBefore, ownerAfter)
	}
	if foreignAfter != foreignBefore {
		t.Errorf("rejected selections changed the other account: before=%+v after=%+v", foreignBefore, foreignAfter)
	}
	if own.File.ID == foreignPhoto.File.ID {
		t.Fatal("test fixture assigned the same file id to both accounts")
	}
}

func TestProfilePhotoSelectionTimesOutOnProfileLockWithoutMessagingLock(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()
	profilePartsT(t, f.s, f.owner, 91, part('a', 100), part('b', 100))
	photo, err := f.upload(91, 2, 0, 1<<30)
	if err != nil {
		t.Fatalf("upload photo: %v", err)
	}
	before := profileSnapshot(t, ctx, f.dsn, f.owner, 91)

	holder, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect profile-lock holder: %v", err)
	}
	t.Cleanup(func() {
		if err := holder.Close(context.Background()); err != nil {
			t.Errorf("close profile-lock holder: %v", err)
		}
	})
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1::int, $2::int)`, store.ProfileLockDomain, f.owner); err != nil {
		t.Fatalf("hold profile-domain lock: %v", err)
	}

	started := time.Now()
	if _, _, err := f.s.SelectProfilePhoto(ctx, f.owner, photo.File.ID); !errors.Is(err, store.ErrProfilePhotoBusy) {
		t.Fatalf("selection while the profile lock is held = %v, want ErrProfilePhotoBusy", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("selection waited %s for the profile lock, want a bounded wait under two seconds", elapsed)
	}
	if got := profileMessagingOwnerLocks(t, ctx, holder, f.owner); got != 0 {
		t.Fatalf("selection touched the messaging owner lock: %d locks held", got)
	}
	if after := profileSnapshot(t, ctx, f.dsn, f.owner, 91); after != before {
		t.Fatalf("busy selection changed profile state: before=%+v after=%+v", before, after)
	}

	var unlocked bool
	if err := holder.QueryRow(ctx, `SELECT pg_advisory_unlock($1::int, $2::int)`, store.ProfileLockDomain, f.owner).Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("release profile-domain lock: unlocked=%t err=%v", unlocked, err)
	}
	if _, changed, err := f.s.SelectProfilePhoto(ctx, f.owner, photo.File.ID); err != nil || changed {
		t.Fatalf("selection after profile lock release = changed %t, err %v; want a usable pool connection and a no-op", changed, err)
	}
}

func profileMessagingOwnerLocks(t *testing.T, ctx context.Context, conn *pgx.Conn, ownerID int64) int {
	t.Helper()
	var count int
	if err := conn.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_locks l
		JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE a.datname = current_database()
		  AND l.locktype = 'advisory'
		  AND l.objsubid = 1
		  AND l.classid::bigint = $1
		  AND l.objid::bigint = $2`, ownerID>>32, ownerID&0xffffffff).Scan(&count); err != nil {
		t.Fatalf("count messaging owner locks: %v", err)
	}
	return count
}
