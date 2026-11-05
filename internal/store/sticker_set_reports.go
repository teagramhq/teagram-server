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
	AdminUserStickerSetReportPageSize          int32 = AdminStickerSetPageSize
	AdminUserStickerSetReportsPerGroupPageSize int32 = 50
	UserStickerSetReportsPerHour                     = 20
	UserStickerSetReportSweepBatch                   = 1000
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

// UserStickerSetReportMessage carries server-resolved sets for one visible
// message selected in a report request.
type UserStickerSetReportMessage struct {
	MessageID int64
	SetIDs    []int64
}

type userStickerSetReportTarget struct {
	SetID     int64
	MessageID int64
}

// UserStickerSetsInMessagesForReport resolves only message rows visible in the
// caller's own message space or channel history. Set ids come from stored
// document references on those rows.
func (s *Store) UserStickerSetsInMessagesForReport(
	ctx context.Context,
	reporterID int64,
	peerType PeerType,
	peerID int64,
	messageIDs []int64,
) ([]UserStickerSetReportMessage, bool, error) {
	if reporterID <= 0 || peerID <= 0 || len(messageIDs) == 0 {
		return nil, false, ErrUserStickerSetReportInputInvalid
	}
	for _, messageID := range messageIDs {
		if messageID <= 0 {
			return nil, false, ErrUserStickerSetReportInputInvalid
		}
	}
	messageIDs = sortedUniquePositiveIDs(messageIDs)
	var rows []db.UserStickerSetsInOwnMessagesForReportRow
	var err error
	if peerType == PeerTypeChannel {
		channelRows, queryErr := s.q.UserStickerSetsInReadableChannelMessagesForReport(ctx, db.UserStickerSetsInReadableChannelMessagesForReportParams{
			ReporterID: reporterID,
			PeerID:     peerID,
			MessageIds: messageIDs,
		})
		if queryErr != nil {
			return nil, false, fmt.Errorf("resolve channel messages for sticker-set report: %w", queryErr)
		}
		setIDsByMessage := make(map[int64][]int64, len(messageIDs))
		visible := make(map[int64]struct{}, len(messageIDs))
		for _, row := range channelRows {
			visible[row.LocalID] = struct{}{}
			if row.SetID != nil {
				setIDsByMessage[row.LocalID] = append(setIDsByMessage[row.LocalID], *row.SetID)
			}
		}
		return reportMessagesFromResolvedRows(messageIDs, visible, setIDsByMessage)
	}
	if peerType != PeerTypeUser && peerType != PeerTypeChat {
		return nil, false, ErrUserStickerSetReportInputInvalid
	}
	rows, err = s.q.UserStickerSetsInOwnMessagesForReport(ctx, db.UserStickerSetsInOwnMessagesForReportParams{
		ReporterID: reporterID,
		MessageIds: messageIDs,
		PeerType:   int16(peerType),
		PeerID:     peerID,
	})
	if err != nil {
		return nil, false, fmt.Errorf("resolve owned messages for sticker-set report: %w", err)
	}
	setIDsByMessage := make(map[int64][]int64, len(messageIDs))
	visible := make(map[int64]struct{}, len(messageIDs))
	for _, row := range rows {
		visible[row.LocalID] = struct{}{}
		if row.SetID != nil {
			setIDsByMessage[row.LocalID] = append(setIDsByMessage[row.LocalID], *row.SetID)
		}
	}
	return reportMessagesFromResolvedRows(messageIDs, visible, setIDsByMessage)
}

func reportMessagesFromResolvedRows(
	messageIDs []int64,
	visible map[int64]struct{},
	setIDsByMessage map[int64][]int64,
) ([]UserStickerSetReportMessage, bool, error) {
	if len(visible) != len(messageIDs) {
		return nil, false, nil
	}
	messages := make([]UserStickerSetReportMessage, 0, len(messageIDs))
	for _, messageID := range messageIDs {
		messages = append(messages, UserStickerSetReportMessage{
			MessageID: messageID,
			SetIDs:    sortedUniquePositiveIDs(setIDsByMessage[messageID]),
		})
	}
	return messages, true, nil
}

