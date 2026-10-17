package e2e_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/store"
)

func seedSmokeProfilePhoto(t *testing.T, f *smokeFixture, ownerID int64, payload []byte) int64 {
	t.Helper()
	ctx := context.Background()
	file, err := f.store.AllocateAndCompletePhotoFile(ctx, ownerID, int64(len(payload)), "image/jpeg", "profile.jpg", 1<<30,
		func(file store.File) (store.PhotoDimensions, error) {
			if _, err := f.blobs.Put(ctx, blob.Key(file.ID), bytes.NewReader(payload)); err != nil {
				return store.PhotoDimensions{}, err
			}
			return store.PhotoDimensions{Width: 64, Height: 48}, nil
		})
	if err != nil {
		t.Fatalf("allocate smoke profile photo: %v", err)
	}
	conn, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect to seed smoke profile photo: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close smoke profile photo connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx,
		`INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $2)`,
		ownerID, file.ID); err != nil {
		t.Fatalf("insert smoke profile photo: %v", err)
	}
	return file.ID
}
