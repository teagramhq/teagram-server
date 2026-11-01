//go:build performance

package store

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

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

	q := db.New(dialogPageProfileDBTX{
		DBTX:    customPlanDB{DBTX: conn},
		profile: &profile,
	})
	if params.offsetID == 0 && params.offsetPeerType == 0 {
		first, err := q.DialogsFirstPageWithPins(ctx, db.DialogsFirstPageWithPinsParams{
			OwnerID: params.ownerID, ExcludePinned: params.excludePinned,
			Lim: int32(params.limit), //nolint:gosec // limit is validated and bounded by the handler
		})
		if err != nil {
			return nil, profile, err
		}
		started = time.Now()
		rows := make([]db.DialogsCursorPageWithPinsRow, len(first))
		for i, row := range first {
			rows[i] = db.DialogsCursorPageWithPinsRow(row)
		}
		profile.postQueryGo += time.Since(started)
		return rows, profile, nil
	}
	rows, err := q.DialogsCursorPageWithPins(ctx, db.DialogsCursorPageWithPinsParams{
		OwnerID: params.ownerID, OffsetID: params.offsetID,
		OffsetPeerType: int16(params.offsetPeerType), OffsetPeerID: params.offsetPeerID,
		ExcludePinned: params.excludePinned,
		Lim:           int32(params.limit), //nolint:gosec // limit is validated and bounded by the handler
	})
	return rows, profile, err
}

type dialogPageProfileDBTX struct {
	db.DBTX

	profile *dialogsPageProfile
}

func (q dialogPageProfileDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	started := time.Now()
	rows, err := q.DBTX.Query(ctx, sql, args...)
	q.profile.queryRoundTrip += time.Since(started)
	if err != nil {
		return rows, err
	}
	return dialogPageProfileRows{Rows: rows, profile: q.profile}, nil
}

type dialogPageProfileRows struct {
	pgx.Rows

	profile *dialogsPageProfile
}

func (r dialogPageProfileRows) Next() bool {
	started := time.Now()
	next := r.Rows.Next()
	r.profile.rowScanDecode += time.Since(started)
	return next
}

func (r dialogPageProfileRows) Scan(dest ...any) error {
	started := time.Now()
	err := r.Rows.Scan(dest...)
	r.profile.rowScanDecode += time.Since(started)
	return err
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