// CreateUserStickerSetReportsForMessages stores distinct selected message
// reports, deduplicates identical open reports, and enforces the rolling-hour
// quota in one transaction.
func (s *Store) CreateUserStickerSetReportsForMessages(
	ctx context.Context,
	reporterID int64,
	messages []UserStickerSetReportMessage,
	peerType PeerType,
	peerID int64,
	reason, comment string,
) error {
	if reporterID <= 0 || peerID <= 0 || len(messages) == 0 || !validReportPeerType(peerType) ||
		!validUserStickerSetReportReason(reason) || len(comment) > 512 || !utf8.ValidString(comment) ||
		strings.ContainsRune(comment, '\x00') {
		return ErrUserStickerSetReportInputInvalid
	}
	targets := make([]userStickerSetReportTarget, 0)
	seenTargets := make(map[userStickerSetReportTarget]struct{})
	setIDs := make([]int64, 0)
	for _, message := range messages {
		if message.MessageID <= 0 {
			return ErrUserStickerSetReportInputInvalid
		}
		for _, setID := range message.SetIDs {
			if setID <= 0 {
				return ErrUserStickerSetReportInputInvalid
			}
			target := userStickerSetReportTarget{SetID: setID, MessageID: message.MessageID}
			if _, ok := seenTargets[target]; ok {
				continue
			}
			seenTargets[target] = struct{}{}
			targets = append(targets, target)
			setIDs = append(setIDs, setID)
		}
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
	liveSetIDSet := make(map[int64]struct{}, len(liveSetIDs))
	for _, id := range liveSetIDs {
		liveSetIDSet[id] = struct{}{}
	}
	liveTargets := make([]userStickerSetReportTarget, 0, len(targets))
	for _, target := range targets {
		if _, ok := liveSetIDSet[target.SetID]; ok {
			liveTargets = append(liveTargets, target)
		}
	}
	if len(liveTargets) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit empty sticker-set report: %w", err)
		}
		return nil
	}
	targetSetIDs := make([]int64, len(liveTargets))
	targetMessageIDs := make([]int64, len(liveTargets))
	for i, target := range liveTargets {
		targetSetIDs[i] = target.SetID
		targetMessageIDs[i] = target.MessageID
	}
	existingTargets, err := qtx.ExistingOpenUserStickerSetReportTargets(ctx, db.ExistingOpenUserStickerSetReportTargetsParams{
		ReporterID:      reporterID,
		MessagePeerType: int16(peerType),
		MessagePeerID:   peerID,
		Reason:          reason,
		Comment:         comment,
		SetIds:          targetSetIDs,
		MessageIds:      targetMessageIDs,
	})
	if err != nil {
		return fmt.Errorf("read identical open sticker-set reports for reporter %d: %w", reporterID, err)
	}
	existing := make(map[userStickerSetReportTarget]struct{}, len(existingTargets))
	for _, target := range existingTargets {
		existing[userStickerSetReportTarget{SetID: target.SetID, MessageID: target.MessageID}] = struct{}{}
	}
	newTargets := make([]userStickerSetReportTarget, 0, len(liveTargets))
	for _, target := range liveTargets {
		if _, ok := existing[target]; !ok {
			newTargets = append(newTargets, target)
		}
	}
	if len(newTargets) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit duplicate sticker-set report: %w", err)
		}
		return nil
	}
	count, err := qtx.CountRecentUserStickerSetReports(ctx, reporterID)
	if err != nil {
		return fmt.Errorf("count recent sticker-set reports for reporter %d: %w", reporterID, err)
	}
	if count+int64(len(newTargets)) > UserStickerSetReportsPerHour {
		return ErrUserStickerSetReportRateLimited
	}
	for _, target := range newTargets {
		if err := qtx.InsertUserStickerSetReport(ctx, db.InsertUserStickerSetReportParams{
			ReporterID:      reporterID,
			SetID:           target.SetID,
			MessagePeerType: int16(peerType),
			MessagePeerID:   peerID,
			MessageID:       target.MessageID,
			Reason:          reason,
			Comment:         comment,
		}); err != nil {
			return fmt.Errorf("insert report for sticker set %d message %d: %w", target.SetID, target.MessageID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit user sticker-set report: %w", err)
	}
	return nil
}

// CreateUserStickerSetReportsForMessage preserves the single-message call for
// store consumers while sharing the multi-message transaction path.
func (s *Store) CreateUserStickerSetReportsForMessage(
	ctx context.Context,
	reporterID int64,
	setIDs []int64,
	peerType PeerType,
	peerID, messageID int64,
	reason, comment string,
) error {
	return s.CreateUserStickerSetReportsForMessages(ctx, reporterID, []UserStickerSetReportMessage{{
		MessageID: messageID,
		SetIDs:    setIDs,
	}}, peerType, peerID, reason, comment)
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

func (s *Store) AdminOpenUserStickerSetReportsForSets(
	ctx context.Context,
	setIDs []int64,
	pageSetID int64,
	offset, limit int32,
) ([]AdminUserStickerSetReport, error) {
	setIDs = sortedUniquePositiveIDs(setIDs)
	if pageSetID < 0 || offset < 0 || limit < 1 || limit > AdminUserStickerSetReportsPerGroupPageSize+1 ||
		(pageSetID == 0 && offset != 0) {
		return nil, ErrUserStickerSetReportInputInvalid
	}
	if len(setIDs) == 0 {
		return nil, nil
	}
	if pageSetID > 0 && !slices.Contains(setIDs, pageSetID) {
		return nil, ErrUserStickerSetReportInputInvalid
	}
	rows, err := s.q.AdminOpenUserStickerSetReportsForSets(ctx, db.AdminOpenUserStickerSetReportsForSetsParams{
		SetIds:      setIDs,
		PageSetID:   pageSetID,
		OffsetCount: offset,
		LimitCount:  limit,
	})
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
