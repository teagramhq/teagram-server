package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const (
	AdminUserStickerSetReportPageSize int32 = AdminStickerSetPageSize
	UserStickerSetReportsPerHour            = 20
	UserStickerSetReportSweepBatch          = 1000
)

var (
	ErrUserStickerSetReportInputInvalid = errors.New("invalid user sticker-set report")
	ErrUserStickerSetReportRateLimited  = errors.New("user sticker-set report rate limit reached")
	ErrUserStickerSetReportNotFound     = errors.New("open user sticker-set report not found")
)

// AdminUserStickerSetReport is visible only through the authenticated admin
// surface. The reporter and comment never leave the admin store methods.
type AdminUserStickerSetReport struct {
	ID              int64
	SetID           int64
	SetTitle        string
	SetShortName    string
	SetIsLive       bool
	ReporterID      int64
	MessagePeerType PeerType
	MessagePeerID   int64
	MessageID       int64
	Reason          string
	Comment         string
	CreatedAt       time.Time
}

// UserStickerSetsInMessageForReport resolves only message rows visible in the
// caller's own message space or channel history. Set ids come from stored
// document references on that row.
func (s *Store) UserStickerSetsInMessageForReport(
	ctx context.Context,
	reporterID int64,
	peerType PeerType,
	peerID, messageID int64,
) ([]int64, bool, error) {
	if reporterID <= 0 || peerID <= 0 || messageID <= 0 {
		return nil, false, ErrUserStickerSetReportInputInvalid
	}
	var rows []db.UserStickerSetsInOwnMessageForReportRow
	var err error
	if peerType == PeerTypeChannel {
		channelRows, queryErr := s.q.UserStickerSetsInReadableChannelMessageForReport(ctx, db.UserStickerSetsInReadableChannelMessageForReportParams{
			ReporterID: reporterID,
			PeerID:     peerID,
			MessageID:  messageID,
		})
		if queryErr != nil {
			return nil, false, fmt.Errorf("resolve channel message for sticker-set report: %w", queryErr)
		}
		setIDs := reportableSetIDsFromChannelRows(channelRows)
		return setIDs, len(channelRows) > 0, nil
	}
	if peerType != PeerTypeUser && peerType != PeerTypeChat {
		return nil, false, ErrUserStickerSetReportInputInvalid
	}
	rows, err = s.q.UserStickerSetsInOwnMessageForReport(ctx, db.UserStickerSetsInOwnMessageForReportParams{
		ReporterID: reporterID,
		MessageID:  messageID,
		PeerType:   int16(peerType),
		PeerID:     peerID,
	})
	if err != nil {
		return nil, false, fmt.Errorf("resolve owned message for sticker-set report: %w", err)
	}
	setIDs := reportableSetIDsFromOwnRows(rows)
	return setIDs, len(rows) > 0, nil
}

