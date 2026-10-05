package store_test

import (
	"context"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestPhotoAssemblyAcceptsDimensionBoundaries(t *testing.T) {
	t.Parallel()
	s := open(t)
	uploader := mustUser(t, s, "+15559101005")
	for _, dimensions := range []store.PhotoDimensions{
		{Width: 1, Height: 1},
		{Width: 8000, Height: 400},
		{Width: 4096, Height: 4096},
		{Width: 9000, Height: 1000},
	} {
		file, err := s.AllocateAndCompletePhotoFile(context.Background(), uploader.ID, 10, "image/jpeg", "boundary.jpg", bigQuota, func(store.File) (store.PhotoDimensions, error) {
			return dimensions, nil
		})
		if err != nil {
			t.Errorf("dimensions %+v: %v", dimensions, err)
			continue
		}
		if file.Kind != store.FileKindPhoto || file.Width != int(dimensions.Width) || file.Height != int(dimensions.Height) {
			t.Errorf("stored photo = kind %q, dimensions %d x %d, want %d x %d", file.Kind, file.Width, file.Height, dimensions.Width, dimensions.Height)
		}
	}
}
