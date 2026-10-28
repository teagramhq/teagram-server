package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// SearchResultsCalendarPeriod describes the matching shared media on one UTC
// day in a dialog.
type SearchResultsCalendarPeriod struct {
	Date     int64
	MinMsgID int64
	MaxMsgID int64
	Count    int
}

// SearchResultsCalendar returns per-day photo/video totals and a page of the
// matching media at or before offsetDate. Chat membership and the results are
// read from one snapshot so a removed member cannot retain access to copies.
func (s *Store) SearchResultsCalendar(
	ctx context.Context,
	ownerID int64,
	peerType PeerType,
	peerID int64,
	offsetID int64,
	offsetDate int64,
	limit int,
) ([]Message, []SearchResultsCalendarPeriod, int, error) {
	if peerType != PeerTypeUser && peerType != PeerTypeChat {
		return nil, nil, 0, fmt.Errorf("unsupported calendar search peer type %d", peerType)
	}
	if limit < 0 {
		return nil, nil, 0, fmt.Errorf("negative calendar search limit %d", limit)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("begin search results calendar snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	if peerType == PeerTypeChat {
		member, err := qtx.IsChatMember(ctx, db.IsChatMemberParams{ChatID: peerID, UserID: ownerID})
		if err != nil {
			return nil, nil, 0, fmt.Errorf("check search results calendar membership: %w", err)
		}
		if !member {
			return nil, nil, 0, ErrNotMember
		}
	}

	params := db.SearchResultsCalendarPeriodsParams{
		OwnerID: ownerID, PeerType: int16(peerType), PeerID: peerID,
		OffsetDate: offsetDate, OffsetID: offsetID,
	}
	periodRows, err := qtx.SearchResultsCalendarPeriods(ctx, params)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("query search results calendar periods: %w", err)
	}
	periods := make([]SearchResultsCalendarPeriod, len(periodRows))
	count := 0
	for i, period := range periodRows {
		periods[i] = SearchResultsCalendarPeriod{
			Date: period.Date, MinMsgID: period.MinMsgID, MaxMsgID: period.MaxMsgID,
			Count: int(period.Count),
		}
		count += int(period.Count)
	}

	var rows []db.Message
	if limit > 0 {
		rows, err = qtx.SearchResultsCalendarPage(ctx, db.SearchResultsCalendarPageParams{
			OwnerID: ownerID, PeerType: int16(peerType), PeerID: peerID,
			OffsetDate: offsetDate, OffsetID: offsetID,
			Lim: int32(limit), //nolint:gosec // the API caps this to maxHistoryLimit
		})
		if err != nil {
			return nil, nil, 0, fmt.Errorf("query search results calendar page: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, 0, fmt.Errorf("commit search results calendar snapshot: %w", err)
	}
	return messagesFromRows(rows), periods, count, nil
}