// CreateUserStickerSetReportsForMessage stores at most one report per
// reporter/set pair and enforces the rolling-hour quota in one transaction.
func (s *Store) CreateUserStickerSetReportsForMessage(
	ctx context.Context,
	reporterID int64,
	setIDs []int64,
	peerType PeerType,
	peerID, messageID int64,
	reason, comment string,
) error {
	if reporterID <= 0 || peerID <= 0 || messageID <= 0 || !validReportPeerType(peerType) ||
		!validUserStickerSetReportReason(reason) || len(comment) > 512 || !utf8.ValidString(comment) ||
		strings.ContainsRune(comment, '\x00') {
		return ErrUserStickerSetReportInputInvalid
	}
	setIDs = sortedUniquePositiveIDs(setIDs)
	if len(setIDs) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin user sticker-set report: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if _, err = qtx.LockUserStickerSetReportReporter(ctx, reporterID); errors.Is(err, pgx.ErrNoRows) {
		return ErrUserStickerSetReportInputInvalid
	} else if err != nil {
		return fmt.Errorf("lock report quota for reporter %d: %w", reporterID, err)
	}

	// This row lock serializes report creation for a reporter, while ordered set
	// locks make concurrent reports involving several sets deterministic.
	liveSetIDs, err := qtx.LockLiveUserStickerSetReportTargets(ctx, setIDs)
	if err != nil {
		return fmt.Errorf("lock live sticker-set report targets: %w", err)
	}
	if len(liveSetIDs) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit empty sticker-set report: %w", err)
		}
		return nil
	}
	existingSetIDs, err := qtx.ExistingUserStickerSetReportSetIDs(ctx, db.ExistingUserStickerSetReportSetIDsParams{
		ReporterID: reporterID,
		SetIds:     liveSetIDs,
	})
	if err != nil {
		return fmt.Errorf("read prior sticker-set reports for reporter %d: %w", reporterID, err)
	}
	existing := make(map[int64]struct{}, len(existingSetIDs))
	for _, id := range existingSetIDs {
		existing[id] = struct{}{}
	}
	newSetIDs := make([]int64, 0, len(liveSetIDs))
	for _, id := range liveSetIDs {
		if _, ok := existing[id]; !ok {
			newSetIDs = append(newSetIDs, id)
		}
	}
	if len(newSetIDs) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit duplicate sticker-set report: %w", err)
		}
		return nil
	}
	count, err := qtx.CountRecentUserStickerSetReports(ctx, reporterID)
	if err != nil {
		return fmt.Errorf("count recent sticker-set reports for reporter %d: %w", reporterID, err)
	}
	if count+int64(len(newSetIDs)) > UserStickerSetReportsPerHour {
		return ErrUserStickerSetReportRateLimited
	}
	for _, setID := range newSetIDs {
		if err := qtx.InsertUserStickerSetReport(ctx, db.InsertUserStickerSetReportParams{
			ReporterID:      reporterID,
			SetID:           setID,
			MessagePeerType: int16(peerType),
			MessagePeerID:   peerID,
			MessageID:       messageID,
			Reason:          reason,
			Comment:         comment,
		}); err != nil {
			return fmt.Errorf("insert report for sticker set %d: %w", setID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit user sticker-set report: %w", err)
	}
	return nil
}

func (s *Store) AdminOpenUserStickerSetReportSetIDsPage(ctx context.Context, offset, limit int32) ([]int64, error) {
	if offset < 0 || limit < 1 || limit > AdminUserStickerSetReportPageSize+1 {
		return nil, ErrUserStickerSetReportInputInvalid
	}
	setIDs, err := s.q.AdminOpenUserStickerSetReportSetIDsPage(ctx, db.AdminOpenUserStickerSetReportSetIDsPageParams{
		OffsetCount: offset,
		LimitCount:  limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list open user sticker-set report groups: %w", err)
	}
	return setIDs, nil
}

func (s *Store) AdminOpenUserStickerSetReportsForSets(ctx context.Context, setIDs []int64) ([]AdminUserStickerSetReport, error) {
	setIDs = sortedUniquePositiveIDs(setIDs)
	if len(setIDs) == 0 {
		return nil, nil
	}
	rows, err := s.q.AdminOpenUserStickerSetReportsForSets(ctx, setIDs)
	if err != nil {
		return nil, fmt.Errorf("list open user sticker-set reports: %w", err)
	}
	reports := make([]AdminUserStickerSetReport, 0, len(rows))
	for _, row := range rows {
		reports = append(reports, AdminUserStickerSetReport{
			ID: row.ReportID, SetID: row.SetID, SetTitle: row.SetTitle,
			SetShortName: row.SetShortName, SetIsLive: row.SetIsLive,
			ReporterID: row.ReporterID, MessagePeerType: PeerType(row.MessagePeerType),
			MessagePeerID: row.MessagePeerID, MessageID: row.MessageID,
			Reason: row.Reason, Comment: row.Comment, CreatedAt: row.CreatedAt.Time,
		})
	}
	return reports, nil
}

func (s *Store) DismissUserStickerSetReport(ctx context.Context, reportID int64) error {
	if reportID <= 0 {
		return ErrUserStickerSetReportInputInvalid
	}
	updated, err := s.q.DismissUserStickerSetReport(ctx, reportID)
	if err != nil {
		return fmt.Errorf("dismiss user sticker-set report %d: %w", reportID, err)
	}
	if updated != 1 {
		return ErrUserStickerSetReportNotFound
	}
	return nil
}

// SweepExpiredUserStickerSetReports deletes closed reports past their 180-day
// retention period in bounded batches until the expired backlog is drained.
func (s *Store) SweepExpiredUserStickerSetReports(ctx context.Context) (int64, error) {
	var deleted int64
	for {
		batch, err := s.q.DeleteExpiredUserStickerSetReports(ctx, UserStickerSetReportSweepBatch)
		if err != nil {
			return deleted, fmt.Errorf("purge closed user sticker-set reports: %w", err)
		}
		deleted += batch
		if batch < UserStickerSetReportSweepBatch {
			return deleted, nil
		}
	}
}

func reportableSetIDsFromOwnRows(rows []db.UserStickerSetsInOwnMessageForReportRow) []int64 {
	setIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.SetID != nil {
			setIDs = append(setIDs, *row.SetID)
		}
	}
	return sortedUniquePositiveIDs(setIDs)
}

func reportableSetIDsFromChannelRows(rows []db.UserStickerSetsInReadableChannelMessageForReportRow) []int64 {
	setIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.SetID != nil {
			setIDs = append(setIDs, *row.SetID)
		}
	}
	return sortedUniquePositiveIDs(setIDs)
}

func sortedUniquePositiveIDs(ids []int64) []int64 {
	filtered := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > 0 {
			filtered = append(filtered, id)
		}
	}
	slices.Sort(filtered)
	return slices.Compact(filtered)
}

func validReportPeerType(peerType PeerType) bool {
	return peerType == PeerTypeUser || peerType == PeerTypeChat || peerType == PeerTypeChannel
}

func validUserStickerSetReportReason(reason string) bool {
	return reason == "copyright" || reason == "abuse" || reason == "other"
}
