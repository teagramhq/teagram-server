package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

var (
	ErrChannelPostSummarySchemaMissing  = errors.New("channel post summary schema is missing")
	ErrChannelPostSummaryMissing        = errors.New("channel post summaries are not initialized")
	ErrChannelPostSummaryWrongVersion   = errors.New("channel post summary version is unsupported")
	ErrChannelPostSummaryNotReady       = errors.New("channel post summaries are not ready")
	ErrChannelPostSummaryCorrupt        = errors.New("channel post summaries are inconsistent")
	ErrChannelPostSummaryChannelMissing = errors.New("channel state is missing for post summaries")
)

// ChannelPostSummaryReadiness distinguishes an initialized empty channel from
// absent or unusable derived state. Count-serving callers must require Ready.
type ChannelPostSummaryReadiness uint8

const (
	ChannelPostSummarySchemaUnavailable ChannelPostSummaryReadiness = iota
	ChannelPostSummaryMissing
	ChannelPostSummaryWrongVersion
	ChannelPostSummaryNotReady
	ChannelPostSummaryReady
)

// ChannelPostSummaryReadiness reports whether one channel can safely use its
// exact live-post summaries. It never initializes or repairs derived state.
func (s *Store) ChannelPostSummaryReadiness(ctx context.Context, channelID int64) (ChannelPostSummaryReadiness, error) {
	if channelID <= 0 {
		return ChannelPostSummarySchemaUnavailable, errors.New("channel post summary: channel ID must be positive")
	}

	installed, err := channelPostSummarySchemaInstalled(ctx, s.q)
	if err != nil {
		return ChannelPostSummarySchemaUnavailable, fmt.Errorf("check channel post summary schema: %w", err)
	}
	if !installed {
		return ChannelPostSummarySchemaUnavailable, nil
	}

	row, err := s.q.ChannelPostSummaryReadinessByChannel(ctx, channelID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ChannelPostSummaryMissing, nil
	case err != nil:
		return ChannelPostSummarySchemaUnavailable, fmt.Errorf("read channel post summary readiness: %w", err)
	case row.Version != 1:
		return ChannelPostSummaryWrongVersion, nil
	case !row.Ready:
		return ChannelPostSummaryNotReady, nil
	default:
		return ChannelPostSummaryReady, nil
	}
}

// ValidateChannelPostSummariesReady is the gate for code that consumes
// summaries. This stage deliberately leaves existing RPCs independent of it.
func (s *Store) ValidateChannelPostSummariesReady(ctx context.Context) error {
	installed, err := channelPostSummarySchemaInstalled(ctx, s.q)
	if err != nil {
		return fmt.Errorf("check channel post summary schema: %w", err)
	}
	if !installed {
		return ErrChannelPostSummarySchemaMissing
	}

	unavailable, err := s.q.ChannelPostSummaryUnavailableChannels(ctx)
	if err != nil {
		return fmt.Errorf("validate channel post summary readiness: %w", err)
	}
	if unavailable != 0 {
		return fmt.Errorf("%w: %d channels", ErrChannelPostSummaryNotReady, unavailable)
	}
	return nil
}

func channelPostUnreadSummaryCount(
	entitled, statusExists bool,
	version int16,
	ready bool,
	totalLive, authorLive int64,
) (int64, error) {
	if !entitled {
		return 0, ErrNotMember
	}
	if !statusExists {
		return 0, ErrChannelPostSummaryMissing
	}
	if version != 1 {
		return 0, ErrChannelPostSummaryWrongVersion
	}
	if !ready {
		return 0, ErrChannelPostSummaryNotReady
	}
	if totalLive < authorLive {
		return 0, fmt.Errorf("%w: author suffix exceeds total suffix", ErrChannelPostSummaryCorrupt)
	}
	return totalLive - authorLive, nil
}

func channelOwnerUnreadCount(row db.UnreadCountForOwnerRow) (int, error) {
	if row.UnavailableChannelCount != 0 {
		return 0, fmt.Errorf("%w: %d channel summaries unavailable", ErrChannelPostSummaryNotReady, row.UnavailableChannelCount)
	}
	if row.CorruptChannelCount != 0 {
		return 0, fmt.Errorf("%w: %d channel summaries inconsistent", ErrChannelPostSummaryCorrupt, row.CorruptChannelCount)
	}
	return int(row.UnreadCount), nil
}

func saturatedChannelPostUnreadCount(count int64) int {
	if count > 1000 {
		return 1000
	}
	return int(count)
}

