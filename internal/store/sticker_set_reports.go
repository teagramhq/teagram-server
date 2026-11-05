package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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

type userStickerSetReportTarget struct {
	SetID     int64
	MessageID int64
}

// userStickerSetReportMessage carries the server-resolved sets of one visible
// message selected in a report request.
type userStickerSetReportMessage struct {
	MessageID int64
	SetIDs    []int64
}

// UserStickerSetReportMessagesVisible reports whether every selected message is
// visible to the reporter. It is the read-only check for the early steps of
// the report flow; nothing is stored.
func (s *Store) UserStickerSetReportMessagesVisible(
	ctx context.Context,
	reporterID int64,
	peerType PeerType,
	peerID int64,
	messageIDs []int64,
) (bool, error) {
	_, visible, err := resolveUserStickerSetReportMessages(ctx, s.q, reporterID, peerType, peerID, messageIDs)
	return visible, err
}

// resolveUserStickerSetReportMessages resolves only message rows visible in the
// reporter's own message space or channel history. Set ids come from stored
// document references on those rows, never from the caller.
func resolveUserStickerSetReportMessages(
	ctx context.Context,
	q *db.Queries,
	reporterID int64,
	peerType PeerType,
	peerID int64,
	messageIDs []int64,
) ([]userStickerSetReportMessage, bool, error) {
	if reporterID <= 0 || peerID <= 0 || len(messageIDs) == 0 {
		return nil, false, ErrUserStickerSetReportInputInvalid
	}
	for _, messageID := range messageIDs {
		if messageID <= 0 {
			return nil, false, ErrUserStickerSetReportInputInvalid
		}
	}
	messageIDs = sortedUniquePositiveIDs(messageIDs)
	setIDsByMessage := make(map[int64][]int64, len(messageIDs))
	visible := make(map[int64]struct{}, len(messageIDs))
	switch peerType {
	case PeerTypeChannel:
		rows, err := q.UserStickerSetsInReadableChannelMessagesForReport(ctx, db.UserStickerSetsInReadableChannelMessagesForReportParams{
			ReporterID: reporterID,
			PeerID:     peerID,
			MessageIds: messageIDs,
		})
		if err != nil {
			return nil, false, fmt.Errorf("resolve channel messages for sticker-set report: %w", err)
		}
		for _, row := range rows {
			visible[row.LocalID] = struct{}{}
			if row.SetID != nil {
				setIDsByMessage[row.LocalID] = append(setIDsByMessage[row.LocalID], *row.SetID)
			}
		}
	case PeerTypeUser, PeerTypeChat:
		rows, err := q.UserStickerSetsInOwnMessagesForReport(ctx, db.UserStickerSetsInOwnMessagesForReportParams{
			ReporterID: reporterID,
			MessageIds: messageIDs,
			PeerType:   int16(peerType),
			PeerID:     peerID,
		})
		if err != nil {
			return nil, false, fmt.Errorf("resolve owned messages for sticker-set report: %w", err)
		}
		for _, row := range rows {
			visible[row.LocalID] = struct{}{}
			if row.SetID != nil {
				setIDsByMessage[row.LocalID] = append(setIDsByMessage[row.LocalID], *row.SetID)
			}
		}
	default:
		return nil, false, ErrUserStickerSetReportInputInvalid
	}
	if len(visible) != len(messageIDs) {
		return nil, false, nil
	}
	messages := make([]userStickerSetReportMessage, 0, len(messageIDs))
	for _, messageID := range messageIDs {
		messages = append(messages, userStickerSetReportMessage{
			MessageID: messageID,
			SetIDs:    sortedUniquePositiveIDs(setIDsByMessage[messageID]),
		})
	}
	return messages, true, nil
}

// CreateUserStickerSetReports files one report per live user set found in the
// selected messages. Visibility and set resolution run inside the same
// transaction as the insert, under the reporter lock, so the stored report
// reflects what the reporter could see when it was written. It returns false
// without storing anything when a selected message is not visible to the
// reporter. Identical open reports count once and the rolling-hour quota is
// enforced in the same transaction. A set taken down before the insert is
// skipped silently.
//
// Lock order: reporter owner advisory lock, then live set rows (FOR SHARE, by
// id), the same order message sends use.
func (s *Store) CreateUserStickerSetReports(
	ctx context.Context,
	reporterID int64,
	peerType PeerType,
	peerID int64,
	messageIDs []int64,
	reason, comment string,
) (bool, error) {
	if reporterID <= 0 || peerID <= 0 || len(messageIDs) == 0 || !validReportPeerType(peerType) ||
		!validUserStickerSetReportReason(reason) || len(comment) > 512 || !utf8.ValidString(comment) ||
		strings.ContainsRune(comment, '\x00') {
		return false, ErrUserStickerSetReportInputInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin user sticker-set report: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	// The reporter's owner advisory lock serializes the quota check, and it is
	// the lock LeaveChannel, SetChannelBan and RemoveChatUser hold while they
	// revoke membership, so the visibility read below cannot interleave with a
	// revocation: it either sees the revocation or finishes before it starts.
	// Those paths take their channel or chat row lock first and never wait on
	// this transaction for it, so taking only the owner lock adds no lock edge.
	if err = lockOwners(ctx, tx, reporterID); err != nil {
		return false, fmt.Errorf("lock report quota for reporter %d: %w", reporterID, err)
	}

	messages, visible, err := resolveUserStickerSetReportMessages(ctx, qtx, reporterID, peerType, peerID, messageIDs)
	if err != nil || !visible {
		return false, err
	}
	targets := make([]userStickerSetReportTarget, 0)
	setIDs := make([]int64, 0)
	for _, message := range messages {
		for _, setID := range message.SetIDs {
			targets = append(targets, userStickerSetReportTarget{SetID: setID, MessageID: message.MessageID})
			setIDs = append(setIDs, setID)
		}
	}
	setIDs = sortedUniquePositiveIDs(setIDs)
	if len(setIDs) == 0 {
		return true, nil
	}

	// Ordered set locks make concurrent reports involving several sets
	// deterministic.
	liveSetIDs, err := qtx.LockLiveUserStickerSetReportTargets(ctx, setIDs)
	if err != nil {
		return false, fmt.Errorf("lock live sticker-set report targets: %w", err)
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
		return true, nil
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
		return false, fmt.Errorf("read identical open sticker-set reports for reporter %d: %w", reporterID, err)
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
		return true, nil
	}
	count, err := qtx.CountRecentUserStickerSetReports(ctx, reporterID)
	if err != nil {
		return false, fmt.Errorf("count recent sticker-set reports for reporter %d: %w", reporterID, err)
	}
	if count+int64(len(newTargets)) > UserStickerSetReportsPerHour {
		return false, ErrUserStickerSetReportRateLimited
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
			return false, fmt.Errorf("insert report for sticker set %d message %d: %w", target.SetID, target.MessageID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit user sticker-set report: %w", err)
	}
	return true, nil
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
