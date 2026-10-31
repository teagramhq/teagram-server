//go:build performance

package db

import (
	"context"
	"time"
)

// DialogPageWithPinsProfileParams selects either the first page or a cursor
// page for the performance-only phase profiler.
type DialogPageWithPinsProfileParams struct {
	OwnerID        int64
	OffsetID       int64
	OffsetPeerType int16
	OffsetPeerID   int64
	ExcludePinned  bool
	Lim            int32
	FirstPage      bool
}

// DialogPageWithPinsTimings separates query startup from fetching and decoding
// all rows. The caller measures pool acquisition and post-query mapping.
type DialogPageWithPinsTimings struct {
	QueryRoundTrip time.Duration
	RowScanDecode  time.Duration
	PostQueryGo    time.Duration
}

// DialogPageWithPinsProfiled runs the same SQL as the generated first and
// cursor page methods while exposing the query and row-consumption phases.
func (q *Queries) DialogPageWithPinsProfiled(ctx context.Context, arg DialogPageWithPinsProfileParams) ([]DialogsCursorPageWithPinsRow, DialogPageWithPinsTimings, error) {
	var query string
	var args []any
	if arg.FirstPage {
		query = dialogsFirstPageWithPins
		args = []any{arg.OwnerID, arg.ExcludePinned, arg.Lim}
	} else {
		query = dialogsCursorPageWithPins
		args = []any{
			arg.OwnerID, arg.OffsetID, arg.OffsetPeerType, arg.OffsetPeerID,
			arg.ExcludePinned, arg.Lim,
		}
	}

	var timings DialogPageWithPinsTimings
	started := time.Now()
	rows, err := q.db.Query(ctx, query, args...)
	timings.QueryRoundTrip = time.Since(started)
	if err != nil {
		return nil, timings, err
	}
	defer rows.Close()

	scanStarted := time.Now()
	items := make([]DialogsCursorPageWithPinsRow, 0, int(arg.Lim))
	var firstPageItems []DialogsFirstPageWithPinsRow
	for rows.Next() {
		if arg.FirstPage {
			var item DialogsFirstPageWithPinsRow
			if err := rows.Scan(
				&item.OwnerID,
				&item.PeerID,
				&item.TopMessage,
				&item.UnreadCount,
				&item.ReadInboxMaxID,
				&item.ReadOutboxMaxID,
				&item.PeerType,
				&item.Pinned,
			); err != nil {
				timings.RowScanDecode = time.Since(scanStarted)
				return nil, timings, err
			}
			firstPageItems = append(firstPageItems, item)
		} else {
			var item DialogsCursorPageWithPinsRow
			if err := rows.Scan(
				&item.OwnerID,
				&item.PeerID,
				&item.TopMessage,
				&item.UnreadCount,
				&item.ReadInboxMaxID,
				&item.ReadOutboxMaxID,
				&item.PeerType,
				&item.Pinned,
			); err != nil {
				timings.RowScanDecode = time.Since(scanStarted)
				return nil, timings, err
			}
			items = append(items, item)
		}
	}
	timings.RowScanDecode = time.Since(scanStarted)
	if err := rows.Err(); err != nil {
		return nil, timings, err
	}
	if arg.FirstPage {
		postQueryStarted := time.Now()
		items = make([]DialogsCursorPageWithPinsRow, len(firstPageItems))
		for i, item := range firstPageItems {
			items[i] = DialogsCursorPageWithPinsRow(item)
		}
		timings.PostQueryGo = time.Since(postQueryStarted)
	}
	return items, timings, nil
}