// InitializeChannelPostSummaries rebuilds only derived nodes and readiness for
// one channel. The committed not-ready phase makes a failed rebuild unavailable
// until a later retry succeeds; writers continue to commit source rows and skip
// derived maintenance during that interval.
func (s *Store) InitializeChannelPostSummaries(ctx context.Context, channelID int64) error {
	if channelID <= 0 {
		return errors.New("initialize channel post summaries: channel ID must be positive")
	}
	installed, err := channelPostSummarySchemaInstalled(ctx, s.q)
	if err != nil {
		return fmt.Errorf("check channel post summary schema: %w", err)
	}
	if !installed {
		return ErrChannelPostSummarySchemaMissing
	}

	markTx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin channel post summary readiness update: %w", err)
	}
	defer func() { _ = markTx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qmark := s.q.WithTx(markTx)
	_, err = qmark.LockChannelStateForPostSummaryInitialization(ctx, channelID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: channel %d", ErrChannelPostSummaryChannelMissing, channelID)
	case err != nil:
		return fmt.Errorf("lock channel state for summary initialization: %w", err)
	}
	if err = qmark.MarkChannelPostSummariesNotReady(ctx, channelID); err != nil {
		return fmt.Errorf("mark channel post summaries not ready: %w", err)
	}
	if err = markTx.Commit(ctx); err != nil {
		return fmt.Errorf("commit channel post summary readiness update: %w", err)
	}

	populateTx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin channel post summary initialization: %w", err)
	}
	defer func() { _ = populateTx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	if err = s.q.WithTx(populateTx).InitializeChannelPostSummaries(ctx, channelID); err != nil {
		return fmt.Errorf("initialize channel post summaries: %w", err)
	}
	if err = populateTx.Commit(ctx); err != nil {
		return fmt.Errorf("commit channel post summary initialization: %w", err)
	}
	return nil
}

