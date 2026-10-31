//go:build performance

package store

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

type dialogsPageProfile struct {
	enabled        bool
	poolAcquire    time.Duration
	queryRoundTrip time.Duration
	rowScanDecode  time.Duration
	postQueryGo    time.Duration
}

func (s *Store) dialogsPageWithPins(ctx context.Context, params dialogsPageParams) ([]db.DialogsCursorPageWithPinsRow, dialogsPageProfile, error) {
	if os.Getenv("TG_PERF_PROFILE_GETDIALOGS") == "" {
		rows, err := s.dialogsPageWithPinsGenerated(ctx, params)
		return rows, dialogsPageProfile{}, err
	}

	profile := dialogsPageProfile{enabled: true}
	started := time.Now()
	conn, err := s.pool.Acquire(ctx)
	profile.poolAcquire = time.Since(started)
	if err != nil {
		return nil, profile, err
	}
	defer conn.Release()

	rows, timings, err := db.New(customPlanDB{DBTX: conn}).DialogPageWithPinsProfiled(ctx, db.DialogPageWithPinsProfileParams{
		OwnerID: params.ownerID, OffsetID: params.offsetID,
		OffsetPeerType: int16(params.offsetPeerType), OffsetPeerID: params.offsetPeerID,
		ExcludePinned: params.excludePinned,
		Lim:           int32(params.limit), //nolint:gosec // limit is validated and bounded by the handler
		FirstPage:     params.offsetID == 0 && params.offsetPeerType == 0,
	})
	profile.queryRoundTrip = timings.QueryRoundTrip
	profile.rowScanDecode = timings.RowScanDecode
	profile.postQueryGo = timings.PostQueryGo
	return rows, profile, err
}

func (profile dialogsPageProfile) log() {
	if !profile.enabled {
		return
	}
	fixture := "unknown"
	switch os.Getenv("TG_PERF_FIXTURE") {
	case "1x":
		fixture = "1x"
	case "100x":
		fixture = "100x"
	}
	toMillis := func(duration time.Duration) float64 { return float64(duration.Microseconds()) / 1000 }
	log.Printf("messages.getDialogs %s dialog-page query phases: pool acquire=%.2f ms query round trip=%.2f ms row scan/decode=%.2f ms post-query Go work=%.2f ms",
		fixture, toMillis(profile.poolAcquire), toMillis(profile.queryRoundTrip),
		toMillis(profile.rowScanDecode), toMillis(profile.postQueryGo))
}
