package e2e_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/store"
)

func seedSmokeProfilePhoto(t *testing.T, f *smokeFixture, ownerID int64, payload, preview []byte) int64 {
	t.Helper()
	ctx := context.Background()
	file, derivativesStored, err := f.store.AllocateAndCompletePhotoFileWithDerivatives(ctx, ownerID, int64(len(payload)), "image/jpeg", "profile.jpg", 1<<30,
		func(file store.File) (store.PhotoAssembly, error) {
			if _, err := f.blobs.Put(ctx, blob.Key(file.ID), bytes.NewReader(payload)); err != nil {
				return store.PhotoAssembly{}, err
			}
			return store.PhotoAssembly{
				Dimensions: store.PhotoDimensions{Width: 640, Height: 480},
				Derivatives: &store.PhotoDerivativeInput{
					MWidth: 320, MHeight: 240, MBytes: preview, Stripped: []byte{1, 1, 1},
				},
			}, nil
		})
	if err != nil {
		t.Fatalf("allocate smoke profile photo: %v", err)
	}
	if !derivativesStored {
		t.Fatal("smoke profile photo derivative was not stored")
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

func assertSmokeProfilePhotoSizes(t *testing.T, ctx context.Context, client *smokeClient, photo *tg.Photo, original, preview []byte, label string) {
	t.Helper()
	downloaded := make(map[string][]byte, 2)
	var originalType string
	for _, class := range photo.Sizes {
		size, ok := class.(*tg.PhotoSize)
		if !ok {
			continue
		}
		want := preview
		if size.Type == "m" {
			// The m response must be the derivative returned by the gallery gate.
		} else {
			originalType = size.Type
			want = original
		}
		var result tg.UploadFileClass
		if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			result, err = api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
				Location: &tg.InputPhotoFileLocation{
					ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: size.Type,
				},
				Offset: 0, Limit: size.Size,
			})
			return err
		}); err != nil {
			t.Fatalf("%s size %q download: %v", label, size.Type, err)
		}
		file, ok := result.(*tg.UploadFile)
		if !ok {
			t.Fatalf("%s size %q result = %T, want *tg.UploadFile", label, size.Type, result)
		}
		if !bytes.Equal(file.Bytes, want) {
			t.Errorf("%s size %q returned %q, want %q", label, size.Type, file.Bytes, want)
		}
		if len(file.Bytes) != size.Size {
			t.Errorf("%s size %q returned %d bytes, advertised %d", label, size.Type, len(file.Bytes), size.Size)
		}
		downloaded[size.Type] = bytes.Clone(file.Bytes)
	}
	if len(downloaded) != 2 || downloaded["m"] == nil || originalType == "" || downloaded[originalType] == nil {
		t.Fatalf("%s advertised downloadable sizes = %v, want m and the original", label, downloaded)
	}
	if bytes.Equal(downloaded["m"], downloaded[originalType]) {
		t.Fatalf("%s m and %q sizes returned identical bytes", label, originalType)
	}
}