// ChannelPostUnreadCount returns the exact unsaturated count above marker for
// a currently entitled viewer. It uses only the authenticated member's author
// scope and at most 126 indexed summary-key lookups; it never reads history.
func (s *Store) ChannelPostUnreadCount(ctx context.Context, channelID, viewerID, marker int64) (int64, error) {
	if channelID <= 0 || viewerID <= 0 || marker < 0 {
		return 0, errors.New("channel post unread count: IDs must be positive and marker nonnegative")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return 0, fmt.Errorf("begin channel post unread snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit

	qtx := s.q.WithTx(tx)
	installed, err := channelPostSummarySchemaInstalled(ctx, qtx)
	if err != nil {
		return 0, fmt.Errorf("check channel post summary schema: %w", err)
	}
	if !installed {
		return 0, ErrChannelPostSummarySchemaMissing
	}

	row, err := qtx.ChannelPostUnreadSuffixCounts(ctx, db.ChannelPostUnreadSuffixCountsParams{
		ChannelID: channelID,
		ViewerID:  viewerID,
		Marker:    marker,
	})
	if err != nil {
		return 0, fmt.Errorf("count channel post unread suffix: %w", err)
	}
	unread, err := channelPostUnreadSummaryCount(
		row.Entitled,
		row.StatusExists,
		row.Version,
		row.Ready,
		row.TotalLive,
		row.AuthorLive,
	)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit channel post unread snapshot: %w", err)
	}
	return unread, nil
}

// ChannelReadMarker returns zero for a current member whose existing marker
// predates the additive marker table. A departed or currently banned member
// cannot read a marker through this method.
func (s *Store) ChannelReadMarker(ctx context.Context, channelID, viewerID int64) (int64, error) {
	if channelID <= 0 || viewerID <= 0 {
		return 0, errors.New("channel read marker: IDs must be positive")
	}
	installed, err := channelPostSummarySchemaInstalled(ctx, s.q)
	if err != nil {
		return 0, fmt.Errorf("check channel read marker schema: %w", err)
	}
	if !installed {
		return 0, ErrChannelPostSummarySchemaMissing
	}
	marker, err := s.q.ChannelReadMarkerForMember(ctx, db.ChannelReadMarkerForMemberParams{
		ChannelID: channelID,
		UserID:    viewerID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrNotMember
	case err != nil:
		return 0, fmt.Errorf("read channel read marker: %w", err)
	default:
		return marker, nil
	}
}

// AdvanceChannelReadMarker monotonically acknowledges committed channel IDs.
// It clamps to channel_state's committed allocator top and changes no events,
// pts or notifications.
func (s *Store) AdvanceChannelReadMarker(ctx context.Context, channelID, viewerID, requestedID int64) (int64, error) {
	if channelID <= 0 || viewerID <= 0 || requestedID < 0 {
		return 0, errors.New("advance channel read marker: IDs must be positive and requested ID nonnegative")
	}
	installed, err := channelPostSummarySchemaInstalled(ctx, s.q)
	if err != nil {
		return 0, fmt.Errorf("check channel read marker schema: %w", err)
	}
	if !installed {
		return 0, ErrChannelPostSummarySchemaMissing
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin channel read marker update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if _, err = qtx.LockChannel(ctx, channelID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotMember
		}
		return 0, fmt.Errorf("lock channel for read marker update: %w", err)
	}
	state, err := qtx.ChannelStateForUpdate(ctx, channelID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotMember
	}
	if err != nil {
		return 0, fmt.Errorf("lock channel state for read marker update: %w", err)
	}

	current, err := qtx.ChannelReadMarkerForMember(ctx, db.ChannelReadMarkerForMemberParams{
		ChannelID: channelID,
		UserID:    viewerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotMember
	}
	if err != nil {
		return 0, fmt.Errorf("read current channel marker: %w", err)
	}
	committedTop := state.NextLocalID - 1
	if committedTop < 0 {
		return 0, fmt.Errorf("channel %d has invalid committed post top", channelID)
	}
	next := min(requestedID, committedTop)
	if next <= current {
		if err = tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("commit unchanged channel read marker: %w", err)
		}
		return current, nil
	}
	if err = qtx.AdvanceChannelReadMarker(ctx, db.AdvanceChannelReadMarkerParams{
		ChannelID: channelID,
		UserID:    viewerID,
		ReadMaxID: next,
	}); err != nil {
		return 0, fmt.Errorf("advance channel read marker: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit channel read marker update: %w", err)
	}
	return next, nil
}

func channelPostSummarySchemaInstalled(ctx context.Context, q *db.Queries) (bool, error) {
	installed, err := q.ChannelPostSummarySchemaInstalled(ctx)
	return installed != nil && *installed, err
}

// ChannelPostExpiringSummaryMaxBatchSize bounds the channels rebuilt by one
// InitializeChannelPostExpiringSummaries call.
const ChannelPostExpiringSummaryMaxBatchSize = 1000

// ChannelPostExpiringSummaryPending returns how many ready channels still lack
// expiring-post nodes. Group history scans expired rows directly for each.
func (s *Store) ChannelPostExpiringSummaryPending(ctx context.Context) (int64, error) {
	pending, err := s.q.ChannelPostExpiringSummaryPendingCount(ctx)
	if err != nil {
		return 0, fmt.Errorf("count channels pending expiring summaries: %w", err)
	}
	return pending, nil
}

// InitializeChannelPostExpiringSummaries rebuilds the expiring-post nodes of up
// to batchSize ready channels that still lack them, one transaction per channel,
// and returns how many it rebuilt and how many channels remain pending. Group
// history uses the cheap expiry subtraction for a channel only after its
// rebuild, so the rollout is complete once remaining is zero with no error.
// A channel that fails is skipped for this run and reported in the joined
// error; progress and the pending count are returned either way.
func (s *Store) InitializeChannelPostExpiringSummaries(ctx context.Context, batchSize int) (initialized, remaining int64, err error) {
	if batchSize < 1 || batchSize > ChannelPostExpiringSummaryMaxBatchSize {
		return 0, 0, fmt.Errorf("initialize channel post expiring summaries: batch size must be between 1 and %d", ChannelPostExpiringSummaryMaxBatchSize)
	}
	installed, err := channelPostSummarySchemaInstalled(ctx, s.q)
	if err != nil {
		return 0, 0, fmt.Errorf("check channel post summary schema: %w", err)
	}
	if !installed {
		return 0, 0, ErrChannelPostSummarySchemaMissing
	}
	channelIDs, err := s.q.ChannelPostExpiringSummaryPendingChannels(ctx, int32(batchSize))
	if err != nil {
		return 0, 0, fmt.Errorf("list channels pending expiring summaries: %w", err)
	}
	var failures []error
	for _, channelID := range channelIDs {
		rebuilt, rebuildErr := s.initializeChannelPostExpiringSummaries(ctx, channelID)
		if rebuildErr != nil {
			failures = append(failures, rebuildErr)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if rebuilt {
			initialized++
		}
	}
	remaining, countErr := s.q.ChannelPostExpiringSummaryPendingCount(ctx)
	if countErr != nil {
		failures = append(failures, fmt.Errorf("count channels pending expiring summaries: %w", countErr))
	}
	if len(failures) > 0 {
		return initialized, remaining, fmt.Errorf("rebuilt %d channels, %d pending: %w", initialized, remaining, errors.Join(failures...))
	}
	return initialized, remaining, nil
}

func (s *Store) initializeChannelPostExpiringSummaries(ctx context.Context, channelID int64) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin channel %d expiring summary rebuild: %w", channelID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	rebuilt, err := s.q.WithTx(tx).InitializeChannelPostExpiringSummaries(ctx, channelID)
	if err != nil {
		return false, fmt.Errorf("rebuild channel %d expiring summaries: %w", channelID, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit channel %d expiring summary rebuild: %w", channelID, err)
	}
	return rebuilt, nil
}
