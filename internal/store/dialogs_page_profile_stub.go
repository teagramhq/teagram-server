//go:build !performance

package store

import (
	"context"
	"time"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

type dialogsPageProfile struct {
	enabled     bool
	postQueryGo time.Duration
}

func (s *Store) dialogsPageWithPins(ctx context.Context, params dialogsPageParams) ([]db.DialogsCursorPageWithPinsRow, dialogsPageProfile, error) {
	rows, err := s.dialogsPageWithPinsGenerated(ctx, params)
	return rows, dialogsPageProfile{}, err
}

func (dialogsPageProfile) log() {}
